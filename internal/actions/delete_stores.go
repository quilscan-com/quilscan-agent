package actions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
)

// DeleteNodeStoreDeps wires the controlled store cleanup action. The action
// never trusts a frontend path; it derives store locations from state.ConfigPath.
type DeleteNodeStoreDeps struct {
	StatePath       string
	BackupRootDir   string
	UnitName        string
	Systemd         SystemdControl
	EmitRaw         func(map[string]interface{})
	PatchNodeStatus func(map[string]interface{})
}

// DeleteNodeStoreBackupDeps wires the irreversible cleanup action for backup
// directories created by NewDeleteNodeStoreHandler.
type DeleteNodeStoreBackupDeps struct {
	BackupRootDir   string
	EmitRaw         func(map[string]interface{})
	PatchNodeStatus func(map[string]interface{})
}

// StoreBackupItem is one store directory moved into the backup directory.
type StoreBackupItem struct {
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// NewDeleteNodeStoreHandler stops the managed node and moves the selected
// store directory into a timestamped backup. It intentionally does not restart
// the node after completion.
func NewDeleteNodeStoreHandler(d DeleteNodeStoreDeps) Handler {
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "preparing", Progress: 0.05})

		target, err := targetStoreName(c.Args)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		state, err := config.LoadState(d.StatePath)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("load state: %v", err)})
			return err
		}
		cfgDir, err := cleanConfigPath(state.ConfigPath)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		if d.Systemd != nil && d.Systemd.IsActive(d.UnitName) {
			emit(Status{ID: c.ID, Step: "stopping", Progress: 0.20})
			if err := d.Systemd.Stop(d.UnitName); err != nil {
				emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("stop node: %v", err)})
				return err
			}
		}

		ts := time.Now().UTC().Format("20060102-150405")
		backupDir := filepath.Join(d.BackupRootDir, "node-"+target+"-"+ts)
		if err := os.MkdirAll(backupDir, 0o755); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("mkdir backup: %v", err)})
			return err
		}

		emit(Status{ID: c.ID, Step: "backing_up", Progress: 0.55})

		src, err := safeStorePath(cfgDir, target)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		var item *StoreBackupItem
		if _, err := os.Lstat(src); err == nil {
			dst := filepath.Join(backupDir, target)
			if err := movePath(src, dst); err != nil {
				emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("move %s: %v", target, err)})
				return err
			}
			item = &StoreBackupItem{Label: target, From: src, To: dst}
		}

		if d.PatchNodeStatus != nil {
			patch := map[string]interface{}{
				"worker_store_path":        filepath.Join(cfgDir, "worker-store"),
				"node_store_path":          filepath.Join(cfgDir, "store"),
				"node_store_exists":        storePathExists(filepath.Join(cfgDir, "store")),
				"node_worker_store_exists": storePathExists(filepath.Join(cfgDir, "worker-store")),
				"node_store_backup_target": target,
				"node_store_last_backup":   backupDir,
				"node_store_backup_status": "moved",
			}
			if target == "worker-store" {
				state.WorkerStoreBytes = 0
				state.WorkerStoreMeasuredAt = time.Time{}
				_ = config.SaveState(d.StatePath, state)
				patch["node_disk_bytes"] = int64(0)
				patch["node_disk_sub"] = ""
				patch["worker_store_bytes"] = int64(0)
				patch["worker_store_measured_at"] = ""
			}
			d.PatchNodeStatus(patch)
		} else if target == "worker-store" {
			state.WorkerStoreBytes = 0
			state.WorkerStoreMeasuredAt = time.Time{}
			_ = config.SaveState(d.StatePath, state)
		}

		if d.EmitRaw != nil {
			var emitted interface{}
			if item != nil {
				emitted = *item
			}
			d.EmitRaw(map[string]interface{}{
				"type":       "node_store_moved",
				"cmd_id":     c.ID,
				"target":     target,
				"backup_dir": backupDir,
				"item":       emitted,
			})
		}

		emit(Status{ID: c.ID, Step: "done", Progress: 1})
		return nil
	}
}

