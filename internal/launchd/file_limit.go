package launchd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const systemLimitPlistPath = "/Library/LaunchDaemons/limit.maxfiles.plist"
const sysctlConfPath = "/etc/sysctl.conf"

func EnsureSystemFileLimit(limit int) (bool, error) {
	if limit <= 0 {
		return false, fmt.Errorf("invalid system file limit %d", limit)
	}
	changed := false
	for _, key := range []string{"kern.maxfiles", "kern.maxfilesperproc"} {
		if didChange, err := ensureLiveSysctlAtLeast(key, limit); err != nil {
			return changed, err
		} else if didChange {
			changed = true
		}
		if didChange, err := ensureSysctlConfValue(sysctlConfPath, key, limit); err != nil {
			return changed, err
		} else if didChange {
			changed = true
		}
	}
	if didChange, err := ensureLimitMaxfilesPlist(systemLimitPlistPath, limit); err != nil {
		return changed, err
	} else if didChange {
		changed = true
	}
	if changed {
		if err := reloadLimitMaxfilesPlist(systemLimitPlistPath); err != nil {
			return true, err
		}
	}
	return changed, nil
}

func ensureLiveSysctlAtLeast(key string, limit int) (bool, error) {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err == nil {
		current, parseErr := strconv.Atoi(strings.TrimSpace(string(out)))
		if parseErr == nil && current >= limit {
			return false, nil
		}
	}
	cmd := exec.Command("sysctl", "-w", fmt.Sprintf("%s=%d", key, limit))
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("sysctl -w %s=%d: %w — %s", key, limit, err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

func ensureSysctlConfValue(path, key string, limit int) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	updated := upsertSysctlConfValue(string(raw), key, limit)
	if string(raw) == updated {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(updated), 0o644)
}

func upsertSysctlConfValue(raw, key string, limit int) string {
	target := fmt.Sprintf("%s=%d", key, limit)
	lines := strings.Split(raw, "\n")
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+"=") || strings.HasPrefix(trimmed, key+" ") {
			lines[i] = target
			replaced = true
		}
	}
	if !replaced {
		if strings.TrimSpace(raw) == "" {
			return target + "\n"
		}
		if strings.HasSuffix(raw, "\n") {
			return raw + target + "\n"
		}
		return raw + "\n" + target + "\n"
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

func ensureLimitMaxfilesPlist(path string, limit int) (bool, error) {
	body := renderLimitMaxfilesPlist(limit)
	raw, err := os.ReadFile(path)
	if err == nil && bytes.Equal(raw, []byte(body)) {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, os.WriteFile(path, []byte(body), 0o644)
}

func renderLimitMaxfilesPlist(limit int) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>limit.maxfiles</string>
  <key>ProgramArguments</key>
  <array>
    <string>launchctl</string>
    <string>limit</string>
    <string>maxfiles</string>
    <string>%d</string>
    <string>%d</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>ServiceIPC</key>
  <false/>
</dict>
</plist>
`, limit, limit)
}

func reloadLimitMaxfilesPlist(path string) error {
	_ = exec.Command("launchctl", "bootout", "system/limit.maxfiles").Run()
	if out, err := exec.Command("launchctl", "bootstrap", "system", path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap system %s: %w — %s", path, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("launchctl", "kickstart", "-k", "system/limit.maxfiles").CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart system/limit.maxfiles: %w — %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
