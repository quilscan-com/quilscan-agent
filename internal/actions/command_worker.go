package actions

import (
	"context"
	"errors"
	"sync"
)

const (
	commandQueueCapacity = 8
	readOnlyWorkerCount  = 2
)

var (
	ErrMutationInProgress    = errors.New("another mutation is already in progress")
	ErrCommandQueueFull      = errors.New("command queue is full")
	ErrAgentRestartScheduled = errors.New("Agent restart is already scheduled")
)

type queuedCommand struct {
	command Command
	release func()
	restart bool
}

type CommandWorker struct {
	ctx        context.Context
	dispatcher *Dispatcher
	gate       *MutationGate
	emit       Emitter
	completed  func(Command, error)
	mutations  chan queuedCommand
	readOnly   chan queuedCommand
	recovery   chan queuedCommand
	restartMu  sync.Mutex
	restarting bool
}

func NewCommandWorker(ctx context.Context, dispatcher *Dispatcher, gate *MutationGate, emit Emitter, completed func(Command, error)) *CommandWorker {
	if gate == nil {
		gate = &MutationGate{}
	}
	worker := &CommandWorker{
		ctx:        ctx,
		dispatcher: dispatcher,
		gate:       gate,
		emit:       emit,
		completed:  completed,
		mutations:  make(chan queuedCommand, 1),
		readOnly:   make(chan queuedCommand, commandQueueCapacity),
		recovery:   make(chan queuedCommand, 1),
	}
	go worker.run(worker.mutations)
	for index := 0; index < readOnlyWorkerCount; index++ {
		go worker.run(worker.readOnly)
	}
	go worker.run(worker.recovery)
	return worker
}

func (w *CommandWorker) Submit(command Command) error {
	if command.Action == "restart_agent" {
		return w.submitRestart(command)
	}
	if !IsMutatingAction(command.Action) {
		return w.enqueue(w.readOnly, queuedCommand{command: command})
	}
	return w.submitWithGate(command)
}

func (w *CommandWorker) submitRestart(command Command) error {
	recovery, _ := command.Args["recovery"].(bool)
	blockingCmdID, _ := command.Args["blocking_cmd_id"].(string)
	if !recovery {
		return w.submitNormalRestart(command)
	}
	owner := w.gate.Owner()
	if blockingCmdID == "" || (owner.CmdID != "" && blockingCmdID != owner.CmdID) {
		return w.rejectMutation(command, owner)
	}
	var release func()
	if owner == (MutationOwner{}) {
		acquiredRelease, blocker, ok := w.gate.TryAcquire(MutationOwner{Action: command.Action, CmdID: command.ID})
		if ok {
			release = acquiredRelease
		} else if blocker.CmdID != blockingCmdID {
			return w.rejectMutation(command, blocker)
		}
	}
	if !w.reserveRestart() {
		if release != nil {
			release()
		}
		return w.rejectRestart(command)
	}
	err := w.enqueue(w.recovery, queuedCommand{command: command, release: release, restart: true})
	if err != nil {
		w.clearRestart()
		if release != nil {
			release()
		}
	}
	return err
}

func (w *CommandWorker) submitNormalRestart(command Command) error {
	release, blocker, ok := w.gate.TryAcquire(MutationOwner{Action: command.Action, CmdID: command.ID})
	if !ok {
		return w.rejectMutation(command, blocker)
	}
	if !w.reserveRestart() {
		release()
		return w.rejectRestart(command)
	}
	err := w.enqueue(w.mutations, queuedCommand{command: command, release: release, restart: true})
	if err != nil {
		w.clearRestart()
		release()
	}
	return err
}

func (w *CommandWorker) submitWithGate(command Command) error {
	release, blocker, ok := w.gate.TryAcquire(MutationOwner{Action: command.Action, CmdID: command.ID})
	if !ok {
		return w.rejectMutation(command, blocker)
	}
	err := w.enqueue(w.mutations, queuedCommand{command: command, release: release})
	if err != nil {
		release()
	}
	return err
}

func (w *CommandWorker) rejectMutation(command Command, blocker MutationOwner) error {
	w.emitStatus(Status{
		ID:             command.ID,
		Action:         command.Action,
		Step:           "rejected",
		Error:          ErrMutationInProgress.Error(),
		BlockingCmdID:  blocker.CmdID,
		BlockingAction: blocker.Action,
	})
	w.complete(command, ErrMutationInProgress)
	return ErrMutationInProgress
}

func (w *CommandWorker) rejectRestart(command Command) error {
	w.emitStatus(Status{ID: command.ID, Action: command.Action, Step: "rejected", Error: ErrAgentRestartScheduled.Error()})
	w.complete(command, ErrAgentRestartScheduled)
	return ErrAgentRestartScheduled
}

func (w *CommandWorker) enqueue(queue chan queuedCommand, item queuedCommand) error {
	select {
	case <-w.ctx.Done():
		w.complete(item.command, w.ctx.Err())
		return w.ctx.Err()
	case queue <- item:
		return nil
	default:
		w.emitStatus(Status{ID: item.command.ID, Action: item.command.Action, Step: "rejected", Error: ErrCommandQueueFull.Error()})
		w.complete(item.command, ErrCommandQueueFull)
		return ErrCommandQueueFull
	}
}

func (w *CommandWorker) run(queue <-chan queuedCommand) {
	for {
		select {
		case <-w.ctx.Done():
			return
		case item := <-queue:
			w.execute(item)
		}
	}
}

func (w *CommandWorker) execute(item queuedCommand) {
	if item.release != nil {
		defer item.release()
	}
	var err error
	if w.dispatcher == nil {
		err = errors.New("command dispatcher is unavailable")
	} else {
		err = w.dispatcher.Dispatch(item.command, func(status Status) {
			status.ID = item.command.ID
			status.Action = item.command.Action
			w.emitStatus(status)
		})
	}
	if item.restart && err != nil {
		w.clearRestart()
	}
	w.complete(item.command, err)
}

func (w *CommandWorker) reserveRestart() bool {
	w.restartMu.Lock()
	defer w.restartMu.Unlock()
	if w.restarting {
		return false
	}
	w.restarting = true
	return true
}

func (w *CommandWorker) clearRestart() {
	w.restartMu.Lock()
	w.restarting = false
	w.restartMu.Unlock()
}

func (w *CommandWorker) emitStatus(status Status) {
	if w.emit != nil {
		w.emit(status)
	}
}

func (w *CommandWorker) complete(command Command, err error) {
	if w.completed != nil {
		w.completed(command, err)
	}
}
