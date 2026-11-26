//go:build linux

package svcctl

import (
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v3/process"
)

func ReadNodeProcess(ctx context.Context, unit string) NodeProcess {
	raw, err := exec.CommandContext(ctx, "systemctl", "show", "--property=MainPID", "--value", "--", unit).Output()
	if err != nil {
		return NodeProcess{}
	}
	pid, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 32)
	if err != nil || pid <= 0 {
		return NodeProcess{}
	}
	info := NodeProcess{PID: int(pid)}
	p, err := process.NewProcessWithContext(ctx, int32(pid))
	if err != nil {
		return info
	}
	args, err := p.CmdlineSliceWithContext(ctx)
	if err != nil || len(args) == 0 {
		return info
	}
	cwd, _ := p.CwdWithContext(ctx)
	info.ConfigFile = nodeConfigFile(args[1:], cwd)
	return info
}
