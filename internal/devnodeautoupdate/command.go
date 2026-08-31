package devnodeautoupdate

import (
	"errors"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/actions"
	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
)

var (
	ErrMissingEnabled        = errors.New("enabled must be a boolean")
	ErrDevSourceOnly         = errors.New("Dev Node automatic updates require node source dev")
	ErrControllerUnavailable = errors.New("Dev Node automatic update controller is unavailable")
)

// NewSetEnabledHandler persists the automatic-update preference and updates
// the local status immediately.
func NewSetEnabledHandler(controller *Controller) actions.Handler {
	if controller == nil {
		return func(command actions.Command, emit actions.Emitter) error {
			emitSettingFailure(command, emit, ErrControllerUnavailable)
			return ErrControllerUnavailable
		}
	}
	return func(command actions.Command, emit actions.Emitter) error {
		enabled, ok := command.Args["enabled"].(bool)
		if !ok {
			emitSettingFailure(command, emit, ErrMissingEnabled)
			return ErrMissingEnabled
		}
		updated, err := config.UpdateState(controller.deps.StatePath, func(state *config.State) error {
			if enabled && (state.NodeSource != nodemanifest.SourceDev || state.ConfigPath == "") {
				return ErrDevSourceOnly
			}
			state.DevNodeAutoUpdateEnabled = enabled
			state.DevNodeAutoUpdateRevision++
			return nil
		})
		if err != nil {
			emitSettingFailure(command, emit, err)
			return err
		}
		controller.rememberState(updated)
		if enabled {
			controller.markEnabled()
		} else {
			controller.CancelScheduled()
		}
		controller.publish(updated)
		if enabled {
			controller.Wake()
		}
		if emit != nil {
			emit(actions.Status{ID: command.ID, Action: command.Action, Step: "done", Progress: 1})
		}
		return nil
	}
}

func (c *Controller) markEnabled() {
	c.mu.Lock()
	cancel := c.detachDueUnlocked()
	c.lifecycle++
	c.autoEligible = true
	if c.running {
		c.state = StateUpdating
		c.mu.Unlock()
		callCancel(cancel)
		return
	}
	c.target = candidate{}
	c.nextAttemptAt = time.Time{}
	c.retrying = false
	c.state = StateWatching
	c.mu.Unlock()
	callCancel(cancel)
}

func emitSettingFailure(command actions.Command, emit actions.Emitter, err error) {
	if emit == nil {
		return
	}
	emit(actions.Status{ID: command.ID, Action: command.Action, Step: "failed", Error: err.Error()})
}
