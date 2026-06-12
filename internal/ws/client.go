// Package ws is the agent's WebSocket client: persistent outbound connection
// to backend with auth handshake, heartbeat, and exponential reconnect.
package ws

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quilscan-com/quilscan-agent/internal/agentlog"
)

// MaxInboundMessageSize caps a single inbound WebSocket frame from backend.
// Backend's largest legitimate frame (install command with full args) is well
// under 4 KiB; 1 MiB leaves headroom for future protocol growth while keeping
// a misbehaving or compromised backend from blowing up agent memory with one
// giant message. Frames larger than this trigger gorilla to send a close
// frame to the peer and surface ErrReadLimit on the next ReadMessage.
const MaxInboundMessageSize = 1 << 20

// Meta is included in the auth handshake so backend knows what's connected.
type Meta struct {
	Version         string `json:"version"`
	OS              string `json:"os"`
	HasNode         bool   `json:"has_node"`
	HasQClient      bool   `json:"has_qclient"`
	ServiceMode     string `json:"service_mode"`
	NodeServiceMode string `json:"node_service_mode"`
}

// Client is a long-running WebSocket client.
type Client struct {
	URL         string
	Token       string
	Meta        Meta
	OnMessage   func([]byte) // called for every inbound frame after auth
	OnConnected func()       // called once after auth frame is sent on each new connection

	mu   sync.Mutex
	wmu  sync.Mutex
	conn *websocket.Conn
}

// Run connects, authenticates, and pumps messages until ctx is cancelled.
// Reconnects with exponential backoff on any failure.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := c.connectAndPump(ctx); err != nil {
			log.Printf("[ws] connection dropped: %v; reconnecting in %v", err, backoff)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 60*time.Second {
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
		}
	}
}

func (c *Client) connectAndPump(ctx context.Context) error {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, c.URL, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(MaxInboundMessageSize)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		conn.Close()
	}()

	// auth
	auth := map[string]interface{}{
		"type":              "auth",
		"token":             c.Token,
		"version":           c.Meta.Version,
		"os":                c.Meta.OS,
		"has_node":          c.Meta.HasNode,
		"has_qclient":       c.Meta.HasQClient,
		"service_mode":      c.Meta.ServiceMode,
		"node_service_mode": c.Meta.NodeServiceMode,
	}
	if err := c.writeJSON(conn, auth); err != nil {
		return err
	}

	if c.OnConnected != nil {
		go c.OnConnected()
	}

	// heartbeat ticker
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	done := make(chan error, 2)

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			if summary := agentlog.SummarizeInbound(msg); summary != "" {
				log.Printf("[ws] %s", summary)
			}
			if c.OnMessage != nil {
				c.OnMessage(msg)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			return err
		case <-ping.C:
			if err := c.writeMessage(conn, websocket.PingMessage, nil); err != nil {
				return err
			}
		}
	}
}

// Send writes a JSON-encoded message on the active connection. Returns an
// error if not currently connected.
func (c *Client) Send(v interface{}) error {
	summary := agentlog.SummarizeOutbound(v)
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		if summary != "" {
			log.Printf("[ws] drop %s: not connected", summary)
		}
		return websocket.ErrCloseSent
	}
	b, err := json.Marshal(v)
	if err != nil {
		if summary != "" {
			log.Printf("[ws] encode failed %s: %v", summary, err)
		}
		return err
	}
	if err := c.writeMessage(conn, websocket.TextMessage, b); err != nil {
		if summary != "" {
			log.Printf("[ws] send failed %s: %v", summary, err)
		}
		return err
	}
	if summary != "" {
		log.Printf("[ws] %s", summary)
	}
	return nil
}

func (c *Client) writeJSON(conn *websocket.Conn, v interface{}) error {
	summary := agentlog.SummarizeOutbound(v)
	b, err := json.Marshal(v)
	if err != nil {
		if summary != "" {
			log.Printf("[ws] encode failed %s: %v", summary, err)
		}
		return err
	}
	if err := c.writeMessage(conn, websocket.TextMessage, b); err != nil {
		if summary != "" {
			log.Printf("[ws] send failed %s: %v", summary, err)
		}
		return err
	}
	if summary != "" {
		log.Printf("[ws] %s", summary)
	}
	return nil
}

func (c *Client) writeMessage(conn *websocket.Conn, messageType int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(messageType, payload)
}
