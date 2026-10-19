package qclient

// FrameCadence contains node-owned observations; zero advances is a measured
// zero, while an absent cadence or window remains unknown.
type FrameCadence struct {
	LastObservedUnixSeconds uint64       `json:"last_observed_unix_seconds"`
	Hour                    *FrameWindow `json:"hour"`
	Day                     *FrameWindow `json:"day"`
}

type FrameWindow struct {
	WindowSeconds   uint64 `json:"window_seconds"`
	ObservedSeconds uint64 `json:"observed_seconds"`
	AdvancedFrames  uint64 `json:"advanced_frames"`
}

type ShardProverCounts struct {
	Joining    uint32 `json:"joining"`
	Activating uint32 `json:"activating"`
	Active     uint32 `json:"active"`
	Paused     uint32 `json:"paused"`
	Leaving    uint32 `json:"leaving"`
	Departing  uint32 `json:"departing"`
}

type ShardRecovery struct {
	Phase                    string `json:"phase"`
	PhaseSinceUnixMillis     uint64 `json:"phase_since_unix_ms"`
	TreePhase                uint32 `json:"tree_phase"`
	InstalledLeaves          uint64 `json:"installed_leaves"`
	PlannedLeaves            uint64 `json:"planned_leaves"`
	LeavesObservedUnixMillis uint64 `json:"leaves_observed_unix_ms"`
	LastOutcome              string `json:"last_outcome"`
	LastOutcomeUnixMillis    uint64 `json:"last_outcome_unix_ms"`
	LastError                string `json:"last_error"`
	LastErrorUnixMillis      uint64 `json:"last_error_unix_ms"`
	Source                   string `json:"source"`
	Attempts                 uint32 `json:"attempts"`
	ConsecutiveFailures      uint32 `json:"consecutive_failures"`
	AttemptStartedUnixMillis uint64 `json:"attempt_started_unix_ms"`
	PermitWaitMillis         uint64 `json:"permit_wait_ms"`
	AttemptMillis            uint64 `json:"attempt_ms"`
	AttemptInstalledLeaves   uint64 `json:"attempt_installed_leaves"`
}

type SessionSeal struct {
	Generation              uint64 `json:"generation"`
	Checkpoint              uint64 `json:"checkpoint"`
	View                    uint64 `json:"view"`
	GlobalExecuted          uint64 `json:"global_executed"`
	Drain                   string `json:"drain"`
	DrainHeaders            uint32 `json:"drain_headers"`
	FinalizedUnixMillis     uint64 `json:"finalized_unix_ms"`
	LastSubmittedUnixMillis uint64 `json:"last_submitted_unix_ms"`
	Submissions             uint32 `json:"submissions"`
	RecordedUnixMillis      uint64 `json:"recorded_unix_ms"`
}

// ObservedRewardWindows retains the producer's daily estimates verbatim.
// Its units use the prover reward scale, independently of wallet base units.
type ObservedRewardWindows struct {
	Hour *ObservedRewardWindow `json:"60m"`
	Day  *ObservedRewardWindow `json:"24h"`
}

type ObservedRewardWindow struct {
	UnitsPerDay string `json:"units_per_day"`
	QuilPerDay  string `json:"quil_per_day"`
}
