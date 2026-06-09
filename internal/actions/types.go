package actions

import "errors"

// Command is one inbound action request from backend.
type Command struct {
	ID     string                 `json:"id"`
	Action string                 `json:"action"`
	Args   map[string]interface{} `json:"args"`
}

// Status is one progress event emitted by a handler.
type Status struct {
	ID       string  `json:"id"`
	Action   string  `json:"action,omitempty"`
	Step     string  `json:"step"`
	Progress float64 `json:"progress,omitempty"`
	Error    string  `json:"error,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// Emitter receives Status events from a handler.
type Emitter func(Status)

// Handler runs one whitelisted action.
type Handler func(Command, Emitter) error

// ErrForbidden is returned when the action is outside the whitelist.
var ErrForbidden = errors.New("action not allowed")
