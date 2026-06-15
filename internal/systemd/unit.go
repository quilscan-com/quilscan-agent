// Package systemd renders unit files and wraps systemctl calls.
package systemd

import "fmt"

// UnitInput feeds the node unit template.
//
// Two install shapes are supported:
//
//  1. Fresh install (managed by agent): leave ConfigPath empty and set
//     WorkDir to the directory whose `.config/` subdir holds node state.
//     The node binary, run without `-config`, defaults to `.config`
//     relative to WorkDir.
//
//  2. Migrated install: leave ConfigPath empty and set WorkDir to the
//     parent of the user-supplied `.config` directory. Service startup
//     relies on the working directory, matching the fresh install shape.
type UnitInput struct {
	BinaryPath string
	User       string
	ConfigPath string // when non-empty, emit `-config <path>`; otherwise omit the flag
	WorkDir    string // when non-empty, emit WorkingDirectory=<path>
}

// RenderNodeUnit returns the systemd unit file content for the node service.
// Template is const — users auditing can grep for it and be sure no dynamic
// fields influence directives like ExecStart beyond BinaryPath/ConfigPath.
func RenderNodeUnit(in UnitInput) string {
	// Defence in depth: a control character in any field would either bleed
	// into a new directive (\n, \r) or truncate the unit (\x00). Inputs are
	// already filtered upstream; we panic here so any future caller that
	// forgets to sanitize fails loudly rather than silently rendering an
	// injectable unit.
	for label, v := range map[string]string{
		"BinaryPath": in.BinaryPath,
		"User":       in.User,
		"ConfigPath": in.ConfigPath,
		"WorkDir":    in.WorkDir,
	} {
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				panic(fmt.Sprintf("RenderNodeUnit: %s contains control character", label))
			}
		}
	}
	configFlag := ""
	if in.ConfigPath != "" {
		configFlag = " --config " + in.ConfigPath
	}
	workDirLine := ""
	if in.WorkDir != "" {
		workDirLine = "WorkingDirectory=" + in.WorkDir + "\n"
	}
	return fmt.Sprintf(`[Unit]
Description=Quilibrium Node (managed by quilscan-agent)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
%sExecStart=%s%s
Restart=on-failure
RestartSec=5
LimitNOFILE=%d

[Install]
WantedBy=multi-user.target
`, in.User, workDirLine, in.BinaryPath, configFlag, NodeFileLimit)
}
