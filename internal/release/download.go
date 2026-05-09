// Package release downloads the Quilibrium release bundle for the current
// platform. Release URL is a hardcoded const so auditors can grep-verify.
package release

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// Observed signer indices as of 2026-04. Agent downloads whichever are
// published; the node itself verifies threshold at runtime.
var signerIndices = []int{1, 2, 10, 13, 14, 16, 17}

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

// FilesFor returns the file list for a version + platform: binary, .dgst, and
// one .dgst.sig.N per known signer index.
func FilesFor(version, platform string) []string {
	base := fmt.Sprintf("node-%s-%s", version, platform)
	names := []string{base, base + ".dgst"}
	for _, i := range signerIndices {
		names = append(names, fmt.Sprintf("%s.dgst.sig.%d", base, i))
	}
	return names
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
