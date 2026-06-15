package launchd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FSOps implements actions.SystemdOps + (a superset of) what install
// needs to put a launchd plist on disk and ask launchd to bootstrap it.
// The struct field name is "UnitDir" purely for parallelism with
// systemd.FSOps. On macOS it can be either the legacy user LaunchAgents
// directory or the current system LaunchDaemons directory.
type FSOps struct {
	UnitDir string
}

// WriteUnit drops the rendered plist body at <UnitDir>/<name>. `name` is
// expected to already include the .plist extension (callers pass label +
// ".plist"). Permissions 0644 match launchd's plist requirements.
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

// Start bootstraps the launchd job into the right domain and then kickstarts
// it so it transitions to the running state immediately.
// `name` is the launchd label (without .plist suffix) — the install
// handler passes the same identifier used as systemd unit name on Linux.
func (f FSOps) Start(name string) error {
	unitDir := unitDirOrDefault(f.UnitDir)
	plistPath := filepath.Join(unitDir, plistFileName(name))
	if _, err := os.Stat(plistPath); err != nil {
		return fmt.Errorf("plist not found: %s", plistPath)
	}
	// Idempotent: if already loaded skip bootstrap; just kickstart.
	domain := domainForUnitDir(unitDir)
	return startJobWithRetry(domain, name, plistPath, isLoaded, launchctl, func() {
		time.Sleep(launchdStartRetryDelay)
	})
}

var launchdStartRetryDelay = time.Second

func startJobWithRetry(domain, name, plistPath string, loaded func(string, string) bool, run func(...string) error, sleep func()) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && sleep != nil {
			sleep()
		}
		if !loaded(domain, name) {
			if err := run("bootstrap", domain, plistPath); err != nil {
				lastErr = err
				continue
			}
		}
		if err := run("kickstart", launchTarget(domain, name)); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

func plistFileName(name string) string {
	if strings.HasSuffix(name, ".plist") {
		return name
	}
	return name + ".plist"
}

func isLoaded(domain, label string) bool {
	return exec.Command("launchctl", "print", launchTarget(domain, label)).Run() == nil
}

func launchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %v: %w — %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func unitDirOrDefault(unitDir string) string {
	if strings.TrimSpace(unitDir) != "" {
		return unitDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library/LaunchAgents")
}

func domainForUnitDir(unitDir string) string {
	if filepath.Clean(unitDir) == "/Library/LaunchDaemons" {
		return "system"
	}
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func launchTarget(domain, label string) string {
	return domain + "/" + label
}
