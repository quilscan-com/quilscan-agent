package qclient

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type Allocation struct {
	Filter        string `json:"filter"`
	Provers       int64  `json:"provers"`
	Ring          int64  `json:"ring"`
	SizeMB        string `json:"sizeMb"`
	Shards        int64  `json:"shards"`
	Reward        string `json:"reward"`
	Worker        string `json:"worker"`
	Status        string `json:"status"`
	Mode          string `json:"mode"`
	NextAction    string `json:"nextAction"`
	DefaultAction string `json:"defaultAction"`
}

type ManageActionRequest struct {
	RunRequest
	Action  string
	Filters []string
	Workers []uint32
}

type ManageActionResult struct {
	Output string `json:"output"`
}

func RunManageOnce(ctx context.Context, req RunRequest, timeout time.Duration) ([]Allocation, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{}
	if req.ConfigPath != "" {
		args = append(args, "--config", req.ConfigPath)
	}
	args = append(args, "node", "prover", "manage", "--once")
	cmd := exec.CommandContext(ctx, req.BinaryPath, args...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run %s %s: %w", req.BinaryPath, strings.Join(args, " "), err)
	}
	return ParseManageAllocations(string(out))
}

func RunManageAction(ctx context.Context, req ManageActionRequest, timeout time.Duration) (*ManageActionResult, error) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{}
	if req.ConfigPath != "" {
		args = append(args, "--config", req.ConfigPath)
	}
	args = append(args, "node", "prover", "manage", "--action", strings.ToLower(strings.TrimSpace(req.Action)))
	for _, worker := range req.Workers {
		args = append(args, "--worker", fmt.Sprintf("%d", worker))
	}
	for _, filter := range req.Filters {
		filter = strings.TrimSpace(filter)
		if filter != "" {
			args = append(args, filter)
		}
	}

	cmd := exec.CommandContext(ctx, req.BinaryPath, args...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		if output != "" {
			return nil, fmt.Errorf("run %s %s: %w: %s", req.BinaryPath, strings.Join(args, " "), err, output)
		}
		return nil, fmt.Errorf("run %s %s: %w", req.BinaryPath, strings.Join(args, " "), err)
	}
	if output == "" {
		output = fmt.Sprintf("%s completed", titleAction(req.Action))
	}
	return &ManageActionResult{Output: output}, nil
}

func ParseManageAllocations(raw string) ([]Allocation, error) {
	var rows []Allocation
	inAllocations := false
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Allocations") {
			inAllocations = true
			continue
		}
		if inAllocations && strings.HasPrefix(line, "Available Shards") {
			break
		}
		if !inAllocations || strings.HasPrefix(line, "Select ") || !strings.HasPrefix(line, "[") {
			continue
		}
		row, ok := parseAllocationRow(line)
		if ok {
			rows = append(rows, row)
		}
	}
	if err := sc.Err(); err != nil {
		return rows, err
	}
	return rows, nil
}

func parseAllocationRow(line string) (Allocation, bool) {
	fields := strings.Fields(line)
	offset := 0
	if len(fields) >= 2 && fields[0] == "[" && fields[1] == "]" {
		offset = 2
	} else if len(fields) >= 1 && strings.HasPrefix(fields[0], "[") {
		offset = 1
	}
	if len(fields) < offset+8 {
		return Allocation{}, false
	}

	rest := append([]string(nil), fields[offset+8:]...)
	mode := ""
	if len(rest) > 0 && isManageMode(rest[0]) {
		mode = rest[0]
		rest = rest[1:]
	}
	nextAction := strings.Join(rest, " ")
	defaultAction := ""
	status := fields[offset+7]
	if len(rest) > 0 && hasManageDefaultAction(status) {
		defaultAction = rest[len(rest)-1]
		nextAction = strings.Join(rest[:len(rest)-1], " ")
	}

	return Allocation{
		Filter:        fields[offset],
		Provers:       atoi64(fields[offset+1]),
		Ring:          atoi64(fields[offset+2]),
		SizeMB:        fields[offset+3],
		Shards:        atoi64(fields[offset+4]),
		Reward:        fields[offset+5],
		Worker:        fields[offset+6],
		Status:        status,
		Mode:          mode,
		NextAction:    nextAction,
		DefaultAction: defaultAction,
	}, true
}

func isManageMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "a", "auto", "m", "manual":
		return true
	default:
		return false
	}
}

func hasManageDefaultAction(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "joining", "leaving":
		return true
	default:
		return false
	}
}

func titleAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "join":
		return "Join"
	case "leave":
		return "Leave"
	case "confirm":
		return "Confirm"
	case "reject":
		return "Reject"
	case "pause":
		return "Pause"
	case "resume":
		return "Resume"
	case "manual":
		return "Manual"
	case "auto":
		return "Auto"
	default:
		return strings.TrimSpace(action)
	}
}
