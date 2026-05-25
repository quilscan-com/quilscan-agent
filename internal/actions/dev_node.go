package actions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
)

type DevNodeInstallResult struct {
	Version     string
	BaseVersion string
	BuildNumber int
	SHA256      string
	URL         string
	ManifestURL string
	CheckedAt   time.Time
}

type DevNodeInstaller interface {
	InstallLatest(platform, binaryPath, manifestURL string) (DevNodeInstallResult, error)
}

type ManifestDevNodeInstaller struct{}

func (ManifestDevNodeInstaller) InstallLatest(platform, binaryPath, manifestURL string) (DevNodeInstallResult, error) {
	platform = strings.TrimSpace(platform)
	if strings.TrimSpace(platform) == "" {
		return DevNodeInstallResult{}, fmt.Errorf("missing platform")
	}
	if platform == "linux-arm64" {
		return DevNodeInstallResult{}, fmt.Errorf("Dev node is not available for Linux ARM64 yet. Please use the official release node on this server.")
	}
	if strings.TrimSpace(binaryPath) == "" {
		return DevNodeInstallResult{}, fmt.Errorf("missing binary path")
	}
	if strings.TrimSpace(manifestURL) == "" {
		manifestURL = nodemanifest.DefaultURL
	}
	manifest, err := nodemanifest.Fetch(manifestURL)
	if err != nil {
		return DevNodeInstallResult{}, fmt.Errorf("fetch node manifest: %w", err)
	}
	latest, artifact, ok := manifest.LatestDev(platform)
	if !ok {
		return DevNodeInstallResult{}, fmt.Errorf("node manifest missing latest dev artifact for %s", platform)
	}
	if strings.TrimSpace(artifact.URL) == "" {
		return DevNodeInstallResult{}, fmt.Errorf("latest dev artifact missing url for %s", platform)
	}

	releaseDir, cleanupReleaseDir, err := makeNodeReleaseTempDir(binaryPath)
	if err != nil {
		return DevNodeInstallResult{}, err
	}
	defer cleanupReleaseDir()

	tmpBinary := filepath.Join(releaseDir, "dev-node-"+platform)
	if err := nodemanifest.DownloadFile(artifact.URL, tmpBinary); err != nil {
		return DevNodeInstallResult{}, fmt.Errorf("download dev node: %w", err)
	}
	gotSHA, err := nodemanifest.HashFile(tmpBinary)
	if err != nil {
		return DevNodeInstallResult{}, fmt.Errorf("hash dev node: %w", err)
	}
	if !strings.EqualFold(gotSHA, artifact.SHA256) {
		return DevNodeInstallResult{}, fmt.Errorf("dev node sha mismatch: got %s want %s", gotSHA, artifact.SHA256)
	}
	if err := installDevNodeBinary(tmpBinary, binaryPath); err != nil {
		return DevNodeInstallResult{}, err
	}
	return DevNodeInstallResult{
		Version:     latest.Version,
		BaseVersion: latest.BaseVersion,
		BuildNumber: latest.BuildNumber,
		SHA256:      gotSHA,
		URL:         artifact.URL,
		ManifestURL: manifestURL,
		CheckedAt:   time.Now().UTC(),
	}, nil
}

func installDevNodeBinary(src, dst string) error {
	if err := moveFile(src, dst, 0o755); err != nil {
		return err
	}
	removeNodeSidecars(dst)
	return nil
}

func removeNodeSidecars(binaryPath string) {
	_ = os.Remove(binaryPath + ".dgst")
	if sigs, err := filepath.Glob(binaryPath + ".dgst.sig.*"); err == nil {
		for _, sig := range sigs {
			_ = os.Remove(sig)
		}
	}
}
