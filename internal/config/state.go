package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// State is mutable data the agent owns: which config_path the node runs with,
// what version is installed, when it was installed, RPC patch status, and
// cached worker-store size. Persisted as YAML at /etc/quilscan-agent/state.yaml.
//
// Schema version bumps go in the SchemaVersion field so future migrations have
// a stable signal.
type State struct {
	SchemaVersion int `yaml:"schema_version"`

	// Identity
	ConfigPath  string `yaml:"config_path"`
	BinaryPath  string `yaml:"binary_path,omitempty"`
	ServiceUnit string `yaml:"service_unit,omitempty"`
	NodeVersion string `yaml:"node_version"`

	QClientBinaryPath string `yaml:"qclient_binary_path,omitempty"`
	QClientVersion    string `yaml:"qclient_version,omitempty"`

	// Node binary release source detected from the local binary sha256 and
	// the Quilscan node manifest. This is agent-owned metadata; the node's
	// config.yml is never modified for release bookkeeping.
	NodeSource               string    `yaml:"node_source,omitempty"` // "releases" | "dev" | "unknown"
	InstalledNodeVersion     string    `yaml:"installed_node_version,omitempty"`
	NodeBaseVersion          string    `yaml:"node_base_version,omitempty"`
	NodeBuildNumber          int       `yaml:"node_build_number,omitempty"`
	NodeBinarySHA256         string    `yaml:"node_binary_sha256,omitempty"`
	NodeManifestURL          string    `yaml:"node_manifest_url,omitempty"`
	NodeManifestCheckedAt    time.Time `yaml:"node_manifest_checked_at,omitempty"`
	DevNodeSignatureVerified bool      `yaml:"dev_node_signature_verified,omitempty"`
	LatestDevNodeVersion     string    `yaml:"latest_dev_node_version,omitempty"`
	LatestDevNodeURL         string    `yaml:"latest_dev_node_url,omitempty"`
	LatestDevNodeSHA256      string    `yaml:"latest_dev_node_sha256,omitempty"`
	LatestDevNodeBuildNumber int       `yaml:"latest_dev_node_build_number,omitempty"`

	// Dev Node automatic update state is versioned separately so stale
	// full-state saves from other agent goroutines cannot overwrite it.
	DevNodeAutoUpdateRevision                uint64    `yaml:"dev_node_auto_update_revision,omitempty"`
	DevNodeAutoUpdateEnabled                 bool      `yaml:"dev_node_auto_update_enabled,omitempty"`
	DevNodeAutoUpdateLastEventID             string    `yaml:"dev_node_auto_update_last_event_id,omitempty"`
	DevNodeAutoUpdateLastResult              string    `yaml:"dev_node_auto_update_last_result,omitempty"`
	DevNodeAutoUpdateLastFromVersion         string    `yaml:"dev_node_auto_update_last_from_version,omitempty"`
	DevNodeAutoUpdateLastTargetVersion       string    `yaml:"dev_node_auto_update_last_target_version,omitempty"`
	DevNodeAutoUpdateLastCompletedAt         time.Time `yaml:"dev_node_auto_update_last_completed_at,omitempty"`
	DevNodeAutoUpdateLastFailedTargetVersion string    `yaml:"dev_node_auto_update_last_failed_target_version,omitempty"`

	// Origin: how the node entered our care.
	InstallSource string `yaml:"install_source,omitempty"` // "fresh" | "migrated"
	MigratedFrom  string `yaml:"migrated_from,omitempty"`

	// Lifecycle timestamps (UTC). Zero values mean "not yet recorded".
	InstalledAt        time.Time `yaml:"installed_at,omitempty"`
	QClientInstalledAt time.Time `yaml:"qclient_installed_at,omitempty"`
	LastVerifiedAt     time.Time `yaml:"last_verified_at,omitempty"`
	LastStartedAt      time.Time `yaml:"last_started_at,omitempty"`

	// RPC config patch (we set listenGrpcMultiaddr / listenRESTMultiaddr to
	// 127.0.0.1 so the agent can talk to the node without exposing it publicly).
	RPCPatched   bool      `yaml:"rpc_patched,omitempty"`
	RPCGRPCPort  int       `yaml:"rpc_grpc_port,omitempty"`
	RPCRESTPort  int       `yaml:"rpc_rest_port,omitempty"`
	RPCPatchedAt time.Time `yaml:"rpc_patched_at,omitempty"`

	// Worker-store size cache (du is expensive; refreshed every few minutes
	// by the reconcile loop, served in metrics frames in between).
	WorkerStoreBytes      int64     `yaml:"worker_store_bytes,omitempty"`
	WorkerStoreMeasuredAt time.Time `yaml:"worker_store_measured_at,omitempty"`

	// Optional identity (filled later if/when we can read it from RPC).
	PeerID string `yaml:"peer_id,omitempty"`

	stateGeneration    uint64 `yaml:"-"`
	stateGenerationSet bool   `yaml:"-"`
}

// CurrentSchemaVersion is bumped whenever State adds breaking field changes.
const CurrentSchemaVersion = 3

var stateMu sync.Mutex

var errNilState = errors.New("state must not be nil")

