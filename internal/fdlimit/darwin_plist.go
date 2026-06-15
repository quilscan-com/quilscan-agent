package fdlimit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var launchctlPrint = func(target string) (string, error) {
	out, err := exec.Command("launchctl", "print", target).CombinedOutput()
	return string(out), err
}

func InspectDarwinPlist(path string, required int) Status {
	st := Status{
		Status:   StatusUnknown,
		Required: required,
		Source:   path,
	}
	if strings.TrimSpace(path) == "" {
		st.Status = StatusMissing
		st.RestartRequired = true
		return st
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			st.Status = StatusMissing
			st.RestartRequired = true
			return st
		}
		st.Status = StatusError
		st.Error = err.Error()
		return st
	}
	soft, softOK := parsePlistNumberOfFiles(string(raw), "SoftResourceLimits")
	hard, hardOK := parsePlistNumberOfFiles(string(raw), "HardResourceLimits")
	if !softOK || !hardOK {
		st.Status = StatusMissing
		st.RestartRequired = true
		return st
	}
	st.Current = soft
	if hard < st.Current {
		st.Current = hard
	}
	if soft >= required && hard >= required {
		if runtimeCurrent, ok := inspectLaunchdRuntimeMaxfiles(path, string(raw)); ok && runtimeCurrent < required {
			st.Current = runtimeCurrent
			st.Status = StatusLow
			st.RestartRequired = true
			return st
		}
		st.Status = StatusOK
		return st
	}
	st.Status = StatusLow
	st.RestartRequired = true
	return st
}

func parsePlistNumberOfFiles(raw, key string) (int, bool) {
	pattern := `(?s)<key>` + regexp.QuoteMeta(key) + `</key>\s*<dict>.*?<key>NumberOfFiles</key>\s*<integer>\s*([0-9]+)\s*</integer>.*?</dict>`
	match := regexp.MustCompile(pattern).FindStringSubmatch(raw)
	if len(match) < 2 {
		return 0, false
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	return value, true
}

func inspectLaunchdRuntimeMaxfiles(path, raw string) (int, bool) {
	label := parsePlistStringValue(raw, "Label")
	if label == "" {
		base := filepath.Base(path)
		label = strings.TrimSuffix(base, ".plist")
	}
	if label == "" {
		return 0, false
	}
	out, err := launchctlPrint("system/" + label)
	if err != nil {
		return 0, false
	}
	return parseLaunchdPrintMaxfiles(out)
}

func parsePlistStringValue(raw, key string) string {
	pattern := `(?s)<key>` + regexp.QuoteMeta(key) + `</key>\s*<string>\s*([^<]+?)\s*</string>`
	match := regexp.MustCompile(pattern).FindStringSubmatch(raw)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func parseLaunchdPrintMaxfiles(raw string) (int, bool) {
	re := regexp.MustCompile(`maxfiles\s+\((soft|hard)\)\s*=>\s*([0-9]+|unlimited)`)
	matches := re.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return 0, false
	}
	values := map[string]int{}
	for _, match := range matches {
		value, err := parseLaunchdLimitValue(match[2])
		if err != nil {
			continue
		}
		values[match[1]] = value
	}
	soft, softOK := values["soft"]
	hard, hardOK := values["hard"]
	if !softOK && !hardOK {
		return 0, false
	}
	if !softOK {
		return hard, true
	}
	if !hardOK {
		return soft, true
	}
	if hard < soft {
		return hard, true
	}
	return soft, true
}

func parseLaunchdLimitValue(raw string) (int, error) {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "unlimited" {
		return int(^uint(0) >> 1), nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse launchd maxfiles %q: %w", raw, err)
	}
	return n, nil
}
