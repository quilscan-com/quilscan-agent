// Package release downloads the Quilibrium release bundle for the current
// platform. Release URL is a hardcoded const so auditors can grep-verify.
package release

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/download"
)

// ReleaseBaseURL is hardcoded for supply-chain auditability.
const ReleaseBaseURL = "https://releases.quilibrium.com/"
const QClientVersionManifest = "qclient-version.json"

// DownloadTimeout bounds each release artifact fetch. A stalled CDN/socket
// should fail the current install/update attempt instead of leaving the agent
// command in an unbounded downloading state.
const DownloadTimeout = 20 * time.Minute

var downloadHTTPClient = download.NewClient(DownloadTimeout)

// DetectPlatform maps Go runtime GOOS/GOARCH to the release file suffix.
// Supported: linux-amd64, linux-arm64, darwin-arm64. Unknown combos return empty.
func DetectPlatform(goos, goarch string) string {
	switch goos {
	case "linux":
		if goarch == "amd64" || goarch == "arm64" {
			return "linux-" + goarch
		}
	case "darwin":
		if goarch == "arm64" {
			return "darwin-arm64"
		}
	}
	return ""
}

// BaseName returns the versioned Quilibrium node artifact prefix.
func BaseName(version, platform string) string {
	return ArtifactBaseName("node", version, platform)
}

func ArtifactBaseName(prefix, version, platform string) string {
	return fmt.Sprintf("%s-%s-%s", prefix, version, platform)
}

// FilesForManifest returns the release files for a version + platform from the
// official /release manifest. Signature suffixes are intentionally discovered
// dynamically because the signer index set can change between node releases.
func FilesForManifest(raw, version, platform string) ([]string, error) {
	return FilesForPrefixManifest(raw, "node", version, platform)
}

func FilesForPrefixManifest(raw, prefix, version, platform string) ([]string, error) {
	base := ArtifactBaseName(prefix, version, platform)
	binary := false
	digest := false
	sigSeen := map[string]bool{}
	sigPrefix := base + ".dgst.sig."
	sigPattern := regexp.MustCompile("^" + regexp.QuoteMeta(sigPrefix) + `[0-9]+$`)

	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		name := releaseFileName(sc.Text())
		switch {
		case name == base:
			binary = true
		case name == base+".dgst":
			digest = true
		case sigPattern.MatchString(name):
			sigSeen[name] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !binary {
		return nil, fmt.Errorf("release manifest missing %s", base)
	}
	if !digest {
		return nil, fmt.Errorf("release manifest missing %s.dgst", base)
	}
	if len(sigSeen) == 0 {
		return nil, fmt.Errorf("release manifest missing %s*. signatures", sigPrefix)
	}
	sigs := make([]string, 0, len(sigSeen))
	for name := range sigSeen {
		sigs = append(sigs, name)
	}
	sort.Slice(sigs, func(i, j int) bool {
		return signatureSuffixLess(sigs[i], sigs[j], sigPrefix)
	})
	files := []string{base, base + ".dgst"}
	files = append(files, sigs...)
	return files, nil
}

func releaseFileName(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	line = strings.Trim(line, `"'`)
	if i := strings.IndexByte(line, '?'); i >= 0 {
		line = line[:i]
	}
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	return filepath.Base(line)
}

func signatureSuffixLess(a, b, prefix string) bool {
	ai := strings.TrimPrefix(a, prefix)
	bi := strings.TrimPrefix(b, prefix)
	if len(ai) != len(bi) {
		return len(ai) < len(bi)
	}
	return ai < bi
}

// FilesFor returns the minimum fixed artifacts for a version + platform. The
// complete bundle, including dynamic .dgst.sig.N files, must come from
// FilesForManifest.
func FilesFor(version, platform string) []string {
	base := BaseName(version, platform)
	return []string{base, base + ".dgst"}
}

// DownloadRelease fetches the official manifest, discovers all signature
// sidecars for the requested bundle, then downloads the exact listed files.
func DownloadRelease(baseURL, version, platform, destDir string) error {
	manifestURL := strings.TrimRight(baseURL, "/") + "/release"
	raw, err := fetchText(manifestURL)
	if err != nil {
		return fmt.Errorf("fetch release manifest: %w", err)
	}
	names, err := FilesForManifest(raw, version, platform)
	if err != nil {
		return err
	}
	return DownloadAll(baseURL, names, destDir)
}

func DownloadQClientLatest(baseURL, platform, destDir string) (string, error) {
	return DownloadQClientLatestFromVersionManifest(baseURL, platform, destDir)
}

func FetchQClientLatestVersion(baseURL, platform string) (string, error) {
	manifestURL := strings.TrimRight(baseURL, "/") + "/" + QClientVersionManifest
	raw, err := fetchText(manifestURL)
	if err != nil {
		return "", fmt.Errorf("fetch qclient version manifest: %w", err)
	}
	version := LatestVersionForQClientVersionManifest(raw, platform)
	if version == "" {
		return "", fmt.Errorf("qclient version manifest missing qclient for %s", platform)
	}
	return version, nil
}

type QClientVersionInfo struct {
	Schema      int                  `json:"schema"`
	Channel     string               `json:"channel"`
	Version     string               `json:"version"`
	GeneratedAt string               `json:"generated_at"`
	BaseURL     string               `json:"base_url"`
	Manifest    string               `json:"manifest"`
	Files       []QClientVersionFile `json:"files"`
}

type QClientVersionFile struct {
	Platform      string `json:"platform"`
	Binary        string `json:"binary"`
	Digest        string `json:"digest"`
	Signature     string `json:"signature"`
	SignatureType string `json:"signature_type"`
	SHA3256       string `json:"sha3_256"`
}

func DownloadQClientLatestFromVersionManifest(baseURL, platform, destDir string) (string, error) {
	manifestURL := strings.TrimRight(baseURL, "/") + "/" + QClientVersionManifest
	raw, err := fetchText(manifestURL)
	if err != nil {
		return "", fmt.Errorf("fetch qclient version manifest: %w", err)
	}
	manifest, target, err := ParseQClientVersionManifest(raw, platform)
	if err != nil {
		return "", err
	}
	names, err := qclientVersionFileNames(target)
	if err != nil {
		return "", err
	}
	if err := DownloadAll(baseURL, names, destDir); err != nil {
		return "", err
	}
	return manifest.Version, nil
}

func LatestVersionForQClientVersionManifest(raw, platform string) string {
	manifest, _, err := ParseQClientVersionManifest(raw, platform)
	if err != nil {
		return ""
	}
	return manifest.Version
}

func ParseQClientVersionManifest(raw, platform string) (*QClientVersionInfo, *QClientVersionFile, error) {
	var manifest QClientVersionInfo
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return nil, nil, fmt.Errorf("parse qclient version manifest: %w", err)
	}
	if manifest.Version == "" {
		return nil, nil, fmt.Errorf("qclient version manifest missing version")
	}
	var target *QClientVersionFile
	for i := range manifest.Files {
		if manifest.Files[i].Platform == platform {
			target = &manifest.Files[i]
			break
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("qclient version manifest missing qclient for %s", platform)
	}
	if target.Binary == "" {
		return nil, nil, fmt.Errorf("qclient version manifest missing binary for %s", platform)
	}
	if target.Signature == "" {
		return nil, nil, fmt.Errorf("qclient version manifest missing signature for %s", platform)
	}
	return &manifest, target, nil
}

func qclientVersionFileNames(file *QClientVersionFile) ([]string, error) {
	var names []string
	for _, raw := range []string{file.Binary, file.Digest, file.Signature} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		name := releaseFileName(raw)
		if name == "" || name != raw {
			return nil, fmt.Errorf("unsafe qclient manifest file name %q", raw)
		}
		names = append(names, name)
	}
	return names, nil
}

