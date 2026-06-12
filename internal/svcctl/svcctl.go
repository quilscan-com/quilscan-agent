// Package svcctl is the platform-agnostic service-controller interface used
// by command handlers to manage long-running services. On Linux this is a
// thin wrapper around systemd; on macOS, around launchd.
//
// The "name" argument is the service identifier in whatever form the
// underlying service manager expects:
//
//	Linux  : a systemd unit name, e.g. "quilibrium-node.service"
//	macOS  : a launchd label,       e.g. "com.quilscan.node"
//
// The svcctl package never assumes one form or the other — handlers just
// pass through whatever the platform's PathBundle gave them.
package svcctl

import (
	"path/filepath"
	"strings"
	"time"
)

// UnitFilePath returns the on-disk file path of the service definition
// for the given unit/label. systemd unit names already carry their own
// suffix (`.service`, `.timer`, `.socket`) and are used verbatim as
// the file name. launchd labels (e.g. "com.quilscan.node") are
// suffix-free; the on-disk plist file appends ".plist". Without this
// helper, callers that hand-roll filepath.Join(unitDir, unitName)
// silently miss the plist on macOS and the residue / cleanup paths
// break.
//
// We deliberately key off the *name shape* rather than runtime.GOOS so
// the helper behaves correctly even when test fixtures simulate a
// Linux scenario from a Darwin build host.
func UnitFilePath(unitDir, unitName string) string {
	if !hasServiceSuffix(unitName) {
		unitName = unitName + ".plist"
	}
	return filepath.Join(unitDir, unitName)
}

func hasServiceSuffix(name string) bool {
	for _, ext := range []string{".plist", ".service", ".timer", ".socket"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// NodeServiceInput feeds a platform-specific service-definition renderer
// (systemd unit on Linux, launchd plist on macOS). The same struct works
// for both because the agent only ever needs to encode three node-launch
// concerns: which binary, working directory for state, and an optional
// explicit config path for callers that deliberately need one. The current
// install/migrate flow leaves ConfigPath empty for service startup and uses
// WorkDir so relative paths inside config.yml continue to resolve.
type NodeServiceInput struct {
	BinaryPath string
	User       string // ignored on macOS; launchd scope is selected by the plist location
	ConfigPath string // when non-empty, emit `-config <path>`; otherwise omit the flag
	WorkDir    string // when non-empty, set as service WorkingDirectory
	Label      string // service identifier (systemd unit name OR launchd label)
	LogPath    string // launchd: redirect StandardOut/Err here. systemd ignores (journalctl handles it).
}

// FSOps is the narrow contract install + cleanup need to put a service
// definition on disk and ask the service manager to start it. Linux uses
// systemd unit files + daemon-reload; macOS uses launchd plists +
// bootstrap.
type FSOps interface {
	WriteUnit(name, content string) error // file path is determined by the implementation
	DaemonReload() error                  // no-op on macOS
	Start(name string) error
}

// Ctl is the subset of service-manager actions that command handlers and
// the reconcile loop need. Each platform package provides a Ctl
// implementation; consumers receive it via a New() factory keyed off
// runtime.GOOS so handlers stay portable.
type Ctl interface {
	// Start ensures the service is loaded and running. Idempotent.
	Start(name string) error
	// Stop terminates the service but leaves the on-disk definition in
	// place so a subsequent Start works.
	Stop(name string) error
	// Restart bounces the service. On Linux uses systemctl restart
	// --no-block so a self-restart from inside the service does not
	// deadlock; on macOS uses launchctl kickstart -k for the same reason.
	Restart(name string) error
	// Enable marks the service to start at boot/login. On Linux:
	// systemctl enable. On macOS this is a no-op because the launchd
	// plist's RunAtLoad attribute already encodes that behaviour.
	Enable(name string) error
	// Disable removes the start-at-boot/login link. On Linux:
	// systemctl disable. On macOS: launchctl bootout (also stops the job).
	Disable(name string) error
	// Reload reloads the service manager's config. On Linux:
	// systemctl daemon-reload. On macOS: no-op (launchd reloads from the
	// plist on next bootstrap).
	Reload() error
	// IsActive reports whether the service is running right now.
	IsActive(name string) bool
	// StartedAt returns when the service entered its current run state,
	// or zero time if the service is not running or the timestamp is
	// unavailable.
	StartedAt(name string) time.Time
}
