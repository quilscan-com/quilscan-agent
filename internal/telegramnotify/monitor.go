package telegramnotify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/actions"
	"github.com/quilscan-com/quilscan-agent/internal/diskinspect"
)

const (
	nodeGrace            = 30 * time.Second
	alertRetry           = time.Minute
	thresholdRecovery    = 5 * time.Minute
	maxRecoverySampleGap = 2 * time.Minute // Idle metrics normally arrive every minute.
)

// Observe keeps one copy of the metrics needed for local alerts. Telegram
// requests run in Run, so they cannot pause the Agent's metrics collector.
func (m *Notifier) Observe(frame map[string]interface{}) {
	next := sample{at: time.Now()}
	next.memory, next.hasMemory = frame["mem"].(float64)
	next.nodeRunning, next.nodeKnown = frame["node_running"].(bool)
	var hasRx, hasTx bool
	next.rx, hasRx = frame["net_rx_bytes"].(uint64)
	next.tx, hasTx = frame["net_tx_bytes"].(uint64)
	next.hasNetwork = hasRx && hasTx
	m.mu.Lock()
	m.latest = next
	m.mu.Unlock()
}

// NodeOperation distinguishes operations known to the Agent from unexpected
// exits. This wraps the shared update handler for both manual and auto updates.
func (m *Notifier) NodeOperation(action string, handler actions.Handler) actions.Handler {
	return func(c actions.Command, emit actions.Emitter) error {
		m.mu.Lock()
		m.nodeBusy++
		m.nodeDownAt = time.Time{}
		m.nodeRestarted = false
		m.mu.Unlock()
		err := handler(c, emit)
		stopped := err == nil && (action == "stop" || action == "cleanup_residue" || action == "delete_node_store")
		if err != nil && m.nodeProcess != nil && (action == "cleanup_residue" || action == "delete_node_store") {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			stopped = m.nodeProcess(ctx).PID == 0
			cancel()
		}
		m.mu.Lock()
		m.nodeBusy--
		m.nodeQuietTo = time.Now().Add(nodeGrace)
		m.nodePID = 0
		if stopped {
			m.nodeWanted = false
			delete(m.alerted, "node")
		} else if err == nil {
			switch action {
			case "start", "restart_node", "install", "switch_node_source":
				m.nodeWanted = true
			}
		}
		m.mu.Unlock()
		return err
	}
}

type alert struct {
	key  string
	text string
}

func (m *Notifier) evaluate(ctx context.Context, recordedNodeConfig func() string) {
	m.mu.Lock()
	next := m.latest
	if next.at.IsZero() || !next.at.After(m.checkedAt) {
		m.mu.Unlock()
		return
	}
	if !m.checkedAt.IsZero() && next.at.Sub(m.checkedAt) > maxRecoverySampleGap {
		clear(m.recoverySince)
	}
	m.checkedAt = next.at
	lastConfig := m.nodeConfigFile
	nodeBusy, nodeQuietTo := m.nodeBusy, m.nodeQuietTo
	m.mu.Unlock()

	var disk diskinspect.Usage
	var processConfig string
	diskReady := false
	usingRecordedConfig := false
	if m.nodeProcess != nil {
		diskCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		process := m.nodeProcess(diskCtx)
		next.nodePID = process.PID
		next.nodeRunning = next.nodeRunning || process.PID > 0
		configFile := process.ConfigFile
		if process.PID > 0 {
			processConfig = configFile
		}
		if configFile == "" && next.nodeKnown && !next.nodeRunning && nodeBusy == 0 && recordedNodeConfig != nil {
			recorded := recordedNodeConfig()
			if recorded != "" && (lastConfig == "" || lastConfig == recorded) {
				configFile = recorded
				usingRecordedConfig = true
			}
		}
		disk, diskReady = nodeConfigDisk(diskCtx, configFile)
		cancel()
	}

	m.mu.Lock()
	if processConfig != "" {
		m.nodeConfigFile = processConfig
	}
	// A node operation may have changed the installation while the disk was sampled.
	if usingRecordedConfig && (m.nodeBusy != 0 || m.nodeQuietTo != nodeQuietTo) {
		diskReady = false
	}
	if diskReady && m.disk.Mountpoint != disk.Mountpoint {
		m.resetThresholdAlertLocked("disk")
	}
	if diskReady {
		m.disk = disk
	}
	m.diskReady = diskReady
	settings := m.config
	pending := m.alertsLocked(settings, next)
	now := time.Now()
	canSend := settings.Enabled && settings.ChatID != 0 && len(pending) > 0 && !now.Before(m.retryAt)
	if canSend {
		m.retryAt = now.Add(alertRetry)
	}
	m.mu.Unlock()
	if !canSend {
		return
	}

	lines := []string{"Quilscan Node alert"}
	for _, warning := range pending {
		lines = append(lines, warning.text)
	}
	err := m.bot.send(ctx, settings, strings.Join(lines, "\n"))
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.config
	current.UpdateReceipts = settings.UpdateReceipts
	if current != settings {
		return
	}
	m.recordErrorLocked(err)
	if err == nil {
		for _, warning := range pending {
			m.alerted[warning.key] = true
		}
		m.lastSent = time.Now()
		m.retryAt = time.Time{}
	}
}

