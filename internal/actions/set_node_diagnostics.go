package actions

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/rpcconfig"
)

// SetNodeDiagnosticsDeps only accepts host-owned paths, never command paths.
type SetNodeDiagnosticsDeps struct {
	StatePath             string
	ManagedConfigDir      string
	BackupRootDir         string
	UnitName              string
	Svc                   NodeRestarter
	LoadState             func() (*config.State, error)
	PatchNodeStatus       func(map[string]interface{})
	InvalidateLogRequests func()
}

func parseNodeDiagnosticsArgs(args map[string]interface{}) (rpcconfig.DiagnosticsEdit, bool, error) {
	edit := rpcconfig.DiagnosticsEdit{}
	section, _ := args["section"].(string)
	edit.Section = section
	if section != "healthReport" && section != "logRequests" {
		return edit, false, errors.New("section must be healthReport or logRequests")
	}
	known := map[string]bool{"section": true, "enabled": true, "restart": true, "allowed_peers": true, "min_interval_secs": true}
	if section == "logRequests" {
		delete(known, "min_interval_secs")
		for _, key := range []string{"requests_per_day", "max_pending", "ttl_secs", "max_bundle_bytes"} {
			known[key] = true
		}
	}
	for key := range args {
		if !known[key] {
			return edit, false, errors.New("unknown diagnostics section argument")
		}
	}
	enabled, ok := args["enabled"].(bool)
	if !ok {
		return edit, false, errors.New("enabled must be a boolean")
	}
	edit.Enabled = enabled
	restart, ok := args["restart"].(bool)
	if !ok || !restart {
		return edit, false, errors.New("restart must be true: saving requires a confirmed Node restart")
	}
	if raw, present := args["allowed_peers"]; present {
		items, ok := raw.([]interface{})
		if !ok || len(items) > rpcconfig.MaxHealthPeers {
			return edit, false, errors.New("allowed_peers must be a string array with at most 32 entries")
		}
		peers := []string{}
		for _, item := range items {
			p, ok := item.(string)
			if !ok {
				return edit, false, errors.New("allowed_peers must be a string array")
			}
			peers = append(peers, p)
		}
		normalized, err := rpcconfig.NormalizeHealthPeers(peers)
		if err != nil {
			return edit, false, err
		}
		edit.AllowedPeers = &normalized
	}
	if raw, present := args["min_interval_secs"]; present {
		value, ok := raw.(float64)
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 10 || value > 3600 || math.Trunc(value) != value {
			return edit, false, errors.New("min_interval_secs must be an integer from 10 to 3600")
		}
		interval := int64(value)
		edit.MinIntervalSecs = &interval
	}
	if section == "logRequests" {
		fields := []struct {
			key      string
			min, max float64
			dst      **int64
		}{{"requests_per_day", 1, 24, &edit.RequestsPerDay}, {"max_pending", 1, 16, &edit.MaxPending}, {"ttl_secs", 600, 604800, &edit.TTLSecs}, {"max_bundle_bytes", 65536, 268435456, &edit.MaxBundleBytes}}
		for _, field := range fields {
			raw, present := args[field.key]
			if !present {
				if edit.Enabled {
					return edit, false, fmt.Errorf("enabling requires %s", field.key)
				}
				continue
			}
			value, ok := raw.(float64)
			if !ok || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value < field.min || value > field.max {
				return edit, false, fmt.Errorf("%s must be an integer from %.0f to %.0f", field.key, field.min, field.max)
			}
			n := int64(value)
			*field.dst = &n
		}
		if edit.Enabled && edit.AllowedPeers == nil {
			return edit, false, errors.New("enabling requires allowed_peers")
		}
	}
	if section == "healthReport" && edit.Enabled && (edit.AllowedPeers == nil || edit.MinIntervalSecs == nil) {
		return edit, false, errors.New("enabling requires allowed_peers and min_interval_secs")
	}
	return edit, true, nil
}