func FetchLatestNodeVersion(baseURL, platform string) (string, error) {
	manifestURL := strings.TrimRight(baseURL, "/") + "/release"
	raw, err := fetchText(manifestURL)
	if err != nil {
		return "", err
	}
	version := LatestVersionForPrefix(raw, "node", platform)
	if version == "" {
		return "", fmt.Errorf("release manifest missing node for %s", platform)
	}
	return version, nil
}

// DownloadAll fetches every name from baseURL into destDir. Returns the first
// error encountered and stops.
func DownloadAll(baseURL string, names []string, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for _, n := range names {
		url := strings.TrimRight(baseURL, "/") + "/" + n
		if err := downloadOne(url, filepath.Join(destDir, n)); err != nil {
			return fmt.Errorf("download %s: %w", n, err)
		}
	}
	return nil
}

func fetchText(url string) (string, error) {
	resp, err := downloadHTTPClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func downloadOne(url, dst string) error {
	resp, err := downloadHTTPClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func LatestVersionForPrefix(raw, prefix, platform string) string {
	best := ""
	re := regexp.MustCompile("^" + regexp.QuoteMeta(prefix+"-") + `([0-9]+(?:\.[0-9]+){2,3})-` + regexp.QuoteMeta(platform) + `(?:$|\.)`)
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		name := releaseFileName(sc.Text())
		m := re.FindStringSubmatch(name)
		if len(m) != 2 {
			continue
		}
		if best == "" || compareDottedVersions(m[1], best) > 0 {
			best = m[1]
		}
	}
	return best
}

func compareDottedVersions(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(ap) {
			fmt.Sscanf(ap[i], "%d", &ai)
		}
		if i < len(bp) {
			fmt.Sscanf(bp[i], "%d", &bi)
		}
		if ai > bi {
			return 1
		}
		if ai < bi {
			return -1
		}
	}
	return 0
}
