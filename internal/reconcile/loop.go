// Package reconcile runs three background loops:
//
//  1. Verify (60s): confirm the recorded node install still matches reality —
//     binary present, service definition known, config dir on disk. Stamps
//     state.yaml's last_verified_at and last_started_at when verification
//     passes.
//
//  2. Worker-store du (5m): measure ${cfgDir}/worker-store size, cache it on
//     state.yaml, fold it into the unified node_status frame so the UI can
//     show it without waiting for the next 5-minute tick.
//
//  3. Latest-version poll (1h): GET https://releases.quilibrium.com/release,
//     compare against state.NodeVersion, fold into node_status as
//     node_update_available + latest_node_version.
//
// All loops swallow individual errors — they're best-effort observability,
// not critical path.
package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/nodeinfo"
	"github.com/quilscan-com/quilscan-agent/internal/nodeinstall"
	"github.com/quilscan-com/quilscan-agent/internal/peerinfo"
	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
	"gopkg.in/yaml.v3"
)

// Sender abstracts the WS client for testability.
type Sender interface {
	Send(v interface{}) error
}

// ServiceCtl is the narrow contract reconcile uses for is-active /
// started-at queries. Matches svcctl.Ctl so cmd/agent wires the
// platform-specific implementation in (systemd on Linux, launchd on
// macOS) without this package importing svcctl directly — keeps the
// import graph tidy and lets tests inject a fake.
type ServiceCtl interface {
	IsActive(name string) bool
	StartedAt(name string) time.Time
}

// Loop holds the dependencies for the reconcile goroutines.
type Loop struct {
	StatePath        string
	UnitName         string // node service identifier (systemd unit / launchd label)
	BinaryPath       string // node binary path
	ManagedConfigDir string
	UnitDir          string
	Sender           Sender

	// Agent paths surfaced to the Settings tab. Populated from
	// config.DefaultConfig in cmd/agent/main.go so the same loop emits
	// the right paths on Linux (/etc/...) vs macOS (~/Library/...).
	AgentBinaryPath     string
	AgentTokenPath      string
	AgentConfigYAMLPath string
	AgentAuditLogPath   string
	AgentServiceName    string // systemd unit name OR launchd label

	// NodeLogPath is the flat-file log target the launchd plist
	// redirects the node's stdout/stderr to on macOS (e.g.
	// ~/Library/Logs/quilibrium-node.log). Empty on Linux where
	// journalctl is the source of truth.
	NodeLogPath string

	// Svc is the service-manager probe used for IsActive / StartedAt.
	// When nil, runVerify falls back to a Linux-only systemctl path so
	// existing tests don't have to inject one.
	Svc ServiceCtl

	// Override cadences (mainly for tests). Zero values fall back to defaults.
	VerifyTick        time.Duration
	DuTick            time.Duration
	LatestVersionTick time.Duration

	// LatestVersionURL points at the source-of-truth release endpoint.
	// Defaults to https://releases.quilibrium.com/release.
	LatestVersionURL     string
	LatestVersionFetcher func(string) (string, error)

	NodeInfoRunner      func(context.Context, nodeinfo.RunRequest, time.Duration) (*nodeinfo.Info, error)
	PeerInfoRunner      func(context.Context, peerinfo.RunRequest, time.Duration) (int, bool, error)
	PeerIDFromJournaler func(context.Context, string, int, time.Duration) string

	// nodeStatus is the cumulative snapshot we publish. Each loop updates
	// its slice of keys and triggers a send.
	mu             sync.Mutex
	nodeStatus     map[string]interface{}
	lastConfigHash string // hash of last broadcast config.yml — gates re-emit
	lastFilesHash  string // hash of last broadcast system files — gates re-emit

	// verifyMu serialises calls to runVerify so the 60s ticker and manual
	// RunVerifyNow triggers cannot race on state.yaml writes / detection
	// reads. lastManualVerify enforces a small debounce so a user that
	// mashes the rescan button doesn't spawn back-to-back node-info
	// subprocesses.
	verifyMu         sync.Mutex
	lastManualVerify time.Time
}

