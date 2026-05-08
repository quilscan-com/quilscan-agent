package launchd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FSOps implements actions.SystemdOps + (a superset of) what install
// needs to put a LaunchAgent on disk and ask launchd to bootstrap it.
// The struct field name is "UnitDir" purely for parallelism with
// systemd.FSOps — for launchd it's the LaunchAgents directory
// (~/Library/LaunchAgents).
type FSOps struct {
	UnitDir string
}

// WriteUnit drops the rendered plist body at <UnitDir>/<name>. `name` is
// expected to already include the .plist extension (callers pass label +
// ".plist"). Permissions 0644 — the file is read by launchd as the user.
func (f FSOps) WriteUnit(name, content string) error {
	if !strings.HasSuffix(name, ".plist") {
		// systemd path passed "quilibrium-node.service"; on macOS we need
		// "<label>.plist". Be permissive: append .plist if the caller
		// forgot, so existing install handler logic stays portable.
		name = name + ".plist"
	}
	return os.WriteFile(filepath.Join(f.UnitDir, name), []byte(content), 0o644)
}

// DaemonReload is a no-op on macOS. launchd reloads from the plist file
// each time we bootstrap a job, so there's no equivalent of
// systemctl daemon-reload to trigger.
func (FSOps) DaemonReload() error { return nil }

// Start bootstraps the LaunchAgent into the user's gui/<UID> domain and
// then kickstarts it so it transitions to the running state immediately.
// `name` is the launchd label (without .plist suffix) — the install
// handler passes the same identifier used as systemd unit name on Linux.
func (f FSOps) Start(name string) error {
	plistPath := filepath.Join(f.UnitDir, plistFileName(name))
	if _, err := os.Stat(plistPath); err != nil {
		return fmt.Errorf("plist not found: %s", plistPath)
	}
	// Idempotent: if already loaded skip bootstrap; just kickstart.
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if !isLoaded(domain, name) {
		if err := launchctl("bootstrap", domain, plistPath); err != nil {
			return err
		}
	}
	return launchctl("kickstart", fmt.Sprintf("%s/%s", domain, name))
}

func plistFileName(name string) string {
	if strings.HasSuffix(name, ".plist") {
		return name
	}
	return name + ".plist"
}

func isLoaded(domain, label string) bool {
	return exec.Command("launchctl", "print", fmt.Sprintf("%s/%s", domain, label)).Run() == nil
}

func launchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %v: %w — %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}
