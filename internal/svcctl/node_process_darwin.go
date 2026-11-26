//go:build darwin

package svcctl

import (
	"context"
	"os/exec"
	"strings"
)

func ReadNodeProcess(ctx context.Context, unit string) NodeProcess {
	ctl := launchdCtl{}
	raw, err := exec.CommandContext(ctx, "launchctl", "print", ctl.domainTarget(unit)).Output()
	if err != nil {
		return NodeProcess{}
	}
	return parseLoadedNode(string(raw))
}

func parseLoadedNode(raw string) NodeProcess {
	info := NodeProcess{PID: parsePid(raw)}
	if info.PID <= 0 {
		return info
	}
	var cwd string
	var args []string
	inArguments := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "arguments = {":
			inArguments = true
		case inArguments && line == "}":
			inArguments = false
		case inArguments:
			args = append(args, line)
		default:
			if value, ok := strings.CutPrefix(line, "working directory = "); ok {
				cwd = value
			}
		}
	}
	if len(args) > 0 {
		info.ConfigFile = nodeConfigFile(args[1:], cwd)
	}
	return info
}