// configReadLimit caps the bytes we'll ship per WS frame so a pathological
// config doesn't blow out the connection. Real Quil configs are <100KB.
const configReadLimit = 256 * 1024

const defaultLatestVersionURL = "https://releases.quilibrium.com/release"
const unknownPeerID = "--"

var nodeVersionPattern = regexp.MustCompile(`\b(?:node-)?([0-9]+(?:\.[0-9]+){3})(?:-[A-Za-z0-9_-]+)?\b`)

const rustNodeReleaseFloor = "2.1.0.23"

// Run blocks until ctx is cancelled, ticking each loop on its own schedule.
func (l *Loop) Run(ctx context.Context) {
	verifyTick := l.VerifyTick
	if verifyTick <= 0 {
		verifyTick = 60 * time.Second
	}
	duTick := l.DuTick
	if duTick <= 0 {
		// 60s: a worker-store on disk runs `du -sb` in <1s for typical
		// sizes (a few hundred MB to a few GB); 5min was too slow for
		// users who watch Storage tick up during initial sync. Override
		// via Loop.DuTick.
		duTick = 60 * time.Second
	}
	versionTick := l.LatestVersionTick
	if versionTick <= 0 {
		// 10 min: balances freshness (so a Quilibrium release shows up
		// in the UI within ~10 min) against polite cadence to the
		// upstream releases endpoint. Override via Loop.LatestVersionTick.
		versionTick = 10 * time.Minute
	}
	if l.LatestVersionURL == "" {
		l.LatestVersionURL = defaultLatestVersionURL
	}
	l.nodeStatus = map[string]interface{}{}

	// Run all three immediately on startup so a fresh agent doesn't have to
	// wait a full hour for first version-availability signal.
	l.verifyLocked()
	l.runDu()
	l.runVersionPoll()

	vt := time.NewTicker(verifyTick)
	defer vt.Stop()
	dt := time.NewTicker(duTick)
	defer dt.Stop()
	lt := time.NewTicker(versionTick)
	defer lt.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-vt.C:
			l.verifyLocked()
		case <-dt.C:
			l.runDu()
		case <-lt.C:
			l.runVersionPoll()
		}
	}
}

// verifyLocked is the mutex-guarded entry point used by both the 60s
// ticker and any other internal trigger. RunVerifyNow uses this same
// mutex but layers a debounce on top.
func (l *Loop) verifyLocked() {
	l.verifyMu.Lock()
	defer l.verifyMu.Unlock()
	l.runVerify()
}

// RunVerifyNow exposes a manual rescan trigger to handlers. The
// debounce stops a user mashing the rescan button from spawning
// back-to-back node-info / peer-info subprocesses; 3s is short enough
// to feel responsive yet long enough to bound the cost. Returns true
// when a fresh verify ran, false when the call was debounced.
func (l *Loop) RunVerifyNow() bool {
	l.verifyMu.Lock()
	defer l.verifyMu.Unlock()
	if !l.lastManualVerify.IsZero() && time.Since(l.lastManualVerify) < 3*time.Second {
		return false
	}
	l.lastManualVerify = time.Now()
	l.runVerify()
	// Also refresh the worker-store size on rescan so the Storage badge
	// reflects current disk usage immediately. Cheap (du -sb on a few
	// hundred MB → <1s).
	l.runDu()
	return true
}

