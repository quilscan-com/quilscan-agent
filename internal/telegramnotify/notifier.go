// Package telegramnotify sends optional Node alerts directly from the Agent.
package telegramnotify

import (
	"context"
	"errors"
	"math"
	"net/url"
	"sync"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/actions"
	"github.com/quilscan-com/quilscan-agent/internal/diskinspect"
	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
)

type Status struct {
	Enabled        bool    `json:"enabled"`
	Configured     bool    `json:"configured"`
	Bound          bool    `json:"bound"`
	Tested         bool    `json:"tested"`
	Testing        bool    `json:"testing,omitempty"`
	SetupPending   bool    `json:"setup_pending,omitempty"`
	CanCancelSetup bool    `json:"can_cancel_setup"`
	BindingURL     string  `json:"binding_url,omitempty"`
	LastError      string  `json:"last_error,omitempty"`
	LastSentAt     string  `json:"last_sent_at,omitempty"`
	DiskMount      string  `json:"disk_mount,omitempty"`
	DiskReady      bool    `json:"disk_available"`
	DiskUsed       float64 `json:"disk_used_percent"`
	Rules
}

type Notifier struct {
	path        string
	nodeProcess func(context.Context) svcctl.NodeProcess
	onStatus    func(Status)
	bot         botClient

	mu                    sync.Mutex
	config                Config
	loaded                bool
	fileError             string
	lastError             string
	lastSent              time.Time
	testing               bool
	offset                int64
	disk                  diskinspect.Usage
	diskReady             bool
	nodeConfigFile        string
	latest                sample
	checkedAt             time.Time
	traffic               trafficWindow
	alerted               map[string]bool
	recoverySince         map[string]time.Time
	retryAt               time.Time
	nodeBusy              int
	nodeWanted            bool
	nodePID               int
	nodeRestarted         bool
	nodeDownAt            time.Time
	nodeQuietTo           time.Time
	pendingUpdateReceipts updateReceipts

	publishMu  sync.Mutex
	lastStatus *Status
}

func New(path string, nodeProcess func(context.Context) svcctl.NodeProcess, onStatus func(Status)) *Notifier {
	return &Notifier{
		path: path, nodeProcess: nodeProcess, onStatus: onStatus,
		bot: newBotClient(), config: defaultConfig(), alerted: make(map[string]bool),
		recoverySince: make(map[string]time.Time),
	}
}

func (m *Notifier) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.config
	out := Status{
		Enabled: c.Enabled, Configured: c.BotToken != "", Bound: c.ChatID != 0, Tested: c.Tested,
		Testing: m.testing, SetupPending: c.SetupPending,
		CanCancelSetup: c.BotToken != "" && c.ChatID == 0 && m.fileError == "",
		DiskReady:      m.diskReady, DiskMount: m.disk.Mountpoint, Rules: c.Rules, LastError: m.lastError,
	}
	if m.diskReady {
		out.DiskUsed = math.Round(m.disk.UsedPercent*10) / 10
	}
	if m.fileError != "" {
		out.LastError = m.fileError
	}
	if !m.lastSent.IsZero() {
		out.LastSentAt = m.lastSent.UTC().Format(time.RFC3339)
	}
	if c.ChatID == 0 && c.BotUsername != "" && c.PairingCode != "" {
		query := url.Values{"start": {c.PairingCode}}
		out.BindingURL = "https://t.me/" + c.BotUsername + "?" + query.Encode()
	}
	return out
}

func (m *Notifier) publish() {
	if m.onStatus == nil {
		return
	}
	m.publishMu.Lock()
	defer m.publishMu.Unlock()
	status := m.Status()
	if m.lastStatus == nil || *m.lastStatus != status {
		m.lastStatus = &status
		m.onStatus(status)
	}
}

func (m *Notifier) Run(ctx context.Context, recordedNodeConfig func() string) {
	m.reload()
	m.publish()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.reload()
			m.evaluate(ctx, recordedNodeConfig)
			m.syncBinding(ctx)
			m.publish()
		}
	}
}

