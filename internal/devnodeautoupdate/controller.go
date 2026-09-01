// Package devnodeautoupdate owns the Agent-side Dev Node automatic-update
// scheduler. It is deliberately independent of browser connections: state is
// persisted in config.State and every tick patches the cumulative node status.
package devnodeautoupdate

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/actions"
	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
)

const (
	StateDisabled  = "disabled"
	StateWatching  = "watching"
	StateScheduled = "scheduled"
	StateUpdating  = "updating"
	StateRetrying  = "retrying"
)

const updateAction = "update_node"

// Deps contains the controller's side effects and time sources.
type Deps struct {
	StatePath         string
	LoadState         func(string) (*config.State, error)
	PersistState      func(string, func(*config.State) error) (*config.State, error)
	Update            actions.Handler
	UpdateOwner       func() string
	PatchNodeStatus   func(map[string]interface{})
	EmitCommandStatus func(actions.Status)
	Tick              time.Duration
	RetryDelay        time.Duration
	Jitter            func() time.Duration
	Now               func() time.Time
	Start             func(func())
	Schedule          func(time.Duration, func()) (cancel func())
}

// Controller schedules and runs at most one automatic update at a time.
type Controller struct {
	deps Deps

	mu            sync.Mutex
	completionMu  sync.Mutex
	state         string
	target        candidate
	nextAttemptAt time.Time
	retrying      bool
	running       bool
	wake          chan struct{}
	cancelDue     func()
	dueToken      uint64
	lifecycle     uint64
	autoEligible  bool
	pendingResult *pendingResult
	lastKnown     config.State
	hasLastKnown  bool
}

type candidate struct {
	FromVersion   string
	TargetVersion string
	BuildNumber   int
	SHA256        string
}

type pendingResult struct {
	eventID      string
	result       string
	from         string
	target       string
	completedAt  time.Time
	failedTarget string
	retryTarget  candidate
	retryDue     time.Time
	runLifecycle uint64
}

var errPendingResultInapplicable = errors.New("automatic update result no longer applies")
var errPendingResultSuperseded = errors.New("automatic update result is superseded by pending result")

var fallbackEventSequence atomic.Uint64

// NewController applies production defaults while retaining deterministic
// injection points for tests.
func NewController(deps Deps) *Controller {
	if deps.LoadState == nil {
		deps.LoadState = config.LoadState
	}
	if deps.PersistState == nil {
		deps.PersistState = config.UpdateState
	}
	if deps.Tick <= 0 {
		deps.Tick = 3 * time.Second
	}
	if deps.RetryDelay <= 0 {
		deps.RetryDelay = 5 * time.Minute
	}
	if deps.Jitter == nil {
		deps.Jitter = defaultJitter
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Start == nil {
		deps.Start = func(fn func()) { go fn() }
	}
	if deps.Schedule == nil {
		deps.Schedule = func(delay time.Duration, fn func()) func() {
			timer := time.AfterFunc(delay, fn)
			return func() { timer.Stop() }
		}
	}
	return &Controller{
		deps:  deps,
		state: StateDisabled,
		wake:  make(chan struct{}, 1),
	}
}

func defaultJitter() time.Duration {
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(11))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64()) * time.Second
}

func candidateFromState(s *config.State) (candidate, bool) {
	if s == nil || !s.DevNodeAutoUpdateEnabled || s.NodeSource != nodemanifest.SourceDev {
		return candidate{}, false
	}
	targetVersion := strings.TrimSpace(s.LatestDevNodeVersion)
	if targetVersion == "" {
		return candidate{}, false
	}
	currentSHA := strings.TrimSpace(s.NodeBinarySHA256)
	targetSHA := strings.TrimSpace(s.LatestDevNodeSHA256)
	available := strings.TrimSpace(s.InstalledNodeVersion) != targetVersion ||
		s.NodeBuildNumber != s.LatestDevNodeBuildNumber ||
		!strings.EqualFold(currentSHA, targetSHA)
	if !available {
		return candidate{}, false
	}
	return candidate{
		FromVersion:   strings.TrimSpace(s.InstalledNodeVersion),
		TargetVersion: targetVersion,
		BuildNumber:   s.LatestDevNodeBuildNumber,
		SHA256:        strings.ToLower(targetSHA),
	}, true
}

