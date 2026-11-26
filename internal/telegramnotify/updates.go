package telegramnotify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/actions"
	agentconfig "github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/download"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
	"github.com/quilscan-com/quilscan-agent/internal/release"
)

const (
	updateNotificationTick = 30 * time.Second
	agentVersionTick       = time.Minute
	maxUpdateReceipts      = 64
)

const updateVersionSuffix = `(?:[-+][0-9A-Za-z][0-9A-Za-z.-]*)?$`

var (
	agentVersionPattern  = regexp.MustCompile(`^v?[0-9]+(?:\.[0-9]+){2,3}` + updateVersionSuffix)
	updateVersionPattern = regexp.MustCompile(`^v?[0-9]+(?:\.[0-9]+){2,4}` + updateVersionSuffix)
)

// updateReceipts records recent target and event identities in the existing
// Telegram configuration, so reconnects and restarts do not repeat reminders.
type updateReceipts string

func (r updateReceipts) with(key string) (updateReceipts, bool) {
	digest := sha256.Sum256([]byte(key))
	id := hex.EncodeToString(digest[:])
	ids := strings.Fields(string(r))
	if slices.Contains(ids, id) {
		return r, false
	}
	ids = append(ids, id)
	if len(ids) > maxUpdateReceipts {
		ids = ids[len(ids)-maxUpdateReceipts:]
	}
	return updateReceipts(strings.Join(ids, " ")), true
}

func validUpdateVersion(version string) bool {
	return len(version) <= 64 && updateVersionPattern.MatchString(version)
}

func (c Config) updatesEnabled() bool {
	return c.Enabled && c.AlertUpdates && c.ChatID != 0
}

// Establish the start of the current opt-in window, keeping it across restarts.
func (c *Config) updateNotificationCutoff(wasEnabled bool) bool {
	previous := c.UpdateNotificationsSince
	switch {
	case !c.updatesEnabled():
		c.UpdateNotificationsSince = time.Time{}
	case !wasEnabled || previous.IsZero():
		c.UpdateNotificationsSince = time.Now().UTC()
	}
	return c.UpdateNotificationsSince != previous
}

// RunUpdateNotifications reuses the node status persisted by reconciliation
// and both automatic-update controllers. The Agent version comes from the
// same release directory used by the Agent updater.
func (m *Notifier) RunUpdateNotifications(ctx context.Context, statePath, agentVersion string) {
	client := download.NewClient(8 * time.Second)
	checkTime := time.Now()
	ticker := time.NewTicker(updateNotificationTick)
	defer ticker.Stop()
	var latestAgent string
	var nextVersionCheck time.Time
	for ctx.Err() == nil {
		c := m.configSnapshot()
		if c.updatesEnabled() {
			if !checkTime.Before(nextVersionCheck) {
				latest, err := fetchAgentVersion(ctx, client, actions.DefaultAgentReleaseURLPrefix+"/version")
				if err == nil {
					latestAgent = latest
				}
				nextVersionCheck = checkTime.Add(agentVersionTick)
			}
			state, _ := agentconfig.LoadState(statePath)
			m.sendUpdateNotifications(ctx, state, agentVersion, latestAgent)
		}
		select {
		case <-ctx.Done():
			return
		case checkTime = <-ticker.C:
		}
	}
}

func fetchAgentVersion(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent version HTTP status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return "", err
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(body)), "v")
	if len(body) == 1024 || len(version) > 64 || !agentVersionPattern.MatchString(version) {
		return "", fmt.Errorf("invalid Agent release version")
	}
	return version, nil
}