func (m *Notifier) reload() {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, err := loadConfig(m.path)
	if err != nil {
		m.fileError = "Telegram settings could not be loaded"
		return
	}
	credentialsChanged := m.loaded && next.BotToken != "" && (next.BotToken != m.config.BotToken || next.ChatID != m.config.ChatID)
	if credentialsChanged {
		// A local credential change invalidates the previous test result.
		next.Enabled, next.Tested = false, false
		next.SetupPending = true
		if next.BotToken != m.config.BotToken {
			next.BotUsername, next.PairingCode, next.ChatID = "", "", 0
		}
	}
	cutoffChanged := next.updateNotificationCutoff(!m.loaded || m.config.updatesEnabled())
	if credentialsChanged || cutoffChanged {
		if err := saveConfig(m.path, next); err != nil {
			m.fileError = "Telegram settings could not be saved"
			return
		}
	}
	m.fileError = ""
	if next != m.config {
		m.replaceLocked(next)
	}
	m.loaded = true
}

func (m *Notifier) replaceLocked(next Config) {
	old := m.config
	if old.BotToken != next.BotToken || old.ChatID != next.ChatID {
		m.pendingUpdateReceipts = ""
	}
	if old.Enabled != next.Enabled || old.BotToken != next.BotToken || old.ChatID != next.ChatID {
		clear(m.alerted)
		clear(m.recoverySince)
		m.retryAt = time.Time{}
		m.offset = 0
		m.nodeRestarted = false
	}
	if old.DiskThreshold != next.DiskThreshold {
		m.resetThresholdAlertLocked("disk")
	}
	if old.MemoryThreshold != next.MemoryThreshold {
		m.resetThresholdAlertLocked("memory")
	}
	if old.DownThreshold != next.DownThreshold {
		m.resetThresholdAlertLocked("download")
	}
	if old.UpThreshold != next.UpThreshold {
		m.resetThresholdAlertLocked("upload")
	}
	if old.AlertNodeStop != next.AlertNodeStop {
		delete(m.alerted, "node")
	}
	m.config = next
}

func (m *Notifier) saveLocked(next Config) error {
	next.updateNotificationCutoff(m.config.updatesEnabled())
	if err := saveConfig(m.path, next); err != nil {
		return errors.New("Telegram settings could not be saved")
	}
	m.replaceLocked(next)
	m.loaded = true
	m.fileError, m.lastError = "", ""
	return nil
}

func (m *Notifier) set(ctx context.Context, args map[string]interface{}) error {
	m.reload()
	if cancel, present := args["cancel_setup"]; present {
		if cancel != true || len(args) != 1 {
			return errors.New("invalid Telegram cancellation")
		}
		return m.cancelSetup()
	}
	m.mu.Lock()
	next, err := applyArgs(m.config, args)
	m.mu.Unlock()
	if err != nil {
		return err
	}

	var username, pairing string
	if _, supplied := args["bot_token"]; supplied {
		username, err = m.bot.username(ctx, next.BotToken)
		if err != nil {
			return err
		}
		pairing, err = newPairingCode()
		if err != nil {
			return err
		}
	}

	m.reload()
	m.mu.Lock()
	defer m.mu.Unlock()
	next, err = applyArgs(m.config, args)
	if err != nil {
		return err
	}
	if next.BotToken != m.config.BotToken {
		next.BotUsername, next.PairingCode, next.ChatID = username, pairing, 0
		next.Enabled, next.Tested = false, false
		next.SetupPending = true
	}
	if enabled, _ := args["enabled"].(bool); enabled {
		if !next.Tested {
			return errors.New("send a successful Telegram test message before enabling notifications")
		}
		next.SetupPending = false
	}
	return m.saveLocked(next)
}

