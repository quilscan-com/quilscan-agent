package actions

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/rpcconfig"
)

const maxDiagnosticsPeers = 32

// SetNodeDiagnosticsDeps wires set_node_diagnostics. The config path comes
// from agent state, never from the command.
type SetNodeDiagnosticsDeps struct {
	StatePath        string
	ManagedConfigDir string
	BackupRootDir    string
	UnitName         string
	Svc              NodeRestarter
	LoadState        func() (*config.State, error)
}

// NewSetNodeDiagnosticsHandler edits one section under `diagnostics` in the
// node's config.yml. Args: section ("healthReport" or "logRequests"),
// enabled (bool), the section's numeric settings, and restart (bool).
// presets (labels) and allowed_peers (peer IDs), when either is given,
// together replace allowedPeers; pass both empty to clear it. The node
// reads the file at startup, so the change applies on its next restart.
func NewSetNodeDiagnosticsHandler(d SetNodeDiagnosticsDeps) Handler {
	return func(c Command, emit Emitter) error {
		fail := func(err error) error {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		emit(Status{ID: c.ID, Step: "preparing", Progress: 0.05})
		edit, restart, err := parseNodeDiagnosticsArgs(c.Args)
		if err != nil {
			return fail(err)
		}
		cfgFile, err := nodeConfigFile(d)
		if err != nil {
			return fail(err)
		}
		info, err := os.Stat(cfgFile)
		if err != nil {
			return fail(fmt.Errorf("stat config.yml: %w", err))
		}
		raw, err := os.ReadFile(cfgFile)
		if err != nil {
			return fail(fmt.Errorf("read config.yml: %w", err))
		}
		out, changed, err := rpcconfig.ApplyDiagnostics(raw, edit)
		if err != nil {
			return fail(err)
		}
		if !changed {
			emit(Status{ID: c.ID, Step: "done", Progress: 1, Message: "Node diagnostics already set; config.yml unchanged."})
			return nil
		}
		emit(Status{ID: c.ID, Step: "backing_up", Progress: 0.25})
		if err := backupNodeConfig(d.BackupRootDir, cfgFile, info); err != nil {
			return fail(err)
		}
		emit(Status{ID: c.ID, Step: "writing_config", Progress: 0.5})
		if err := writeFileAtomic(cfgFile, out, info); err != nil {
			return fail(err)
		}
		if !restart {
			emit(Status{ID: c.ID, Step: "done", Progress: 1, Message: "Node diagnostics saved; restart the node to apply."})
			return nil
		}
		if d.Svc == nil || !d.Svc.IsActive(d.UnitName) {
			emit(Status{ID: c.ID, Step: "done", Progress: 1, Message: "Node diagnostics saved; the node is not running and applies them when it starts."})
			return nil
		}
		emit(Status{ID: c.ID, Step: "restarting", Progress: 0.75})
		if err := d.Svc.RestartAndWait(d.UnitName); err != nil {
			return fail(fmt.Errorf("node diagnostics saved, restart failed: %w", err))
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1, Message: "Node diagnostics saved; node restarted."})
		return nil
	}
}

