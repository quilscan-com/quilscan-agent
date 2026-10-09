package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/diskinspect"
	"github.com/quilscan-com/quilscan-agent/internal/diskselection"
)

type diskSelectRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Path string `json:"path"`
}

type diskSelectResult struct {
	Type       string           `json:"type"`
	ID         string           `json:"id"`
	Mountpoint string           `json:"mountpoint,omitempty"`
	Error      diskinspect.Code `json:"error,omitempty"`
}

type diskSelectionFrame struct {
	Type string `json:"type"`
	diskselection.Selection
}

func diskSelectionMessage(selection diskselection.Selection) diskSelectionFrame {
	return diskSelectionFrame{Type: "disk_selection", Selection: selection}
}

type diskSelectDispatcher struct {
	ctx      context.Context
	manager  *diskselection.Manager
	requests chan diskSelectRequest
	results  chan diskSelectResult
}

func newDiskSelectDispatcher(ctx context.Context, manager *diskselection.Manager, send func(interface{}) error) *diskSelectDispatcher {
	d := &diskSelectDispatcher{
		ctx: ctx, manager: manager,
		requests: make(chan diskSelectRequest, 2),
		results:  make(chan diskSelectResult, 8),
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-d.requests:
				requestCtx, cancel := context.WithTimeout(ctx, 4500*time.Millisecond)
				selection, code := manager.Select(requestCtx, request.Path)
				cancel()
				result := diskSelectResult{Type: "disk_select_result", ID: request.ID, Error: code}
				if code == "" {
					result.Mountpoint = selection.Mountpoint
				}
				d.queueResult(result)
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case result := <-d.results:
				_ = send(result)
			}
		}
	}()
	return d
}

// Handle never inspects a filesystem or writes a WebSocket frame on the
// WebSocket read loop. The bounded queue serializes last-arriving choices.
func (d *diskSelectDispatcher) Handle(frame []byte) {
	if len(frame) > 2048 || d.ctx.Err() != nil {
		return
	}
	var request diskSelectRequest
	if err := json.Unmarshal(frame, &request); err != nil || request.Type != "disk_select" || !validDiskSelectID(request.ID) {
		return
	}
	if len(request.Path) > 1024 {
		d.queueResult(diskSelectResult{Type: "disk_select_result", ID: request.ID, Error: diskinspect.InvalidPath})
		return
	}
	select {
	case d.requests <- request:
	default:
		d.queueResult(diskSelectResult{Type: "disk_select_result", ID: request.ID, Error: diskinspect.Busy})
	}
}

func (d *diskSelectDispatcher) queueResult(result diskSelectResult) {
	select {
	case d.results <- result:
	default:
	}
}

func validDiskSelectID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if id[i] != '-' {
				return false
			}
			continue
		}
		if id[i] < '0' || id[i] > '9' {
			if id[i] < 'a' || id[i] > 'f' {
				return false
			}
		}
	}
	return true
}