// NewSetNodeDiagnosticsHandler saves one fixed diagnostics section, then waits for service
// manager restart completion. It does not assert application or report health.
func NewSetNodeDiagnosticsHandler(d SetNodeDiagnosticsDeps) Handler {
	return func(c Command, emit Emitter) error {
		fail := func(err error) error { emit(Status{ID: c.ID, Step: "failed", Error: err.Error()}); return err }
		edit, _, err := parseNodeDiagnosticsArgs(c.Args)
		if err != nil {
			return fail(err)
		}
		rpcconfig.DiagnosticsMu.Lock()
		defer rpcconfig.DiagnosticsMu.Unlock()
		path, err := nodeDiagnosticsConfigFile(d)
		if err != nil {
			return fail(err)
		}
		publish := func() {
			if d.PatchNodeStatus != nil {
				d.PatchNodeStatus(map[string]interface{}{"node_diagnostics": rpcconfig.ReadDiagnosticsFile(path)})
			}
		}
		failed := func(err error) error { publish(); return fail(err) }
		if d.Svc == nil || !d.Svc.IsActive(d.UnitName) {
			return failed(ErrNodeNotRunning)
		}
		emit(Status{ID: c.ID, Step: "preparing", Progress: 0.05})
		raw, info, err := rpcconfig.ReadDiagnosticsConfigFile(path)
		if err != nil {
			return failed(err)
		}
		original := diagnosticsFileSnapshot{raw: raw, info: info}
		out, changed, err := rpcconfig.ApplyDiagnostics(raw, edit)
		if err != nil {
			return failed(err)
		}
		emit(Status{ID: c.ID, Step: "backing_up", Progress: 0.25})
		if err := backupHealthConfig(d.BackupRootDir, raw); err != nil {
			return failed(err)
		}
		current := original
		if changed {
			emit(Status{ID: c.ID, Step: "writing_config", Progress: 0.5})
			next, committed, err := replaceHealthConfig(path, out, info, current)
			if committed {
				current = next
			}
			if err != nil {
				return failed(rollbackHealthConfig(path, original, current, err, false, d))
			}
		} else if err := verifyHealthConfig(path, current); err != nil {
			return failed(err)
		}
		// A stopped Node is never started merely to apply settings.
		if !d.Svc.IsActive(d.UnitName) {
			return failed(rollbackHealthConfig(path, original, current, ErrNodeNotRunning, false, d))
		}
		invalidateLogs := func() {
			if d.InvalidateLogRequests != nil {
				d.InvalidateLogRequests()
			}
			if d.PatchNodeStatus != nil {
				d.PatchNodeStatus(LogRequestsInvalidationPatch("Node configuration restart invalidated log requests; refresh required"))
			}
		}
		// Lists are read-only and can start while the service manager is waiting.
		// Bracket the whole restart and rollback recovery so those reads cannot
		// republish a pre-restart queue after the final service attempt completes.
		invalidateLogs()
		defer func() {
			invalidateLogs()
			// Preserve the final authoritative config readback after completion invalidation.
			publish()
		}()
		emit(Status{ID: c.ID, Step: "restarting", Progress: 0.75})
		if err := d.Svc.RestartAndWait(d.UnitName); err != nil {
			return failed(rollbackHealthConfig(path, original, current, errors.New("Node service-manager restart failed"), true, d))
		}
		if !d.Svc.IsActive(d.UnitName) {
			return failed(rollbackHealthConfig(path, original, current, errors.New("Node service is inactive after service-manager restart"), true, d))
		}
		// Recheck after restart: an operator may have edited config during the wait.
		if err := verifyHealthConfig(path, current); err != nil {
			return failed(rollbackHealthConfig(path, original, current, err, true, d))
		}
		publish()
		emit(Status{ID: c.ID, Step: "done", Progress: 1, Message: "Node diagnostics configuration saved; Node service-manager restart completed. Runtime health is checked separately."})
		return nil
	}
}
func nodeDiagnosticsConfigFile(d SetNodeDiagnosticsDeps) (string, error) {
	load := d.LoadState
	if load == nil {
		load = func() (*config.State, error) { return config.LoadState(d.StatePath) }
	}
	state, err := load()
	if err != nil {
		return "", errors.New("cannot load Agent state")
	}
	dir := d.ManagedConfigDir
	if state != nil && state.ConfigPath != "" {
		dir = state.ConfigPath
	}
	dir, err = cleanConfigPath(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yml"), nil
}

type diagnosticsFileSnapshot struct {
	raw  []byte
	info os.FileInfo
}

func sameHealthMetadata(a, b os.FileInfo) bool {
	if a.Mode() != b.Mode() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	as, aok := a.Sys().(*syscall.Stat_t)
	bs, bok := b.Sys().(*syscall.Stat_t)
	return !aok || !bok || (as.Uid == bs.Uid && as.Gid == bs.Gid)
}
func verifyHealthConfig(path string, want diagnosticsFileSnapshot) error {
	raw, info, err := rpcconfig.ReadDiagnosticsConfigFile(path)
	if err != nil || want.info == nil || !os.SameFile(want.info, info) || !sameHealthMetadata(want.info, info) || !bytes.Equal(raw, want.raw) {
		return errors.New("config.yml changed concurrently; refusing to overwrite it")
	}
	return nil
}
func backupHealthConfig(root string, raw []byte) error {
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("missing private backup root")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return errors.New("cannot create config backup root")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("config backup root must be a directory, not a symlink")
	}
	dir, err := os.MkdirTemp(root, "node-health-config-")
	if err != nil {
		return errors.New("cannot create private config backup")
	}
	f, err := os.OpenFile(filepath.Join(dir, "config.yml"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create private config backup")
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = syncHealthDirectory(dir)
	}
	if err == nil {
		err = syncHealthDirectory(root)
	}
	if err != nil {
		return errors.New("cannot persist private config backup")
	}
	return nil
}
func syncHealthDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	err = dir.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

// replaceHealthConfig returns committed=true once rename succeeded, even when
// a later durability/readback check fails, so the caller can roll back safely.
func replaceHealthConfig(path string, data []byte, metadata os.FileInfo, expected diagnosticsFileSnapshot) (diagnosticsFileSnapshot, bool, error) {
	var next diagnosticsFileSnapshot
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.yml.health-*")
	if err != nil {
		return next, false, errors.New("cannot stage config.yml")
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if st, ok := metadata.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid()) {
		if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return next, false, errors.New("cannot preserve config.yml owner")
		}
	}
	// Chown may clear special permission bits, so apply mode afterwards.
	if err := tmp.Chmod(metadata.Mode()); err != nil {
		return next, false, errors.New("cannot preserve config.yml mode")
	}
	if _, err := tmp.Write(data); err != nil {
		return next, false, errors.New("cannot stage config.yml")
	}
	if err := tmp.Sync(); err != nil {
		return next, false, errors.New("cannot sync staged config.yml")
	}
	info, err := tmp.Stat()
	if err != nil {
		return next, false, errors.New("cannot inspect staged config.yml")
	}
	if err := tmp.Close(); err != nil {
		return next, false, errors.New("cannot close staged config.yml")
	}
	if err := verifyHealthConfig(path, expected); err != nil {
		return next, false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return next, false, errors.New("cannot atomically replace config.yml")
	}
	next = diagnosticsFileSnapshot{raw: data, info: info}
	if err := syncHealthDirectory(filepath.Dir(path)); err != nil {
		return next, true, errors.New("cannot sync config.yml directory")
	}
	if err := verifyHealthConfig(path, next); err != nil {
		return next, true, errors.New("config.yml readback verification failed")
	}
	return next, true, nil
}
func rollbackHealthConfig(path string, original, current diagnosticsFileSnapshot, cause error, recoverService bool, d SetNodeDiagnosticsDeps) error {
	// Identity and exact bytes both matter. Refuse a human's intervening edit,
	// even if it happens to equal the bytes we originally wrote.
	if err := verifyHealthConfig(path, current); err != nil {
		return fmt.Errorf("%v; rollback failed: config.yml changed concurrently; private backup retained", cause)
	}
	if !bytes.Equal(original.raw, current.raw) {
		if _, _, err := replaceHealthConfig(path, original.raw, original.info, current); err != nil {
			return fmt.Errorf("%v; rollback failed: cannot restore and verify original config.yml; private backup retained", cause)
		}
	}
	recovery := ""
	if recoverService && d.Svc != nil {
		if err := d.Svc.RestartAndWait(d.UnitName); err != nil || !d.Svc.IsActive(d.UnitName) {
			recovery = "; old-service recovery failed"
		} else {
			recovery = "; old-service recovery completed (service manager only)"
		}
	}
	return fmt.Errorf("%v; rollback succeeded%s; private backup retained", cause, recovery)
}
