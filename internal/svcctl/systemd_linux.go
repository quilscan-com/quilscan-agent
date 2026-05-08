//go:build linux

package svcctl

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// systemdCtl wraps systemctl invocations. The list of allowed verbs is a
// closed set — `s.run("reboot", "...")` won't compile.
type systemdCtl struct{}

// New returns the platform-appropriate Ctl. On Linux: systemd.
func New() Ctl { return systemdCtl{} }

func (systemdCtl) Start(name string) error  { return run("start", name) }
func (systemdCtl) Stop(name string) error   { return run("stop", name) }
func (systemdCtl) Enable(name string) error { return run("enable", name) }
func (systemdCtl) Disable(name string) error {
	return run("disable", name)
}
func (systemdCtl) Reload() error { return run("daemon-reload") }

// Restart uses --no-block so a self-restart (from inside the service we
// are about to bounce) does not deadlock waiting for the unit we are
// about to be SIGTERM'd on.
func (systemdCtl) Restart(name string) error {
	return run("restart", "--no-block", name)
}

func (systemdCtl) IsActive(name string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", name).Run() == nil
}

// StartedAt converts systemd's ActiveEnterTimestampMonotonic — microseconds
// since system boot — into a wall-clock time using /proc/uptime as the boot
// anchor. The monotonic field is purely numeric and present on every
// systemd version we support, so we never touch the human-formatted
// ActiveEnterTimestamp string (locale and timezone hazards).
func (systemdCtl) StartedAt(name string) time.Time {
	out, err := exec.Command("systemctl", "show", name,
		"--property=ActiveEnterTimestampMonotonic", "--value").Output()
	if err != nil {
		return time.Time{}
	}
	// 0 means the unit never reached active state (masked / inactive / never started).
	monoUs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || monoUs <= 0 {
		return time.Time{}
	}
	upBytes, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}
	}
	fields := strings.Fields(string(upBytes))
	if len(fields) == 0 {
		return time.Time{}
	}
	upSec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Time{}
	}
	bootTime := time.Now().Add(-time.Duration(upSec * float64(time.Second)))
	return bootTime.Add(time.Duration(monoUs) * time.Microsecond)
}

func run(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w — %s", args, err, string(out))
	}
	return nil
}
