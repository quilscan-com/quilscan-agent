package main

import (
	"sync"

	"github.com/quilscan-com/quilscan-agent/internal/actions"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
)

// Keep the latest undelivered node operation status per command so a
// temporary WebSocket disconnect does not lose its completion.
type nodeCommandStatusRelay struct {
	mu      sync.Mutex
	send    func(actions.Status) error
	pending []actions.Status
}

func (r *nodeCommandStatusRelay) emit(status actions.Status) {
	releasesUpdate := status.Action == "automatic_update" && status.ActionDetail == nodemanifest.SourceReleases
	telegramCommand := status.Action == "set_telegram_notifications" || status.Action == "test_telegram_notification"
	if status.ID == "" || (status.Action != "switch_node_source" && status.Action != "update_node" &&
		status.Action != "set_releases_node_auto_update" && !releasesUpdate && !telegramCommand) {
		_ = r.send(status)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for i, pending := range r.pending {
		if pending.ID == status.ID {
			r.pending = append(r.pending[:i], r.pending[i+1:]...)
			break
		}
	}
	if err := r.send(status); err != nil {
		const maxPendingNodeCommands = 16
		if len(r.pending) == maxPendingNodeCommands {
			r.pending = r.pending[1:]
		}
		r.pending = append(r.pending, status)
	}
}

func (r *nodeCommandStatusRelay) replay() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.pending) != 0 {
		if err := r.send(r.pending[0]); err != nil {
			return
		}
		r.pending = r.pending[1:]
	}
	r.pending = nil
}
