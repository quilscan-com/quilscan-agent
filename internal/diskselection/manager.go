// Package diskselection owns the single Agent-wide mounted filesystem chosen
// for browser display. Root metrics remain independently available.
package diskselection

import (
	"context"
	"sync"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/diskinspect"
)

type InspectFunc func(context.Context, string) (diskinspect.Usage, diskinspect.Code)
type PersistFunc func(string, string) error

// Selection identifies the Agent-owned mount and its current verification
// state. Generation changes whenever the active selection changes.
type Selection struct {
	Mountpoint      string `json:"mountpoint"`
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
	AgentInstanceID string `json:"agent_instance_id"`
	Generation      uint64 `json:"selection_generation"`
}

type SelectedUsage struct {
	diskinspect.Usage
	Points          []float64 `json:"points"`
	AgentInstanceID string    `json:"agent_instance_id"`
	Generation      uint64    `json:"selection_generation"`
}

type Deps struct {
	ConfigPath   string
	InitialMount string
	InstanceID   string
	Inspect      InspectFunc
	Persist      PersistFunc
	Emit         func(Selection)
}

type Manager struct {
	ctx                context.Context
	deps               Deps
	selectMu           sync.Mutex
	verifyOnce         sync.Once
	mu                 sync.RWMutex
	selection          Selection
	latest             SelectedUsage
	sampleBusy         bool
	pendingPersistRoot bool
	verifyTimeout      time.Duration
}

// LoadAndVerify checks a persisted custom mount asynchronously so an
// unavailable filesystem cannot hold Agent startup or WebSocket connection.
func (m *Manager) LoadAndVerify() {
	m.verifyOnce.Do(func() {
		initial := m.Snapshot()
		if initial.State != "verifying" {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(m.ctx, m.verifyTimeout)
			defer cancel()
			type outcome struct {
				usage diskinspect.Usage
				code  diskinspect.Code
			}
			result := make(chan outcome, 1)
			go func() {
				usage, code := m.deps.Inspect(ctx, initial.Mountpoint)
				result <- outcome{usage: usage, code: code}
			}()
			var usage diskinspect.Usage
			var code diskinspect.Code
			select {
			case got := <-result:
				usage, code = got.usage, got.code
			case <-ctx.Done():
				code = diskinspect.Timeout
			}
			m.selectMu.Lock()
			defer m.selectMu.Unlock()
			current := m.Snapshot()
			if current.Generation != initial.Generation || current.State != "verifying" || m.ctx.Err() != nil {
				return
			}
			var persistErr error
			if code == "" && usage.Mountpoint == initial.Mountpoint {
				current.State = "ready"
			} else {
				if code == "" {
					code = diskinspect.MountChanged
				}
				persistErr = m.deps.Persist(m.deps.ConfigPath, "/")
				current.Mountpoint = "/"
				current.State = "ready"
				current.Reason = string(code)
				current.Generation++
			}
			m.mu.Lock()
			m.selection = current
			m.pendingPersistRoot = persistErr != nil
			m.mu.Unlock()
			if m.deps.Emit != nil {
				m.deps.Emit(current)
			}
		}()
	})
}

func New(ctx context.Context, deps Deps) *Manager {
	if deps.InitialMount == "" {
		deps.InitialMount = "/"
	}
	if deps.Inspect == nil {
		deps.Inspect = diskinspect.Inspect
	}
	if deps.Persist == nil {
		deps.Persist = config.SaveSelectedDiskMount
	}
	state := "ready"
	if deps.InitialMount != "/" {
		state = "verifying"
	}
	return &Manager{
		ctx:           ctx,
		deps:          deps,
		verifyTimeout: 5 * time.Second,
		selection: Selection{
			Mountpoint: deps.InitialMount, State: state,
			AgentInstanceID: deps.InstanceID, Generation: 1,
		},
	}
}

func (m *Manager) Snapshot() Selection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selection
}

// Latest returns only a sample matching the current ready selection.
func (m *Manager) Latest() (SelectedUsage, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.selection.State != "ready" || m.selection.Mountpoint == "/" || m.latest.Mountpoint != m.selection.Mountpoint || m.latest.Generation != m.selection.Generation {
		return SelectedUsage{}, false
	}
	latest := m.latest
	latest.Points = append([]float64(nil), m.latest.Points...)
	return latest, true
}