func (m *Notifier) resetThresholdAlertLocked(key string) {
	delete(m.alerted, key)
	delete(m.recoverySince, key)
}

func (m *Notifier) alertsLocked(c Config, next sample) []alert {
	var pending []alert
	add := func(key, message string) {
		if !m.alerted[key] {
			pending = append(pending, alert{key: key, text: message})
		}
	}
	checkThreshold := func(key string, available bool, value, threshold float64, message string) {
		switch {
		case !available || threshold <= 0:
			delete(m.recoverySince, key)
		case value > threshold:
			delete(m.recoverySince, key)
			add(key, message)
		case m.alerted[key]:
			since := m.recoverySince[key]
			if since.IsZero() {
				m.recoverySince[key] = next.at
			} else if next.at.Sub(since) >= thresholdRecovery {
				m.resetThresholdAlertLocked(key)
			}
		}
	}
	diskUsed := m.disk.UsedPercent
	checkThreshold("disk", m.diskReady, diskUsed, c.DiskThreshold,
		fmt.Sprintf("Node config disk %s is %.1f%% full (threshold %g%%).", m.disk.Mountpoint, diskUsed, c.DiskThreshold))
	checkThreshold("memory", next.hasMemory, next.memory, c.MemoryThreshold,
		fmt.Sprintf("Host memory is %.1f%% in use (threshold %g%%).", next.memory, c.MemoryThreshold))

	var down, up float64
	var networkReady bool
	if next.hasNetwork {
		down, up, networkReady = m.traffic.update(next.at, next.rx, next.tx)
	}
	checkThreshold("download", networkReady, down, c.DownThreshold,
		fmt.Sprintf("10-minute average download is %.2f Mbit/s (threshold %g Mbit/s).", down, c.DownThreshold))
	checkThreshold("upload", networkReady, up, c.UpThreshold,
		fmt.Sprintf("10-minute average upload is %.2f Mbit/s (threshold %g Mbit/s).", up, c.UpThreshold))

	if next.nodeKnown && m.nodeBusy == 0 && !next.at.Before(m.nodeQuietTo) {
		if next.nodeRunning {
			restarted := next.nodePID > 0 && m.nodePID > 0 && next.nodePID != m.nodePID
			if next.nodePID > 0 {
				m.nodePID = next.nodePID
			}
			if restarted {
				m.nodeRestarted = m.nodeWanted
			} else if m.alerted["node"] {
				m.nodeRestarted = false
				delete(m.alerted, "node")
			}
			m.nodeWanted = true
			m.nodeDownAt = time.Time{}
			if c.AlertNodeStop && m.nodeRestarted {
				add("node", "The Node restarted unexpectedly.")
			}
		} else if m.nodeWanted {
			if m.nodeDownAt.IsZero() {
				m.nodeDownAt = next.at
			}
			if c.AlertNodeStop && next.at.Sub(m.nodeDownAt) >= nodeGrace {
				add("node", "The Node stopped unexpectedly.")
			}
		}
	}
	return pending
}
