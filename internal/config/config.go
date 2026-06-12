// Package config loads the agent's config.yaml (read-only settings).
package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

const DefaultQClientReleaseURL = "https://releases.quilscan.com"

// Config holds startup settings. Fields fall back to DefaultConfig() when the
// file is absent or missing a key.
//
// Platform notes:
//   - On Linux, NodeServiceName is the systemd unit ("quilibrium-node.service")
//     and the unit file lives under UnitDir.
//   - On macOS, NodeServiceName is the launchd label
//     ("com.quilscan.node") and the plist file lives under UnitDir.
type Config struct {
	ConfigPath        string `yaml:"-"` // path of the loaded config file itself; not persisted
	BackendURL        string `yaml:"backend_url"`
	AgentBinaryPath   string `yaml:"agent_binary_path"`
	NodeBinaryPath    string `yaml:"node_binary_path"`
	QClientBinaryPath string `yaml:"qclient_binary_path"`
	QClientReleaseURL string `yaml:"qclient_release_url"`
	NodeServiceName   string `yaml:"node_service_name"`  // systemd unit name OR launchd label
	AgentServiceName  string `yaml:"agent_service_name"` // ditto, but for the agent itself
	ServiceMode       string `yaml:"service_mode"`       // user OR system, primarily for macOS migration gating
	NodeServiceMode   string `yaml:"node_service_mode"`  // user OR system, mirrors the managed node service scope
	TokenPath         string `yaml:"token_path"`
	StatePath         string `yaml:"state_path"`
	AuditLogPath      string `yaml:"audit_log_path"`
	UnitDir           string `yaml:"unit_dir"`           // where the service def file lives
	ManagedConfigDir  string `yaml:"managed_config_dir"` // fresh-install node .config parent
	BackupRootDir     string `yaml:"backup_root_dir"`    // cleanup_residue / removal target
	NodeLogPath       string `yaml:"node_log_path"`      // macOS only: redirect target for plist Stdout/Err
}

// DefaultConfig returns sane defaults for the current platform.
//   - Linux: /usr/local/bin + /etc/quilscan-agent + /etc/systemd/system + ...
//   - macOS system mode uses /usr/local/bin, /Library/Application Support,
//     and /Library/LaunchDaemons; legacy user mode remains supported for migration.
func DefaultConfig() Config {
	if runtime.GOOS == "darwin" {
		if os.Geteuid() == 0 {
			appSupport := "/Library/Application Support/quilscan-agent"
			return Config{
				ConfigPath:        filepath.Join(appSupport, "config.yaml"),
				BackendURL:        "wss://api.quilscan.com/api/agent/ws",
				AgentBinaryPath:   "/usr/local/bin/quilscan-agent",
				NodeBinaryPath:    "/usr/local/bin/quilibrium-node",
				QClientBinaryPath: "/usr/local/bin/qclient",
				QClientReleaseURL: DefaultQClientReleaseURL,
				NodeServiceName:   "com.quilscan.node",
				AgentServiceName:  "com.quilscan.agent",
				ServiceMode:       "system",
				NodeServiceMode:   "system",
				TokenPath:         filepath.Join(appSupport, "token"),
				StatePath:         filepath.Join(appSupport, "state.yaml"),
				AuditLogPath:      "/Library/Logs/quilscan-agent.log",
				UnitDir:           "/Library/LaunchDaemons",
				ManagedConfigDir:  "/Library/Application Support/quilibrium/.config",
				BackupRootDir:     filepath.Join(appSupport, "backups"),
				NodeLogPath:       "/Library/Logs/quilibrium-node.log",
			}
		}
		home, _ := os.UserHomeDir()
		appSupport := filepath.Join(home, "Library/Application Support/quilscan-agent")
		return Config{
			ConfigPath:        filepath.Join(appSupport, "config.yaml"),
			BackendURL:        "wss://api.quilscan.com/api/agent/ws",
			AgentBinaryPath:   filepath.Join(home, ".local/bin/quilscan-agent"),
			NodeBinaryPath:    filepath.Join(home, ".local/bin/quilibrium-node"),
			QClientBinaryPath: filepath.Join(home, ".local/bin/qclient"),
			QClientReleaseURL: DefaultQClientReleaseURL,
			NodeServiceName:   "com.quilscan.node",
			AgentServiceName:  "com.quilscan.agent",
			ServiceMode:       "user",
			NodeServiceMode:   "user",
			TokenPath:         filepath.Join(appSupport, "token"),
			StatePath:         filepath.Join(appSupport, "state.yaml"),
			AuditLogPath:      filepath.Join(home, "Library/Logs/quilscan-agent.log"),
			UnitDir:           filepath.Join(home, "Library/LaunchAgents"),
			ManagedConfigDir:  filepath.Join(home, "Library/Application Support/quilibrium/.config"),
			BackupRootDir:     filepath.Join(appSupport, "backups"),
			NodeLogPath:       filepath.Join(home, "Library/Logs/quilibrium-node.log"),
		}
	}
	return Config{
		ConfigPath:        "/etc/quilscan-agent/config.yaml",
		BackendURL:        "wss://api.quilscan.com/api/agent/ws",
		AgentBinaryPath:   "/usr/local/bin/quilscan-agent",
		NodeBinaryPath:    "/usr/local/bin/quilibrium-node",
		QClientBinaryPath: "/usr/local/bin/qclient",
		QClientReleaseURL: DefaultQClientReleaseURL,
		NodeServiceName:   "quilibrium-node.service",
		AgentServiceName:  "quilscan-agent.service",
		ServiceMode:       "system",
		NodeServiceMode:   "system",
		TokenPath:         "/etc/quilscan-agent/token",
		StatePath:         "/etc/quilscan-agent/state.yaml",
		AuditLogPath:      "/var/log/quilscan-agent.log",
		UnitDir:           "/etc/systemd/system",
		ManagedConfigDir:  "/var/lib/quilscan/node/.config",
		BackupRootDir:     "/var/lib/quilscan/backups",
		NodeLogPath:       "", // unused on Linux (journalctl handles it)
	}
}

// Load reads the YAML at path. A missing file yields DefaultConfig() and no
// error. Malformed YAML is surfaced.
func Load(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
