package actions

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
	"github.com/quilscan-com/quilscan-agent/internal/release"
)

var (
	ErrDevNodeAutoUpdateRequiresDev = errors.New("Dev Node auto update requires node source dev")
	ErrReleasesAutoUpdateInactive   = errors.New("Releases Node auto update requires a running node and an enabled newer release")
)

type NodeUpdateService interface {
	Start(unit string) error
	Stop(unit string) error
	IsActive(unit string) bool
	CheckReleaseStartup(unit string) error
}

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

	StartStop NodeUpdateService

	Downloader      Downloader
	DevPreparer     DevNodePreparer
	NodeManifestURL string
	LoadState       func() (*config.State, error)
	UpdateState     func(func(*config.State) error) (*config.State, error)
	EmitRaw         func(map[string]interface{})
	// PatchNodeStatus folds an authoritative patch into reconcile's cached
	// node_status snapshot. Used right after a successful update so the
	// stale `node_update_available: true` doesn't linger for up to an hour
	// (until the next runVersionPoll tick).
	PatchNodeStatus func(patch map[string]interface{})
	transactionOps  fileTransactionOps
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
		automatic, _ := c.Args["automatic"].(bool)
		releasesAutomatic := isAutomaticReleaseUpdate(c)
		if d.LoadState == nil {
			emit(Status{ID: c.ID, Step: "failed", Error: "agent state unavailable"})
			return fmt.Errorf("LoadState dep missing")
		}
		state, err := d.LoadState()
		if err != nil || state == nil || state.ConfigPath == "" {
			emit(Status{ID: c.ID, Step: "failed", Error: "no install recorded — run install first"})
			return fmt.Errorf("no install recorded")
		}
		if releasesAutomatic {
			if state.NodeSource != nodemanifest.SourceReleases || !state.ReleasesNodeAutoUpdate.Enabled ||
				version != state.LatestReleasesNodeVersion || !release.VersionNewerThan(version, CurrentReleasesNodeVersion(state)) ||
				d.StartStop == nil || !d.StartStop.IsActive(d.UnitName) {
				emit(Status{ID: c.ID, Step: "interrupted", Message: ErrReleasesAutoUpdateInactive.Error()})
				return ErrReleasesAutoUpdateInactive
			}
		} else if automatic && state.NodeSource != nodemanifest.SourceDev {
			emit(Status{ID: c.ID, Step: "failed", Error: ErrDevNodeAutoUpdateRequiresDev.Error()})
			return ErrDevNodeAutoUpdateRequiresDev
		}
		fromVersion := state.NodeVersion
		if releasesAutomatic {
			fromVersion = CurrentReleasesNodeVersion(state)
		}
		if state.NodeSource == nodemanifest.SourceDev {
			return updateDevNode(c, emit, d, state)
		}

		releaseDir, cleanupReleaseDir, err := makeNodeReleaseTempDir(d.BinaryPath)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		defer cleanupReleaseDir()

		emit(Status{ID: c.ID, Step: "downloading", Progress: 0.35})
		if err := d.Downloader.Download(version, d.Platform, releaseDir); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		versionedBinary := filepath.Join(releaseDir, fmt.Sprintf("node-%s-%s", version, d.Platform))
		var releaseSHA string
		if state.NodeSource == nodemanifest.SourceReleases {
			releaseSHA, err = nodemanifest.HashFile(versionedBinary)
			if err != nil {
				emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
				return err
			}
		}
		tx, err := prepareNodeReleaseTransaction(versionedBinary, d.BinaryPath, d.transactionOps)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		defer func() { _ = tx.Finalize() }()

		wasActive := d.StartStop.IsActive(d.UnitName)
		if releasesAutomatic && !wasActive {
			emit(Status{ID: c.ID, Step: "interrupted", Message: ErrReleasesAutoUpdateInactive.Error()})
			return ErrReleasesAutoUpdateInactive
		}
		emit(Status{ID: c.ID, Step: "stopping", Progress: 0.60})
		if err := d.StartStop.Stop(d.UnitName); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		emit(Status{ID: c.ID, Step: "installing_binary", Progress: 0.65})
		if err := tx.Commit(); err != nil {
			return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
		}

		emit(Status{ID: c.ID, Step: "starting", Progress: 0.92})
		if err := d.StartStop.Start(d.UnitName); err != nil {
			return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
		}
		emit(Status{ID: c.ID, Step: "verifying_startup", Progress: 0.96})
		if err := d.StartStop.CheckReleaseStartup(d.UnitName); err != nil {
			return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
		}

		startedAt := time.Now().UTC()
		if d.UpdateState == nil {
			err := fmt.Errorf("UpdateState dep missing")
			return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
		}
		if _, err := d.UpdateState(func(latest *config.State) error {
			latest.NodeVersion = version
			latest.LastStartedAt = startedAt
			if releaseSHA != "" {
				latest.InstalledNodeVersion = version
				latest.NodeBaseVersion = version
				latest.NodeBinarySHA256 = releaseSHA
				latest.VerifiedReleasesNodeSHA256 = releaseSHA
			}
			return nil
		}); err != nil {
			return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
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
			patch := map[string]interface{}{
				"current_node_version":  version,
				"node_info_version":     version,
				"node_version":          version,
				"node_update_available": false,
				"version_polled_at":     time.Now().UTC().Format(time.RFC3339),
			}
			if releaseSHA != "" {
				patch["installed_node_version"] = version
				patch["node_base_version"] = version
				patch["node_binary_sha256"] = releaseSHA
			}
			d.PatchNodeStatus(patch)
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

func isAutomaticReleaseUpdate(command Command) bool {
	automatic, _ := command.Args["automatic"].(bool)
	return automatic && command.Args["source"] == nodemanifest.SourceReleases
}

// CurrentReleasesNodeVersion uses the higher known version while the installed
// metadata and runtime report settle, so release automation cannot downgrade
// or reinstall the current version because one of them lags.
func CurrentReleasesNodeVersion(state *config.State) string {
	installed := strings.TrimSpace(state.InstalledNodeVersion)
	reported := strings.TrimSpace(state.NodeVersion)
	_, hasInstalled := release.ParseDottedVersion(installed)
	if !hasInstalled || release.VersionNewerThan(reported, installed) {
		return reported
	}
	return installed
}

func updateDevNode(c Command, emit Emitter, d NodeUpdaterDeps, state *config.State) error {
	fromVersion := state.InstalledNodeVersion
	manifestURL := d.NodeManifestURL
	if manifestURL == "" {
		manifestURL = state.NodeManifestURL
	}
	if manifestURL == "" {
		manifestURL = nodemanifest.DefaultURL
	}
	preparer := d.DevPreparer
	if preparer == nil {
		preparer = ManifestDevNodeInstaller{
			progress: func(step string, progress float64) {
				emit(Status{ID: c.ID, Step: step, Progress: progress})
			},
		}
	}

	emit(Status{ID: c.ID, Step: "downloading", Progress: 0.35})
	prepared, err := preparer.PrepareLatest(d.Platform, d.BinaryPath, manifestURL)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	defer prepared.Cleanup()
	tx, err := prepareDevNodeTransaction(prepared.BinaryPath, d.BinaryPath, d.transactionOps)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	defer func() { _ = tx.Finalize() }()

	wasActive := d.StartStop.IsActive(d.UnitName)
	emit(Status{ID: c.ID, Step: "stopping", Progress: 0.10})
	if err := d.StartStop.Stop(d.UnitName); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	emit(Status{ID: c.ID, Step: "installing_binary", Progress: 0.65})
	if err := tx.Commit(); err != nil {
		return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
	}

	emit(Status{ID: c.ID, Step: "starting", Progress: 0.92})
	if err := d.StartStop.Start(d.UnitName); err != nil {
		return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
	}

	startedAt := time.Now().UTC()
	if d.UpdateState == nil {
		err := fmt.Errorf("UpdateState dep missing")
		return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
	}
	if _, err := d.UpdateState(func(latest *config.State) error {
		applyDevInstallResult(latest, prepared.Result)
		latest.LastStartedAt = startedAt
		return nil
	}); err != nil {
		return failNodeUpdate(c, emit, err, tx, d.StartStop, d.UnitName, wasActive)
	}

	if d.PatchNodeStatus != nil {
		d.PatchNodeStatus(nodeStatusPatchForDevResult(prepared.Result, false))
	}
	if d.EmitRaw != nil {
		d.EmitRaw(map[string]interface{}{
			"type":         "node_updated",
			"from_version": fromVersion,
			"to_version":   prepared.Result.Version,
			"node_source":  nodemanifest.SourceDev,
		})
	}
	emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
	return nil
}

func failNodeUpdate(c Command, emit Emitter, operation error, tx *fileTransaction, service NodeUpdateService, unit string, wasActive bool) error {
	err := withRollbackOutcome(operation, func() error {
		if err := rollbackNodeUpdate(tx, service, unit, wasActive); err != nil {
			return err
		}
		if isAutomaticReleaseUpdate(c) && wasActive {
			return service.CheckReleaseStartup(unit)
		}
		return nil
	})
	emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
	return err
}

func rollbackNodeUpdate(tx *fileTransaction, service NodeUpdateService, unit string, wasActive bool) error {
	var rollbackErrors []error
	if err := service.Stop(unit); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("stop replacement service: %w", err))
	}
	if err := tx.Rollback(); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}
	if len(rollbackErrors) == 0 && wasActive {
		if err := service.Start(unit); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restart previous service: %w", err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func applyDevInstallResult(state *config.State, result DevNodeInstallResult) {
	state.NodeSource = nodemanifest.SourceDev
	state.ReleasesNodeAutoUpdate.Enabled = false
	state.LatestReleasesNodeVersion = ""
	state.VerifiedReleasesNodeSHA256 = ""
	state.InstalledNodeVersion = result.Version
	state.NodeBaseVersion = result.BaseVersion
	state.NodeBuildNumber = result.BuildNumber
	state.NodeBinarySHA256 = result.SHA256
	state.NodeManifestURL = result.ManifestURL
	state.NodeManifestCheckedAt = result.CheckedAt
	state.DevNodeSignatureVerified = result.SignatureVerified
	state.LatestDevNodeVersion = result.Version
	state.LatestDevNodeURL = result.URL
	state.LatestDevNodeSHA256 = result.SHA256
	state.LatestDevNodeBuildNumber = result.BuildNumber
}

func nodeStatusPatchForDevResult(result DevNodeInstallResult, updateAvailable bool) map[string]interface{} {
	checkedAt := result.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	return map[string]interface{}{
		"node_source":                  nodemanifest.SourceDev,
		"installed_node_version":       result.Version,
		"node_base_version":            result.BaseVersion,
		"node_build_number":            result.BuildNumber,
		"node_binary_sha256":           result.SHA256,
		"node_manifest_url":            result.ManifestURL,
		"node_manifest_checked_at":     checkedAt.Format(time.RFC3339),
		"dev_node_signature_verified":  result.SignatureVerified,
		"latest_node_version":          result.Version,
		"latest_dev_node_version":      result.Version,
		"latest_dev_node_url":          result.URL,
		"latest_dev_node_sha256":       result.SHA256,
		"latest_dev_node_build_number": result.BuildNumber,
		"node_update_source":           nodemanifest.SourceDev,
		"node_update_available":        updateAvailable,
	}
}

// Compile-time check that ReleaseDownloader (defined in install.go) satisfies
// the Downloader interface — same shape as InstallDeps.
var _ Downloader = ReleaseDownloader{}
