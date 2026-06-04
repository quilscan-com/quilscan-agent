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
//     node_update_available + latest_node_version. The same cadence also
//     polls qclient-release when qclient is installed.
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
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
	"github.com/quilscan-com/quilscan-agent/internal/qclient"
	"github.com/quilscan-com/quilscan-agent/internal/release"
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
	StatePath         string
	UnitName          string // node service identifier (systemd unit / launchd label)
	BinaryPath        string // node binary path
	QClientBinaryPath string
	ManagedConfigDir  string
	UnitDir           string
	Platform          string // release platform suffix, e.g. linux-amd64
	Sender            Sender

	// Agent paths surfaced to the Settings tab. Populated from
	// config.DefaultConfig in cmd/agent/main.go so the same loop emits
	// the right paths on Linux (/etc/...) vs macOS (~/Library/...).
	AgentBinaryPath     string
	AgentTokenPath      string
	AgentConfigYAMLPath string
	AgentAuditLogPath   string
	AgentServiceName    string // systemd unit name OR launchd label
	NodeLogPath         string // macOS node log file; Linux uses journalctl

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
	LatestVersionURL            string
	LatestVersionFetcher        func(string) (string, error)
	LatestQClientVersionURL     string
	LatestQClientVersionFetcher func(string, string) (string, error)
	NodeManifestURL             string
	NodeManifestFetcher         func(string) (*nodemanifest.Manifest, error)
	OfficialArtifactsURL        string
	OfficialArtifactsFetcher    func(string) (*nodemanifest.OfficialArtifacts, error)

	NodeInfoRunner           func(context.Context, nodeinfo.RunRequest, time.Duration) (*nodeinfo.Info, error)
	QClientStatusRunner      func(context.Context, qclient.RunRequest, time.Duration) (*qclient.ProverStatus, error)
	PeerConnectionsLogReader func(context.Context, string, string, int, time.Duration) (int, bool)

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
const defaultLatestQClientVersionURL = "https://releases.quilibrium.com/qclient-release"
const defaultNodeManifestURL = nodemanifest.DefaultURL
const defaultOfficialArtifactsURL = nodemanifest.DefaultOfficialArtifactsURL
const unknownPeerID = "--"

