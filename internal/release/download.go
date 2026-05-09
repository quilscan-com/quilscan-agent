// Package release downloads the Quilibrium release bundle for the current
// platform. Release URL is a hardcoded const so auditors can grep-verify.
package release

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ReleaseBaseURL is hardcoded for supply-chain auditability.
const ReleaseBaseURL = "https://releases.quilibrium.com/"

// DownloadTimeout bounds each release artifact fetch. A stalled CDN/socket
// should fail the current install/update attempt instead of leaving the agent
// command in an unbounded downloading state.
const DownloadTimeout = 5 * time.Minute

var downloadHTTPClient = &http.Client{Timeout: DownloadTimeout}

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
	return fmt.Sprintf("node-%s-%s", version, platform)
}

// FilesForManifest returns the release files for a version + platform from the
// official /release manifest. Signature suffixes are intentionally discovered
// dynamically because the signer index set can change between node releases.
func FilesForManifest(raw, version, platform string) ([]string, error) {
	base := BaseName(version, platform)
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