// runVerify checks orthogonal install signals. If at least three pass we
// stamp last_verified_at; if the platform service manager reports the unit
// running we also pull last_started_at.
func (l *Loop) runVerify() {
	state, err := config.LoadState(l.StatePath)
	if err != nil {
		state = &config.State{}
	}
	detection := nodeinstall.Detect(nodeinstall.Paths{
		BinaryPath:       l.BinaryPath,
		ManagedConfigDir: l.managedConfigDir(),
		StatePath:        l.StatePath,
		UnitFilePath:     svcctl.UnitFilePath(l.UnitDir, l.UnitName),
		ProcessRunning:   processRunning("quilibrium-node"),
	})

	signals := 0
	if detection.HasNode {
		signals++
	}
	if _, err := os.Stat(state.ConfigPath); err == nil {
		signals++
	}
	if l.serviceActive(l.UnitName) {
		signals++
	}

	now := time.Now().UTC()
	if signals >= 3 && state.ConfigPath != "" {
		state.LastVerifiedAt = now
	}
	if started := l.serviceStartedAt(l.UnitName); !started.IsZero() {
		state.LastStartedAt = started.UTC()
	}
	nodePatch := map[string]interface{}{
		"install_source": state.InstallSource,
		"node_managed":   state.InstallSource != "migrated",
		"node_residues":  detection.Residues,
	}
	if detection.HasNode {
		nodePatch["node_running_workers"] = int64(0)
		nodePatch["node_active_workers"] = int64(0)
		nodePatch["node_connections"] = nil
		foundPeerID := ""
		info := l.readNodeInfo(state)
		rustNode := usesRustNodeCommands(state, info)
		if info != nil {
			if info.PeerID != "" && (!rustNode || isLegacyPeerID(info.PeerID)) {
				foundPeerID = info.PeerID
			}
			if info.Version != "" {
				nodePatch["node_info_version"] = info.Version
				nodePatch["current_node_version"] = info.Version
				if state.NodeVersion != info.Version {
					state.NodeVersion = info.Version
					_ = config.SaveState(l.StatePath, state)
				}
				if latest := l.statusString("latest_node_version"); latest != "" {
					nodePatch["latest_node_version"] = latest
					nodePatch["node_update_available"] = latest != info.Version
				}
			}
			nodePatch["node_running_workers"] = info.RunningWorkers
			nodePatch["node_active_workers"] = info.ActiveWorkers
		}
		if rustNode {
			if latest := l.statusString("latest_node_version"); latest != "" && state.NodeVersion != "" {
				nodePatch["latest_node_version"] = latest
				nodePatch["node_update_available"] = latest != state.NodeVersion
			}
			if status := l.readRuntimeStatusFromLogs(); status != nil {
				if status.TotalAllocations > 0 {
					nodePatch["node_running_workers"] = status.TotalAllocations
				}
				if status.FrameNumber > 0 {
					nodePatch["node_frame_height"] = status.FrameNumber
				}
				if status.Peers > 0 {
					nodePatch["node_connections"] = status.Peers
				}
			}
		}
		if foundPeerID == "" {
			if peerID := l.readPeerIDFromConfig(state); peerID != "" {
				foundPeerID = peerID
			}
		}
		if foundPeerID == "" {
			if peerID := l.readPeerIDFromJournal(); peerID != "" {
				foundPeerID = peerID
			}
		}
		// If both --node-info and the log scrape miss this tick (subprocess
		// timeout, recently rotated log, etc.), prefer the value we
		// previously persisted to state.yaml over the "--" placeholder.
		// The peer ID is stable for the lifetime of keys.yml so a cached
		// value can never become wrong — only stale by definition.
		if foundPeerID == "" && state.PeerID != "" && (!rustNode || isLegacyPeerID(state.PeerID)) {
			foundPeerID = state.PeerID
		}
		if rustNode && state.PeerID != "" && !isLegacyPeerID(state.PeerID) {
			state.PeerID = ""
		}
		if foundPeerID != "" {
			state.PeerID = foundPeerID
			nodePatch["peer_id"] = foundPeerID
		} else {
			nodePatch["peer_id"] = unknownPeerID
		}
		if !rustNode {
			if connections, ok := l.readPeerConnectionCount(state); ok {
				nodePatch["node_connections"] = connections
			}
		} else if nodePatch["node_connections"] == nil {
			nodePatch["node_connections"] = nil
		}
	} else {
		nodePatch["peer_id"] = unknownPeerID
		nodePatch["node_running_workers"] = int64(0)
		nodePatch["node_active_workers"] = int64(0)
		nodePatch["node_connections"] = nil
	}
	// Surface the node service start timestamp so the UI can show how long the
	// managed quilibrium-node has been running, separate from the agent-process
	// uptime that metrics frames already carry.
	if !state.LastStartedAt.IsZero() {
		nodePatch["node_started_at"] = state.LastStartedAt.UTC().Format(time.RFC3339)
	}
	if state.ConfigPath != "" || state.NodeVersion != "" || state.PeerID != "" {
		_ = config.SaveState(l.StatePath, state)
	}
	l.updateNodeStatus(nodePatch)
	if l.Sender != nil {
		meta := map[string]interface{}{
			"type":           "meta_update",
			"has_node":       detection.HasNode,
			"node_version":   state.NodeVersion,
			"install_source": state.InstallSource,
			"node_residues":  detection.Residues,
		}
		if peerID, _ := nodePatch["peer_id"].(string); peerID != "" {
			meta["peer_id"] = peerID
		}
		_ = l.Sender.Send(meta)
	}

	// Piggy-back: read node config.yml and broadcast (hash-gated so we don't
	// re-ship the same content every minute). The Settings tab on the my-nodes
	// page consumes this. Failure is silent — config might not exist yet on a
	// freshly installed node that hasn't done its first start.
	if state.ConfigPath != "" {
		l.broadcastConfigYAMLIfChanged(state.ConfigPath)
	}
	// Same idea for the surrounding agent + node service files. Lets the
	// Settings tab show every relevant path and (where safe) the file body.
	l.broadcastSystemFilesIfChanged(state.ConfigPath)
}