// Sample launches at most one custom filesystem inspection. A stuck kernel
// call occupies that slot rather than accumulating unbounded workers.
func (m *Manager) Sample() {
	m.mu.Lock()
	selection := m.selection
	if m.ctx.Err() != nil || m.sampleBusy || selection.State != "ready" {
		m.mu.Unlock()
		return
	}
	if selection.Mountpoint == "/" {
		if m.pendingPersistRoot {
			m.sampleBusy = true
			m.mu.Unlock()
			go m.retryRootPersistence(selection.Generation)
			return
		}
		m.mu.Unlock()
		return
	}
	m.sampleBusy = true
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, 4500*time.Millisecond)
		defer cancel()
		usage, code := m.deps.Inspect(ctx, selection.Mountpoint)
		if ctx.Err() != nil {
			code = diskinspect.Timeout
		}
		m.mu.Lock()
		current := m.selection
		if current.Generation != selection.Generation || current.State != "ready" || m.ctx.Err() != nil {
			m.sampleBusy = false
			m.mu.Unlock()
			return
		}
		if code == "" && usage.Mountpoint == selection.Mountpoint {
			points := append(append([]float64(nil), m.latest.Points...), usage.UsedPercent)
			if len(points) > 24 {
				points = points[len(points)-24:]
			}
			m.latest = SelectedUsage{Usage: usage, Points: points, AgentInstanceID: selection.AgentInstanceID, Generation: selection.Generation}
			m.sampleBusy = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		if code == diskinspect.Timeout || code == diskinspect.Busy {
			m.mu.Lock()
			m.sampleBusy = false
			m.mu.Unlock()
			return
		}
		if code == "" {
			code = diskinspect.MountChanged
		}
		m.fallBackFromSample(selection, code)
		m.mu.Lock()
		m.sampleBusy = false
		m.mu.Unlock()
	}()
}

func (m *Manager) retryRootPersistence(generation uint64) {
	m.selectMu.Lock()
	defer m.selectMu.Unlock()
	current := m.Snapshot()
	if current.Generation != generation || current.Mountpoint != "/" {
		m.mu.Lock()
		m.sampleBusy = false
		m.mu.Unlock()
		return
	}
	err := m.deps.Persist(m.deps.ConfigPath, "/")
	m.mu.Lock()
	m.sampleBusy = false
	if err == nil && m.selection.Generation == generation {
		m.pendingPersistRoot = false
	}
	m.mu.Unlock()
}

func (m *Manager) fallBackFromSample(previous Selection, code diskinspect.Code) {
	m.selectMu.Lock()
	defer m.selectMu.Unlock()
	current := m.Snapshot()
	if current.Generation != previous.Generation || current.Mountpoint != previous.Mountpoint {
		return
	}
	persistErr := m.deps.Persist(m.deps.ConfigPath, "/")
	current.Mountpoint = "/"
	current.State = "ready"
	current.Reason = string(code)
	current.Generation++
	m.mu.Lock()
	m.selection = current
	m.latest = SelectedUsage{}
	m.pendingPersistRoot = persistErr != nil
	m.mu.Unlock()
	if m.deps.Emit != nil {
		m.deps.Emit(current)
	}
}

// Select validates and persists before changing the visible Agent-wide
// selection. Concurrent callers are serialized; the later successful call wins.
func (m *Manager) Select(ctx context.Context, path string) (Selection, diskinspect.Code) {
	m.selectMu.Lock()
	defer m.selectMu.Unlock()
	if ctx.Err() != nil || m.ctx.Err() != nil {
		return m.Snapshot(), diskinspect.Timeout
	}
	mount := "/"
	var selectedUsage diskinspect.Usage
	if path != "/" {
		usage, code := m.deps.Inspect(ctx, path)
		if code != "" {
			return m.Snapshot(), code
		}
		mount = usage.Mountpoint
		selectedUsage = usage
	}
	if ctx.Err() != nil {
		return m.Snapshot(), diskinspect.Timeout
	}
	current := m.Snapshot()
	if current.State == "ready" && current.Mountpoint == mount {
		return current, ""
	}
	if err := m.deps.Persist(m.deps.ConfigPath, mount); err != nil {
		return current, diskinspect.PersistenceFailed
	}
	current.Mountpoint = mount
	current.State = "ready"
	current.Reason = ""
	current.Generation++
	m.mu.Lock()
	m.selection = current
	m.latest = SelectedUsage{}
	if mount != "/" {
		m.latest = SelectedUsage{Usage: selectedUsage, Points: []float64{selectedUsage.UsedPercent}, AgentInstanceID: current.AgentInstanceID, Generation: current.Generation}
	}
	m.pendingPersistRoot = false
	m.mu.Unlock()
	if m.deps.Emit != nil {
		m.deps.Emit(current)
	}
	return current, ""
}