// Run evaluates immediately, then on the configured ticker and every wake.
func (c *Controller) Run(ctx context.Context) {
	c.Tick()
	ticker := time.NewTicker(c.deps.Tick)
	defer ticker.Stop()
	defer c.cancelDueTimer()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Tick()
		case <-c.wake:
			c.Tick()
		}
	}
}

// Wake requests an immediate evaluation without blocking the caller.
func (c *Controller) Wake() {
	if c == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// CancelScheduled clears scheduled and retry attempts. A running update is not
// interrupted and remains in the updating state until its handler returns.
func (c *Controller) CancelScheduled() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel := c.detachDueUnlocked()
	c.lifecycle++
	c.autoEligible = false
	c.nextAttemptAt = time.Time{}
	c.retrying = false
	if c.running {
		c.state = StateUpdating
	} else {
		c.state = StateDisabled
		c.target = candidate{}
	}
	c.mu.Unlock()
	callCancel(cancel)
}

// Tick performs one deterministic state-machine evaluation and always emits a
// complete automatic-update status patch.
func (c *Controller) Tick() {
	state, err := c.loadNormalizedState()
	if err != nil {
		c.publish(c.lastKnownSnapshot())
		return
	}
	if state == nil {
		c.publish(c.lastKnownSnapshot())
		return
	}
	state, pending := c.flushPendingResult(state)
	if pending {
		c.publish(state)
		return
	}
	if !state.DevNodeAutoUpdateEnabled {
		c.setInactive(StateDisabled)
		c.publish(state)
		return
	}
	target, available := candidateFromState(state)
	if !available {
		c.setInactive(StateWatching)
		c.publish(state)
		return
	}

	if c.runningNow() {
		c.publish(state)
		return
	}

	c.mu.Lock()
	needsSchedule := c.target != target || c.nextAttemptAt.IsZero()
	c.mu.Unlock()
	if needsSchedule {
		delay := c.deps.Jitter()
		if delay < 0 {
			delay = 0
		}
		c.armAttempt(target, c.now().Add(delay), delay)
		c.publish(state)
		return
	}

	now := c.now()
	owner := ""
	if c.deps.UpdateOwner != nil {
		owner = c.deps.UpdateOwner()
	}
	var launch bool
	var cancelDue func()
	var runLifecycle uint64
	c.mu.Lock()
	if !c.running && c.target == target {
		if now.Before(c.nextAttemptAt) {
			if c.retrying {
				c.state = StateRetrying
			} else {
				c.state = StateScheduled
			}
		} else if owner == "" {
			cancelDue = c.detachDueUnlocked()
			c.running = true
			c.retrying = false
			c.nextAttemptAt = time.Time{}
			c.state = StateUpdating
			runLifecycle = c.lifecycle
			launch = true
		} else if c.retrying {
			c.state = StateRetrying
		} else {
			c.state = StateScheduled
		}
	}
	c.mu.Unlock()
	callCancel(cancelDue)
	c.publish(state)
	if launch {
		c.deps.Start(func() { c.execute(target, runLifecycle) })
	}
}

func (c *Controller) runningNow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		c.state = StateUpdating
	}
	return c.running
}

func (c *Controller) setInactive(runtimeState string) {
	c.mu.Lock()
	cancel := c.detachDueUnlocked()
	if c.autoEligible {
		c.lifecycle++
		c.autoEligible = false
	}
	c.nextAttemptAt = time.Time{}
	c.retrying = false
	if c.running {
		c.state = StateUpdating
		c.mu.Unlock()
		callCancel(cancel)
		return
	}
	c.state = runtimeState
	c.target = candidate{}
	c.mu.Unlock()
	callCancel(cancel)
}