func (l *Loop) readNodeInfo(state *config.State) *nodeinfo.Info {
	runner := l.NodeInfoRunner
	if runner == nil {
		runner = nodeinfo.Run
	}
	req := nodeinfo.RunRequest{
		BinaryPath: l.BinaryPath,
		ConfigPath: l.nodeInfoConfigPath(state),
	}
	req.WorkDir = nodeCommandWorkDir(req.ConfigPath, l.managedConfigDir())
	info, err := runner(context.Background(), req, 8*time.Second)
	if err != nil {
		return nil
	}
	return info
}

func (l *Loop) nodeInfoConfigPath(state *config.State) string {
	if state != nil && state.ConfigPath != "" {
		return state.ConfigPath
	}
	cfgDir := l.managedConfigDir()
	if _, err := os.Stat(cfgDir); err == nil {
		return cfgDir
	}
	return ""
}

func (l *Loop) readPeerIDFromJournal() string {
	if l.PeerIDFromJournaler != nil {
		return l.PeerIDFromJournaler(context.Background(), l.UnitName, 500, 5*time.Second)
	}
	// macOS: the plist redirects stdout/err to NodeLogPath, so the
	// peer-id line lands in that file rather than the journal. Fall
	// through to journalctl on Linux where NodeLogPath is empty.
	if l.NodeLogPath != "" {
		return nodeinfo.PeerIDFromLogFile(context.Background(), l.NodeLogPath, 500, 5*time.Second)
	}
	return nodeinfo.PeerIDFromJournal(context.Background(), l.UnitName, 500, 5*time.Second)
}

func (l *Loop) readPeerIDFromConfig(state *config.State) string {
	cfg := l.nodeInfoConfigPath(state)
	if cfg == "" {
		return ""
	}
	return nodeinfo.PeerIDFromConfigDir(cfg)
}

func isLegacyPeerID(peerID string) bool {
	return strings.HasPrefix(peerID, "Qm")
}

func (l *Loop) readPeerConnectionCount(state *config.State) (int, bool) {
	runner := l.PeerInfoRunner
	if runner == nil {
		runner = peerinfo.Run
	}
	req := peerinfo.RunRequest{
		BinaryPath: l.BinaryPath,
		ConfigPath: l.nodeInfoConfigPath(state),
	}
	req.WorkDir = nodeCommandWorkDir(req.ConfigPath, l.managedConfigDir())
	count, ok, err := runner(context.Background(), req, 15*time.Second)
	if err != nil {
		return 0, false
	}
	return count, ok
}

func (l *Loop) readRuntimeStatusFromLogs() *nodeinfo.RuntimeStatus {
	if l.NodeLogPath != "" {
		return nodeinfo.RuntimeStatusFromLogFile(context.Background(), l.NodeLogPath, 1000, 5*time.Second)
	}
	return nodeinfo.RuntimeStatusFromJournal(context.Background(), l.UnitName, 1000, 5*time.Second)
}

