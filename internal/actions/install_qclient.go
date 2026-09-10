package actions

import (
	"bytes"
	"crypto/sha3"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/release"
)

type QClientDownloader interface {
	DownloadLatest(platform, destDir string) (string, error)
	LatestVersion(platform string) (string, error)
}

type QClientInstallDeps struct {
	BinaryPath string
	Platform   string

	Downloader             QClientDownloader
	SigningPublicKeyBase64 string
	LoadState              func() (*config.State, error)
	UpdateState            func(func(*config.State) error) (*config.State, error)
	EmitRaw                func(map[string]interface{})
	PatchNodeStatus        func(map[string]interface{})
	transactionOps         fileTransactionOps
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

func EnsureQClientCurrent(d QClientInstallDeps) (string, error) {
	if d.Downloader == nil {
		d.Downloader = QClientReleaseDownloader{}
	}
	if d.BinaryPath == "" {
		return "", fmt.Errorf("missing qclient binary path")
	}
	if d.Platform == "" {
		return "", fmt.Errorf("missing platform")
	}

	var state *config.State
	if d.LoadState != nil {
		state, _ = d.LoadState()
	}
	if state == nil {
		state = &config.State{}
	}
	current := strings.TrimSpace(state.QClientVersion)
	managed := qclientReleaseSidecarsValid(d.BinaryPath, d.SigningPublicKeyBase64)
	if !qclientFileExists(d.BinaryPath) || current == "" || !managed {
		return installQClient(d, nil)
	}
	latest, err := d.Downloader.LatestVersion(d.Platform)
	if err != nil {
		return current, err
	}
	if release.VersionNewerThan(latest, current) {
		return installQClient(d, nil)
	}
	return current, nil
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

	versionedBinary := filepath.Join(releaseDir, release.ArtifactBaseName("qclient", version, d.Platform))
	if progress != nil {
		progress("verifying_signature", 0.55)
	}
	if err := verifyQClientReleaseSignature(versionedBinary, d.SigningPublicKeyBase64); err != nil {
		return "", err
	}
	if err := verifyQClientReleaseDigest(versionedBinary); err != nil {
		return "", err
	}

	if progress != nil {
		progress("installing_binary", 0.75)
	}
	tx, err := prepareQClientTransaction(versionedBinary, d.BinaryPath, d.transactionOps)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Finalize() }()
	if err := tx.Commit(); err != nil {
		return "", withRollbackOutcome(err, tx.Rollback)
	}

	installedAt := time.Now().UTC()
	if d.UpdateState != nil {
		if _, err := d.UpdateState(func(state *config.State) error {
			state.QClientBinaryPath = d.BinaryPath
			state.QClientVersion = version
			state.QClientInstalledAt = installedAt
			return nil
		}); err != nil {
			return "", withRollbackOutcome(err, tx.Rollback)
		}
	}

	if d.PatchNodeStatus != nil {
		d.PatchNodeStatus(map[string]interface{}{
			"has_qclient":               true,
			"qclient_binary_path":       d.BinaryPath,
			"qclient_version":           version,
			"latest_qclient_version":    version,
			"qclient_update_available":  false,
			"qclient_version_polled_at": installedAt.Format(time.RFC3339),
		})
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

type QClientReleaseDownloader struct {
	BaseURL string
}

func (d QClientReleaseDownloader) DownloadLatest(platform, destDir string) (string, error) {
	baseURL := strings.TrimSpace(d.BaseURL)
	if baseURL == "" {
		return "", fmt.Errorf("missing qclient release url")
	}
	return release.DownloadQClientLatest(baseURL, platform, destDir)
}

func (d QClientReleaseDownloader) LatestVersion(platform string) (string, error) {
	baseURL := strings.TrimSpace(d.BaseURL)
	if baseURL == "" {
		return "", fmt.Errorf("missing qclient release url")
	}
	return release.FetchQClientLatestVersion(baseURL, platform)
}

func qclientReleaseSidecarsValid(binaryPath, publicKeyBase64 string) bool {
	return verifyQClientReleaseSignature(binaryPath, publicKeyBase64) == nil &&
		verifyQClientReleaseDigest(binaryPath) == nil
}

func verifyQClientReleaseSignature(versionedBinary, publicKeyBase64 string) error {
	signaturePath := versionedBinary + ".sig"
	if _, err := os.Stat(signaturePath); err != nil {
		return fmt.Errorf("missing qclient signature %s: %w", filepath.Base(signaturePath), err)
	}
	return verifyEd25519BinarySignature(versionedBinary, signaturePath, publicKeyBase64, "qclient")
}

func verifyQClientReleaseDigest(versionedBinary string) error {
	digestPath := versionedBinary + ".dgst"
	raw, err := os.ReadFile(digestPath)
	if err != nil {
		return fmt.Errorf("read qclient digest: %w", err)
	}
	fields := bytes.Fields(raw)
	if len(fields) == 0 {
		return fmt.Errorf("invalid qclient digest format")
	}
	want := string(fields[len(fields)-1])
	want = strings.TrimSpace(want)
	if len(want) < 64 {
		return fmt.Errorf("invalid qclient digest format")
	}
	want = want[:64]
	binary, err := os.ReadFile(versionedBinary)
	if err != nil {
		return fmt.Errorf("read qclient binary: %w", err)
	}
	got := sha3.Sum256(binary)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return fmt.Errorf("qclient digest mismatch")
	}
	return nil
}

func qclientFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
