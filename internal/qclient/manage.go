package qclient

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Allocation struct {
	Filter               string `json:"filter"`
	Provers              int64  `json:"provers"`
	Ring                 int64  `json:"ring"`
	SizeMB               string `json:"sizeMb"`
	Shards               int64  `json:"shards"`
	MaterializedFrame    string `json:"materializedFrame"`
	Lag                  string `json:"lag"`
	MaterializationState string `json:"materializationState"`
	Reward               string `json:"reward"`
	Worker               string `json:"worker"`
	Status               string `json:"status"`
	Mode                 string `json:"mode"`
	NextAction           string `json:"nextAction"`
	DefaultAction        string `json:"defaultAction"`
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
	markerOffset := 0
	if len(fields) >= 2 && fields[0] == "[" && fields[1] == "]" {
		markerOffset = 2
	} else if len(fields) >= 1 && strings.HasPrefix(fields[0], "[") {
		markerOffset = 1
	}

	for _, candidate := range []int{markerOffset, markerOffset + 1} {
		allocation, ok := parseLatestAllocation(fields, candidate)
		if !ok {
			continue
		}
		if candidate == markerOffset+1 {
			allocation.Filter = fields[markerOffset]
		}
		return allocation, true
	}
	return Allocation{}, false
}

var signedIntegerPattern = regexp.MustCompile(`^[+-]?[0-9]+$`)
var unsignedIntegerPattern = regexp.MustCompile(`^[0-9]+$`)
var decimalPattern = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`)
var manageSizePattern = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|<0\.1)$`)
var frameActionHintPattern = regexp.MustCompile(`^f[0-9]+$`)
var renewActionHintPattern = regexp.MustCompile(`^renew<f[0-9]+$`)
var activeEpochActionHintPattern = regexp.MustCompile(`^(?:active|departs)@e[0-9]+$`)

func parseLatestAllocation(fields []string, valueOffset int) (Allocation, bool) {
	const fixedValues = 10
	if len(fields) < valueOffset+fixedValues {
		return Allocation{}, false
	}
	values := fields[valueOffset : valueOffset+fixedValues]
	provers, ok := parseManageSignedInteger(values[0])
	if !ok {
		return Allocation{}, false
	}
	ring, ok := parseManageSignedInteger(values[1])
	if !ok {
		return Allocation{}, false
	}
	shards, ok := parseManageSignedInteger(values[3])
	if !ok ||
		!isManageSize(values[2]) ||
		!isManageUnsignedInteger(values[4]) ||
		(values[5] != "-" && !isManageUnsignedInteger(values[5])) ||
		!isMaterializationState(values[6]) ||
		!isManageReward(values[7]) ||
		(values[8] != "-" && !isManageSigned(values[8])) ||
		!isManageStatus(values[9]) {
		return Allocation{}, false
	}

	rest := append([]string(nil), fields[valueOffset+fixedValues:]...)
	mode := ""
	if len(rest) > 0 && isManageMode(rest[0]) {
		mode = rest[0]
		rest = rest[1:]
	}
	nextAction, defaultAction := splitManageActionHints(rest)

	return Allocation{
		Provers:              provers,
		Ring:                 ring,
		SizeMB:               values[2],
		Shards:               shards,
		MaterializedFrame:    values[4],
		Lag:                  values[5],
		MaterializationState: values[6],
		Reward:               values[7],
		Worker:               values[8],
		Status:               values[9],
		Mode:                 mode,
		NextAction:           nextAction,
		DefaultAction:        defaultAction,
	}, true
}

func parseManageSignedInteger(value string) (int64, bool) {
	if !signedIntegerPattern.MatchString(value) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil
}

func isManageSigned(value string) bool {
	_, ok := parseManageSignedInteger(value)
	return ok
}

func isManageUnsignedInteger(value string) bool {
	if !unsignedIntegerPattern.MatchString(value) {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func isManageSize(value string) bool {
	return manageSizePattern.MatchString(value)
}

func isMaterializationState(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unknown", "unmat", "lag", "current":
		return true
	default:
		return false
	}
}

func isManageReward(value string) bool {
	return strings.HasPrefix(value, "~") && decimalPattern.MatchString(strings.TrimPrefix(value, "~"))
}

func isManageStatus(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "idle", "joining", "active", "paused", "leaving", "expiredjoin", "expiredleave", "re-confirm!", "rejected", "kicked", "unknown":
		return true
	default:
		return false
	}
}

func isManageMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "a", "auto", "m", "manual":
		return true
	default:
		return false
	}
}

func splitManageActionHints(actions []string) (nextAction, defaultAction string) {
	defaultStart := len(actions)
	if len(actions) >= 2 &&
		((actions[len(actions)-2] == "thru" && frameActionHintPattern.MatchString(actions[len(actions)-1])) ||
			(actions[len(actions)-2] == "epoch" && isManageUnsignedInteger(actions[len(actions)-1]))) {
		defaultStart -= 2
	} else if len(actions) > 0 && isSingleManageActionHint(actions[len(actions)-1]) {
		defaultStart--
	}

	nextAction = strings.Join(actions[:defaultStart], " ")
	defaultAction = strings.Join(actions[defaultStart:], " ")
	if nextAction == "-" {
		nextAction = ""
	}
	if defaultAction == "-" {
		defaultAction = ""
	}
	return nextAction, defaultAction
}

func isSingleManageActionHint(value string) bool {
	switch value {
	case "expired", "re-confirm!", "-":
		return true
	default:
		return renewActionHintPattern.MatchString(value) || activeEpochActionHintPattern.MatchString(value)
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