// ErrStaleState reports an attempt to save a State loaded before the same
// path was removed or moved. Reload the path before intentionally saving it.
var ErrStaleState = errors.New("state was loaded before the current file generation")

var stateGenerations = map[string]uint64{}

// LoadState reads the YAML at path; missing file returns an empty State.
func LoadState(path string) (*State, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	return loadStateUnlocked(path)
}

func loadStateUnlocked(path string) (*State, error) {
	s := &State{
		stateGeneration:    stateGenerationUnlocked(path),
		stateGenerationSet: true,
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := yaml.Unmarshal(b, s); err != nil {
		return s, err
	}
	return s, nil
}

// SaveState writes the state file atomically via rename. Stamps the schema
// version automatically so older agents can detect newer files.
func SaveState(path string, s *State) error {
	stateMu.Lock()
	defer stateMu.Unlock()

	if s == nil {
		return errNilState
	}
	currentGeneration := stateGenerationUnlocked(path)
	if s.stateGenerationSet && s.stateGeneration < currentGeneration {
		return ErrStaleState
	}
	current, err := loadStateUnlocked(path)
	if err == nil && current.DevNodeAutoUpdateRevision > s.DevNodeAutoUpdateRevision {
		preserveDevNodeAutoUpdateState(s, current)
	}
	if err := saveStateUnlocked(path, s); err != nil {
		return err
	}
	s.stateGeneration = currentGeneration
	s.stateGenerationSet = true
	return nil
}

// UpdateState loads, mutates, and saves State while holding the package lock.
// The returned State is a copy of the persisted value. mutate executes while
// locked and must not call LoadState, SaveState, UpdateState, RemoveState, or
// MoveState.
func UpdateState(path string, mutate func(*State) error) (*State, error) {
	stateMu.Lock()
	defer stateMu.Unlock()

	state, err := loadStateUnlocked(path)
	if err != nil {
		return nil, err
	}
	if mutate == nil {
		return nil, errors.New("state mutation is required")
	}
	if err := mutate(state); err != nil {
		return nil, err
	}
	if err := saveStateUnlocked(path, state); err != nil {
		return nil, err
	}
	state.stateGeneration = stateGenerationUnlocked(path)
	state.stateGenerationSet = true
	copy := *state
	return &copy, nil
}

// RemoveState removes a state file while coordinating with state reads,
// writes, and moves in this package.
func RemoveState(path string) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if err := os.Remove(path); err != nil {
		return err
	}
	incrementStateGenerationUnlocked(path)
	return nil
}

// MoveState moves a state file while coordinating with state reads and writes.
// It first attempts an atomic rename, then copies and removes the source when
// the source and destination are on different filesystems.
func MoveState(path, dst string) error {
	stateMu.Lock()
	defer stateMu.Unlock()

	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("state file %s is not a regular file", path)
	}
	if _, err := os.Lstat(dst); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, dst); err == nil {
		incrementStateGenerationUnlocked(path)
		incrementStateGenerationUnlocked(dst)
		return nil
	}
	if err := copyAndRemoveState(path, dst, info); err != nil {
		return err
	}
	incrementStateGenerationUnlocked(path)
	incrementStateGenerationUnlocked(dst)
	return nil
}

func copyAndRemoveState(path, dst string, sourceInfo os.FileInfo) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		_ = source.Close()
		return err
	}
	tmpPath := tmp.Name()
	tmpOpen := true
	cleanup := func() {
		if tmpOpen {
			_ = tmp.Close()
		}
		_ = source.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := io.Copy(tmp, source); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	tmpOpen = false
	if err := os.Chmod(tmpPath, sourceInfo.Mode().Perm()); err != nil {
		cleanup()
		return err
	}
	if err := os.Chtimes(tmpPath, sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
		cleanup()
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		cleanup()
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanup()
		return err
	}
	if err := source.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	if err := os.Remove(path); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func stateGenerationUnlocked(path string) uint64 {
	return stateGenerations[stateGenerationKey(path)]
}

func incrementStateGenerationUnlocked(path string) {
	stateGenerations[stateGenerationKey(path)]++
}

func stateGenerationKey(path string) string {
	clean := filepath.Clean(path)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return clean
	}
	return abs
}

func preserveDevNodeAutoUpdateState(dst, src *State) {
	dst.DevNodeAutoUpdateRevision = src.DevNodeAutoUpdateRevision
	dst.DevNodeAutoUpdateEnabled = src.DevNodeAutoUpdateEnabled
	dst.DevNodeAutoUpdateLastEventID = src.DevNodeAutoUpdateLastEventID
	dst.DevNodeAutoUpdateLastResult = src.DevNodeAutoUpdateLastResult
	dst.DevNodeAutoUpdateLastFromVersion = src.DevNodeAutoUpdateLastFromVersion
	dst.DevNodeAutoUpdateLastTargetVersion = src.DevNodeAutoUpdateLastTargetVersion
	dst.DevNodeAutoUpdateLastCompletedAt = src.DevNodeAutoUpdateLastCompletedAt
	dst.DevNodeAutoUpdateLastFailedTargetVersion = src.DevNodeAutoUpdateLastFailedTargetVersion
}

func saveStateUnlocked(path string, s *State) error {
	if s.SchemaVersion == 0 {
		s.SchemaVersion = CurrentSchemaVersion
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