func (m *Notifier) cancelSetup() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fileError != "" {
		return errors.New(m.fileError)
	}
	if m.config.ChatID != 0 {
		return errors.New("Telegram binding is already complete")
	}
	if m.config.BotToken == "" {
		return nil
	}
	return m.saveLocked(Config{Rules: m.config.Rules, UpdateReceipts: m.config.UpdateReceipts})
}

func (m *Notifier) configSnapshot() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config
}

func (m *Notifier) syncBinding(ctx context.Context) {
	m.mu.Lock()
	c, offset, testing := m.config, m.offset, m.testing
	m.mu.Unlock()
	if c.BotToken == "" || c.ChatID != 0 || testing {
		return
	}
	if c.BotUsername == "" || c.PairingCode == "" {
		username, err := m.bot.username(ctx, c.BotToken)
		var pairing string
		if err == nil {
			pairing, err = newPairingCode()
		}
		m.reload()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.config == c && m.fileError == "" {
			if err == nil {
				c.BotUsername, c.PairingCode = username, pairing
				err = m.saveLocked(c)
			}
			m.recordErrorLocked(err)
		}
		return
	}

	chat, nextOffset, err := m.bot.pairedChat(ctx, c, offset)
	m.reload()
	m.mu.Lock()
	if m.config != c || m.fileError != "" || m.testing {
		m.mu.Unlock()
		return
	}
	if err != nil || chat == 0 {
		if err == nil {
			m.offset = nextOffset
		}
		m.recordErrorLocked(err)
		m.mu.Unlock()
		return
	}
	c.ChatID, c.PairingCode = chat, ""
	c.Enabled, c.Tested, c.SetupPending = false, false, true
	if err := m.saveLocked(c); err != nil {
		m.recordErrorLocked(err)
		m.mu.Unlock()
		return
	}
	m.offset = nextOffset
	m.testing = true
	m.mu.Unlock()
	_ = m.sendTest(ctx, c)
}

func (m *Notifier) recordErrorLocked(err error) {
	m.lastError = ""
	if err != nil {
		m.lastError = err.Error()
	}
}

func (m *Notifier) SettingsHandler(ctx context.Context) actions.Handler {
	return func(c actions.Command, emit actions.Emitter) error {
		err := m.set(ctx, c.Args)
		m.publish()
		return commandResult(c, emit, err)
	}
}

func (m *Notifier) TestHandler(ctx context.Context) actions.Handler {
	return func(c actions.Command, emit actions.Emitter) error {
		if len(c.Args) != 0 {
			return commandResult(c, emit, errors.New("Telegram test does not take settings"))
		}
		m.reload()
		m.mu.Lock()
		settings := m.config
		if settings.ChatID == 0 || m.fileError != "" {
			m.mu.Unlock()
			return commandResult(c, emit, errors.New("complete Telegram binding first"))
		}
		if m.testing {
			m.mu.Unlock()
			return commandResult(c, emit, errors.New("Telegram test already in progress"))
		}
		m.testing = true
		m.mu.Unlock()
		return commandResult(c, emit, m.sendTest(ctx, settings))
	}
}

func (m *Notifier) sendTest(ctx context.Context, settings Config) error {
	m.publish()
	err := m.bot.send(ctx, settings, "Quilscan: Telegram notifications are connected.")
	m.reload()
	m.mu.Lock()
	defer func() {
		m.mu.Unlock()
		m.publish()
	}()
	m.testing = false
	if m.config.BotToken != settings.BotToken || m.config.ChatID != settings.ChatID || m.fileError != "" {
		return errors.New("Telegram settings changed during the test; try again")
	}
	if err == nil {
		next := m.config
		next.Tested = true
		err = m.saveLocked(next)
		if err == nil {
			m.lastSent = time.Now()
		}
	}
	m.recordErrorLocked(err)
	return err
}

func commandResult(c actions.Command, emit actions.Emitter, err error) error {
	if err != nil {
		emit(actions.Status{ID: c.ID, Step: "failed", Error: err.Error()})
		return err
	}
	emit(actions.Status{ID: c.ID, Step: "done", Progress: 1})
	return nil
}
