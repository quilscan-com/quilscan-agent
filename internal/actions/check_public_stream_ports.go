package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"gopkg.in/yaml.v3"
)

type CheckPublicStreamPortsDeps struct {
	StatePath        string
	ManagedConfigDir string
	LoadState        func() (*config.State, error)
	ReadFile         func(string) ([]byte, error)
	ListenRunner     func(context.Context) (string, error)
	Now              func() time.Time
}

type publicStreamPortsReport struct {
	Ports     []publicStreamPort `json:"ports"`
	CheckedAt string             `json:"checked_at"`
}

type publicStreamPort struct {
	Protocol        string `json:"protocol"`
	Bind            string `json:"bind"`
	Port            int    `json:"port"`
	Source          string `json:"source"`
	ConfigMultiaddr string `json:"config_multiaddr,omitempty"`
	Process         string `json:"process,omitempty"`
}

type streamConfigHints struct {
	StreamPort      int
	StreamMultiaddr string
	ExcludedPorts   map[int]bool
}

func NewCheckPublicStreamPortsHandler(d CheckPublicStreamPortsDeps) Handler {
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "checking_public_stream_ports", Progress: 0.20})

		rawConfig, err := readNodeConfigForPortCheck(d)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		runner := d.ListenRunner
		if runner == nil {
			runner = runListenTCP
		}
		out, err := runner(context.Background())
		if err != nil {
			wrapped := fmt.Errorf("scan TCP listeners: %w", err)
			emit(Status{ID: c.ID, Step: "failed", Error: wrapped.Error()})
			return wrapped
		}

		now := time.Now().UTC()
		if d.Now != nil {
			now = d.Now().UTC()
		}
		report := publicStreamPortsReport{
			Ports:     detectPublicStreamPorts(rawConfig, out),
			CheckedAt: now.Format(time.RFC3339),
		}
		payload, err := json.Marshal(report)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0, Message: string(payload)})
		return nil
	}
}

func readNodeConfigForPortCheck(d CheckPublicStreamPortsDeps) ([]byte, error) {
	state, err := loadCheckPublicStreamPortsState(d)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	cfgDir := firstNonEmpty(state.ConfigPath, d.ManagedConfigDir)
	if cfgDir == "" {
		return nil, nil
	}
	readFile := d.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	raw, err := readFile(filepath.Join(filepath.Clean(cfgDir), "config.yml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config.yml: %w", err)
	}
	return raw, nil
}

func loadCheckPublicStreamPortsState(d CheckPublicStreamPortsDeps) (*config.State, error) {
	if d.LoadState != nil {
		return d.LoadState()
	}
	return config.LoadState(d.StatePath)
}

type listenCommandRunner func(context.Context, string, ...string) ([]byte, error)

func runListenTCP(ctx context.Context) (string, error) {
	return runListenTCPForGOOS(ctx, runtime.GOOS, execListenCommand)
}

func runListenTCPForGOOS(ctx context.Context, goos string, runner listenCommandRunner) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if runner == nil {
		runner = execListenCommand
	}
	name, args := listenTCPCommandForGOOS(goos)
	out, err := runner(ctx, name, args...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

func execListenCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func listenTCPCommandForGOOS(goos string) (string, []string) {
	if goos == "darwin" {
		return "lsof", []string{"-nP", "-iTCP", "-sTCP:LISTEN"}
	}
	return "ss", []string{"-H", "-ltnp"}
}

func detectPublicStreamPorts(rawConfig []byte, ssOutput string) []publicStreamPort {
	hints := parseStreamConfigHints(rawConfig)
	listeners := parsePublicNodeTCPListeners(ssOutput)
	if len(listeners) == 0 {
		return nil
	}
	for i := range listeners {
		if hints.StreamMultiaddr != "" {
			listeners[i].ConfigMultiaddr = hints.StreamMultiaddr
		}
	}
	if hints.StreamPort > 0 {
		return portsMatching(listeners, hints.StreamPort, "config")
	}
	if matches := portsMatching(listeners, 8340, "default"); len(matches) > 0 {
		return matches
	}
	out := make([]publicStreamPort, 0, len(listeners))
	for _, p := range listeners {
		if hints.ExcludedPorts[p.Port] {
			continue
		}
		p.Source = "detected"
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port == out[j].Port {
			return out[i].Bind < out[j].Bind
		}
		return out[i].Port < out[j].Port
	})
	return out
}