func (c *Controller) loadNormalizedState() (*config.State, error) {
	state, err := c.deps.LoadState(c.deps.StatePath)
	if err != nil || state == nil {
		return state, err
	}
	if !state.DevNodeAutoUpdateEnabled || state.NodeSource == nodemanifest.SourceDev {
		c.rememberState(state)
		return state, nil
	}
	updated, err := c.deps.PersistState(c.deps.StatePath, func(current *config.State) error {
		if current.DevNodeAutoUpdateEnabled && current.NodeSource != nodemanifest.SourceDev {
			current.DevNodeAutoUpdateEnabled = false
			current.DevNodeAutoUpdateRevision++
		}
		return nil
	})
	if err != nil {
		return state, err
	}
	c.rememberState(updated)
	return updated, nil
}

func (c *Controller) execute(target candidate, runLifecycle uint64) {
	state, err := c.loadNormalizedState()
	current, available := candidateFromState(state)
	if err != nil || !available || current != target {
		c.finishWithoutResult(state, runLifecycle)
		return
	}

	commandID := c.newEventID()
	command := actions.Command{
		ID:     commandID,
		Action: updateAction,
		Args: map[string]interface{}{
			"version":   target.TargetVersion,
			"automatic": true,
		},
	}
	var updateErr error
	if c.deps.Update == nil {
		updateErr = errors.New("automatic node update handler is unavailable")
	} else {
		updateErr = c.deps.Update(command, func(status actions.Status) {
			status.ID = command.ID
			status.Action = command.Action
			if c.deps.EmitCommandStatus != nil {
				c.deps.EmitCommandStatus(status)
			}
		})
	}
	if errors.Is(updateErr, actions.ErrNodeUpdateInProgress) {
		c.finishContention()
		return
	}
	if updateErr != nil {
		c.finishFailure(target, commandID, runLifecycle)
		return
	}
	c.finishSuccess(target, commandID, runLifecycle)
}

func (c *Controller) finishWithoutResult(state *config.State, runLifecycle uint64) {
	if state == nil {
		state, _ = c.loadNormalizedState()
	}
	c.mu.Lock()
	c.running = false
	cancel := c.clearAttemptUnlocked()
	c.state = runtimeStateFor(state)
	c.invalidateRunLifecycleUnlocked(runLifecycle)
	c.mu.Unlock()
	callCancel(cancel)
	if state == nil {
		state = &config.State{}
	}
	c.publish(state)
}

func (c *Controller) finishContention() {
	state, _ := c.loadNormalizedState()
	c.mu.Lock()
	c.running = false
	cancel := c.clearAttemptUnlocked()
	c.state = runtimeStateFor(state)
	c.mu.Unlock()
	callCancel(cancel)
	if state == nil {
		state = &config.State{}
	}
	c.publish(state)
}

func (c *Controller) finishFailure(target candidate, eventID string, runLifecycle uint64) {
	completedAt := c.now()
	record := pendingResult{
		eventID: eventID, result: "failed", from: target.FromVersion, target: target.TargetVersion,
		completedAt: completedAt, failedTarget: target.TargetVersion, retryTarget: target,
		retryDue: completedAt.Add(c.deps.RetryDelay), runLifecycle: runLifecycle,
	}
	state, pending := c.completeResult(record)
	if pending {
		effective, ok := c.pendingSnapshot()
		if ok && effective.result == "failed" {
			c.finishPendingFailure(effective, state)
		} else {
			c.finishWithPending(state)
		}
		return
	}
	current, available := candidateFromState(state)
	next := record.retryDue
	var oldCancel func()
	var token uint64
	var retry bool
	c.mu.Lock()
	if c.lifecycle == runLifecycle && available && current == target {
		oldCancel = c.detachDueUnlocked()
		c.running = false
		c.target = target
		c.nextAttemptAt = next
		c.retrying = true
		c.state = StateRetrying
		c.autoEligible = true
		token = c.dueToken
		retry = true
	} else {
		c.running = false
		oldCancel = c.clearAttemptUnlocked()
		c.state = runtimeStateFor(state)
		c.invalidateRunLifecycleUnlocked(runLifecycle)
	}
	c.mu.Unlock()
	callCancel(oldCancel)
	if retry {
		state = c.attachRetryDueTimer(target, next, c.deps.RetryDelay, token, runLifecycle)
	}
	if state == nil {
		state = &config.State{}
	}
	c.publish(state)
}

