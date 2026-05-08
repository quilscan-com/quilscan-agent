//go:build darwin

package svcctl

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// launchdCtl wraps launchctl invocations against the user's gui/<UID>
// domain. We never touch the system domain because Mac users (per the
// A2 design) run their agent as a user-level LaunchAgent — no sudo
// required for any of these calls.
type launchdCtl struct{}

// New returns the platform-appropriate Ctl. On macOS: launchd.
func New() Ctl { return launchdCtl{} }

func (launchdCtl) plistPath(label string) string {
	home, _ := os.UserHomeDir()
	return home + "/Library/LaunchAgents/" + label + ".plist"
}

func (l launchdCtl) domainTarget(label string) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
}

// Start loads the plist if needed and ensures the job is running.
// launchctl bootstrap is idempotent for already-loaded jobs only when
// the plist hasn't changed; we treat "already bootstrapped" as success
// then issue a kickstart so the job reaches running state regardless.
func (l launchdCtl) Start(label string) error {
	plist := l.plistPath(label)
	if _, err := os.Stat(plist); err != nil {
		return fmt.Errorf("plist not found: %s", plist)
	}
	if !l.isLoaded(label) {
		if err := launchctl("bootstrap",
			fmt.Sprintf("gui/%d", os.Getuid()), plist); err != nil {
			return err
		}
	}
	// kickstart starts the job if not running, no-op otherwise. Without
	// it, a bootstrap of a job missing RunAtLoad would just register it.
	return launchctl("kickstart", l.domainTarget(label))
}

// Stop unloads the job entirely (bootout). Subsequent Start re-bootstraps.
// We picked bootout over `launchctl stop` because stop is deprecated and
// only sends a hint; bootout reliably terminates the process.
func (l launchdCtl) Stop(label string) error {
	if !l.isLoaded(label) {
		return nil
	}
	return launchctl("bootout", l.domainTarget(label))
}

// Restart uses kickstart -k which kills the existing process and starts
// a new one. -k is the launchctl idiom for "restart".
func (l launchdCtl) Restart(label string) error {
	if !l.isLoaded(label) {
		// Not currently loaded — fall back to a fresh Start.
		return l.Start(label)
	}
	return launchctl("kickstart", "-k", l.domainTarget(label))
}

// Enable is a no-op on macOS: the plist's RunAtLoad / KeepAlive attributes
// already control whether the job runs at user-login time.
func (launchdCtl) Enable(string) error { return nil }

// Disable bootouts and removes the plist from LaunchAgents — equivalent
// to systemctl disable (won't auto-start at next login). The plist file
// stays on disk for the caller's own bookkeeping; only the launchd
// registration is removed.
func (l launchdCtl) Disable(label string) error {
	if !l.isLoaded(label) {
		return nil
	}
	return launchctl("bootout", l.domainTarget(label))
}

// Reload is a no-op: launchd reloads from the plist file on next bootstrap.
func (launchdCtl) Reload() error { return nil }

// IsActive parses `launchctl print` output for `state = running`. The
// command exits non-zero when the service isn't loaded at all, in which
// case we return false without inspecting output.
func (l launchdCtl) IsActive(label string) bool {
	out, err := exec.Command("launchctl", "print", l.domainTarget(label)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "state = running")
}

// StartedAt is a two-step lookup: launchctl print gives us the pid, then
// `ps -p PID -o etime=` returns the elapsed time as a numeric
// [[DD-]HH:]MM:SS string. The wall-clock start time is now() - elapsed.
// Using etime rather than lstart sidesteps every pitfall of parsing a
// human-formatted timestamp (locale, timezone, weekday padding).
func (l launchdCtl) StartedAt(label string) time.Time {
	out, err := exec.Command("launchctl", "print", l.domainTarget(label)).Output()
	if err != nil {
		return time.Time{}
	}
	pid := parsePid(string(out))
	if pid <= 0 {
		return time.Time{}
	}
	psOut, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "etime=").Output()
	if err != nil {
		return time.Time{}
	}
	d, err := parseEtime(strings.TrimSpace(string(psOut)))
	if err != nil {
		return time.Time{}
	}
	return time.Now().Add(-d)
}

// parseEtime parses ps's elapsed-time field. ps emits one of:
//
//	"MM:SS"        — under one hour
//	"HH:MM:SS"     — under one day
//	"DD-HH:MM:SS"  — one day or more
func parseEtime(s string) (time.Duration, error) {
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, fmt.Errorf("etime days: %w", err)
		}
		days = d
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, fmt.Errorf("etime: unexpected layout %q", s)
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, fmt.Errorf("etime field %d: %w", i, err)
		}
		nums[i] = n
	}
	var hh, mm, ss int
	if len(parts) == 3 {
		hh, mm, ss = nums[0], nums[1], nums[2]
	} else {
		mm, ss = nums[0], nums[1]
	}
	return time.Duration(days)*24*time.Hour +
		time.Duration(hh)*time.Hour +
		time.Duration(mm)*time.Minute +
		time.Duration(ss)*time.Second, nil
}

func (l launchdCtl) isLoaded(label string) bool {
	return exec.Command("launchctl", "print", l.domainTarget(label)).Run() == nil
}

// parsePid scans the multi-line `launchctl print` output for a line
// matching `\tpid = <num>` (the literal indentation inside the print
// dictionary), returning 0 if not found.
func parsePid(out string) int {
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, "pid = ") {
			continue
		}
		v := strings.TrimPrefix(s, "pid = ")
		v = strings.TrimSpace(v)
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func launchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %v: %w — %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}