func portsMatching(listeners []publicStreamPort, port int, source string) []publicStreamPort {
	var out []publicStreamPort
	for _, p := range listeners {
		if p.Port != port {
			continue
		}
		p.Source = source
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bind < out[j].Bind })
	return out
}

func parseStreamConfigHints(raw []byte) streamConfigHints {
	hints := streamConfigHints{ExcludedPorts: map[int]bool{}}
	if len(raw) == 0 {
		return hints
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return hints
	}
	p2p := mapValue(root["p2p"])
	streamMA := firstStringValue(p2p, "streamListenMultiaddr", "stream_listen_multiaddr")
	if streamMA == "" {
		streamMA = firstStringValue(root, "p2p.streamListenMultiaddr", "p2p.stream_listen_multiaddr")
	}
	if port, ok := tcpPortFromMultiaddr(streamMA); ok {
		hints.StreamPort = port
		hints.StreamMultiaddr = streamMA
	}
	for _, ma := range []string{
		firstStringValue(root, "listenGrpcMultiaddr", "listen_grpc_multiaddr"),
		firstStringValue(root, "listenRESTMultiaddr", "listenRestMultiaddr", "listen_rest_multiaddr"),
	} {
		if port, ok := tcpPortFromMultiaddr(ma); ok {
			hints.ExcludedPorts[port] = true
		}
	}
	return hints
}

func parsePublicNodeTCPListeners(ssOutput string) []publicStreamPort {
	seen := map[string]bool{}
	var out []publicStreamPort
	for _, line := range strings.Split(ssOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !isQuilibriumNodeListenerLine(line) {
			continue
		}
		bind, port, ok := parseSSLocalAddress(line)
		if !ok || isLoopbackBind(bind) {
			continue
		}
		key := fmt.Sprintf("%s:%d", bind, port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, publicStreamPort{
			Protocol: "tcp",
			Bind:     bind,
			Port:     port,
			Process:  "quilibrium-node",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port == out[j].Port {
			return out[i].Bind < out[j].Bind
		}
		return out[i].Port < out[j].Port
	})
	return out
}

func isQuilibriumNodeListenerLine(line string) bool {
	if strings.Contains(line, "quilibrium-node") {
		return true
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	command := strings.Trim(fields[0], `"'`)
	return len(command) >= 8 && strings.HasPrefix("quilibrium-node", command)
}

func parseSSLocalAddress(line string) (string, int, bool) {
	for _, field := range strings.Fields(line) {
		bind, port, ok := splitHostPortLoose(field)
		if !ok {
			continue
		}
		if strings.HasSuffix(bind, ":") {
			continue
		}
		return bind, port, true
	}
	return "", 0, false
}

func splitHostPortLoose(value string) (string, int, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "users:(") {
		return "", 0, false
	}
	if strings.HasPrefix(value, "[") {
		end := strings.LastIndex(value, "]:")
		if end < 0 {
			return "", 0, false
		}
		port, ok := parsePort(value[end+2:])
		if !ok {
			return "", 0, false
		}
		return strings.Trim(value[1:end], "[]"), port, true
	}
	idx := strings.LastIndex(value, ":")
	if idx < 0 {
		return "", 0, false
	}
	port, ok := parsePort(value[idx+1:])
	if !ok {
		return "", 0, false
	}
	host := strings.Trim(value[:idx], "[]")
	if host == "" {
		host = "*"
	}
	return host, port, true
}

func parsePort(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > 65535 {
		return 0, false
	}
	return n, true
}

func isLoopbackBind(bind string) bool {
	bind = strings.Trim(strings.ToLower(bind), "[]")
	if bind == "localhost" || bind == "::1" {
		return true
	}
	if ip := net.ParseIP(bind); ip != nil {
		return ip.IsLoopback()
	}
	return strings.HasPrefix(bind, "127.")
}

func tcpPortFromMultiaddr(ma string) (int, bool) {
	parts := strings.Split(strings.Trim(ma, "/ "), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "tcp") {
			return parsePort(parts[i+1])
		}
	}
	return 0, false
}

func mapValue(v interface{}) map[string]interface{} {
	switch m := v.(type) {
	case map[string]interface{}:
		return m
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(m))
		for k, v := range m {
			if key, ok := k.(string); ok {
				out[key] = v
			}
		}
		return out
	default:
		return nil
	}
}

func firstStringValue(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