func parseNodeDiagnosticsArgs(args map[string]interface{}) (rpcconfig.DiagnosticsEdit, bool, error) {
	edit := rpcconfig.DiagnosticsEdit{Settings: map[string]int64{}}
	edit.Section, _ = args["section"].(string)
	settings, ok := rpcconfig.DiagnosticsSections[edit.Section]
	if !ok {
		return edit, false, fmt.Errorf("section must be healthReport or logRequests")
	}
	known := map[string]bool{"section": true, "enabled": true, "presets": true, "allowed_peers": true, "restart": true}
	for _, setting := range settings {
		known[setting.Arg] = true
		if raw, present := args[setting.Arg]; present {
			value, ok := raw.(float64)
			if !ok || value != float64(int64(value)) || int64(value) < setting.Min || int64(value) > setting.Max {
				return edit, false, fmt.Errorf("%s must be an integer from %d to %d", setting.Arg, setting.Min, setting.Max)
			}
			edit.Settings[setting.Key] = int64(value)
		}
	}
	for name := range args {
		if !known[name] {
			return edit, false, fmt.Errorf("unknown argument %q for section %s", name, edit.Section)
		}
	}
	if edit.Enabled, ok = args["enabled"].(bool); !ok {
		return edit, false, fmt.Errorf("enabled must be a boolean")
	}
	restart := false
	if raw, present := args["restart"]; present {
		if restart, ok = raw.(bool); !ok {
			return edit, false, fmt.Errorf("restart must be a boolean")
		}
	}
	presets, err := stringListArg(args, "presets")
	if err != nil {
		return edit, false, err
	}
	custom, err := stringListArg(args, "allowed_peers")
	if err != nil {
		return edit, false, err
	}
	if presets == nil && custom == nil {
		return edit, restart, nil
	}
	edit.Peers = []string{}
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			edit.Peers = append(edit.Peers, id)
		}
	}
	for _, label := range presets {
		id := ""
		for _, preset := range rpcconfig.DiagnosticsPeerPresets {
			if preset.Label == label {
				id = preset.PeerID
			}
		}
		if id == "" {
			return edit, false, fmt.Errorf("unknown peer preset %q", label)
		}
		add(id)
	}
	for _, id := range custom {
		if !rpcconfig.ValidPeerID(id) {
			return edit, false, fmt.Errorf("allowed_peers entry %q is not a base58 peer ID", id)
		}
		add(id)
	}
	if len(edit.Peers) > maxDiagnosticsPeers {
		return edit, false, fmt.Errorf("at most %d allowed peers", maxDiagnosticsPeers)
	}
	return edit, restart, nil
}

// stringListArg returns args[name] as strings, or nil when it is absent.
func stringListArg(args map[string]interface{}, name string) ([]string, error) {
	raw, present := args[name]
	if !present {
		return nil, nil
	}
	bad := fmt.Errorf("%s must be a list of at most %d strings", name, maxDiagnosticsPeers)
	items, ok := raw.([]interface{})
	if !ok || len(items) > maxDiagnosticsPeers {
		return nil, bad
	}
	out := []string{}
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			return nil, bad
		}
		out = append(out, value)
	}
	return out, nil
}

func nodeConfigFile(d SetNodeDiagnosticsDeps) (string, error) {
	loadState := d.LoadState
	if loadState == nil {
		loadState = func() (*config.State, error) { return config.LoadState(d.StatePath) }
	}
	state, err := loadState()
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}
	cfgDir, err := cleanConfigPath(firstNonEmpty(state.ConfigPath, d.ManagedConfigDir))
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "config.yml"), nil
}

// backupNodeConfig copies config.yml into a timestamped directory under the
// agent backup root, next to the store backups.
func backupNodeConfig(root, cfgFile string, info os.FileInfo) error {
	if root == "" {
		return fmt.Errorf("missing backup root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("mkdir backup: %w", err)
	}
	dir, err := os.MkdirTemp(root, "node-config-"+time.Now().UTC().Format("20060102-150405")+"-")
	if err != nil {
		return fmt.Errorf("mkdir backup: %w", err)
	}
	if err := copyFile(cfgFile, filepath.Join(dir, "config.yml"), info.Mode().Perm()); err != nil {
		return fmt.Errorf("back up config.yml: %w", err)
	}
	return nil
}

// writeFileAtomic replaces path with data through a temporary file in the
// same directory, keeping the original mode and owner.
func writeFileAtomic(path string, data []byte, info os.FileInfo) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(info.Mode().Perm())
	if st, ok := info.Sys().(*syscall.Stat_t); ok && err == nil && (int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid()) {
		err = tmp.Chown(int(st.Uid), int(st.Gid))
	}
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write config.yml: %w", err)
	}
	return nil
}
