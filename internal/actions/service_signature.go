package actions

import (
	"fmt"
	"os"
	"strings"

	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
)

const nodeSignatureCheckDisabledArg = "--signature-check=false"

func (d NodeSourceSwitcherDeps) setNodeSignatureCheckDisabled(disabled bool) error {
	if strings.TrimSpace(d.UnitDir) == "" {
		return nil
	}
	path := svcctl.UnitFilePath(d.UnitDir, d.UnitName)
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	patched, changed, err := patchNodeServiceSignatureCheck(string(raw), disabled)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := os.WriteFile(path, []byte(patched), st.Mode().Perm()); err != nil {
		return err
	}
	if d.Reload != nil {
		return d.Reload()
	}
	return nil
}

func patchNodeServiceSignatureCheck(raw string, disabled bool) (string, bool, error) {
	if strings.Contains(raw, "<plist") && strings.Contains(raw, "<key>ProgramArguments</key>") {
		return patchPlistSignatureCheck(raw, disabled)
	}
	return patchSystemdSignatureCheck(raw, disabled)
}

func patchSystemdSignatureCheck(raw string, disabled bool) (string, bool, error) {
	lines := strings.SplitAfter(raw, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "ExecStart=") {
			continue
		}
		withoutNewline, newline := splitTrailingNewline(line)
		updated := strings.ReplaceAll(withoutNewline, " "+nodeSignatureCheckDisabledArg, "")
		if disabled {
			updated += " " + nodeSignatureCheckDisabledArg
		}
		lines[i] = updated + newline
		if lines[i] == line {
			return raw, false, nil
		}
		return strings.Join(lines, ""), true, nil
	}
	if disabled {
		return raw, false, fmt.Errorf("service definition missing ExecStart")
	}
	return raw, false, nil
}

func patchPlistSignatureCheck(raw string, disabled bool) (string, bool, error) {
	if strings.Contains(raw, "<string>"+nodeSignatureCheckDisabledArg+"</string>") {
		if disabled {
			return raw, false, nil
		}
		return removePlistSignatureCheck(raw), true, nil
	}
	if !disabled {
		return raw, false, nil
	}

	programArgs := strings.Index(raw, "<key>ProgramArguments</key>")
	if programArgs < 0 {
		return raw, false, fmt.Errorf("plist missing ProgramArguments")
	}
	arrayStartRel := strings.Index(raw[programArgs:], "<array")
	if arrayStartRel < 0 {
		return raw, false, fmt.Errorf("plist ProgramArguments missing array")
	}
	arrayStart := programArgs + arrayStartRel
	firstStringStartRel := strings.Index(raw[arrayStart:], "<string>")
	if firstStringStartRel < 0 {
		return raw, false, fmt.Errorf("plist ProgramArguments missing binary argument")
	}
	firstStringStart := arrayStart + firstStringStartRel
	firstStringEndRel := strings.Index(raw[firstStringStart:], "</string>")
	if firstStringEndRel < 0 {
		return raw, false, fmt.Errorf("plist ProgramArguments has malformed string")
	}
	firstStringEnd := firstStringStart + firstStringEndRel + len("</string>")
	insertAt := firstStringEnd
	if newlineRel := strings.Index(raw[firstStringEnd:], "\n"); newlineRel >= 0 {
		insertAt = firstStringEnd + newlineRel + 1
	}
	lineStart := strings.LastIndex(raw[:firstStringStart], "\n") + 1
	indent := leadingWhitespace(raw[lineStart:firstStringStart])
	insert := indent + "<string>" + nodeSignatureCheckDisabledArg + "</string>\n"
	return raw[:insertAt] + insert + raw[insertAt:], true, nil
}

func removePlistSignatureCheck(raw string) string {
	lines := strings.SplitAfter(raw, "\n")
	kept := lines[:0]
	target := "<string>" + nodeSignatureCheckDisabledArg + "</string>"
	for _, line := range lines {
		if strings.Contains(line, target) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "")
}

func splitTrailingNewline(s string) (string, string) {
	switch {
	case strings.HasSuffix(s, "\r\n"):
		return strings.TrimSuffix(s, "\r\n"), "\r\n"
	case strings.HasSuffix(s, "\n"):
		return strings.TrimSuffix(s, "\n"), "\n"
	default:
		return s, ""
	}
}

func leadingWhitespace(s string) string {
	for i, r := range s {
		if r != ' ' && r != '\t' {
			return s[:i]
		}
	}
	return s
}