func (c *Controller) finishSuccess(target candidate, eventID string, runLifecycle uint64) {
	completedAt := c.now()
	record := pendingResult{eventID: eventID, result: "success", from: target.FromVersion, target: target.TargetVersion, completedAt: completedAt}
	state, pending := c.completeResult(record)
	if pending {
		effective, ok := c.pendingSnapshot()
		if ok && effective.result == "failed" {
			c.finishPendingFailure(effective, state)
		} else {
			c.finishWithPending(state)
		}
		return
	}
	c.mu.Lock()
	c.running = false
	cancel := c.clearAttemptUnlocked()
	c.state = runtimeStateFor(state)
	c.invalidateRunLifecycleUnlocked(runLifecycle)
	c.mu.Unlock()
	callCancel(cancel)
	if state == nil {
		state = &config.State{}
	}
	c.publish(state)
}

func (c *Controller) finishWithPending(state *config.State) {
	c.mu.Lock()
	c.running = false
	cancel := c.clearAttemptUnlocked()
	c.state = runtimeStateFor(state)
	c.mu.Unlock()
	callCancel(cancel)
	if state == nil {
		state = &config.State{}
	}
	c.publish(state)
}

func (c *Controller) finishPendingFailure(record pendingResult, state *config.State) {
	current, available := candidateFromState(state)
	var cancel func()
	var token uint64
	var schedule bool
	c.mu.Lock()
	if c.lifecycle == record.runLifecycle && available && current == record.retryTarget {
		cancel = c.detachDueUnlocked()
		c.running = false
		c.target = record.retryTarget
		c.nextAttemptAt = record.retryDue
		c.retrying = true
		c.state = StateRetrying
		c.autoEligible = true
		token = c.dueToken
		schedule = true
	} else {
		c.running = false
		cancel = c.clearAttemptUnlocked()
		c.state = runtimeStateFor(state)
		c.invalidateRunLifecycleUnlocked(record.runLifecycle)
	}
	c.mu.Unlock()
	callCancel(cancel)
	if schedule {
		delay := record.retryDue.Sub(c.now())
		if delay < 0 {
			delay = 0
		}
		state = c.attachRetryDueTimer(record.retryTarget, record.retryDue, delay, token, record.runLifecycle)
	}
	if state == nil {
		state = &config.State{}
	}
	c.publish(state)
}

func (c *Controller) completeResult(record pendingResult) (*config.State, bool) {
	c.completionMu.Lock()
	defer c.completionMu.Unlock()
	if current, ok := c.pendingSnapshotUnlocked(); ok && !pendingSupersedes(record, current) && !samePendingResult(record, current) {
		state, err := c.loadNormalizedState()
		if err != nil || state == nil {
			state = c.lastKnownSnapshot()
		}
		return state, true
	}
	state, err := c.persistResultUnlocked(record)
	if err == nil {
		return state, false
	}
	if errors.Is(err, errPendingResultInapplicable) {
		c.dropPendingUnlocked(record)
		state, err = c.loadNormalizedState()
		if err != nil || state == nil {
			state = c.lastKnownSnapshot()
		}
		return state, false
	}
	c.rememberPendingUnlocked(record)
	state, err = c.loadNormalizedState()
	if err != nil || state == nil {
		state = c.lastKnownSnapshot()
	}
	return state, true
}

