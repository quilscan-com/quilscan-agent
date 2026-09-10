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

type NodeSourceSwitcherDeps struct {
	UnitName              string
	UnitDir               string
	BinaryPath            string
	Platform              string
	StartStop             NodeUpdateService
	Reload                func() error
	Downloader            Downloader
	DevPreparer           DevNodePreparer
	NodeManifestURL       string
	LatestOfficialVersion func(platform string) (string, error)
	LoadState             func() (*config.State, error)
	UpdateState           func(func(*config.State) error) (*config.State, error)
	EmitRaw               func(map[string]interface{})
	PatchNodeStatus       func(patch map[string]interface{})
	transactionOps        fileTransactionOps
}

func NewSwitchNodeSourceHandler(d NodeSourceSwitcherDeps) Handler {
	return func(c Command, emit Emitter) error {
		source, _ := c.Args["source"].(string)
		if source == "" {
			source, _ = c.Args["target_source"].(string)
		}
		target := normalizeNodeSource(source)
		if target == "" {
			emit(Status{ID: c.ID, Step: "failed", Error: "missing or invalid source"})
			return fmt.Errorf("missing or invalid source")
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
		if target == nodemanifest.SourceDev {
			return switchToDevNode(c, emit, d, state)
		}
		return switchToReleasesNode(c, emit, d, state)
	}
}

func switchToDevNode(c Command, emit Emitter, d NodeSourceSwitcherDeps, state *config.State) error {
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
	fromSource := state.NodeSource
	fromVersion := state.InstalledNodeVersion

	emit(Status{ID: c.ID, Step: "downloading", Progress: 0.35})
	prepared, err := preparer.PrepareLatest(d.Platform, d.BinaryPath, manifestURL)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	defer prepared.Cleanup()
	bundleTx, err := prepareDevNodeTransaction(prepared.BinaryPath, d.BinaryPath, d.transactionOps)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	defer func() { _ = bundleTx.Finalize() }()
	serviceTx, err := d.prepareNodeServiceTransaction(true)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	if serviceTx != nil {
		defer func() { _ = serviceTx.Finalize() }()
	}
	if err := applyNodeSourceSwitch(c, emit, d, bundleTx, serviceTx, func() error {
		if d.UpdateState == nil {
			return fmt.Errorf("UpdateState dep missing")
		}
		_, err := d.UpdateState(func(latest *config.State) error {
			applyDevInstallResult(latest, prepared.Result)
			latest.LastStartedAt = time.Now().UTC()
			return nil
		})
		return err
	}); err != nil {
		return err
	}
	if d.PatchNodeStatus != nil {
		d.PatchNodeStatus(nodeStatusPatchForDevResult(prepared.Result, false))
	}
	if d.EmitRaw != nil {
		d.EmitRaw(map[string]interface{}{
			"type":         "node_source_switched",
			"from_source":  fromSource,
			"from_version": fromVersion,
			"to_source":    nodemanifest.SourceDev,
			"to_version":   prepared.Result.Version,
		})
	}
	emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
	return nil
}

func switchToReleasesNode(c Command, emit Emitter, d NodeSourceSwitcherDeps, state *config.State) error {
	latestFetcher := d.LatestOfficialVersion
	if latestFetcher == nil {
		latestFetcher = func(platform string) (string, error) {
			return release.FetchLatestNodeVersion(release.ReleaseBaseURL, platform)
		}
	}
	latest, err := latestFetcher(d.Platform)
	if err != nil || latest == "" {
		if err == nil {
			err = fmt.Errorf("official release version unavailable")
		}
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	fromSource := state.NodeSource
	fromVersion := state.InstalledNodeVersion

	releaseDir, cleanupReleaseDir, err := makeNodeReleaseTempDir(d.BinaryPath)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	defer cleanupReleaseDir()

	emit(Status{ID: c.ID, Step: "downloading", Progress: 0.35})
	if err := d.Downloader.Download(latest, d.Platform, releaseDir); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	versionedBinary := filepath.Join(releaseDir, fmt.Sprintf("node-%s-%s", latest, d.Platform))
	sha, _ := nodemanifest.HashFile(versionedBinary)
	bundleTx, err := prepareNodeReleaseTransaction(versionedBinary, d.BinaryPath, d.transactionOps)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	defer func() { _ = bundleTx.Finalize() }()
	serviceTx, err := d.prepareNodeServiceTransaction(false)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	if serviceTx != nil {
		defer func() { _ = serviceTx.Finalize() }()
	}

	manifestURL := nodeManifestURL(d.NodeManifestURL)
	var persisted *config.State
	if err := applyNodeSourceSwitch(c, emit, d, bundleTx, serviceTx, func() error {
		if d.UpdateState == nil {
			return fmt.Errorf("UpdateState dep missing")
		}
		var err error
		checkedAt := time.Now().UTC()
		persisted, err = d.UpdateState(func(current *config.State) error {
			current.NodeSource = nodemanifest.SourceReleases
			current.InstalledNodeVersion = latest
			current.NodeBaseVersion = latest
			current.NodeBuildNumber = 0
			current.NodeBinarySHA256 = sha
			current.NodeManifestURL = manifestURL
			current.NodeManifestCheckedAt = checkedAt
			current.DevNodeSignatureVerified = false
			current.NodeVersion = latest
			current.LastStartedAt = checkedAt
			return nil
		})
		return err
	}); err != nil {
		return err
	}
	if d.PatchNodeStatus != nil {
		d.PatchNodeStatus(map[string]interface{}{
			"node_source":              nodemanifest.SourceReleases,
			"installed_node_version":   latest,
			"node_base_version":        latest,
			"node_build_number":        0,
			"node_binary_sha256":       sha,
			"node_manifest_url":        persisted.NodeManifestURL,
			"node_manifest_checked_at": persisted.NodeManifestCheckedAt.Format(time.RFC3339),
			"current_node_version":     latest,
			"node_info_version":        latest,
			"node_version":             latest,
			"latest_node_version":      latest,
			"node_update_source":       nodemanifest.SourceReleases,
			"node_update_available":    false,
		})
	}
	if d.EmitRaw != nil {
		d.EmitRaw(map[string]interface{}{
			"type":         "node_source_switched",
			"from_source":  fromSource,
			"from_version": fromVersion,
			"to_source":    nodemanifest.SourceReleases,
			"to_version":   latest,
		})
	}
	emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
	return nil
}

func applyNodeSourceSwitch(c Command, emit Emitter, d NodeSourceSwitcherDeps, bundleTx, serviceTx *fileTransaction, persist func() error) error {
	wasActive := d.StartStop.IsActive(d.UnitName)
	emit(Status{ID: c.ID, Step: "stopping", Progress: 0.10})
	if err := d.StartStop.Stop(d.UnitName); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}

	emit(Status{ID: c.ID, Step: "installing_binary", Progress: 0.65})
	bundleChanged := true
	if err := bundleTx.Commit(); err != nil {
		return failNodeSourceSwitch(c, emit, err, d, bundleTx, bundleChanged, serviceTx, false, wasActive)
	}

	emit(Status{ID: c.ID, Step: "configuring_service", Progress: 0.82})
	serviceChanged := serviceTx != nil
	if serviceTx != nil {
		if err := serviceTx.Commit(); err != nil {
			return failNodeSourceSwitch(c, emit, err, d, bundleTx, bundleChanged, serviceTx, serviceChanged, wasActive)
		}
		if d.Reload != nil {
			if err := d.Reload(); err != nil {
				return failNodeSourceSwitch(c, emit, err, d, bundleTx, bundleChanged, serviceTx, serviceChanged, wasActive)
			}
		}
	}

	emit(Status{ID: c.ID, Step: "starting", Progress: 0.92})
	if err := d.StartStop.Start(d.UnitName); err != nil {
		return failNodeSourceSwitch(c, emit, err, d, bundleTx, bundleChanged, serviceTx, serviceChanged, wasActive)
	}
	if err := persist(); err != nil {
		return failNodeSourceSwitch(c, emit, err, d, bundleTx, bundleChanged, serviceTx, serviceChanged, wasActive)
	}
	return nil
}

func failNodeSourceSwitch(c Command, emit Emitter, operation error, d NodeSourceSwitcherDeps, bundleTx *fileTransaction, bundleChanged bool, serviceTx *fileTransaction, serviceChanged, wasActive bool) error {
	err := withRollbackOutcome(operation, func() error {
		return rollbackNodeSourceSwitch(d, bundleTx, bundleChanged, serviceTx, serviceChanged, wasActive)
	})
	emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
	return err
}

func rollbackNodeSourceSwitch(d NodeSourceSwitcherDeps, bundleTx *fileTransaction, bundleChanged bool, serviceTx *fileTransaction, serviceChanged, wasActive bool) error {
	var rollbackErrors []error
	if err := d.StartStop.Stop(d.UnitName); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("stop replacement service: %w", err))
	}
	if bundleChanged {
		if err := bundleTx.Rollback(); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	if serviceChanged {
		if err := serviceTx.Rollback(); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
		if d.Reload != nil {
			if err := d.Reload(); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("reload restored service configuration: %w", err))
			}
		}
	}
	if len(rollbackErrors) == 0 && wasActive {
		if err := d.StartStop.Start(d.UnitName); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restart previous service: %w", err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func normalizeNodeSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "releases", "release", "official", "stable":
		return nodemanifest.SourceReleases
	case "dev", "development", "test", "testing":
		return nodemanifest.SourceDev
	default:
		return ""
	}
}

func nodeManifestURL(url string) string {
	if strings.TrimSpace(url) == "" {
		return nodemanifest.DefaultURL
	}
	return strings.TrimSpace(url)
}
