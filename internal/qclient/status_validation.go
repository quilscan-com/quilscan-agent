package qclient

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Validate required primitive fields before accepting a v1 observation. Raw
// execution payloads are then retained verbatim, including future extra fields.
func statusObject(raw json.RawMessage, fields map[string]string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("qclient status expected object")
	}
	for key, kind := range fields {
		value, ok := object[key]
		if !ok {
			return nil, fmt.Errorf("qclient status missing %s", key)
		}
		nullable := strings.HasSuffix(kind, "?")
		kind = strings.TrimSuffix(kind, "?")
		if string(value) == "null" {
			if nullable {
				continue
			}
			return nil, fmt.Errorf("qclient status %s cannot be null", key)
		}
		var target interface{}
		switch kind {
		case "string":
			target = new(string)
		case "bigint":
			target = new(UnsignedDecimalString)
		case "decimal":
			target = new(DecimalString)
		case "uint32":
			target = new(uint32)
		case "bool":
			target = new(bool)
		case "object":
			target = new(map[string]json.RawMessage)
		case "array":
			target = new([]json.RawMessage)
		case "counts":
			target = new(map[string]uint32)
		}
		if target == nil {
			return nil, fmt.Errorf("unknown qclient validation kind %s", kind)
		}
		if err := json.Unmarshal(value, target); err != nil {
			return nil, fmt.Errorf("qclient status %s: %w", key, err)
		}
		if kind == "counts" {
			var counts map[string]json.RawMessage
			_ = json.Unmarshal(value, &counts)
			for _, count := range counts {
				if string(count) == "null" {
					return nil, fmt.Errorf("qclient status %s contains null count", key)
				}
			}
		}
	}
	return object, nil
}
func validateStatusWire(raw json.RawMessage) error {
	root, err := statusObject(raw, map[string]string{"schema_version": "uint32", "peer_id": "string", "version": "string", "seniority": "bigint", "peer_score": "decimal", "last_received_frame": "decimal", "last_global_head": "decimal", "running_workers": "uint32", "allocated_workers": "uint32", "worker_capacity": "uint32?", "reachable": "bool", "gossip_traffic": "object?", "workers": "array?", "worker_error": "string?"})
	if err != nil {
		return err
	}
	if string(root["gossip_traffic"]) != "null" {
		gossip, err := statusObject(root["gossip_traffic"], map[string]string{"master": "object?", "workers": "array?"})
		if err != nil {
			return err
		}
		if string(gossip["master"]) != "null" {
			if err := validateProcessWire(gossip["master"]); err != nil {
				return err
			}
		}
		var workers []json.RawMessage
		_ = json.Unmarshal(gossip["workers"], &workers)
		for _, rawWorker := range workers {
			w, err := statusObject(rawWorker, map[string]string{"core_id": "uint32", "traffic": "object"})
			if err != nil {
				return err
			}
			if err := validateProcessWire(w["traffic"]); err != nil {
				return err
			}
		}
	}
	if string(root["workers"]) == "null" {
		var workerError string
		if err := json.Unmarshal(root["worker_error"], &workerError); err != nil || strings.TrimSpace(workerError) == "" {
			return fmt.Errorf("unavailable worker status requires worker_error")
		}
		if string(root["gossip_traffic"]) != "null" {
			var g map[string]json.RawMessage
			_ = json.Unmarshal(root["gossip_traffic"], &g)
			if string(g["workers"]) != "null" {
				return fmt.Errorf("unavailable worker status cannot contain worker gossip")
			}
		}
	} else if string(root["worker_error"]) != "null" {
		return fmt.Errorf("successful worker status cannot contain worker_error")
	}
	var workers []json.RawMessage
	_ = json.Unmarshal(root["workers"], &workers)
	for _, rawWorker := range workers {
		w, err := statusObject(rawWorker, map[string]string{"core_id": "uint32", "filter": "string", "available_storage": "decimal", "total_storage": "decimal", "manually_managed": "bool", "draining": "bool", "execution": "object?", "execution_detail": "string"})
		if err != nil {
			return err
		}
		if string(w["execution"]) != "null" {
			if err := validateExecutionWire(w["execution"]); err != nil {
				return err
			}
		}
	}
	return nil
}
func validateProcessWire(raw json.RawMessage) error {
	p, err := statusObject(raw, map[string]string{"uptime_ms": "decimal", "classes": "array"})
	if err != nil {
		return err
	}
	var classes []json.RawMessage
	_ = json.Unmarshal(p["classes"], &classes)
	for _, class := range classes {
		if _, err := statusObject(class, map[string]string{"class": "string", "received_messages": "decimal", "received_bytes": "decimal", "unique_messages": "decimal", "unique_bytes": "decimal", "sent_messages": "decimal", "sent_bytes": "decimal", "published_messages": "decimal", "published_bytes": "decimal"}); err != nil {
			return err
		}
	}
	return nil
}
func validateExecutionWire(raw json.RawMessage) error {
	e, err := statusObject(raw, map[string]string{"state": "string", "blocker": "string", "materialized_frame": "decimal?", "last_advance_unix_ms": "decimal", "observed_unix_ms": "decimal", "recovery": "object?", "seal": "object?", "awaited_frame": "decimal?", "own_votes": "decimal?", "peer_votes": "decimal", "last_peer_vote_unix_ms": "decimal", "host_started_unix_ms": "decimal", "silent_ms": "decimal", "engine_started_unix_ms": "decimal"})
	if err != nil {
		return err
	}
	if string(e["recovery"]) != "null" {
		if _, err := statusObject(e["recovery"], map[string]string{"phase": "string", "phase_since_unix_ms": "decimal", "tree_phase": "uint32", "installed_leaves": "decimal", "planned_leaves": "decimal", "leaves_observed_unix_ms": "decimal", "last_outcome": "string", "last_outcome_unix_ms": "decimal", "last_error": "string", "last_error_unix_ms": "decimal", "source": "string", "attempts": "uint32", "consecutive_failures": "uint32", "attempt_started_unix_ms": "decimal", "permit_wait_ms": "decimal", "attempt_ms": "decimal", "attempt_installed_leaves": "decimal", "failures_by_class": "counts", "longest_permit_wait_ms": "decimal"}); err != nil {
			return err
		}
	}
	if string(e["seal"]) != "null" {
		if _, err := statusObject(e["seal"], map[string]string{"generation": "decimal", "checkpoint": "decimal", "view": "decimal", "global_executed": "decimal", "drain": "string", "drain_headers": "uint32", "finalized_unix_ms": "decimal", "last_submitted_unix_ms": "decimal", "submissions": "uint32", "recorded_unix_ms": "decimal"}); err != nil {
			return err
		}
	}
	return nil
}
