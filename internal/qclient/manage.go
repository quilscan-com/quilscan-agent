package qclient

import (
	"bufio"
	"context"
	"fmt"
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

	args := []string{"node", "prover", "manage", "--once"}
	cmd := newCommand(ctx, req.BinaryPath, args...)
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

	action := strings.ToLower(strings.TrimSpace(req.Action))
	args, err := directProverActionArgs(action, req.Filters, req.Workers)
	if err != nil {
		return nil, err
	}

	cmd := newCommand(ctx, req.BinaryPath, args...)
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
		output = fmt.Sprintf("%s completed", titleAction(action))
	}
	return &ManageActionResult{Output: output}, nil
}

func directProverActionArgs(action string, filters []string, workers []uint32) ([]string, error) {
	if len(workers) > 0 {
		return nil, fmt.Errorf("qclient node prover %s does not support worker selection", action)
	}

	args := []string{"node", "prover", action}
	for _, filter := range filters {
		filter = strings.TrimSpace(filter)
		if filter != "" {
			args = append(args, filter)
		}
	}

	if len(args) == 3 {
		return nil, fmt.Errorf("%s requires at least one filter", action)
	}

	switch action {
	case "join", "leave", "confirm", "reject":
		return args, nil
	case "pause", "resume":
		if len(args) != 4 {
			return nil, fmt.Errorf("%s requires exactly one filter", action)
		}
		return args, nil
	default:
		return nil, fmt.Errorf("unsupported qclient prover action %q", action)
	}
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

	filter := ""
	valueOffset := offset
	if len(fields) >= offset+8 {
		filter = fields[offset]
		valueOffset = offset + 1
	} else if len(fields) >= offset+7 {
		valueOffset = offset
	} else {
		return Allocation{}, false
	}

	rest := append([]string(nil), fields[valueOffset+7:]...)
	mode := ""
	if len(rest) > 0 && isManageMode(rest[0]) {
		mode = rest[0]
		rest = rest[1:]
	}
	nextAction := strings.Join(rest, " ")
	defaultAction := ""
	status := fields[valueOffset+6]
	if len(rest) > 0 && hasManageDefaultAction(status) {
		defaultAction = rest[len(rest)-1]
		nextAction = strings.Join(rest[:len(rest)-1], " ")
	}

	return Allocation{
		Filter:        filter,
		Provers:       atoi64(fields[valueOffset]),
		Ring:          atoi64(fields[valueOffset+1]),
		SizeMB:        fields[valueOffset+2],
		Shards:        atoi64(fields[valueOffset+3]),
		Reward:        fields[valueOffset+4],
		Worker:        fields[valueOffset+5],
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
