package nodemanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const DefaultURL = "https://releases.quilscan.com/node-version.json"
const DefaultOfficialArtifactsURL = "https://api.quilscan.com/api/node/official-artifacts"

const (
	SourceReleases = "releases"
	SourceDev      = "dev"
	SourceUnknown  = "unknown"
)

const fetchLimit = 512 * 1024

type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	UpdatedAt     string    `json:"updated_at"`
	Channels      Channels  `json:"channels"`
	SourceURL     string    `json:"-"`
	FetchedAt     time.Time `json:"-"`
}

type Channels struct {
	Releases Channel `json:"releases"`
	Dev      Channel `json:"dev"`
}

type Channel struct {
	Source   string    `json:"source"`
	Latest   string    `json:"latest"`
	Versions []Version `json:"versions"`
}

type Version struct {
	Version     string              `json:"version"`
	BaseVersion string              `json:"base_version"`
	BuildNumber int                 `json:"build_number"`
	Platforms   map[string]Artifact `json:"platforms"`
}

type Artifact struct {
	SHA256        string   `json:"sha256"`
	URL           string   `json:"url"`
	DigestURL     string   `json:"digest_url"`
	SignatureURL  string   `json:"signature_url"`
	SignatureURLs []string `json:"signature_urls"`
}

type OfficialArtifacts struct {
	Source    string             `json:"source"`
	Artifacts []OfficialArtifact `json:"artifacts"`
}

type OfficialArtifact struct {
	Version   string `json:"version"`
	Platform  string `json:"platform"`
	SHA256    string `json:"sha256"`
	SourceURL string `json:"source_url"`
}

type Match struct {
	Source      string
	Version     string
	BaseVersion string
	BuildNumber int
	Platform    string
	SHA256      string
	URL         string
}

func Parse(r io.Reader) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(io.LimitReader(r, fetchLimit))
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func Fetch(url string) (*Manifest, error) {
	if strings.TrimSpace(url) == "" {
		url = DefaultURL
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	m, err := Parse(resp.Body)
	if err != nil {
		return nil, err
	}
	m.SourceURL = url
	m.FetchedAt = time.Now().UTC()
	return m, nil
}

func FetchOfficialArtifacts(rawURL string) (*OfficialArtifacts, error) {
	if strings.TrimSpace(rawURL) == "" {
		rawURL = DefaultOfficialArtifactsURL
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var out OfficialArtifacts
	dec := json.NewDecoder(io.LimitReader(resp.Body, fetchLimit))
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func OfficialArtifactsURLFromBackend(backendURL string) string {
	u, err := url.Parse(strings.TrimSpace(backendURL))
	if err != nil || u.Host == "" {
		return DefaultOfficialArtifactsURL
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	case "http", "https":
	default:
		u.Scheme = "https"
	}
	u.Path = "/api/node/official-artifacts"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (m *Manifest) Match(platform, sha string) (Match, bool) {
	platform = strings.TrimSpace(platform)
	sha = normalizeSHA(sha)
	if m == nil || platform == "" || sha == "" {
		return Match{}, false
	}
	if match, ok := matchChannel(SourceReleases, m.Channels.Releases, platform, sha); ok {
		return match, true
	}
	if match, ok := matchChannel(SourceDev, m.Channels.Dev, platform, sha); ok {
		return match, true
	}
	return Match{}, false
}

func (m *Manifest) MatchDev(platform, sha string) (Match, bool) {
	platform = strings.TrimSpace(platform)
	sha = normalizeSHA(sha)
	if m == nil || platform == "" || sha == "" {
		return Match{}, false
	}
	latest := strings.TrimSpace(m.Channels.Dev.Latest)
	if latest != "" {
		for _, version := range m.Channels.Dev.Versions {
			if strings.TrimSpace(version.Version) != latest {
				continue
			}
			artifact, ok := version.Platforms[platform]
			if !ok {
				break
			}
			artifactSHA := normalizeSHA(artifact.SHA256)
			if artifactSHA == sha {
				return Match{
					Source:      SourceDev,
					Version:     strings.TrimSpace(version.Version),
					BaseVersion: strings.TrimSpace(version.BaseVersion),
					BuildNumber: version.BuildNumber,
					Platform:    platform,
					SHA256:      artifactSHA,
					URL:         strings.TrimSpace(artifact.URL),
				}, true
			}
			break
		}
	}
	return matchChannel(SourceDev, m.Channels.Dev, platform, sha)
}

func (a *OfficialArtifacts) Match(platform, sha string) (Match, bool) {
	platform = strings.TrimSpace(platform)
	sha = normalizeSHA(sha)
	if a == nil || platform == "" || sha == "" {
		return Match{}, false
	}
	for _, artifact := range a.Artifacts {
		if strings.TrimSpace(artifact.Platform) != platform {
			continue
		}
		artifactSHA := normalizeSHA(artifact.SHA256)
		if artifactSHA == "" || artifactSHA != sha {
			continue
		}
		version := strings.TrimSpace(artifact.Version)
		return Match{
			Source:      SourceReleases,
			Version:     version,
			BaseVersion: version,
			Platform:    platform,
			SHA256:      artifactSHA,
			URL:         strings.TrimSpace(artifact.SourceURL),
		}, true
	}
	return Match{}, false
}

func (m *Manifest) LatestDev(platform string) (Version, Artifact, bool) {
	if m == nil {
		return Version{}, Artifact{}, false
	}
	platform = strings.TrimSpace(platform)
	latest := strings.TrimSpace(m.Channels.Dev.Latest)
	if platform == "" || latest == "" {
		return Version{}, Artifact{}, false
	}
	for _, version := range m.Channels.Dev.Versions {
		if strings.TrimSpace(version.Version) != latest {
			continue
		}
		artifact, ok := version.Platforms[platform]
		if !ok || normalizeSHA(artifact.SHA256) == "" {
			return Version{}, Artifact{}, false
		}
		artifact.SHA256 = normalizeSHA(artifact.SHA256)
		return version, artifact, true
	}
	return Version{}, Artifact{}, false
}

func (m *Manifest) DevUpdate(current Match, platform string) (bool, Version) {
	latest, artifact, ok := m.LatestDev(platform)
	if !ok {
		return false, Version{}
	}
	if current.Source != SourceDev {
		return false, latest
	}
	if strings.TrimSpace(current.Version) != strings.TrimSpace(latest.Version) {
		return true, latest
	}
	if current.BuildNumber != latest.BuildNumber {
		return true, latest
	}
	return normalizeSHA(current.SHA256) != normalizeSHA(artifact.SHA256), latest
}

func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func DownloadFile(url, dst string) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return fmt.Errorf("missing url")
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
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

func matchChannel(source string, channel Channel, platform, sha string) (Match, bool) {
	for _, version := range channel.Versions {
		artifact, ok := version.Platforms[platform]
		if !ok {
			continue
		}
		artifactSHA := normalizeSHA(artifact.SHA256)
		if artifactSHA == "" || artifactSHA != sha {
			continue
		}
		return Match{
			Source:      source,
			Version:     strings.TrimSpace(version.Version),
			BaseVersion: strings.TrimSpace(version.BaseVersion),
			BuildNumber: version.BuildNumber,
			Platform:    platform,
			SHA256:      artifactSHA,
			URL:         strings.TrimSpace(artifact.URL),
		}, true
	}
	return Match{}, false
}

func normalizeSHA(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
