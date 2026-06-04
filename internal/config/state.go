package config

import (
	"errors"
	"os"
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
}

// CurrentSchemaVersion is bumped whenever State adds breaking field changes.
const CurrentSchemaVersion = 3

// LoadState reads the YAML at path; missing file returns an empty State.
func LoadState(path string) (*State, error) {
	s := &State{}
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