func (c *Controller) flushPendingResult(state *config.State) (*config.State, bool) {
	record, ok := c.pendingSnapshot()
	if !ok {
		return state, false
	}
	if state == nil || state.ConfigPath == "" || state.NodeSource != nodemanifest.SourceDev {
		c.dropPending(record)
		c.setInactive(runtimeStateFor(state))
		return state, false
	}
	updated, err := c.persistResult(record)
	if err == nil {
		c.dropPending(record)
		return updated, false
	}
	if errors.Is(err, errPendingResultInapplicable) {
		c.dropPending(record)
		c.setInactive(runtimeStateFor(state))
		return state, false
	}
	c.holdForPending(state, record)
	return state, true
}

func (c *Controller) persistResult(record pendingResult) (*config.State, error) {
	c.completionMu.Lock()
	defer c.completionMu.Unlock()
	if current, ok := c.pendingSnapshotUnlocked(); ok && !pendingSupersedes(record, current) && !samePendingResult(record, current) {
		return nil, errPendingResultSuperseded
	}
	return c.persistResultUnlocked(record)
}

func (c *Controller) persistResultUnlocked(record pendingResult) (*config.State, error) {
	updated, err := c.deps.PersistState(c.deps.StatePath, func(current *config.State) error {
		if current.ConfigPath == "" || current.NodeSource != nodemanifest.SourceDev {
			return errPendingResultInapplicable
		}
		if record.result == "failed" {
			if current.DevNodeAutoUpdateLastResult == "success" && !current.DevNodeAutoUpdateLastCompletedAt.Before(record.completedAt) {
				return nil
			}
			if current.DevNodeAutoUpdateLastFailedTargetVersion == record.failedTarget {
				return nil
			}
			current.DevNodeAutoUpdateLastFailedTargetVersion = record.failedTarget
		} else {
			current.DevNodeAutoUpdateLastFailedTargetVersion = ""
		}
		resultTarget := record.target
		if record.result == "success" && strings.TrimSpace(current.InstalledNodeVersion) != "" {
			resultTarget = strings.TrimSpace(current.InstalledNodeVersion)
		}
		current.DevNodeAutoUpdateRevision++
		current.DevNodeAutoUpdateLastEventID = record.eventID
		current.DevNodeAutoUpdateLastResult = record.result
		current.DevNodeAutoUpdateLastFromVersion = record.from
		current.DevNodeAutoUpdateLastTargetVersion = resultTarget
		current.DevNodeAutoUpdateLastCompletedAt = record.completedAt
		return nil
	})
	if err == nil {
		c.rememberState(updated)
		c.dropPendingSupersededUnlocked(record)
	}
	return updated, err
}

func (c *Controller) rememberState(state *config.State) {
	if state == nil {
		return
	}
	c.mu.Lock()
	c.lastKnown = *state
	c.hasLastKnown = true
	c.mu.Unlock()
}

func (c *Controller) lastKnownSnapshot() *config.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasLastKnown {
		return &config.State{}
	}
	copy := c.lastKnown
	return &copy
}

func (c *Controller) rememberPending(record pendingResult) {
	c.completionMu.Lock()
	defer c.completionMu.Unlock()
	c.rememberPendingUnlocked(record)
}

func (c *Controller) rememberPendingUnlocked(record pendingResult) {
	c.mu.Lock()
	if c.pendingResult == nil || pendingSupersedes(record, *c.pendingResult) {
		copy := record
		c.pendingResult = &copy
	}
	c.mu.Unlock()
}

// pendingSupersedes keeps result history monotonic: success wins over an
// overlapping older/equal failure, while otherwise completion time and then
// stable event/target identity pick the newest pending result.
func pendingSupersedes(next, current pendingResult) bool {
	if next.result == "success" && current.result == "failed" {
		return !next.completedAt.Before(current.completedAt)
	}
	if next.result == "failed" && current.result == "success" {
		return false
	}
	if !next.completedAt.Equal(current.completedAt) {
		return next.completedAt.After(current.completedAt)
	}
	if next.eventID != current.eventID {
		return next.eventID > current.eventID
	}
	return next.target > current.target
}

