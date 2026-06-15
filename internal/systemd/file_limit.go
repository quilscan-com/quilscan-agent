package systemd

import (
	"fmt"
	"os"
	"strings"
)

const NodeFileLimit = 1048576

func EnsureNodeFileLimit(unitPath string, limit int) (bool, error) {
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		return false, err
	}
	updated, changed, err := EnsureNodeFileLimitContent(string(raw), limit)
	if err != nil || !changed {
		return changed, err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(unitPath); err == nil {
		mode = st.Mode().Perm()
	}
	return true, os.WriteFile(unitPath, []byte(updated), mode)
}

func EnsureNodeFileLimitContent(raw string, limit int) (string, bool, error) {
	if limit <= 0 {
		return raw, false, fmt.Errorf("invalid node file limit %d", limit)
	}
	target := fmt.Sprintf("LimitNOFILE=%d", limit)
	lines := strings.Split(raw, "\n")
	inService := false
	serviceIndex := -1
	found := false
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inService = trimmed == "[Service]"
			if inService {
				serviceIndex = i
			}
			continue
		}
		if !inService || trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "LimitNOFILE" {
			continue
		}
		found = true
		prefix := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		replacement := prefix + target
		if line != replacement {
			lines[i] = replacement
			changed = true
		}
	}
	if !found {
		if serviceIndex < 0 {
			return raw, false, fmt.Errorf("[Service] section not found")
		}
		insertAt := serviceIndex + 1
		lines = append(lines[:insertAt], append([]string{target}, lines[insertAt:]...)...)
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	out := strings.Join(lines, "\n")
	if strings.HasSuffix(raw, "\n") && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out, true, nil
}
