package systemd

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
)

// Ctl wraps systemctl invocations. The list of allowed verbs is a closed set —
// `ctl.Run("reboot", "...")` won't compile.
type Ctl struct{}

func (Ctl) Start(unit string) error   { return run("start", unit) }
func (Ctl) Stop(unit string) error    { return run("stop", unit) }
func (Ctl) Enable(unit string) error  { return run("enable", unit) }
func (Ctl) Disable(unit string) error { return run("disable", unit) }
func (Ctl) Reload() error             { return run("daemon-reload") }

// IsActive returns true when systemctl reports the unit as active.
// `--quiet` suppresses output; we only care about the exit code.
func (Ctl) IsActive(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

func (Ctl) CheckReleaseStartup(unit string) error {
	return svcctl.CheckReleaseStartup(unit, func() (int, error) {
		properties, err := releaseServiceProperties(unit)
		if err != nil {
			return 0, err
		}
		return releaseStartupPID(properties)
	})
}

func (Ctl) FailedReleaseStart(unit string) (bool, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		properties, err := releaseServiceProperties(unit)
		if err != nil {
			return false, err
		}
		if failedReleaseStart(properties) {
			return true, nil
		}
		restarts, _ := strconv.Atoi(properties["NRestarts"])
		if restarts == 0 || properties["ActiveState"] != "active" || !time.Now().Before(deadline) {
			return false, nil
		}
		// A restart loop may be between attempts while the new PID still
		// appears active. Observe the exit before treating it as failed.
		time.Sleep(150 * time.Millisecond)
	}
}

func failedReleaseStart(properties map[string]string) bool {
	return properties["ExecMainStatus"] == "1" &&
		(properties["ActiveState"] == "failed" || strings.HasPrefix(properties["SubState"], "auto-restart"))
}

func releaseServiceProperties(unit string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", unit,
		"--property=ActiveState,SubState,MainPID,NRestarts,Result,ExecMainStatus").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(string(out)))
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			properties[key] = value
		}
	}
	return properties, nil
}

func releaseStartupPID(properties map[string]string) (int, error) {
	pid, err := strconv.Atoi(properties["MainPID"])
	if err != nil {
		return 0, fmt.Errorf("invalid systemd MainPID: %w", err)
	}
	restarts, _ := strconv.Atoi(properties["NRestarts"])
	if restarts > 0 || properties["SubState"] == "auto-restart" ||
		properties["ActiveState"] == "failed" || properties["ActiveState"] == "inactive" {
		return 0, fmt.Errorf("systemd state=%s/%s result=%s restarts=%d",
			properties["ActiveState"], properties["SubState"], properties["Result"], restarts)
	}
	return pid, nil
}

func run(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w — %s", args, err, string(out))
	}
	return nil
}