var nodeVersionPattern = regexp.MustCompile(`\b(?:node-)?([0-9]+(?:\.[0-9]+){3})(?:-[A-Za-z0-9_-]+)?\b`)

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
	if l.LatestQClientVersionURL == "" {
		l.LatestQClientVersionURL = defaultLatestQClientVersionURL
	}
	if l.NodeManifestURL == "" {
		l.NodeManifestURL = defaultNodeManifestURL
	}
	if l.OfficialArtifactsURL == "" {
		l.OfficialArtifactsURL = defaultOfficialArtifactsURL
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
		BinaryPath:        l.BinaryPath,
		ManagedConfigDir:  l.managedConfigDir(),
		RecordedConfigDir: state.ConfigPath,
		StatePath:         l.StatePath,
		UnitFilePath:      svcctl.UnitFilePath(l.UnitDir, l.UnitName),
		ProcessRunning:    processRunning("quilibrium-node"),
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
	qclientInstalled := l.qclientInstalled()
	nodePatch := map[string]interface{}{
		"install_source":      state.InstallSource,
		"node_managed":        detection.HasNode && state.InstallSource != "migrated",
		"node_residues":       detection.Residues,
		"has_qclient":         qclientInstalled,
		"qclient_binary_path": l.qclientBinaryPath(),
	}
	if !qclientInstalled {
		nodePatch["qclient_status"] = "not_installed"
	}
	if detection.HasNode {
		mergePatch(nodePatch, l.refreshNodeManifestState(state, now))
		nodePatch["node_running_workers"] = int64(0)
		nodePatch["node_active_workers"] = int64(0)
		nodePatch["node_connections"] = nil
		nodePatch["prover_address"] = ""
		foundPeerID := ""
		info := l.readNodeInfo(state)
		if info != nil {
			if info.PeerID != "" && isLegacyPeerID(info.PeerID) {
				foundPeerID = info.PeerID
			}
			if proverAddress := normalizeProverAddress(info.ProverAddress); proverAddress != "" {
				nodePatch["prover_address"] = proverAddress
			}
			if info.Version != "" {
				nodePatch["node_info_version"] = info.Version
				nodePatch["current_node_version"] = info.Version
				if state.NodeVersion != info.Version {
					state.NodeVersion = info.Version
					_ = config.SaveState(l.StatePath, state)
				}
				if state.NodeSource != nodemanifest.SourceDev {
					if latest := l.statusString("latest_node_version"); latest != "" {
						nodePatch["latest_node_version"] = latest
						nodePatch["node_update_available"] = releaseVersionNewerThan(latest, info.Version)
					}
				}
			}
			nodePatch["node_running_workers"] = info.RunningWorkers
			nodePatch["node_active_workers"] = info.ActiveWorkers
			if info.FrameNumber > 0 {
				nodePatch["node_frame_height"] = info.FrameNumber
			}
		}
		if qclientInstalled {
			if qstatus := l.readQClientProverStatus(state); qstatus != nil {
				applyQClientStatus(nodePatch, qstatus)
				if qstatus.PeerID != "" && isLegacyPeerID(qstatus.PeerID) {
					foundPeerID = qstatus.PeerID
				}
				if qstatus.Version != "" {
					nodePatch["node_info_version"] = qstatus.Version
					nodePatch["current_node_version"] = qstatus.Version
					if state.NodeVersion != qstatus.Version {
						state.NodeVersion = qstatus.Version
						_ = config.SaveState(l.StatePath, state)
					}
					if state.NodeSource != nodemanifest.SourceDev {
						if latest := l.statusString("latest_node_version"); latest != "" {
							nodePatch["latest_node_version"] = latest
							nodePatch["node_update_available"] = releaseVersionNewerThan(latest, qstatus.Version)
						}
					}
				}
				if qstatus.RunningWorkers > 0 {
					nodePatch["node_running_workers"] = qstatus.RunningWorkers
				}
				if qstatus.LastReceived > 0 {
					nodePatch["node_frame_height"] = qstatus.LastReceived
				}
			} else {
				nodePatch["qclient_status"] = "unavailable"
			}
		}
		if foundPeerID == "" {
			if peerID := l.readPeerIDFromConfig(state); peerID != "" {
				foundPeerID = peerID
			}
		}
		// If --node-info and config parsing miss this tick (subprocess timeout,
		// incomplete config, etc.), prefer the value we previously persisted to
		// state.yaml over the "--" placeholder.
		// The peer ID is stable for the lifetime of keys.yml so a cached
		// value can never become wrong — only stale by definition.
		if foundPeerID == "" && state.PeerID != "" && isLegacyPeerID(state.PeerID) {
			foundPeerID = state.PeerID
		}
		if state.PeerID != "" && !isLegacyPeerID(state.PeerID) {
			state.PeerID = ""
		}
		if foundPeerID != "" {
			state.PeerID = foundPeerID
			nodePatch["peer_id"] = foundPeerID
		} else {
			nodePatch["peer_id"] = unknownPeerID
		}
		if connections, ok := l.readPeerConnectionCountFromLogs(); ok {
			nodePatch["node_connections"] = connections
		} else if nodePatch["node_connections"] == nil {
			nodePatch["node_connections"] = nil
		}
	} else {
		nodePatch["peer_id"] = unknownPeerID
		nodePatch["prover_address"] = ""
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
			"type":                        "meta_update",
			"has_node":                    detection.HasNode,
			"has_qclient":                 qclientInstalled,
			"qclient_version":             state.QClientVersion,
			"qclient_binary_path":         l.qclientBinaryPath(),
			"node_version":                state.NodeVersion,
			"node_source":                 state.NodeSource,
			"installed_node_version":      state.InstalledNodeVersion,
			"node_base_version":           state.NodeBaseVersion,
			"node_build_number":           state.NodeBuildNumber,
			"node_binary_sha256":          state.NodeBinarySHA256,
			"node_manifest_url":           state.NodeManifestURL,
			"dev_node_signature_verified": state.DevNodeSignatureVerified,
			"install_source":              state.InstallSource,
			"node_residues":               detection.Residues,
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

func (l *Loop) refreshNodeManifestState(state *config.State, now time.Time) map[string]interface{} {
	patch := map[string]interface{}{}
	sha, err := nodemanifest.HashFile(l.BinaryPath)
	if err != nil {
		return patch
	}
	previousSHA := state.NodeBinarySHA256
	if previousSHA != "" && !strings.EqualFold(previousSHA, sha) {
		state.DevNodeSignatureVerified = false
	}
	state.NodeBinarySHA256 = sha
	patch["node_binary_sha256"] = sha

	officialFetched := false
	officialMatch := nodemanifest.Match{}
	officialMatched := false
	if match, ok, fetched := l.matchOfficialArtifact(sha); ok {
		officialMatch = match
		officialMatched = true
	} else if fetched {
		officialFetched = true
	}

	manifestURL := l.NodeManifestURL
	if manifestURL == "" {
		manifestURL = defaultNodeManifestURL
	}
	fetcher := l.NodeManifestFetcher
	if fetcher == nil {
		fetcher = nodemanifest.Fetch
	}
	manifest, err := fetcher(manifestURL)
	devFetched := err == nil && manifest != nil
	if err != nil || manifest == nil {
		if officialMatched {
			applyNodeManifestMatch(state, officialMatch)
			patchNodeSourceFromState(patch, state)
			return patch
		}
		if !officialFetched && state.NodeSource == nodemanifest.SourceReleases {
			patchNodeSourceFromState(patch, state)
			return patch
		}
		if state.NodeSource == nodemanifest.SourceDev {
			patchNodeSourceFromState(patch, state)
			return patch
		}
		state.NodeSource = nodemanifest.SourceUnknown
		state.InstalledNodeVersion = ""
		state.NodeBaseVersion = ""
		state.NodeBuildNumber = 0
		patchNodeSourceFromState(patch, state)
		patch["node_update_available"] = false
		return patch
	}

	state.NodeManifestURL = manifestURL
	state.NodeManifestCheckedAt = now
	patch["node_manifest_url"] = manifestURL
	patch["node_manifest_checked_at"] = now.Format(time.RFC3339)

	if latest, artifact, ok := manifest.LatestDev(l.Platform); ok {
		state.LatestDevNodeVersion = latest.Version
		state.LatestDevNodeURL = artifact.URL
		state.LatestDevNodeSHA256 = artifact.SHA256
		state.LatestDevNodeBuildNumber = latest.BuildNumber
		patch["latest_dev_node_version"] = latest.Version
		patch["latest_dev_node_url"] = artifact.URL
		patch["latest_dev_node_sha256"] = artifact.SHA256
		patch["latest_dev_node_build_number"] = latest.BuildNumber
	}

	if officialMatched {
		applyNodeManifestMatch(state, officialMatch)
		patchNodeSourceFromState(patch, state)
		return patch
	}

	match, ok := manifest.MatchDev(l.Platform, sha)
	if !ok {
		if !officialFetched && state.NodeSource == nodemanifest.SourceReleases {
			patchNodeSourceFromState(patch, state)
			return patch
		}
		if !devFetched && state.NodeSource == nodemanifest.SourceDev {
			patchNodeSourceFromState(patch, state)
			return patch
		}
		state.NodeSource = nodemanifest.SourceUnknown
		state.InstalledNodeVersion = ""
		state.NodeBaseVersion = ""
		state.NodeBuildNumber = 0
		patchNodeSourceFromState(patch, state)
		patch["node_update_available"] = false
		return patch
	}

	applyNodeManifestMatch(state, match)
	patchNodeSourceFromState(patch, state)
	if match.Source == nodemanifest.SourceDev {
		updateAvailable, latest := manifest.DevUpdate(match, l.Platform)
		patch["latest_node_version"] = latest.Version
		patch["node_update_source"] = nodemanifest.SourceDev
		patch["node_update_available"] = updateAvailable
	}
	return patch
}

func (l *Loop) matchOfficialArtifact(sha string) (nodemanifest.Match, bool, bool) {
	url := l.OfficialArtifactsURL
	if strings.TrimSpace(url) == "" {
		url = defaultOfficialArtifactsURL
	}
	fetcher := l.OfficialArtifactsFetcher
	if fetcher == nil {
		fetcher = nodemanifest.FetchOfficialArtifacts
	}
	artifacts, err := fetcher(url)
	if err != nil || artifacts == nil {
		return nodemanifest.Match{}, false, false
	}
	match, ok := artifacts.Match(l.Platform, sha)
	return match, ok, true
}

func applyNodeManifestMatch(state *config.State, match nodemanifest.Match) {
	state.NodeSource = match.Source
	state.InstalledNodeVersion = match.Version
	state.NodeBaseVersion = match.BaseVersion
	state.NodeBuildNumber = match.BuildNumber
	state.DevNodeSignatureVerified = match.Source == nodemanifest.SourceDev && state.DevNodeSignatureVerified
	if state.NodeBaseVersion == "" && match.Source == nodemanifest.SourceReleases {
		state.NodeBaseVersion = match.Version
	}
}

func patchNodeSourceFromState(patch map[string]interface{}, state *config.State) {
	patch["node_source"] = state.NodeSource
	patch["installed_node_version"] = state.InstalledNodeVersion
	patch["node_base_version"] = state.NodeBaseVersion
	patch["node_build_number"] = state.NodeBuildNumber
	patch["node_binary_sha256"] = state.NodeBinarySHA256
	patch["dev_node_signature_verified"] = state.NodeSource == nodemanifest.SourceDev && state.DevNodeSignatureVerified
	if state.NodeManifestURL != "" {
		patch["node_manifest_url"] = state.NodeManifestURL
	}
	if !state.NodeManifestCheckedAt.IsZero() {
		patch["node_manifest_checked_at"] = state.NodeManifestCheckedAt.UTC().Format(time.RFC3339)
	}
}

func mergePatch(dst, src map[string]interface{}) {
	for k, v := range src {
		dst[k] = v
	}
}

func normalizeProverAddress(value string) string {
	v := strings.TrimSpace(value)
	if len(v) >= 2 && strings.EqualFold(v[:2], "0x") {
		v = v[2:]
	}
	if v == "" || v == "--" {
		return ""
	}
	return strings.ToLower(v)
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

func (l *Loop) readPeerConnectionCountFromLogs() (int, bool) {
	if l.PeerConnectionsLogReader != nil {
		return l.PeerConnectionsLogReader(context.Background(), l.UnitName, l.NodeLogPath, 1000, 5*time.Second)
	}
	if l.NodeLogPath != "" {
		return nodeinfo.PeerConnectionsFromLogFile(context.Background(), l.NodeLogPath, 1000, 5*time.Second)
	}
	return nodeinfo.PeerConnectionsFromJournal(context.Background(), l.UnitName, 1000, 5*time.Second)
}

func (l *Loop) qclientBinaryPath() string {
	if l.QClientBinaryPath != "" {
		return l.QClientBinaryPath
	}
	return "/usr/local/bin/qclient"
}

func (l *Loop) qclientInstalled() bool {
	st, err := os.Stat(l.qclientBinaryPath())
	return err == nil && !st.IsDir()
}

func (l *Loop) readQClientProverStatus(state *config.State) *qclient.ProverStatus {
	runner := l.QClientStatusRunner
	if runner == nil {
		runner = qclient.Run
	}
	cfg := l.nodeInfoConfigPath(state)
	req := qclient.RunRequest{
		BinaryPath: l.qclientBinaryPath(),
		ConfigPath: cfg,
		WorkDir:    nodeCommandWorkDir(cfg, l.managedConfigDir()),
	}
	status, err := runner(context.Background(), req, 8*time.Second)
	if err != nil {
		return nil
	}
	return status
}

func applyQClientStatus(patch map[string]interface{}, status *qclient.ProverStatus) {
	if status == nil {
		return
	}
	patch["qclient_status"] = "reachable"
	if !status.Reachable {
		patch["qclient_status"] = "unreachable"
	}
	patch["qclient_reachable"] = status.Reachable
	patch["qclient_peer_id"] = status.PeerID
	patch["qclient_node_version"] = status.Version
	patch["qclient_seniority"] = status.Seniority
	patch["qclient_peer_score"] = status.PeerScore
	patch["qclient_running_workers"] = status.RunningWorkers
	patch["qclient_allocated_workers"] = status.AllocatedWorkers
	patch["qclient_last_received"] = status.LastReceived
	patch["qclient_last_global_head"] = status.LastGlobalHead
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

func releaseVersionNewerThan(candidate, current string) bool {
	a, okA := parseReleaseVersion(candidate)
	b, okB := parseReleaseVersion(current)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] > b[i] {
			return true
		}
		if a[i] < b[i] {
			return false
		}
	}
	return false
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
		"qclient_binary_path":     l.qclientBinaryPath(),
		"node_managed_config_dir": l.managedConfigDir(),
		"node_service_unit_path":  nodeUnitPath,
	}
	if stateCfgPath != "" {
		storePath := filepath.Join(stateCfgPath, "store")
		workerStorePath := filepath.Join(stateCfgPath, "worker-store")
		patch["node_config_dir"] = stateCfgPath
		patch["node_keys_path"] = filepath.Join(stateCfgPath, "keys.yml")
		patch["node_store_path"] = storePath
		patch["node_store_exists"] = pathExists(storePath)
		patch["node_worker_store_dir"] = workerStorePath
		patch["node_worker_store_exists"] = pathExists(workerStorePath)
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
		"node_binary_path", "qclient_binary_path", "node_managed_config_dir", "node_service_unit_path",
		"node_config_dir", "node_keys_path", "node_store_path", "node_worker_store_dir",
	} {
		if v, ok := patch[k].(string); ok {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write([]byte(v))
			h.Write([]byte{0})
		}
	}
	for _, k := range []string{"node_store_exists", "node_worker_store_exists"} {
		if v, ok := patch[k].(bool); ok {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write([]byte(strconv.FormatBool(v)))
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

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
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
		patch["rpc_config_hint"] = "Enable local gRPC/REST in config.yml to improve node information accuracy."
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

// runVersionPoll fetches canonical latest version strings and folds them into
// node_status. Frontend uses *_update_available as the banner/icon trigger.
func (l *Loop) runVersionPoll() {
	state, err := config.LoadState(l.StatePath)
	if err != nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]interface{}{}

	if state.NodeSource == nodemanifest.SourceDev {
		manifestURL := l.NodeManifestURL
		if manifestURL == "" {
			manifestURL = defaultNodeManifestURL
		}
		fetcher := l.NodeManifestFetcher
		if fetcher == nil {
			fetcher = nodemanifest.Fetch
		}
		if manifest, err := fetcher(manifestURL); err == nil && manifest != nil {
			latest, artifact, ok := manifest.LatestDev(l.Platform)
			if ok {
				current := state.InstalledNodeVersion
				currentMatch := nodemanifest.Match{
					Source:      nodemanifest.SourceDev,
					Version:     state.InstalledNodeVersion,
					BaseVersion: state.NodeBaseVersion,
					BuildNumber: state.NodeBuildNumber,
					Platform:    l.Platform,
					SHA256:      state.NodeBinarySHA256,
				}
				available, _ := manifest.DevUpdate(currentMatch, l.Platform)
				patch["installed_node_version"] = current
				patch["latest_node_version"] = latest.Version
				patch["latest_dev_node_version"] = latest.Version
				patch["latest_dev_node_url"] = artifact.URL
				patch["latest_dev_node_sha256"] = artifact.SHA256
				patch["latest_dev_node_build_number"] = latest.BuildNumber
				patch["node_update_source"] = nodemanifest.SourceDev
				patch["node_update_available"] = available
				patch["version_polled_at"] = now
				state.LatestDevNodeVersion = latest.Version
				state.LatestDevNodeURL = artifact.URL
				state.LatestDevNodeSHA256 = artifact.SHA256
				state.LatestDevNodeBuildNumber = latest.BuildNumber
				_ = config.SaveState(l.StatePath, state)
			}
		}
		if l.qclientInstalled() {
			qclientCurrent := state.QClientVersion
			if observed := l.statusString("qclient_version"); observed != "" {
				qclientCurrent = observed
			}
			qclientFetcher := l.LatestQClientVersionFetcher
			if qclientFetcher == nil {
				qclientFetcher = fetchLatestQClientVersion
			}
			latestQClient, err := qclientFetcher(l.LatestQClientVersionURL, l.Platform)
			if err == nil && latestQClient != "" {
				patch["qclient_version"] = qclientCurrent
				patch["latest_qclient_version"] = latestQClient
				patch["qclient_update_available"] = releaseVersionNewerThan(latestQClient, qclientCurrent)
				patch["qclient_version_polled_at"] = now
			}
		} else {
			patch["qclient_update_available"] = false
		}
		if len(patch) > 0 {
			l.updateNodeStatus(patch)
		}
		return
	}

	if state.NodeSource == nodemanifest.SourceUnknown {
		patch["node_update_source"] = nodemanifest.SourceUnknown
		patch["node_update_available"] = false
		patch["version_polled_at"] = now
		if l.qclientInstalled() {
			qclientCurrent := state.QClientVersion
			if observed := l.statusString("qclient_version"); observed != "" {
				qclientCurrent = observed
			}
			qclientFetcher := l.LatestQClientVersionFetcher
			if qclientFetcher == nil {
				qclientFetcher = fetchLatestQClientVersion
			}
			latestQClient, err := qclientFetcher(l.LatestQClientVersionURL, l.Platform)
			if err == nil && latestQClient != "" {
				patch["qclient_version"] = qclientCurrent
				patch["latest_qclient_version"] = latestQClient
				patch["qclient_update_available"] = releaseVersionNewerThan(latestQClient, qclientCurrent)
				patch["qclient_version_polled_at"] = now
			}
		} else {
			patch["qclient_update_available"] = false
		}
		l.updateNodeStatus(patch)
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
	if err == nil && latest != "" {
		available := releaseVersionNewerThan(latest, current)
		patch["current_node_version"] = current
		patch["latest_node_version"] = latest
		patch["node_update_available"] = available
		patch["version_polled_at"] = now
	}

	if l.qclientInstalled() {
		qclientCurrent := state.QClientVersion
		if observed := l.statusString("qclient_version"); observed != "" {
			qclientCurrent = observed
		}
		qclientFetcher := l.LatestQClientVersionFetcher
		if qclientFetcher == nil {
			qclientFetcher = fetchLatestQClientVersion
		}
		latestQClient, err := qclientFetcher(l.LatestQClientVersionURL, l.Platform)
		if err == nil && latestQClient != "" {
			patch["qclient_version"] = qclientCurrent
			patch["latest_qclient_version"] = latestQClient
			patch["qclient_update_available"] = releaseVersionNewerThan(latestQClient, qclientCurrent)
			patch["qclient_version_polled_at"] = now
		}
	} else {
		patch["qclient_update_available"] = false
	}

	if len(patch) > 0 {
		l.updateNodeStatus(patch)
	}
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

func fetchLatestQClientVersion(url, platform string) (string, error) {
	if platform == "" {
		return "", fmt.Errorf("missing platform")
	}
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
	return release.LatestVersionForPrefix(string(b), "qclient", platform), nil
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
