package actions

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
)

const devNodeSigningPublicKeyBase64 = "CdZQuKKKObFcsQY61nhp4vZw7H6jBGBnlav7NuIh53k="

type DevNodeInstallResult struct {
	Version           string
	BaseVersion       string
	BuildNumber       int
	SHA256            string
	URL               string
	ManifestURL       string
	SignatureVerified bool
	CheckedAt         time.Time
}

type DevNodeInstaller interface {
	InstallLatest(platform, binaryPath, manifestURL string) (DevNodeInstallResult, error)
}

type ManifestDevNodeInstaller struct {
	publicKeyBase64 string
	progress        func(step string, progress float64)
}

func (i ManifestDevNodeInstaller) InstallLatest(platform, binaryPath, manifestURL string) (DevNodeInstallResult, error) {
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
	if strings.TrimSpace(artifact.SignatureURL) == "" {
		return DevNodeInstallResult{}, fmt.Errorf("latest dev artifact missing signature_url for %s", platform)
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
	binary, err := os.ReadFile(tmpBinary)
	if err != nil {
		return DevNodeInstallResult{}, fmt.Errorf("read dev node: %w", err)
	}
	tmpSignature := filepath.Join(releaseDir, "dev-node-"+platform+".sig")
	if err := nodemanifest.DownloadFile(artifact.SignatureURL, tmpSignature); err != nil {
		return DevNodeInstallResult{}, fmt.Errorf("download dev node signature: %w", err)
	}
	signatureRaw, err := os.ReadFile(tmpSignature)
	if err != nil {
		return DevNodeInstallResult{}, fmt.Errorf("read dev node signature: %w", err)
	}
	i.emitProgress("verifying_signature", 0.50)
	if err := i.verifySignature(binary, signatureRaw); err != nil {
		return DevNodeInstallResult{}, err
	}
	gotSHA, err := nodemanifest.HashFile(tmpBinary)
	if err != nil {
		return DevNodeInstallResult{}, fmt.Errorf("hash dev node: %w", err)
	}
	if !strings.EqualFold(gotSHA, artifact.SHA256) {
		return DevNodeInstallResult{}, fmt.Errorf("dev node sha mismatch: got %s want %s", gotSHA, artifact.SHA256)
	}
	i.emitProgress("installing_binary", 0.65)
	if err := installDevNodeBinary(tmpBinary, binaryPath); err != nil {
		return DevNodeInstallResult{}, err
	}
	return DevNodeInstallResult{
		Version:           latest.Version,
		BaseVersion:       latest.BaseVersion,
		BuildNumber:       latest.BuildNumber,
		SHA256:            gotSHA,
		URL:               artifact.URL,
		ManifestURL:       manifestURL,
		SignatureVerified: true,
		CheckedAt:         time.Now().UTC(),
	}, nil
}

func (i ManifestDevNodeInstaller) emitProgress(step string, progress float64) {
	if i.progress != nil {
		i.progress(step, progress)
	}
}

func (i ManifestDevNodeInstaller) verifySignature(binary, signatureRaw []byte) error {
	publicKeyText := strings.TrimSpace(i.publicKeyBase64)
	if publicKeyText == "" {
		publicKeyText = devNodeSigningPublicKeyBase64
	}
	publicKey, err := base64.StdEncoding.DecodeString(publicKeyText)
	if err != nil {
		return fmt.Errorf("parse dev node public key: %w", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("dev node public key has %d bytes, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signatureRaw)))
	if err != nil {
		return fmt.Errorf("parse dev node signature: %w", err)
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("dev node signature has %d bytes, want %d", len(signature), ed25519.SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), binary, signature) {
		return fmt.Errorf("dev node signature verification failed")
	}
	return nil
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
