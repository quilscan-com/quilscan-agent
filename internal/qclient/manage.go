package qclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ManageSnapshot struct {
	SchemaVersion       int                  `json:"schema_version"`
	PeerID              string               `json:"peer_id"`
	FrameNumber         uint64               `json:"frame_number"`
	LastReceivedFrame   uint64               `json:"last_received_frame"`
	LastGlobalHead      uint64               `json:"last_global_head"`
	CurrentEpoch        uint64               `json:"current_epoch"`
	EpochLengthFrames   uint64               `json:"epoch_length_frames"`
	RunningWorkers      uint64               `json:"running_workers"`
	AllocatedWorkers    uint64               `json:"allocated_workers"`
	WorkerInfoAvailable bool                 `json:"worker_info_available"`
	Reachable           bool                 `json:"reachable"`
	Allocations         []SnapshotAllocation `json:"allocations"`
	AvailableShards     []AvailableShard     `json:"available_shards"`
}

type GlobalHead struct {
	Frame       uint64 `json:"frame"`
	GlobalFrame uint64 `json:"global_frame"`
	Generation  uint64 `json:"generation"`
}

type WorkerExecution struct {
	State                 string  `json:"state"`
	Blocker               string  `json:"blocker"`
	MaterializedFrame     *uint64 `json:"materialized_frame"`
	LastAdvanceUnixMillis uint64  `json:"last_advance_unix_ms"`
	ObservedUnixMillis    uint64  `json:"observed_unix_ms"`
}

type SnapshotAllocation struct {
	Filter                string           `json:"filter"`
	Worker                *int64           `json:"worker"`
	Status                string           `json:"status"`
	Mode                  *string          `json:"mode"`
	ActiveProvers         *uint64          `json:"active_provers"`
	Ring                  *int64           `json:"ring"`
	SizeBytes             *string          `json:"size_bytes"`
	DataShards            *uint64          `json:"data_shards"`
	PeerMaterializedFrame *uint64          `json:"peer_materialized_frame"`
	PeerHead              *uint64          `json:"peer_head"`
	PeerState             string           `json:"peer_state"`
	GlobalHead            *GlobalHead      `json:"global_head"`
	Execution             *WorkerExecution `json:"execution"`
	LocalExecutionState   string           `json:"local_execution_state"`
	ExecutionDetail       string           `json:"execution_detail"`
	ExecutionSeverity     string           `json:"execution_severity"`
	RewardUnitsPerFrame   *string          `json:"reward_units_per_frame"`
	RewardQuilPerDay      *string          `json:"reward_quil_per_day"`
	NextAction            string           `json:"next_action"`
	DefaultAction         string           `json:"default_action"`
}

type AvailableShard struct {
	Filter                string      `json:"filter"`
	ActiveProvers         uint64      `json:"active_provers"`
	Ring                  *int64      `json:"ring"`
	SizeBytes             string      `json:"size_bytes"`
	DataShards            uint64      `json:"data_shards"`
	PeerMaterializedFrame uint64      `json:"peer_materialized_frame"`
	PeerHead              *uint64     `json:"peer_head"`
	PeerState             string      `json:"peer_state"`
	GlobalHead            *GlobalHead `json:"global_head"`
	RewardUnitsPerFrame   *string     `json:"reward_units_per_frame"`
	RewardQuilPerDay      *string     `json:"reward_quil_per_day"`
}

func ParseManageSnapshot(raw string) (*ManageSnapshot, error) {
	var snapshot ManageSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return nil, fmt.Errorf("parse qclient manage snapshot JSON: %w", err)
	}
	if snapshot.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported qclient manage snapshot schema_version %d", snapshot.SchemaVersion)
	}
	if snapshot.Allocations == nil {
		snapshot.Allocations = []SnapshotAllocation{}
	}
	if snapshot.AvailableShards == nil {
		snapshot.AvailableShards = []AvailableShard{}
	}
	return &snapshot, nil
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

func RunManageOnce(ctx context.Context, req RunRequest, timeout time.Duration) (*ManageSnapshot, error) {
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
	return ParseManageSnapshot(string(out))
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