func (c *Controller) pendingSnapshot() (pendingResult, bool) {
	c.completionMu.Lock()
	defer c.completionMu.Unlock()
	return c.pendingSnapshotUnlocked()
}

func (c *Controller) pendingSnapshotUnlocked() (pendingResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingResult == nil {
		return pendingResult{}, false
	}
	return *c.pendingResult, true
}

func (c *Controller) dropPending(record pendingResult) {
	c.completionMu.Lock()
	defer c.completionMu.Unlock()
	c.dropPendingUnlocked(record)
}

func (c *Controller) dropPendingUnlocked(record pendingResult) {
	c.mu.Lock()
	if c.pendingResult != nil && c.pendingResult.eventID == record.eventID && c.pendingResult.completedAt.Equal(record.completedAt) {
		c.pendingResult = nil
	}
	c.mu.Unlock()
}

func (c *Controller) dropPendingSuperseded(record pendingResult) {
	c.completionMu.Lock()
	defer c.completionMu.Unlock()
	c.dropPendingSupersededUnlocked(record)
}

func (c *Controller) dropPendingSupersededUnlocked(record pendingResult) {
	c.mu.Lock()
	if c.pendingResult != nil && (pendingSupersedes(record, *c.pendingResult) || samePendingResult(record, *c.pendingResult)) {
		c.pendingResult = nil
	}
	c.mu.Unlock()
}

func samePendingResult(left, right pendingResult) bool {
	return left.eventID == right.eventID && left.target == right.target && left.result == right.result && left.completedAt.Equal(right.completedAt)
}

func (c *Controller) holdForPending(state *config.State, record pendingResult) {
	current, available := candidateFromState(state)
	c.mu.Lock()
	if record.result == "failed" && !record.retryDue.IsZero() && c.lifecycle == record.runLifecycle && available && current == record.retryTarget {
		c.mu.Unlock()
		return
	}
	cancel := c.detachDueUnlocked()
	if !c.running {
		c.target = candidate{}
		c.nextAttemptAt = time.Time{}
		c.retrying = false
		c.state = runtimeStateFor(state)
	}
	c.mu.Unlock()
	callCancel(cancel)
}

func (c *Controller) clearAttemptUnlocked() func() {
	cancel := c.detachDueUnlocked()
	c.target = candidate{}
	c.nextAttemptAt = time.Time{}
	c.retrying = false
	return cancel
}

func (c *Controller) armAttempt(target candidate, next time.Time, delay time.Duration) {
	c.mu.Lock()
	if c.running || (c.target == target && !c.nextAttemptAt.IsZero()) {
		c.mu.Unlock()
		return
	}
	oldCancel := c.detachDueUnlocked()
	if c.autoEligible && c.target != (candidate{}) && c.target != target {
		c.lifecycle++
	}
	c.autoEligible = true
	c.target = target
	c.nextAttemptAt = next
	c.retrying = false
	c.state = StateScheduled
	token := c.dueToken
	lifecycle := c.lifecycle
	c.mu.Unlock()

	callCancel(oldCancel)
	c.attachDueTimer(target, next, delay, false, StateScheduled, token, lifecycle)
}

func (c *Controller) attachDueTimer(target candidate, next time.Time, delay time.Duration, retrying bool, runtimeState string, token, lifecycle uint64) {
	cancel := c.deps.Schedule(delay, c.Wake)

	c.mu.Lock()
	if c.dueToken == token &&
		c.lifecycle == lifecycle &&
		!c.running &&
		c.target == target &&
		c.nextAttemptAt.Equal(next) &&
		c.retrying == retrying &&
		c.state == runtimeState {
		c.cancelDue = cancel
		cancel = nil
	}
	c.mu.Unlock()
	callCancel(cancel)
}