func updateMessages(c Config, state *agentconfig.State, agentVersion, latestAgent string) (updateReceipts, []string) {
	receipts := c.UpdateReceipts
	var messages []string
	add := func(key, message string) {
		updated, fresh := receipts.with(key)
		if fresh {
			receipts = updated
			messages = append(messages, message)
		}
	}
	if agentVersionPattern.MatchString(latestAgent) && agentVersionPattern.MatchString(agentVersion) &&
		release.VersionNewerThan(latestAgent, agentVersion) {
		add("agent:"+latestAgent, fmt.Sprintf("Quilscan Agent update available: %s → %s.", agentVersion, latestAgent))
	}
	if state == nil || state.ConfigPath == "" {
		return receipts, messages
	}

	switch state.NodeSource {
	case nodemanifest.SourceDev:
		latest := state.LatestDevNodeVersion
		current := state.InstalledNodeVersion
		if validUpdateVersion(latest) && validUpdateVersion(current) &&
			(current != latest || state.NodeBuildNumber != state.LatestDevNodeBuildNumber ||
				!strings.EqualFold(state.NodeBinarySHA256, state.LatestDevNodeSHA256)) {
			target := fmt.Sprintf("dev:%s:%d:%s", latest, state.LatestDevNodeBuildNumber, strings.ToLower(state.LatestDevNodeSHA256))
			label := latest
			if current == latest {
				label = fmt.Sprintf("%s (build %d)", latest, state.LatestDevNodeBuildNumber)
			}
			add(target, fmt.Sprintf("Dev Node update available: %s → %s.", current, label))
		}
	case nodemanifest.SourceReleases:
		latest := state.LatestReleasesNodeVersion
		current := actions.CurrentReleasesNodeVersion(state)
		if validUpdateVersion(latest) && validUpdateVersion(current) && release.VersionNewerThan(latest, current) {
			add("releases:"+latest, fmt.Sprintf("Releases Node update available: %s → %s.", current, latest))
		}
	}

	addSuccess := func(source, id, result, from, target string, completedAt time.Time) {
		if result == "success" && id != "" && len(id) <= 128 &&
			!c.UpdateNotificationsSince.IsZero() && !completedAt.Before(c.UpdateNotificationsSince) &&
			validUpdateVersion(from) && validUpdateVersion(target) {
			add("auto:"+source+":"+id, fmt.Sprintf("%s Node updated automatically: %s → %s.", source, from, target))
		}
	}
	addSuccess("Dev", state.DevNodeAutoUpdateLastEventID, state.DevNodeAutoUpdateLastResult,
		state.DevNodeAutoUpdateLastFromVersion, state.DevNodeAutoUpdateLastTargetVersion, state.DevNodeAutoUpdateLastCompletedAt)
	releases := state.ReleasesNodeAutoUpdate
	addSuccess("Releases", releases.LastEventID, releases.LastResult, releases.LastFromVersion,
		releases.LastTargetVersion, releases.LastCompletedAt)
	return receipts, messages
}

func (m *Notifier) sendUpdateNotifications(ctx context.Context, state *agentconfig.State, agentVersion, latestAgent string) {
	m.mu.Lock()
	if !m.persistUpdateReceiptsLocked() || m.fileError != "" {
		m.mu.Unlock()
		return
	}
	c := m.config
	m.mu.Unlock()
	if !c.updatesEnabled() {
		return
	}
	receipts, messages := updateMessages(c, state, agentVersion, latestAgent)
	if len(messages) == 0 {
		return
	}
	err := m.bot.send(ctx, c, "Quilscan updates\n"+strings.Join(messages, "\n"))
	m.reload()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config.BotToken != c.BotToken || m.config.ChatID != c.ChatID {
		return
	}
	m.recordErrorLocked(err)
	if err == nil {
		m.lastSent = time.Now()
		m.pendingUpdateReceipts = receipts
		m.persistUpdateReceiptsLocked()
	}
}

func (m *Notifier) persistUpdateReceiptsLocked() bool {
	if m.pendingUpdateReceipts == "" {
		return true
	}
	if m.fileError != "" {
		return false
	}
	next := m.config
	next.UpdateReceipts = m.pendingUpdateReceipts
	if err := m.saveLocked(next); err != nil {
		m.recordErrorLocked(err)
		return false
	}
	m.pendingUpdateReceipts = ""
	return true
}
