package systemd

import (
	"fmt"
	"os/exec"
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

func run(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w — %s", args, err, string(out))
	}
	return nil
}