func (c *Controller) attachRetryDueTimer(target candidate, next time.Time, delay time.Duration, token, lifecycle uint64) *config.State {
	cancel := c.deps.Schedule(delay, c.Wake)
	state, _ := c.loadNormalizedState()
	current, available := candidateFromState(state)
	var oldCancel func()

	c.mu.Lock()
	matches := c.dueToken == token &&
		c.lifecycle == lifecycle &&
		!c.running &&
		c.target == target &&
		c.nextAttemptAt.Equal(next) &&
		c.retrying &&
		c.state == StateRetrying
	if matches && available && current == target {
		c.cancelDue = cancel
		cancel = nil
	} else if matches {
		oldCancel = c.clearAttemptUnlocked()
		c.state = runtimeStateFor(state)
		c.invalidateRunLifecycleUnlocked(lifecycle)
	}
	c.mu.Unlock()

	callCancel(oldCancel)
	callCancel(cancel)
	if state == nil {
		return &config.State{}
	}
	return state
}

func (c *Controller) invalidateRunLifecycleUnlocked(runLifecycle uint64) {
	if c.lifecycle == runLifecycle && c.autoEligible {
		c.lifecycle++
		c.autoEligible = false
	}
}

func (c *Controller) cancelDueTimer() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel := c.detachDueUnlocked()
	c.mu.Unlock()
	callCancel(cancel)
}

func (c *Controller) detachDueUnlocked() func() {
	cancel := c.cancelDue
	c.cancelDue = nil
	c.dueToken++
	return cancel
}

func callCancel(cancel func()) {
	if cancel != nil {
		cancel()
	}
}

func runtimeStateFor(state *config.State) string {
	if state != nil && state.DevNodeAutoUpdateEnabled && state.NodeSource == nodemanifest.SourceDev {
		return StateWatching
	}
	return StateDisabled
}

func (c *Controller) now() time.Time {
	return c.deps.Now().UTC()
}

func (c *Controller) newEventID() string {
	var random [10]byte
	if _, err := cryptorand.Read(random[:]); err == nil {
		return "auto-dev-node-" + hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("auto-dev-node-%d-%d", c.now().UnixNano(), fallbackEventSequence.Add(1))
}

func (c *Controller) publish(state *config.State) {
	if c == nil || c.deps.PatchNodeStatus == nil {
		return
	}
	if state == nil {
		state = &config.State{}
	}
	c.mu.Lock()
	runtimeState := c.state
	targetVersion := c.target.TargetVersion
	nextAttempt := c.nextAttemptAt
	c.mu.Unlock()
	if runtimeState == StateDisabled || runtimeState == StateWatching {
		targetVersion = ""
		nextAttempt = time.Time{}
	}
	completedAt := ""
	if !state.DevNodeAutoUpdateLastCompletedAt.IsZero() {
		completedAt = state.DevNodeAutoUpdateLastCompletedAt.UTC().Format(time.RFC3339)
	}
	nextAttemptAt := ""
	if !nextAttempt.IsZero() {
		nextAttemptAt = nextAttempt.UTC().Format(time.RFC3339)
	}
	patch := map[string]interface{}{
		"dev_node_auto_update_enabled":             state.DevNodeAutoUpdateEnabled,
		"dev_node_auto_update_state":               runtimeState,
		"dev_node_auto_update_target_version":      targetVersion,
		"dev_node_auto_update_next_attempt_at":     nextAttemptAt,
		"dev_node_auto_update_last_event_id":       state.DevNodeAutoUpdateLastEventID,
		"dev_node_auto_update_last_from_version":   state.DevNodeAutoUpdateLastFromVersion,
		"dev_node_auto_update_last_target_version": state.DevNodeAutoUpdateLastTargetVersion,
		"dev_node_auto_update_last_completed_at":   completedAt,
	}
	if state.DevNodeAutoUpdateLastResult != "" {
		patch["dev_node_auto_update_last_success"] = state.DevNodeAutoUpdateLastResult == "success"
	}
	c.deps.PatchNodeStatus(patch)
}
