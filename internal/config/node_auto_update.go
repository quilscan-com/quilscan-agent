package config

import "time"

// NodeAutoUpdateState stores an opt-in preference and its latest result.
type NodeAutoUpdateState struct {
	Enabled                 bool      `yaml:"enabled,omitempty"`
	LastEventID             string    `yaml:"last_event_id,omitempty"`
	LastResult              string    `yaml:"last_result,omitempty"`
	LastFromVersion         string    `yaml:"last_from_version,omitempty"`
	LastTargetVersion       string    `yaml:"last_target_version,omitempty"`
	LastCompletedAt         time.Time `yaml:"last_completed_at,omitempty"`
	LastFailedTargetVersion string    `yaml:"last_failed_target_version,omitempty"`
	LastError               string    `yaml:"last_error,omitempty"`
	BlockedVersion          string    `yaml:"blocked_version,omitempty"`
}
