package devnodeautoupdate

import (
	"strings"

	"github.com/quilscan-com/quilscan-agent/internal/actions"
	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
	"github.com/quilscan-com/quilscan-agent/internal/release"
)

func (c *Controller) isReleases() bool {
	return c.deps.Source == nodemanifest.SourceReleases
}

func (c *Controller) settings(state *config.State) config.NodeAutoUpdateState {
	if c.isReleases() {
		return state.ReleasesNodeAutoUpdate
	}
	return config.NodeAutoUpdateState{
		Enabled:                 state.DevNodeAutoUpdateEnabled,
		LastEventID:             state.DevNodeAutoUpdateLastEventID,
		LastResult:              state.DevNodeAutoUpdateLastResult,
		LastFromVersion:         state.DevNodeAutoUpdateLastFromVersion,
		LastTargetVersion:       state.DevNodeAutoUpdateLastTargetVersion,
		LastCompletedAt:         state.DevNodeAutoUpdateLastCompletedAt,
		LastFailedTargetVersion: state.DevNodeAutoUpdateLastFailedTargetVersion,
	}
}

func (c *Controller) saveSettings(state *config.State, settings config.NodeAutoUpdateState) {
	if c.isReleases() {
		state.ReleasesNodeAutoUpdate = settings
		return
	}
	state.DevNodeAutoUpdateRevision++
	state.DevNodeAutoUpdateEnabled = settings.Enabled
	state.DevNodeAutoUpdateLastEventID = settings.LastEventID
	state.DevNodeAutoUpdateLastResult = settings.LastResult
	state.DevNodeAutoUpdateLastFromVersion = settings.LastFromVersion
	state.DevNodeAutoUpdateLastTargetVersion = settings.LastTargetVersion
	state.DevNodeAutoUpdateLastCompletedAt = settings.LastCompletedAt
	state.DevNodeAutoUpdateLastFailedTargetVersion = settings.LastFailedTargetVersion
}

func (c *Controller) candidateFromState(state *config.State) (candidate, bool) {
	if !c.isReleases() {
		return candidateFromState(state)
	}
	if state == nil || state.ConfigPath == "" || state.NodeSource != nodemanifest.SourceReleases {
		return candidate{}, false
	}
	settings := state.ReleasesNodeAutoUpdate
	current := actions.CurrentReleasesNodeVersion(state)
	latest := strings.TrimSpace(state.LatestReleasesNodeVersion)
	if !settings.Enabled || latest == settings.BlockedVersion || !release.VersionNewerThan(latest, current) {
		return candidate{}, false
	}
	return candidate{FromVersion: current, TargetVersion: latest}, true
}

func (c *Controller) nodeActive() bool {
	return !c.isReleases() || (c.deps.NodeActive != nil && c.deps.NodeActive())
}
