package qclient

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// UnsignedDecimalString also supports seniority's arbitrary precision integer.
type UnsignedDecimalString string

func (d *UnsignedDecimalString) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("unsigned decimal string: %w", err)
	}
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return fmt.Errorf("invalid unsigned decimal string %q", value)
	}
	*d = UnsignedDecimalString(value)
	return nil
}

// DecimalString keeps protobuf uint64 counters lossless across the browser boundary.
type DecimalString string

func (d *DecimalString) UnmarshalJSON(raw []byte) error {
	var value UnsignedDecimalString
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if _, err := strconv.ParseUint(string(value), 10, 64); err != nil {
		return err
	}
	*d = DecimalString(value)
	return nil
}

type TrafficClass struct {
	Class             string        `json:"class"`
	ReceivedMessages  DecimalString `json:"received_messages"`
	ReceivedBytes     DecimalString `json:"received_bytes"`
	UniqueMessages    DecimalString `json:"unique_messages"`
	UniqueBytes       DecimalString `json:"unique_bytes"`
	SentMessages      DecimalString `json:"sent_messages"`
	SentBytes         DecimalString `json:"sent_bytes"`
	PublishedMessages DecimalString `json:"published_messages"`
	PublishedBytes    DecimalString `json:"published_bytes"`
}
type ProcessTraffic struct {
	UptimeMS DecimalString  `json:"uptime_ms"`
	Classes  []TrafficClass `json:"classes"`
}
type WorkerTraffic struct {
	CoreID  uint32         `json:"core_id"`
	Traffic ProcessTraffic `json:"traffic"`
}
type GossipTraffic struct {
	Master  *ProcessTraffic `json:"master"`
	Workers []WorkerTraffic `json:"workers"`
}
type StatusWorker struct {
	CoreID           uint32        `json:"core_id"`
	Filter           string        `json:"filter"`
	AvailableStorage DecimalString `json:"available_storage"`
	TotalStorage     DecimalString `json:"total_storage"`
	ManuallyManaged  bool          `json:"manually_managed"`
	Draining         bool          `json:"draining"`
	// RawMessage preserves all execution fields, optional values and decimal strings.
	Execution       json.RawMessage `json:"execution"`
	ExecutionDetail string          `json:"execution_detail"`
}
type StatusSnapshot struct {
	SchemaVersion     uint32                `json:"schema_version"`
	PeerID            string                `json:"peer_id"`
	Version           string                `json:"version"`
	Seniority         UnsignedDecimalString `json:"seniority"`
	PeerScore         DecimalString         `json:"peer_score"`
	LastReceivedFrame DecimalString         `json:"last_received_frame"`
	LastGlobalHead    DecimalString         `json:"last_global_head"`
	RunningWorkers    uint32                `json:"running_workers"`
	AllocatedWorkers  uint32                `json:"allocated_workers"`
	WorkerCapacity    *uint32               `json:"worker_capacity"`
	Reachable         bool                  `json:"reachable"`
	GossipTraffic     *GossipTraffic        `json:"gossip_traffic"`
	Workers           []StatusWorker        `json:"workers"`
	WorkerError       *string               `json:"worker_error"`
}
type GossipSnapshot struct {
	PeerID      string          `json:"peer_id"`
	Master      *ProcessTraffic `json:"master"`
	Workers     []WorkerTraffic `json:"workers"`
	WorkerCount *int            `json:"worker_count"`
}
type WorkersSnapshot struct {
	PeerID  string         `json:"peer_id"`
	Workers []StatusWorker `json:"workers"`
}

func ParseProverStatusJSON(raw string) (*ProverStatus, error) {
	var s StatusSnapshot
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("parse qclient prover status JSON: %w", err)
	}
	if s.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported qclient prover status schema %d", s.SchemaVersion)
	}
	if err := validateStatusWire(json.RawMessage(raw)); err != nil {
		return nil, err
	}
	if s.PeerID == "" {
		return nil, fmt.Errorf("qclient prover status did not contain node identity")
	}
	return &ProverStatus{PeerID: s.PeerID, Version: s.Version, Seniority: compatCounter(string(s.Seniority)), PeerScore: compatCounter(string(s.PeerScore)), RunningWorkers: int64(s.RunningWorkers), AllocatedWorkers: int64(s.AllocatedWorkers), LastReceived: compatCounter(string(s.LastReceivedFrame)), Reachable: s.Reachable, Snapshot: &s}, nil
}

// compatCounter retains the established int64 API for representable values.
// Snapshot holds exact source strings; StatusPatch publishes nil on overflow.
func compatCounter(s string) int64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n > math.MaxInt64 {
		return 0
	}
	return int64(n)
}

// StatusPatch is shared by periodic sampling and the focused manual refresh.
// Failures only change read state/error, preserving previous data and timestamps.
func StatusPatch(status *ProverStatus, readErr error, now time.Time) map[string]interface{} {
	p := map[string]interface{}{}
	state := func(group, value, message string) { p[group+"_read_state"] = value; p[group+"_error"] = message }
	both := func(value, message string) {
		state("qclient_gossip", value, message)
		state("qclient_workers", value, message)
	}
	if readErr != nil || status == nil {
		message := "qclient prover status unavailable"
		if readErr != nil {
			message = readErr.Error()
		}
		p["qclient_status"] = "unavailable"
		both("error", message)
		return p
	}
	p["qclient_status"] = "reachable"
	if !status.Reachable {
		p["qclient_status"] = "unreachable"
	}
	p["qclient_reachable"] = status.Reachable
	p["qclient_peer_id"] = status.PeerID
	p["qclient_node_version"] = status.Version
	p["qclient_seniority"] = status.Seniority
	p["qclient_peer_score"] = status.PeerScore
	p["qclient_running_workers"] = status.RunningWorkers
	p["qclient_allocated_workers"] = status.AllocatedWorkers
	p["qclient_last_received"] = status.LastReceived
	if status.Snapshot == nil {
		both("unsupported", "qclient prover status --json is unsupported")
		return p
	}
	s := status.Snapshot
	for key, value := range map[string]string{"qclient_seniority": string(s.Seniority), "qclient_peer_score": string(s.PeerScore), "qclient_last_received": string(s.LastReceivedFrame)} {
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil || n > math.MaxInt64 {
			p[key] = nil
		}
	}
	stamp := now.UTC().Format(time.RFC3339)
	if s.Workers != nil {
		p["qclient_workers"] = WorkersSnapshot{PeerID: s.PeerID, Workers: s.Workers}
		p["qclient_workers_refreshed_at"] = stamp
		state("qclient_workers", "ok", "")
	} else if s.WorkerError != nil {
		state("qclient_workers", "error", *s.WorkerError)
	} else {
		state("qclient_workers", "unavailable", "worker status unavailable")
	}
	g := s.GossipTraffic
	if g == nil || (g.Master == nil && len(g.Workers) == 0) {
		state("qclient_gossip", "unavailable", "node gossip traffic unavailable")
		return p
	}
	var count *int
	if s.Workers != nil {
		n := len(s.Workers)
		count = &n
	}
	p["qclient_gossip"] = GossipSnapshot{PeerID: s.PeerID, Master: g.Master, Workers: g.Workers, WorkerCount: count}
	p["qclient_gossip_refreshed_at"] = stamp
	if g.Master == nil || g.Workers == nil || s.WorkerError != nil {
		message := "gossip traffic partially unavailable"
		if s.WorkerError != nil {
			message = *s.WorkerError
		}
		state("qclient_gossip", "partial", message)
	} else {
		state("qclient_gossip", "ok", "")
	}
	return p
}
