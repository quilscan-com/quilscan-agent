// Package launchd renders LaunchAgent plist files and wraps launchctl
// invocations against the gui/<UID> domain. Mirrors internal/systemd in
// purpose but targets macOS user-level service management — no sudo, no
// /etc/systemd, no daemon-reload.
package launchd

import (
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
)

const NodeFileLimit = 61440

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

type plistInlineDict struct {
	XMLName xml.Name `xml:"dict"`
	Entries []any    `xml:",any"`
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

type plistInteger struct {
	XMLName xml.Name `xml:"integer"`
	Value   int      `xml:",chardata"`
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
//     user-supplied `.config` directory; startup matches the fresh install shape.
//
// RunAtLoad=true so the job starts automatically when the user logs in.
// KeepAlive's `Crashed` clause restarts the process on non-zero exits but
// not on graceful exits (so cleanup_residue / Stop don't trigger relaunch).
func RenderNodePlist(in svcctl.NodeServiceInput) string {
	args := []plistString{
		{Value: in.BinaryPath},
	}
	if in.ConfigPath != "" {
		args = append(args, plistString{Value: "--config"}, plistString{Value: in.ConfigPath})
	}

	entries := []any{
		plistKey{Value: "Label"}, plistString{Value: in.Label},
		plistKey{Value: "ProgramArguments"}, plistArray{Strings: args},
		plistKey{Value: "RunAtLoad"}, plistTrue{},
		plistKey{Value: "ProcessType"}, plistString{Value: "Background"},
		plistKey{Value: "SoftResourceLimits"}, nodeFileLimitDict(NodeFileLimit),
		plistKey{Value: "HardResourceLimits"}, nodeFileLimitDict(NodeFileLimit),
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

func nodeFileLimitDict(limit int) plistInlineDict {
	return plistInlineDict{Entries: []any{
		plistKey{Value: "NumberOfFiles"},
		plistInteger{Value: limit},
	}}
}

// EnsureNodeFileLimit updates an existing LaunchAgent plist so older managed
// node installs get the same file descriptor limit as freshly rendered plists.
func EnsureNodeFileLimit(plistPath string, limit int) (bool, error) {
	raw, err := os.ReadFile(plistPath)
	if err != nil {
		return false, err
	}
	updated, changed, err := EnsureNodeFileLimitXML(string(raw), limit)
	if err != nil || !changed {
		return changed, err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(plistPath); err == nil {
		mode = st.Mode().Perm()
	}
	return true, os.WriteFile(plistPath, []byte(updated), mode)
}

func EnsureNodeFileLimitXML(raw string, limit int) (string, bool, error) {
	if limit <= 0 {
		return raw, false, fmt.Errorf("invalid node file limit %d", limit)
	}
	if hasNodeFileLimit(raw, "SoftResourceLimits", limit) &&
		hasNodeFileLimit(raw, "HardResourceLimits", limit) {
		return raw, false, nil
	}
	out := removeResourceLimitBlock(raw, "SoftResourceLimits")
	out = removeResourceLimitBlock(out, "HardResourceLimits")
	block := resourceLimitBlock("SoftResourceLimits", limit) + resourceLimitBlock("HardResourceLimits", limit)
	insertAt := plistInsertIndex(out)
	if insertAt < 0 {
		return raw, false, fmt.Errorf("plist root dict close not found")
	}
	return out[:insertAt] + block + out[insertAt:], true, nil
}

func plistInsertIndex(raw string) int {
	if loc := regexp.MustCompile(`\n[ \t]*<key>StandardOutPath</key>`).FindStringIndex(raw); loc != nil {
		return loc[0]
	}
	matches := regexp.MustCompile(`\n[ \t]*</dict>`).FindAllStringIndex(raw, -1)
	if len(matches) == 0 {
		return -1
	}
	return matches[len(matches)-1][0]
}

func hasNodeFileLimit(raw, key string, limit int) bool {
	pattern := fmt.Sprintf(`(?s)<key>%s</key>\s*<dict>\s*<key>NumberOfFiles</key>\s*<integer>\s*%d\s*</integer>\s*</dict>`, regexp.QuoteMeta(key), limit)
	return regexp.MustCompile(pattern).MatchString(raw)
}

func removeResourceLimitBlock(raw, key string) string {
	pattern := fmt.Sprintf(`(?s)\s*<key>%s</key>\s*<dict>.*?</dict>`, regexp.QuoteMeta(key))
	return regexp.MustCompile(pattern).ReplaceAllString(raw, "")
}

func resourceLimitBlock(key string, limit int) string {
	return fmt.Sprintf(`
  <key>%s</key>
  <dict>
    <key>NumberOfFiles</key>
    <integer>%d</integer>
  </dict>`, key, limit)
}
