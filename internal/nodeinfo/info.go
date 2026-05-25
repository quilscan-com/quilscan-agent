// Package nodeinfo runs `${binary} --node-info` from the config parent
// directory when possible and parses the labelled output.
// Sample:
//
//	signature check passed
//	Peer ID: QmeH9VMULDg1FizAxUWFwk2Fby85jMQMVhAUQ4JP98kHeq
//	Version: 2.1.0.22
//	Seniority: 0
//	Running Workers: 44
//	Active Workers: 0
//
// The `signature check passed` log line is emitted by the node binary itself
// before stdout starts; we ignore everything that doesn't match a "Field: Value"
// shape so noise doesn't trip the parser.
package nodeinfo

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Info is the parsed --node-info output.
type Info struct {
	PeerID         string
	ProverAddress  string
	Version        string
	Seniority      int64
	RunningWorkers int64
	ActiveWorkers  int64
	FrameNumber    int64
}

// RunRequest describes how the node binary should be invoked. WorkDir should
// be the parent of the running node's .config directory; if WorkDir is empty,
// ConfigPath is passed as --config.
type RunRequest struct {
	BinaryPath string
	ConfigPath string
	WorkDir    string
}

// Run executes the binary with --node-info under the given timeout and parses
// the result. Any unparseable lines are silently skipped — the binary's own
// log output (zerolog JSON) goes to stderr but newer versions sometimes leak
// onto stdout; we don't want that to fail us. If the command exits non-zero
// after printing useful labelled fields, Run still returns the partial info.
func Run(ctx context.Context, req RunRequest, timeout time.Duration) (*Info, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, args := commandForRequest(ctx, req)
	// Quilibrium's --node-info splits its output across both streams:
	// `Peer ID:` and `Version:` go to stderr (alongside the JSON log
	// "signature check passed"), while `Seniority`, `Running Workers`,
	// `Active Workers`, and `Shard Allocations:` go to stdout. Using
	// cmd.Output() (stdout-only) silently swallows peer_id and version,
	// which is exactly the symptom we hit on Mac. CombinedOutput
	// captures both — Parse() is already line-based and tolerates the
	// JSON log line.
	out, err := cmd.CombinedOutput()
	if err != nil {
		if info, parseErr := parse(string(out), false); parseErr == nil && info.hasAny() {
			return info, nil
		}
		return nil, fmt.Errorf("run %s %s: %w", req.BinaryPath, strings.Join(args, " "), err)
	}
	return Parse(string(out))
}

func commandForRequest(ctx context.Context, req RunRequest) (*exec.Cmd, []string) {
	args := []string{"--node-info"}
	if req.WorkDir == "" && req.ConfigPath != "" {
		args = append(args, "--config", req.ConfigPath)
	}
	cmd := exec.CommandContext(ctx, req.BinaryPath, args...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	return cmd, args
}

// Parse pulls structured fields out of combined stdout/stderr and requires a
// Version field. Run uses the same parser in a partial mode when a subprocess
// exits non-zero after printing useful fields.
func Parse(out string) (*Info, error) {
	return parse(out, true)
}

func parse(out string, requireVersion bool) (*Info, error) {
	info := &Info{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "Peer ID":
			info.PeerID = val
		case "Prover Address":
			info.ProverAddress = val
		case "Version":
			info.Version = val
		case "Seniority":
			info.Seniority = atoi64(val)
		case "Running Workers":
			info.RunningWorkers = atoi64(val)
		case "Active Workers":
			info.ActiveWorkers = atoi64(val)
		case "Frame Number":
			info.FrameNumber = atoi64(val)
		}
	}
	if err := sc.Err(); err != nil {
		return info, err
	}
	if requireVersion && info.Version == "" {
		return info, fmt.Errorf("Version field not found in --node-info output")
	}
	return info, nil
}

func (i *Info) hasAny() bool {
	return i != nil &&
		(i.PeerID != "" ||
			i.ProverAddress != "" ||
			i.Version != "" ||
			i.Seniority != 0 ||
			i.RunningWorkers != 0 ||
			i.ActiveWorkers != 0)
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// PeerIDFromConfigDir derives the Go-compatible libp2p peer ID from the Rust
// node's p2p.peerPrivKey config field. The config stores 114 bytes as
// [57-byte seed][57-byte Ed448 public key]; only the public half is used here.
// Seed-only configs cannot be derived without Ed448 support.
func PeerIDFromConfigDir(configDir string) string {
	if configDir == "" {
		return ""
	}
	path := configDir
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, "config.yml")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return PeerIDFromConfigYAML(b)
}

func PeerIDFromConfigYAML(raw []byte) string {
	var cfg struct {
		P2P struct {
			PeerPrivKey      string `yaml:"peerPrivKey"`
			PeerPrivKeySnake string `yaml:"peer_priv_key"`
		} `yaml:"p2p"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	keyHex := strings.TrimSpace(cfg.P2P.PeerPrivKey)
	if keyHex == "" {
		keyHex = strings.TrimSpace(cfg.P2P.PeerPrivKeySnake)
	}
	return PeerIDFromPeerPrivKeyHex(keyHex)
}

func PeerIDFromPeerPrivKeyHex(keyHex string) string {
	raw, err := hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil || len(raw) < 114 {
		return ""
	}
	pub := raw[57:114]

	proto := make([]byte, 0, 4+len(pub))
	proto = append(proto, 0x08, 0x04, 0x12, byte(len(pub)))
	proto = append(proto, pub...)
	sum := sha256.Sum256(proto)

	multihash := make([]byte, 0, 34)
	multihash = append(multihash, 0x12, 0x20)
	multihash = append(multihash, sum[:]...)
	return base58BTCEncode(multihash)
}

func base58BTCEncode(raw []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	n := new(big.Int).SetBytes(raw)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)

	var out []byte
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, b := range raw {
		if b != 0 {
			break
		}
		out = append(out, alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// ParsePeerConnectionsFromLogs extracts the latest Rust node status `peers`
// value from the node's status logs.
func ParsePeerConnectionsFromLogs(raw string) (int, bool) {
	sc := bufio.NewScanner(strings.NewReader(raw))
	latest := 0
	ok := false
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "node status") {
			continue
		}
		payload := trailingJSON(line)
		if payload == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			continue
		}
		peers, hasPeers := jsonInt(entry, "peers")
		if !hasPeers || peers < 0 {
			continue
		}
		latest = int(peers)
		ok = true
	}
	return latest, ok
}

func PeerConnectionsFromJournal(ctx context.Context, unitName string, lines int, timeout time.Duration) (int, bool) {
	if unitName == "" {
		unitName = "quilibrium-node.service"
	}
	if lines <= 0 {
		lines = 1000
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", "-u", unitName, "-n", strconv.Itoa(lines), "--no-pager", "-o", "short-iso").Output()
	if err != nil {
		return 0, false
	}
	return ParsePeerConnectionsFromLogs(string(out))
}

func PeerConnectionsFromLogFile(ctx context.Context, logPath string, lines int, timeout time.Duration) (int, bool) {
	if logPath == "" {
		return 0, false
	}
	if lines <= 0 {
		lines = 1000
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tail", "-n", strconv.Itoa(lines), logPath).Output()
	if err != nil {
		return 0, false
	}
	return ParsePeerConnectionsFromLogs(string(out))
}

func trailingJSON(line string) string {
	idx := strings.LastIndexByte(line, '{')
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(line[idx:])
}

func jsonInt(entry map[string]interface{}, key string) (int64, bool) {
	v, ok := entry[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}
