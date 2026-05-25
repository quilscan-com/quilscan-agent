package actions

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
	"github.com/quilscan-com/quilscan-agent/internal/release"
)

type NodeSourceSwitcherDeps struct {
	UnitName   string
	UnitDir    string
	BinaryPath string
	Platform   string
	StartStop  interface {
		Start(unit string) error
		Stop(unit string) error
	}
	Reload                func() error
	Downloader            Downloader
	DevInstaller          DevNodeInstaller
	NodeManifestURL       string
	LatestOfficialVersion func(platform string) (string, error)
	LoadState             func() (*config.State, error)
	SaveState             func(*config.State) error
	EmitRaw               func(map[string]interface{})
	PatchNodeStatus       func(patch map[string]interface{})
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
	installer := d.DevInstaller
	if installer == nil {
		installer = ManifestDevNodeInstaller{}
	}
	fromSource := state.NodeSource
	fromVersion := state.InstalledNodeVersion

	emit(Status{ID: c.ID, Step: "stopping", Progress: 0.10})
	if err := d.StartStop.Stop(d.UnitName); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	emit(Status{ID: c.ID, Step: "downloading", Progress: 0.35})
	result, err := installer.InstallLatest(d.Platform, d.BinaryPath, manifestURL)
	if err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		_ = d.StartStop.Start(d.UnitName)
		return err
	}
	emit(Status{ID: c.ID, Step: "configuring_service", Progress: 0.82})
	if err := d.setNodeSignatureCheckDisabled(true); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	emit(Status{ID: c.ID, Step: "starting", Progress: 0.92})
	if err := d.StartStop.Start(d.UnitName); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	applyDevInstallResult(state, result)
	state.LastStartedAt = time.Now().UTC()
	if err := d.SaveState(state); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	if d.PatchNodeStatus != nil {
		d.PatchNodeStatus(nodeStatusPatchForDevResult(result, false))
	}
	if d.EmitRaw != nil {
		d.EmitRaw(map[string]interface{}{
			"type":         "node_source_switched",
			"from_source":  fromSource,
			"from_version": fromVersion,
			"to_source":    nodemanifest.SourceDev,
			"to_version":   result.Version,
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
	if err := d.Downloader.Download(latest, d.Platform, releaseDir); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		_ = d.StartStop.Start(d.UnitName)
		return err
	}
	emit(Status{ID: c.ID, Step: "installing_binary", Progress: 0.65})
	versionedBinary := filepath.Join(releaseDir, fmt.Sprintf("node-%s-%s", latest, d.Platform))
	if err := installNodeBinary(versionedBinary, d.BinaryPath); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		_ = d.StartStop.Start(d.UnitName)
		return err
	}
	sha, _ := nodemanifest.HashFile(d.BinaryPath)
	emit(Status{ID: c.ID, Step: "configuring_service", Progress: 0.82})
	if err := d.setNodeSignatureCheckDisabled(false); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}

	emit(Status{ID: c.ID, Step: "starting", Progress: 0.92})
	if err := d.StartStop.Start(d.UnitName); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}

	state.NodeSource = nodemanifest.SourceReleases
	state.InstalledNodeVersion = latest
	state.NodeBaseVersion = latest
	state.NodeBuildNumber = 0
	state.NodeBinarySHA256 = sha
	state.NodeManifestURL = nodeManifestURL(d.NodeManifestURL)
	state.NodeManifestCheckedAt = time.Now().UTC()
	state.NodeVersion = latest
	state.LastStartedAt = time.Now().UTC()
	if err := d.SaveState(state); err != nil {
		emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	if d.PatchNodeStatus != nil {
		d.PatchNodeStatus(map[string]interface{}{
			"node_source":              nodemanifest.SourceReleases,
			"installed_node_version":   latest,
			"node_base_version":        latest,
			"node_build_number":        0,
			"node_binary_sha256":       sha,
			"node_manifest_url":        state.NodeManifestURL,
			"node_manifest_checked_at": state.NodeManifestCheckedAt.Format(time.RFC3339),
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
