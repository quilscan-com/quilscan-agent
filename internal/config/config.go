// Package config loads the agent's config.yaml (read-only settings).
package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Config holds startup settings. Fields fall back to DefaultConfig() when the
// file is absent or missing a key.
//
// Platform notes:
//   - On Linux, NodeServiceName is the systemd unit ("quilibrium-node.service")
//     and the unit file lives under UnitDir.
//   - On macOS, NodeServiceName is the LaunchAgent label
//     ("com.quilscan.node") and the plist file lives under UnitDir.
type Config struct {
	ConfigPath        string `yaml:"-"` // path of the loaded config file itself; not persisted
	BackendURL        string `yaml:"backend_url"`
	AgentBinaryPath   string `yaml:"agent_binary_path"`
	NodeBinaryPath    string `yaml:"node_binary_path"`
	QClientBinaryPath string `yaml:"qclient_binary_path"`
	NodeServiceName   string `yaml:"node_service_name"`  // systemd unit name OR launchd label
	AgentServiceName  string `yaml:"agent_service_name"` // ditto, but for the agent itself
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
//   - macOS: ~/.local/bin + ~/Library/Application Support + ~/Library/LaunchAgents
//
// The macOS layout is intentionally user-scope (Apple Silicon LaunchAgents)
// so install never needs sudo for any path the agent touches.
func DefaultConfig() Config {
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		appSupport := filepath.Join(home, "Library/Application Support/quilscan-agent")
		return Config{
			ConfigPath:        filepath.Join(appSupport, "config.yaml"),
			BackendURL:        "wss://api.quilscan.com/api/agent/ws",
			AgentBinaryPath:   filepath.Join(home, ".local/bin/quilscan-agent"),
			NodeBinaryPath:    filepath.Join(home, ".local/bin/quilibrium-node"),
			QClientBinaryPath: filepath.Join(home, ".local/bin/qclient"),
			NodeServiceName:   "com.quilscan.node",
			AgentServiceName:  "com.quilscan.agent",
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
		NodeServiceName:   "quilibrium-node.service",
		AgentServiceName:  "quilscan-agent.service",
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