func nodeCommandWorkDir(configPath, managedConfigDir string) string {
	if configPath != "" {
		return filepath.Dir(filepath.Clean(configPath))
	}
	if managedConfigDir != "" {
		return filepath.Dir(filepath.Clean(managedConfigDir))
	}
	return ""
}

func usesRustNodeCommands(state *config.State, info *nodeinfo.Info) bool {
	if info != nil && releaseVersionAtLeast(info.Version, rustNodeReleaseFloor) {
		return true
	}
	if state != nil && releaseVersionAtLeast(state.NodeVersion, rustNodeReleaseFloor) {
		return true
	}
	// Manual binary replacement can leave state.NodeVersion at the previous Go
	// release. Rust --node-info exposes Prover Address, so use it as a command
	// compatibility signal while still reporting the release version separately.
	return info != nil && info.ProverAddress != ""
}

func releaseVersionAtLeast(v, floor string) bool {
	a, okA := parseReleaseVersion(v)
	b, okB := parseReleaseVersion(floor)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

func parseReleaseVersion(v string) ([4]int, bool) {
	var out [4]int
	m := nodeVersionPattern.FindStringSubmatch(v)
	if len(m) != 2 {
		return out, false
	}
	parts := strings.Split(m[1], ".")
	if len(parts) != 4 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func (l *Loop) managedConfigDir() string {
	if l.ManagedConfigDir != "" {
		return l.ManagedConfigDir
	}
	return "/var/lib/quilscan/node/.config"
}

// systemFilesPaths are the canonical locations the Settings tab surfaces.
// broadcastSystemFilesIfChanged reads the agent + node configuration and
// service-definition files, plus the static path list, and merges everything
// into the node_status frame iff something changed since the last push.
// Reading is best-effort: a missing file just leaves the corresponding
// *_yaml / *_unit content empty while the path field is still reported.
//
// Path values come from the Loop fields wired by cmd/agent/main.go, which
// in turn pull from config.DefaultConfig() — that lets the same code emit
// /etc/quilscan-agent/... on Linux and ~/Library/Application Support/...
// on macOS without any GOOS branching here.
func (l *Loop) broadcastSystemFilesIfChanged(stateCfgPath string) {
	// svcctl.UnitFilePath resolves the platform-specific filename
	// (.service on Linux, .plist on macOS) so the Settings tab shows
	// the actual on-disk path the user would `cat` to inspect.
	nodeUnitPath := svcctl.UnitFilePath(l.UnitDir, l.UnitName)
	agentUnitPath := svcctl.UnitFilePath(l.UnitDir, l.AgentServiceName)

	patch := map[string]interface{}{
		"agent_binary_path":       l.AgentBinaryPath,
		"agent_token_path":        l.AgentTokenPath,
		"agent_config_yaml_path":  l.AgentConfigYAMLPath,
		"agent_state_yaml_path":   l.StatePath,
		"agent_audit_log_path":    l.AgentAuditLogPath,
		"agent_service_unit_path": agentUnitPath,
		"node_binary_path":        l.BinaryPath,
		"node_managed_config_dir": l.managedConfigDir(),
		"node_service_unit_path":  nodeUnitPath,
	}
	if stateCfgPath != "" {
		patch["node_config_dir"] = stateCfgPath
		patch["node_keys_path"] = filepath.Join(stateCfgPath, "keys.yml")
		patch["node_worker_store_dir"] = filepath.Join(stateCfgPath, "worker-store")
	}

	// Only paths are surfaced — token, state.yaml, service units, keys.yml,
	// audit log, agent + node config files, binaries. The agent never reads
	// or transmits their bodies. Frontend renders the path; users inspect
	// the file body locally if they need it. Notably keys.yml is never
	// opened on principle.
	h := sha256.New()
	for _, k := range []string{
		"agent_binary_path", "agent_token_path", "agent_config_yaml_path",
		"agent_state_yaml_path", "agent_audit_log_path", "agent_service_unit_path",
		"node_binary_path", "node_managed_config_dir", "node_service_unit_path",
		"node_config_dir", "node_keys_path", "node_worker_store_dir",
	} {
		if v, ok := patch[k].(string); ok {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write([]byte(v))
			h.Write([]byte{0})
		}
	}
	hash := hex.EncodeToString(h.Sum(nil))
	l.mu.Lock()
	same := hash == l.lastFilesHash
	if !same {
		l.lastFilesHash = hash
	}
	l.mu.Unlock()
	if same {
		return
	}
	l.updateNodeStatus(patch)
}

// broadcastConfigYAMLIfChanged reads ${cfgDir}/config.yml locally, derives
// the surfaced RPC state, and merges those fields into the node_status
// frame when they change. The yaml content itself never leaves the host —
// only the path and the parsed RPC settings — so backend / frontend get
// what they need to render the Settings tab without ever seeing the file
// body. Hashing is in-process only and gates re-emit.
func (l *Loop) broadcastConfigYAMLIfChanged(cfgDir string) {
	if cfgDir == "" {
		return
	}
	cfgFile := filepath.Join(cfgDir, "config.yml")
	f, err := os.Open(cfgFile)
	if err != nil {
		return // not yet generated, or permission issue — skip silently
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, configReadLimit))
	if err != nil {
		return
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	l.mu.Lock()
	same := hash == l.lastConfigHash
	if !same {
		l.lastConfigHash = hash
	}
	l.mu.Unlock()
	if same {
		return
	}
	rpcState := detectRPCConfig(b)
	patch := map[string]interface{}{
		"config_path":        cfgFile,
		"rpc_configured":     rpcState.Configured,
		"rpc_grpc_multiaddr": rpcState.GRPC,
		"rpc_rest_multiaddr": rpcState.REST,
	}
	if !rpcState.Configured {
		patch["rpc_config_hint"] = "Set listenGrpcMultiaddr and listenRESTMultiaddr in config.yml to enable local node data."
	}
	l.updateNodeStatus(patch)
}

type rpcConfigState struct {
	Configured bool
	GRPC       string
	REST       string
}

func detectRPCConfig(raw []byte) rpcConfigState {
	root := map[string]interface{}{}
	_ = yaml.Unmarshal(raw, &root)
	grpc := stringValue(root["listenGrpcMultiaddr"])
	rest := stringValue(root["listenRESTMultiaddr"])
	return rpcConfigState{
		Configured: grpc != "" && rest != "",
		GRPC:       grpc,
		REST:       rest,
	}
}

func stringValue(v interface{}) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", v))
}

// runDu measures worker-store size and folds it into the node_status snapshot.
// ~tens of GB on a real node, so infrequent.
func (l *Loop) runDu() {
	state, err := config.LoadState(l.StatePath)
	if err != nil || state.ConfigPath == "" {
		return
	}
	target := filepath.Join(state.ConfigPath, "worker-store")
	if _, err := os.Stat(target); err != nil {
		return
	}
	size, err := dirSize(target)
	if err != nil {
		return
	}
	state.WorkerStoreBytes = size
	state.WorkerStoreMeasuredAt = time.Now().UTC()
	_ = config.SaveState(l.StatePath, state)

	l.updateNodeStatus(map[string]interface{}{
		"worker_store_path": target,
		"node_disk_bytes":   size,
		"node_disk_sub":     humanBytes(uint64(size)),
		"measured_at":       state.WorkerStoreMeasuredAt.Format(time.RFC3339),
	})
}

// runVersionPoll fetches the canonical "what's the latest node version" string
// and folds it into node_status. Frontend uses node_update_available as the
// banner trigger.
func (l *Loop) runVersionPoll() {
	state, err := config.LoadState(l.StatePath)
	if err != nil {
		return
	}
	current := state.NodeVersion
	if observed := l.statusString("node_info_version"); observed != "" {
		current = observed
	}
	fetcher := l.LatestVersionFetcher
	if fetcher == nil {
		fetcher = fetchLatestVersion
	}
	latest, err := fetcher(l.LatestVersionURL)
	if err != nil || latest == "" {
		return
	}
	available := current != "" && latest != current
	l.updateNodeStatus(map[string]interface{}{
		"current_node_version":  current,
		"latest_node_version":   latest,
		"node_update_available": available,
		"version_polled_at":     time.Now().UTC().Format(time.RFC3339),
	})
}

// PatchNodeStatus is the exported entry point so command handlers (e.g.
// update_node) can fold authoritative facts into the cached snapshot
// immediately after the action completes, instead of waiting up to an hour
// for the next runVersionPoll tick.
func (l *Loop) PatchNodeStatus(patch map[string]interface{}) {
	l.updateNodeStatus(patch)
}

// updateNodeStatus merges the provided keys into the cached snapshot and
// publishes the full union. Doing it this way keeps the frontend reading
// snapshot.node_status as a single object regardless of which loop just
// fired.
func (l *Loop) updateNodeStatus(patch map[string]interface{}) {
	l.mu.Lock()
	if l.nodeStatus == nil {
		// Defensive: PatchNodeStatus can be called by a command handler
		// before Run() has executed line 103 (goroutine scheduling race).
		// Without this guard, writing to a nil map panics.
		l.nodeStatus = map[string]interface{}{}
	}
	for k, v := range patch {
		l.nodeStatus[k] = v
	}
	out := map[string]interface{}{"type": "node_status"}
	for k, v := range l.nodeStatus {
		out[k] = v
	}
	l.mu.Unlock()
	if l.Sender != nil {
		_ = l.Sender.Send(out)
	}
}

func (l *Loop) statusString(key string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodeStatus == nil {
		return ""
	}
	v, _ := l.nodeStatus[key].(string)
	return v
}

func dirSize(path string) (int64, error) {
	out, err := exec.Command("du", "-sb", path).Output()
	if err != nil {
		// Fallback for BSD/macOS where du -sb is missing
		out, err = exec.Command("du", "-sk", path).Output()
		if err != nil {
			return 0, err
		}
		fields := strings.Fields(string(out))
		if len(fields) == 0 {
			return 0, fmt.Errorf("empty du output")
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty du output")
	}
	return strconv.ParseInt(fields[0], 10, 64)
}

// serviceActive and serviceStartedAt route service-state queries
// through the platform-aware svcctl. Lazily defaults to svcctl.New()
// so tests that exercise runVerify directly (without going through
// Run()) don't have to inject one explicitly.
func (l *Loop) ctl() ServiceCtl {
	if l.Svc == nil {
		l.Svc = svcctl.New()
	}
	return l.Svc
}
func (l *Loop) serviceActive(unit string) bool         { return l.ctl().IsActive(unit) }
func (l *Loop) serviceStartedAt(unit string) time.Time { return l.ctl().StartedAt(unit) }

func processRunning(name string) bool {
	return exec.Command("pgrep", "-x", name).Run() == nil
}

func fetchLatestVersion(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	return parseLatestVersion(string(b)), nil
}

func parseLatestVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	matches := nodeVersionPattern.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return raw
	}
	latest := matches[0][1]
	for _, match := range matches[1:] {
		if compareVersions(match[1], latest) > 0 {
			latest = match[1]
		}
	}
	return latest
}

func compareVersions(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		ai, _ := strconv.Atoi(ap[i])
		bi, _ := strconv.Atoi(bp[i])
		if ai > bi {
			return 1
		}
		if ai < bi {
			return -1
		}
	}
	if len(ap) > len(bp) {
		return 1
	}
	if len(ap) < len(bp) {
		return -1
	}
	return 0
}

func humanBytes(b uint64) string {
	const k = 1024.0
	f := float64(b)
	switch {
	case f < k:
		return fmt.Sprintf("%d B", b)
	case f < k*k:
		return fmt.Sprintf("%.1f KB", f/k)
	case f < k*k*k:
		return fmt.Sprintf("%.1f MB", f/(k*k))
	case f < k*k*k*k:
		return fmt.Sprintf("%.1f GB", f/(k*k*k))
	default:
		return fmt.Sprintf("%.1f TB", f/(k*k*k*k))
	}
}
