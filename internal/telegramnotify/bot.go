package telegramnotify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/download"
)

type botClient struct {
	base string
	http *http.Client
}

func newBotClient() botClient {
	client := download.NewClient(8 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return botClient{base: "https://api.telegram.org", http: client}
}

func (b botClient) request(ctx context.Context, token, method string, args, result interface{}) error {
	if !validToken(token) {
		return errors.New("invalid Telegram Bot Token")
	}
	body, err := json.Marshal(args)
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		// net/http errors can contain the request URL, which contains the token.
		return errors.New("cannot reach Telegram")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram rejected %s (HTTP %d)", method, resp.StatusCode)
	}
	reply := struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&reply); err != nil || !reply.OK {
		return errors.New("Telegram did not accept the request")
	}
	if result != nil && json.Unmarshal(reply.Result, result) != nil {
		return errors.New("invalid Telegram response")
	}
	return nil
}

func (b botClient) username(ctx context.Context, token string) (string, error) {
	var bot struct {
		Username string `json:"username"`
		IsBot    bool   `json:"is_bot"`
	}
	if err := b.request(ctx, token, "getMe", struct{}{}, &bot); err != nil {
		return "", err
	}
	if !bot.IsBot || !botUsernamePattern.MatchString(bot.Username) {
		return "", errors.New("Telegram did not identify the bot")
	}
	return bot.Username, nil
}

func newPairingCode() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("could not create Telegram pairing code")
	}
	return hex.EncodeToString(random[:]), nil
}

func (b botClient) pairedChat(ctx context.Context, c Config, offset int64) (int64, int64, error) {
	var updates []struct {
		ID      int64 `json:"update_id"`
		Message struct {
			Text string `json:"text"`
			Chat struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			} `json:"chat"`
		} `json:"message"`
	}
	args := map[string]interface{}{"offset": offset, "timeout": 0, "allowed_updates": []string{"message"}}
	if err := b.request(ctx, c.BotToken, "getUpdates", args, &updates); err != nil {
		return 0, offset, err
	}
	for _, update := range updates {
		if update.ID >= offset {
			offset = update.ID + 1
		}
		msg := update.Message
		if msg.Chat.Type == "private" && msg.Chat.ID > 0 && msg.Text == "/start "+c.PairingCode {
			return msg.Chat.ID, offset, nil
		}
	}
	return 0, offset, nil
}

func (b botClient) send(ctx context.Context, c Config, message string) error {
	return b.request(ctx, c.BotToken, "sendMessage", map[string]interface{}{
		"chat_id": c.ChatID, "text": message,
	}, nil)
}
