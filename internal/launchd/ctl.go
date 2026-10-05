package launchd

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	stopTimeout      = 30 * time.Second
	stopPollInterval = 100 * time.Millisecond
)

// Ctl is the macOS counterpart to systemd.Ctl. It satisfies the small
// interfaces defined across actions / rpcconfig (Start, Stop, Disable,
// Reload) so command handlers wired in cmd/agent/main.go can swap
// systemd.Ctl <-> launchd.Ctl based on runtime.GOOS without changing
// any handler internals.
//
// Calls target either the user's gui/<UID> domain or the system domain,
// depending on UnitDir. /Library/LaunchDaemons is system-scope; user
// LaunchAgents stay under gui/<UID>.
type Ctl struct {
	UnitDir string // LaunchAgents or LaunchDaemons dir; used to resolve domain + plist
}

// Start re-uses FSOps.Start so the bootstrap+kickstart logic stays in one place.
func (c Ctl) Start(name string) error {
	return FSOps{UnitDir: c.unitDirOrDefault()}.Start(name)
}

// Stop bootouts the job. Idempotent: if the job is not loaded we silently
// succeed.
func (c Ctl) Stop(name string) error {
	domain := c.domain()
	target := launchTarget(domain, name)
	out, err := launchctlOutput("print", target)
	if err != nil {
		return nil
	}
	pid := parseLaunchdPID(out)
	if err := launchctl("bootout", target); err != nil {
		return err
	}
	if pid <= 0 {
		return nil
	}
	if err := waitForPIDExit(pid, stopTimeout, stopPollInterval, processAlive); err != nil {
		return fmt.Errorf("wait for %s to stop: %w", name, err)
	}
	return nil
}

// Disable on macOS is the same as Stop — bootout removes the job and
// prevents auto-start at next login because the RunAtLoad in the plist
// only triggers on bootstrap.
func (c Ctl) Disable(name string) error { return c.Stop(name) }

// Reload is a no-op on macOS (launchd reloads from plist on bootstrap).
func (Ctl) Reload() error { return nil }

// IsActive parses `launchctl print` output for `state = running`. The
// command exits non-zero when the service isn't loaded at all, in
// which case we return false without inspecting output. Mirrors
// svcctl/launchd_darwin.go's IsActive — duplicated rather than
// cross-imported to keep the platform handler-side Ctl symmetric with
// systemd.Ctl (no svcctl dependency leaking into either).
func (c Ctl) IsActive(name string) bool {
	domain := c.domain()
	out, err := launchctlOutput("print", launchTarget(domain, name))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "state = running" {
			return true
		}
	}
	return false
}

// launchctlOutput runs launchctl with the given args and returns stdout
// as a string. Lives next to Ctl.IsActive so the parsing call site
// stays self-contained.
func launchctlOutput(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

func parseLaunchdPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pid = ") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
		if err == nil {
			return pid
		}
	}
	return 0
}

func waitForPIDExit(pid int, timeout, pollInterval time.Duration, alive func(int) bool) error {
	if pid <= 0 || alive == nil {
		return nil
	}
	if pollInterval <= 0 {
		pollInterval = stopPollInterval
	}
	deadline := time.Now().Add(timeout)
	for alive(pid) {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process %d did not exit within %s", pid, timeout)
		}
		time.Sleep(pollInterval)
	}
	return nil
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (c Ctl) unitDirOrDefault() string {
	return unitDirOrDefault(c.UnitDir)
}

func (c Ctl) domain() string {
	return domainForUnitDir(c.unitDirOrDefault())
}
