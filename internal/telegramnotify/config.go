package telegramnotify

import (
	"errors"
	"regexp"
	"strings"
	"time"

	agentconfig "github.com/quilscan-com/quilscan-agent/internal/config"
)

var (
	tokenPattern       = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
	botUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	pairingPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type Rules struct {
	DiskThreshold   float64 `yaml:"disk_threshold_percent" json:"disk_threshold_percent"`
	MemoryThreshold float64 `yaml:"memory_threshold_percent" json:"memory_threshold_percent"`
	DownThreshold   float64 `yaml:"download_threshold_mbit_s" json:"download_threshold_mbit_s"`
	UpThreshold     float64 `yaml:"upload_threshold_mbit_s" json:"upload_threshold_mbit_s"`
	AlertNodeStop   bool    `yaml:"alert_node_stop" json:"alert_node_stop"`
	AlertUpdates    bool    `yaml:"alert_updates" json:"alert_updates"`
}

// Config is only persisted on the Agent. Use Status for console readback.
type Config struct {
	Enabled                  bool           `yaml:"enabled"`
	Tested                   bool           `yaml:"tested,omitempty"`
	SetupPending             bool           `yaml:"setup_pending,omitempty"`
	BotToken                 string         `yaml:"bot_token,omitempty"`
	BotUsername              string         `yaml:"bot_username,omitempty"`
	PairingCode              string         `yaml:"pairing_code,omitempty"`
	ChatID                   int64          `yaml:"chat_id,omitempty"`
	UpdateReceipts           updateReceipts `yaml:"update_notification_receipts,omitempty"`
	UpdateNotificationsSince time.Time      `yaml:"update_notifications_since,omitempty"`
	Rules                    `yaml:",inline"`
}

func defaultConfig() Config {
	return Config{Rules: Rules{DiskThreshold: 90, MemoryThreshold: 90, AlertNodeStop: true, AlertUpdates: true}}
}

func validToken(token string) bool {
	return len(token) <= 256 && tokenPattern.MatchString(token)
}

func validThreshold(number, maximum float64) bool {
	return number >= 0 && number <= maximum
}

func (c Config) validate() error {
	if (c.Enabled || c.BotToken != "") && !validToken(c.BotToken) {
		return errors.New("invalid or missing Telegram Bot Token")
	}
	if c.BotUsername != "" && !botUsernamePattern.MatchString(c.BotUsername) ||
		c.PairingCode != "" && !pairingPattern.MatchString(c.PairingCode) ||
		c.ChatID < 0 || (c.ChatID != 0 && c.BotToken == "") || (c.Tested && c.ChatID == 0) {
		return errors.New("invalid Telegram binding")
	}
	if !validThreshold(c.DiskThreshold, 100) || !validThreshold(c.MemoryThreshold, 100) ||
		!validThreshold(c.DownThreshold, 2000) || !validThreshold(c.UpThreshold, 2000) {
		return errors.New("invalid Telegram alert threshold")
	}
	return nil
}

func applyArgs(c Config, args map[string]interface{}) (Config, error) {
	if len(args) == 0 {
		return c, errors.New("Telegram settings are empty")
	}
	for key, value := range args {
		switch key {
		case "enabled", "alert_node_stop", "alert_updates":
			enabled, ok := value.(bool)
			if !ok {
				return c, errors.New("invalid Telegram switch value")
			}
			switch key {
			case "enabled":
				c.Enabled = enabled
			case "alert_node_stop":
				c.AlertNodeStop = enabled
			case "alert_updates":
				c.AlertUpdates = enabled
			}
		case "bot_token":
			token, ok := value.(string)
			if !ok || !validToken(strings.TrimSpace(token)) {
				return c, errors.New("invalid Telegram Bot Token")
			}
			c.BotToken = strings.TrimSpace(token)
		case "disk_threshold_percent", "memory_threshold_percent", "download_threshold_mbit_s", "upload_threshold_mbit_s":
			number, ok := value.(float64)
			if !ok {
				return c, errors.New("invalid Telegram alert threshold")
			}
			switch key {
			case "disk_threshold_percent":
				c.DiskThreshold = number
			case "memory_threshold_percent":
				c.MemoryThreshold = number
			case "download_threshold_mbit_s":
				c.DownThreshold = number
			case "upload_threshold_mbit_s":
				c.UpThreshold = number
			}
		default:
			return c, errors.New("unsupported Telegram setting")
		}
	}
	return c, c.validate()
}

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	private, err := agentconfig.LoadTelegramSettings(path, &c)
	if err != nil {
		return c, err
	}
	if c.BotToken != "" && !private {
		return c, errors.New("Agent config containing a Telegram token must be private")
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	c.Enabled = c.Enabled && c.Tested
	c.SetupPending = c.SetupPending && c.BotToken != "" && !c.Enabled
	return c, nil
}

func saveConfig(path string, c Config) error {
	return agentconfig.SaveTelegramSettings(path, c, c.BotToken != "")
}
