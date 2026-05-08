// Package launchd renders LaunchAgent plist files and wraps launchctl
// invocations against the gui/<UID> domain. Mirrors internal/systemd in
// purpose but targets macOS user-level service management — no sudo, no
// /etc/systemd, no daemon-reload.
package launchd

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
)

// NodeMetricsAddr is the local-only Prometheus endpoint we ask the node
// to expose. Same value as systemd.NodeMetricsAddr — kept duplicated to
// avoid a cross-package dependency in the platform-specific packages.
const NodeMetricsAddr = "127.0.0.1:49163"

// plistDoc + plistDict are the minimal subset of Apple's plist DTD we
// emit. Using encoding/xml gives us correct character escaping for paths
// containing spaces or odd quotes (e.g. "Application Support") without
// hand-rolling escape logic.
type plistDoc struct {
	XMLName xml.Name  `xml:"plist"`
	Version string    `xml:"version,attr"`
	Dict    plistDict `xml:"dict"`
}

type plistDict struct {
	Entries []any `xml:",any"`
}

type plistKey struct {
	XMLName xml.Name `xml:"key"`
	Value   string   `xml:",chardata"`
}

type plistString struct {
	XMLName xml.Name `xml:"string"`
	Value   string   `xml:",chardata"`
}

type plistTrue struct {
	XMLName xml.Name `xml:"true"`
}

type plistArray struct {
	XMLName xml.Name      `xml:"array"`
	Strings []plistString `xml:"string"`
}

// pair is shorthand for "<key>X</key><string>Y</string>". Returns the
// XML-encoded bytes.
func pair(k, v string) []any {
	return []any{plistKey{Value: k}, plistString{Value: v}}
}

// RenderNodePlist returns the LaunchAgent plist body for the Quilibrium
// node service. Mirrors systemd.RenderNodeUnit semantics:
//
//   - Fresh install: ConfigPath empty, WorkDir set to the parent of the
//     `.config` directory; node uses its built-in `.config` default.
//   - Migrated install: ConfigPath empty, WorkDir set to the parent of the
//     user-supplied `.config` directory; query commands still use `--config`.
//
// RunAtLoad=true so the job starts automatically when the user logs in.
// KeepAlive's `Crashed` clause restarts the process on non-zero exits but
// not on graceful exits (so cleanup_residue / Stop don't trigger relaunch).
func RenderNodePlist(in svcctl.NodeServiceInput) string {
	args := []plistString{
		{Value: in.BinaryPath},
		{Value: "--prometheus-server"},
		{Value: NodeMetricsAddr},
	}
	if in.ConfigPath != "" {
		args = append(args, plistString{Value: "--config"}, plistString{Value: in.ConfigPath})
	}

	entries := []any{
		plistKey{Value: "Label"}, plistString{Value: in.Label},
		plistKey{Value: "ProgramArguments"}, plistArray{Strings: args},
		plistKey{Value: "RunAtLoad"}, plistTrue{},
		plistKey{Value: "ProcessType"}, plistString{Value: "Background"},
	}
	if in.WorkDir != "" {
		entries = append(entries, plistKey{Value: "WorkingDirectory"}, plistString{Value: in.WorkDir})
	}
	if in.LogPath != "" {
		entries = append(entries,
			plistKey{Value: "StandardOutPath"}, plistString{Value: in.LogPath},
			plistKey{Value: "StandardErrorPath"}, plistString{Value: in.LogPath},
		)
	}

	body, err := xml.MarshalIndent(plistDoc{Version: "1.0", Dict: plistDict{Entries: entries}}, "", "  ")
	if err != nil {
		// xml.MarshalIndent on this hand-built tree only fails if Go's
		// internals break — we treat that as a programming error and panic
		// rather than smuggle a partial plist to disk.
		panic(fmt.Sprintf("plist marshal: %v", err))
	}
	// Go's encoding/xml emits empty elements as `<true></true>` (the
	// XML-standard open/close form). macOS launchd's plist parser only
	// accepts the self-closing `<true/>` shorthand — bootstrap fails with
	// "Bootstrap failed: 5: Input/output error" otherwise. Post-process
	// the output to swap the two forms before writing to disk.
	out := string(body)
	out = strings.ReplaceAll(out, "<true></true>", "<true/>")
	out = strings.ReplaceAll(out, "<false></false>", "<false/>")
	return xml.Header + `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" +
		out + "\n"
}
