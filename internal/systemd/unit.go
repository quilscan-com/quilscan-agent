// Package systemd renders unit files and wraps systemctl calls.
package systemd

import (
	"fmt"
)

// NodeMetricsAddr is the local-only Prometheus endpoint we ask the node to
// expose. The agent's metrics collector scrapes
//
//	http://NodeMetricsAddr/metrics
//
// every few seconds and forwards selected gauges to backend. Bound to the
// loopback interface so it never reaches the public internet.
const NodeMetricsAddr = "127.0.0.1:49163"

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
//     parent of the user-supplied `.config` directory. Query commands use
//     `--config`; service startup relies on the working directory.
type UnitInput struct {
	BinaryPath string
	User       string
	ConfigPath string // when non-empty, emit `-config <path>`; otherwise omit the flag
	WorkDir    string // when non-empty, emit WorkingDirectory=<path>
}

// RenderNodeUnit returns the systemd unit file content for the node service.
// Template is const — users auditing can grep for it and be sure no dynamic
// fields influence directives like ExecStart beyond BinaryPath/ConfigPath.
//
// The --prometheus-server flag enables the node's /metrics endpoint at
// NodeMetricsAddr; without it the agent has no way to read live frame height
// or process memory.
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
	// Use double-dash long flags. Quilibrium 2.1.0.23+ uses a clap-based
	// CLI parser that strictly requires `--` for multi-char flags;
	// single-dash forms like `-prometheus-server` are mis-parsed as
	// `-p` and the node fails to start. 2.1.0.22 (Go flag) accepts
	// both `-` and `--` forms, so `--` is backwards compatible.
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
%sExecStart=%s%s --prometheus-server %s
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`, in.User, workDirLine, in.BinaryPath, configFlag, NodeMetricsAddr)
}
