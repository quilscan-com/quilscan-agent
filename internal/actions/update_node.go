package actions

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
)

// NodeUpdaterDeps wires the update_node flow:
//
//	stop → download → install binary → start
//
// The node service definition points at a stable binary path, so node updates
// only swap the binary/digest/signature files at that path. Service-definition
// changes belong in install/migrate or an explicit repair flow, not every
// version update.
type NodeUpdaterDeps struct {
	UnitName   string
	BinaryPath string
	Platform   string

	// Service control: Start/Stop the unit.
	StartStop interface {
		Start(unit string) error
		Stop(unit string) error
	}

	Downloader Downloader
	LoadState  func() (*config.State, error)
	SaveState  func(*config.State) error
	EmitRaw    func(map[string]interface{})
	// PatchNodeStatus folds an authoritative patch into reconcile's cached
	// node_status snapshot. Used right after a successful update so the
	// stale `node_update_available: true` doesn't linger for up to an hour
	// (until the next runVersionPoll tick).
	PatchNodeStatus func(patch map[string]interface{})
}

// NewUpdateNodeHandler returns a Handler for the update_node action.
//
// Args:
//
//	version (string, required) target node version, e.g. "2.1.0.22"
func NewUpdateNodeHandler(d NodeUpdaterDeps) Handler {
	return func(c Command, emit Emitter) error {
		version, _ := c.Args["version"].(string)
		if version == "" {
			emit(Status{ID: c.ID, Step: "failed", Error: "missing version"})
			return fmt.Errorf("missing version")
		}

		if d.LoadState == nil {
			emit(Status{ID: c.ID, Step: "failed", Error: "agent state unavailable"})
			return fmt.Errorf("LoadState dep missing")
		}
		state, err := d.LoadState()
		if err != nil || state == nil || state.ConfigPath == "" {
			emit(Status{ID: c.ID, Step: "failed", Error: "no install recorded — run install first"})
			return fmt.Errorf("no install recorded")
		}
		fromVersion := state.NodeVersion

		emit(Status{ID: c.ID, Step: "stopping", Progress: 0.10})
		if err := d.StartStop.Stop(d.UnitName); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		releaseDir, cleanupReleaseDir, err := makeNodeReleaseTempDir(d.BinaryPath)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			_ = d.StartStop.Start(d.UnitName)
			return err
		}
		defer cleanupReleaseDir()

		emit(Status{ID: c.ID, Step: "downloading", Progress: 0.35})
		if err := d.Downloader.Download(version, d.Platform, releaseDir); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			_ = d.StartStop.Start(d.UnitName)
			return err
		}

		emit(Status{ID: c.ID, Step: "installing_binary", Progress: 0.65})
		versionedBinary := filepath.Join(releaseDir, fmt.Sprintf("node-%s-%s", version, d.Platform))
		if err := installNodeBinary(versionedBinary, d.BinaryPath); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			_ = d.StartStop.Start(d.UnitName)
			return err
		}

		emit(Status{ID: c.ID, Step: "starting", Progress: 0.92})
		if err := d.StartStop.Start(d.UnitName); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		state.NodeVersion = version
		state.LastStartedAt = time.Now().UTC()
		if d.SaveState == nil {
			err := fmt.Errorf("SaveState dep missing")
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		if err := d.SaveState(state); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		// Refresh reconcile's cached node_status so the frontend sees the
		// post-update truth right away. Otherwise the banner stays on
		// "current → latest [Update]" until the next 1h version poll.
		if d.PatchNodeStatus != nil {
			// Both runVersionPoll and the frontend prefer node_info_version
			// over current_node_version. Skipping it here would (a) leave
			// the displayed Node Version stale until the next runVerify and
			// (b) let the next runVersionPoll re-flag node_update_available
			// using the stale node_info_version vs the new latest. We do
			// NOT touch latest_node_version — that field reflects upstream
			// availability and must come from the next poll, not be forged
			// to match the just-installed version.
			d.PatchNodeStatus(map[string]interface{}{
				"current_node_version":  version,
				"node_info_version":     version,
				"node_version":          version,
				"node_update_available": false,
				"version_polled_at":     time.Now().UTC().Format(time.RFC3339),
			})
		}

		if d.EmitRaw != nil {
			d.EmitRaw(map[string]interface{}{
				"type":         "node_updated",
				"from_version": fromVersion,
				"to_version":   version,
			})
		}

		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
		return nil
	}
}

// Compile-time check that ReleaseDownloader (defined in install.go) satisfies
// the Downloader interface — same shape as InstallDeps.
var _ Downloader = ReleaseDownloader{}