// NewDeleteNodeStoreBackupHandler permanently deletes a backup directory
// produced by NewDeleteNodeStoreHandler. It refuses paths outside BackupRootDir
// and only accepts the target-specific node-store backup naming scheme.
func NewDeleteNodeStoreBackupHandler(d DeleteNodeStoreBackupDeps) Handler {
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "preparing", Progress: 0.05})

		target, err := targetStoreName(c.Args)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		backupDir, err := cleanBackupDir(d.BackupRootDir, target, c.Args["backup_dir"])
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		emit(Status{ID: c.ID, Step: "deleting", Progress: 0.45})

		deleted := false
		if fi, err := os.Lstat(backupDir); err == nil {
			if !fi.IsDir() {
				err := fmt.Errorf("backup path is not a directory")
				emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
				return err
			}
			if err := os.RemoveAll(backupDir); err != nil {
				emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("delete backup: %v", err)})
				return err
			}
			deleted = true
		} else if !os.IsNotExist(err) {
			emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("stat backup: %v", err)})
			return err
		}

		if d.PatchNodeStatus != nil {
			d.PatchNodeStatus(map[string]interface{}{
				"node_store_backup_target": target,
				"node_store_last_backup":   backupDir,
				"node_store_backup_status": "deleted",
			})
		}
		if d.EmitRaw != nil {
			d.EmitRaw(map[string]interface{}{
				"type":       "node_store_backup_deleted",
				"cmd_id":     c.ID,
				"target":     target,
				"backup_dir": backupDir,
				"deleted":    deleted,
			})
		}

		emit(Status{ID: c.ID, Step: "done", Progress: 1})
		return nil
	}
}

func targetStoreName(args map[string]interface{}) (string, error) {
	raw, _ := args["target"].(string)
	switch raw {
	case "store", "worker-store":
		return raw, nil
	default:
		return "", fmt.Errorf("target must be store or worker-store")
	}
}

func cleanConfigPath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("missing node config_path in agent state")
	}
	cleaned := filepath.Clean(raw)
	if !filepath.IsAbs(cleaned) || cleaned == string(filepath.Separator) || cleaned == "." {
		return "", fmt.Errorf("unsafe node config_path")
	}
	return cleaned, nil
}

func safeStorePath(configDir, name string) (string, error) {
	if name != "store" && name != "worker-store" {
		return "", fmt.Errorf("unsupported store directory")
	}
	target := filepath.Clean(filepath.Join(configDir, name))
	if filepath.Dir(target) != configDir || filepath.Base(target) != name {
		return "", fmt.Errorf("unsafe store path")
	}
	return target, nil
}

func cleanBackupDir(root, target string, raw interface{}) (string, error) {
	backupRoot := filepath.Clean(root)
	if backupRoot == "" || !filepath.IsAbs(backupRoot) || backupRoot == string(filepath.Separator) || backupRoot == "." {
		return "", fmt.Errorf("unsafe backup root")
	}
	backupRaw, _ := raw.(string)
	if strings.TrimSpace(backupRaw) == "" {
		return "", fmt.Errorf("missing backup_dir")
	}
	backupDir := filepath.Clean(backupRaw)
	if !filepath.IsAbs(backupDir) || backupDir == string(filepath.Separator) || backupDir == "." {
		return "", fmt.Errorf("unsafe backup_dir")
	}
	rel, err := filepath.Rel(backupRoot, backupDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("backup_dir outside backup root")
	}
	if strings.Contains(rel, string(filepath.Separator)) {
		return "", fmt.Errorf("backup_dir must be a direct backup child")
	}
	prefix := "node-" + target + "-"
	if !strings.HasPrefix(filepath.Base(backupDir), prefix) {
		return "", fmt.Errorf("backup_dir does not match target")
	}
	return backupDir, nil
}

func storePathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := exec.Command("cp", "-a", src, dst).Run(); err != nil {
		return err
	}
	return os.RemoveAll(src)
}
