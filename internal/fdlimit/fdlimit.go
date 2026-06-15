package fdlimit

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	LinuxNodeFileLimit  = 1048576
	DarwinNodeFileLimit = 524288
)

const (
	StatusOK       = "ok"
	StatusLow      = "low"
	StatusMissing  = "missing"
	StatusUnknown  = "unknown"
	StatusError    = "error"
	StatusDisabled = "disabled"
)

type Status struct {
	Status          string
	Current         int
	Required        int
	Source          string
	RestartRequired bool
	Error           string
}

type InspectRequest struct {
	Platform string
	UnitDir  string
	UnitName string
}

func Inspect(req InspectRequest) Status {
	platform := strings.TrimSpace(req.Platform)
	if platform == "" {
		platform = runtime.GOOS
	}
	switch {
	case strings.HasPrefix(platform, "linux"):
		unitPath := req.UnitName
		if req.UnitDir != "" && req.UnitName != "" {
			unitPath = filepath.Join(req.UnitDir, req.UnitName)
		}
		return InspectLinuxUnitFileWithRuntime(unitPath, req.UnitName, LinuxNodeFileLimit)
	case strings.HasPrefix(platform, "darwin"):
		plistPath := req.UnitName
		if req.UnitDir != "" && req.UnitName != "" {
			name := req.UnitName
			if !strings.HasSuffix(name, ".plist") {
				name += ".plist"
			}
			plistPath = filepath.Join(req.UnitDir, name)
		}
		return InspectDarwinPlist(plistPath, DarwinNodeFileLimit)
	default:
		return Status{Status: StatusDisabled}
	}
}

func InspectLinuxUnitFile(path string, required int) Status {
	return inspectLinuxUnitFile(path, required, "")
}

func InspectLinuxUnitFileWithRuntime(path, unitName string, required int) Status {
	return inspectLinuxUnitFile(path, required, unitName)
}

func inspectLinuxUnitFile(path string, required int, unitName string) Status {
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
	value, ok, err := ParseSystemdLimitNOFILE(string(raw))
	if err != nil {
		st.Status = StatusError
		st.Error = err.Error()
		return st
	}
	if !ok {
		st.Status = StatusMissing
		st.RestartRequired = true
		return st
	}
	st.Current = value
	if value >= required {
		if runtimeCurrent, ok := inspectSystemdRuntimeNOFILE(unitName); ok && runtimeCurrent < required {
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

var systemdMainPID = func(unit string) (int, error) {
	if strings.TrimSpace(unit) == "" {
		return 0, fmt.Errorf("unit name is empty")
	}
	out, err := exec.Command("systemctl", "show", unit, "--property=MainPID", "--value").CombinedOutput()
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

var readProcLimits = func(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "limits"))
	return string(raw), err
}

func inspectSystemdRuntimeNOFILE(unitName string) (int, bool) {
	pid, err := systemdMainPID(unitName)
	if err != nil || pid <= 0 {
		return 0, false
	}
	raw, err := readProcLimits(pid)
	if err != nil {
		return 0, false
	}
	return parseProcLimitsMaxOpenFiles(raw)
}

func parseProcLimitsMaxOpenFiles(raw string) (int, bool) {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		if fields[0] != "Max" || fields[1] != "open" || fields[2] != "files" {
			continue
		}
		soft, err := parseLimitNumber(fields[3])
		if err != nil {
			return 0, false
		}
		if len(fields) >= 5 {
			hard, err := parseLimitNumber(fields[4])
			if err == nil && hard < soft {
				return hard, true
			}
		}
		return soft, true
	}
	return 0, false
}

func parseLimitNumber(raw string) (int, error) {
	value := strings.TrimSpace(strings.ToLower(raw))
	switch value {
	case "infinity", "infinite", "unlimited":
		return math.MaxInt, nil
	}
	return strconv.Atoi(value)
}

func ParseSystemdLimitNOFILE(raw string) (int, bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	value := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "LimitNOFILE" {
			continue
		}
		value = strings.TrimSpace(val)
	}
	if err := scanner.Err(); err != nil {
		return 0, false, err
	}
	if value == "" {
		return 0, false, nil
	}
	switch strings.ToLower(value) {
	case "infinity", "infinite", "unlimited":
		return math.MaxInt, true, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, true, fmt.Errorf("parse LimitNOFILE %q: %w", value, err)
	}
	return n, true, nil
}

func (s Status) NodeStatusPatch() map[string]interface{} {
	if s.Status == "" || s.Status == StatusDisabled {
		return nil
	}
	patch := map[string]interface{}{
		"fd_limit_status":           s.Status,
		"fd_limit_current":          s.Current,
		"fd_limit_required":         s.Required,
		"fd_limit_source":           s.Source,
		"fd_limit_restart_required": s.RestartRequired,
		"fd_limit_checked_at":       time.Now().UTC().Format(time.RFC3339),
	}
	if s.Error != "" {
		patch["fd_limit_error"] = s.Error
	}
	return patch
}
