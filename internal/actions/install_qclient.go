package actions

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/release"
)

type QClientDownloader interface {
	DownloadLatest(platform, destDir string) (string, error)
}

type QClientInstallDeps struct {
	BinaryPath string
	Platform   string

	Downloader QClientDownloader
	LoadState  func() (*config.State, error)
	SaveState  func(*config.State) error
	EmitRaw    func(map[string]interface{})
}

func NewInstallQClientHandler(d QClientInstallDeps) Handler {
	return func(c Command, emit Emitter) error {
		_, err := installQClient(d, func(step string, progress float64) {
			emit(Status{ID: c.ID, Step: step, Progress: progress})
		})
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
		return nil
	}
}

func InstallQClient(d QClientInstallDeps) (string, error) {
	return installQClient(d, nil)
}

func installQClient(d QClientInstallDeps, progress func(step string, progress float64)) (string, error) {
	if d.Downloader == nil {
		d.Downloader = QClientReleaseDownloader{}
	}
	if d.BinaryPath == "" {
		return "", fmt.Errorf("missing qclient binary path")
	}
	if d.Platform == "" {
		return "", fmt.Errorf("missing platform")
	}

	if progress != nil {
		progress("downloading", 0.25)
	}
	releaseDir, cleanupReleaseDir, err := makeNodeReleaseTempDir(d.BinaryPath)
	if err != nil {
		return "", err
	}
	defer cleanupReleaseDir()

	version, err := d.Downloader.DownloadLatest(d.Platform, releaseDir)
	if err != nil {
		return "", err
	}
	if version == "" {
		return "", fmt.Errorf("empty qclient version")
	}

	if progress != nil {
		progress("installing_binary", 0.75)
	}
	versionedBinary := filepath.Join(releaseDir, release.ArtifactBaseName("qclient", version, d.Platform))
	if err := installNodeBinary(versionedBinary, d.BinaryPath); err != nil {
		return "", err
	}

	var state *config.State
	if d.LoadState != nil {
		state, _ = d.LoadState()
	}
	if state == nil {
		state = &config.State{}
	}
	state.QClientBinaryPath = d.BinaryPath
	state.QClientVersion = version
	state.QClientInstalledAt = time.Now().UTC()
	if d.SaveState != nil {
		if err := d.SaveState(state); err != nil {
			return "", err
		}
	}

	if d.EmitRaw != nil {
		d.EmitRaw(map[string]interface{}{
			"type":                "meta_update",
			"has_qclient":         true,
			"qclient_version":     version,
			"qclient_binary_path": d.BinaryPath,
		})
	}
	return version, nil
}

type QClientReleaseDownloader struct{}

func (QClientReleaseDownloader) DownloadLatest(platform, destDir string) (string, error) {
	return release.DownloadQClientLatest(release.ReleaseBaseURL, platform, destDir)
}
