package actions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
)

// SystemdControl is the subset of service-control methods cleanup needs.
// Mirrors svcctl.Ctl on both Linux (systemd) and macOS (launchd) so tests
// can inject a fake. The name is kept for back-compat with existing
// fixtures even though it covers launchd too.
type SystemdControl interface {
	Stop(unit string) error
	Disable(unit string) error
	Reload() error
	IsActive(unit string) bool
}

// CleanupDeps wires the dependencies for the cleanup_residue handler.
type CleanupDeps struct {
	StatePath         string // /etc/quilscan-agent/state.yaml
	ManagedConfigDir  string // /var/lib/quilscan/node/.config
	BinaryPath        string // /usr/local/bin/quilibrium-node
	QClientBinaryPath string // /usr/local/bin/qclient
	UnitName          string // systemd unit name OR launchd label
	BackupRootDir     string // /var/lib/quilscan/backups
	UnitDir           string // systemd unit dir OR launchd plist dir
	Systemd           SystemdControl
	EmitRaw           func(map[string]interface{})
	// PatchNodeStatus, when set, lets cleanup zero out the reconcile loop's
	// in-memory snapshot so the next push frame and any handler that reads
	// the cache (e.g. update_node) sees a clean slate immediately, not 60s
	// later when the next reconcile tick runs.
	PatchNodeStatus func(map[string]interface{})
}

// CleanupItem is one artifact moved into the backup directory.
type CleanupItem struct {
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// NewCleanupResidueHandler returns a handler that stops the orphan node,
// moves residue artifacts (managed .config dir, binaries, signature bundles,
// agent state file, service definition) into a timestamped backup directory,
// and asks the service manager to reload if needed.
func NewCleanupResidueHandler(d CleanupDeps) Handler {
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "preparing", Progress: 0.05})

		unitFile := svcctl.UnitFilePath(d.UnitDir, d.UnitName)
		state, _ := config.LoadState(d.StatePath)

		// Stop + disable the service if loaded so the node process exits
		// cleanly. IsActive goes through the platform-aware Ctl on both
		// systemd and launchd; install presence is a simple file-stat
		// check (the unit file's existence is what determines whether
		// Disable would do anything useful).
		if d.Systemd.IsActive(d.UnitName) {
			emit(Status{ID: c.ID, Step: "stopping", Progress: 0.20})
			_ = d.Systemd.Stop(d.UnitName)
		}
		if _, err := os.Stat(unitFile); err == nil {
			_ = d.Systemd.Disable(d.UnitName)
		}

		// Kill any orphan quilibrium-node process the unit didn't catch.
		if pid := pgrepFirst("quilibrium-node"); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 6; i++ {
				time.Sleep(500 * time.Millisecond)
				if pgrepFirst("quilibrium-node") == 0 {
					break
				}
			}
			if pid2 := pgrepFirst("quilibrium-node"); pid2 > 0 {
				_ = syscall.Kill(pid2, syscall.SIGKILL)
			}
		}

		// Backup directory: /var/lib/quilscan/backups/residue-YYYYMMDD-HHMMSS
		ts := time.Now().UTC().Format("20060102-150405")
		backupDir := filepath.Join(d.BackupRootDir, "residue-"+ts)
		if err := os.MkdirAll(backupDir, 0o755); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("mkdir backup: %v", err)})
			return err
		}

		emit(Status{ID: c.ID, Step: "backing_up", Progress: 0.45})

		var items []CleanupItem
		mvBackup := func(src, label string) {
			if src == "" {
				return
			}
			if _, err := os.Stat(src); err != nil {
				return
			}
			dst := filepath.Join(backupDir, filepath.Base(src))
			// Avoid name collisions when two artifacts share a basename.
			if _, err := os.Stat(dst); err == nil {
				dst = dst + "-" + ts
			}
			if err := os.Rename(src, dst); err != nil {
				// Fallback for cross-device renames (e.g. when /var and /etc are on
				// different mounts): cp -a then remove the source.
				if exec.Command("cp", "-a", src, dst).Run() != nil {
					return
				}
				_ = os.RemoveAll(src)
			}
			items = append(items, CleanupItem{Label: label, From: src, To: dst})
		}

		mvBinaryBundle := func(binary, label string) {
			if binary == "" {
				return
			}
			mvBackup(binary, label+" binary")
			mvBackup(binary+".dgst", label+" digest")
			for _, sig := range signatureFiles(binary) {
				mvBackup(sig, label+" signature")
			}
		}

		mvBinaryBundle(d.BinaryPath, "node")
		mvBinaryBundle(d.QClientBinaryPath, "qclient")
		for _, cfgPath := range cleanupConfigPaths(state, d.ManagedConfigDir) {
			mvBackup(cfgPath, "managed .config directory")
		}
		mvBackup(unitFile, "systemd unit")
		mvBackup(d.StatePath, "agent state file")

		emit(Status{ID: c.ID, Step: "reloading", Progress: 0.85})
		_ = d.Systemd.Reload()

		// Sync the reconcile loop's in-memory node_status cache so the next
		// frame it broadcasts reflects a removed node. Otherwise we would
		// keep emitting stale config_path / peer_id / version values until
		// the 60s reconcile tick re-detects the world.
		if d.PatchNodeStatus != nil {
			d.PatchNodeStatus(map[string]interface{}{
				"node_residues":               []string{},
				"config_path":                 "",
				"current_node_version":        "",
				"node_info_version":           "",
				"latest_node_version":         "",
				"node_update_available":       false,
				"node_source":                 "",
				"installed_node_version":      "",
				"node_base_version":           "",
				"node_build_number":           0,
				"node_binary_sha256":          "",
				"dev_node_signature_verified": false,
				"node_update_source":          "",
				"latest_dev_node_version":     "",
				"latest_dev_node_url":         "",
				"latest_dev_node_sha256":      "",
				"peer_id":                     "",
				"node_running_workers":        int64(0),
				"node_active_workers":         int64(0),
				"node_connections":            nil,
				"install_source":              "",
				"node_managed":                false,
				"node_disk_bytes":             int64(0),
				"node_disk_sub":               "",
				"has_qclient":                 false,
				"qclient_status":              "not_installed",
				"qclient_version":             "",
				"latest_qclient_version":      "",
				"qclient_update_available":    false,
			})
		}

		if d.EmitRaw != nil {
			d.EmitRaw(map[string]interface{}{
				"type":       "residue_cleaned",
				"cmd_id":     c.ID,
				"backup_dir": backupDir,
				"items":      items,
			})
		}

		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
		return nil
	}
}

func cleanupConfigPaths(state *config.State, managedConfigDir string) []string {
	paths := []string{}
	add := func(path string) {
		if path == "" {
			return
		}
		for _, existing := range paths {
			if existing == path {
				return
			}
		}
		paths = append(paths, path)
	}

	if state != nil && state.InstallSource == "fresh" {
		add(state.ConfigPath)
	}
	add(managedConfigDir)
	return paths
}

func signatureFiles(binary string) []string {
	files := []string{}
	if st, err := os.Stat(binary + ".sig"); err == nil && !st.IsDir() {
		files = append(files, binary+".sig")
	}
	digestSigs, err := filepath.Glob(binary + ".dgst.sig.*")
	if err != nil {
		return files
	}
	files = append(files, digestSigs...)
	return files
}

func pgrepFirst(name string) int {
	out, err := exec.Command("pgrep", "-x", name).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}
