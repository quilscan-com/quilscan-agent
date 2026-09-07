package actions

import "errors"

// ErrNodeNotRunning rejects restart_node when the service is not active.
var ErrNodeNotRunning = errors.New("node is not running")

// NodeRestarter is the service-manager seam used by restart_node.
type NodeRestarter interface {
	Restart(name string) error
	IsActive(name string) bool
}

// RestartNodeDeps configures the managed Node service restart.
type RestartNodeDeps struct {
	UnitName string
	Svc      NodeRestarter
}

// NewRestartNodeHandler asks the platform service manager to restart a
// currently running Node. A done status means the manager accepted the request;
// normal reconciliation observes the resulting Node state asynchronously.
func NewRestartNodeHandler(deps RestartNodeDeps) Handler {
	return func(command Command, emit Emitter) error {
		if deps.Svc == nil || !deps.Svc.IsActive(deps.UnitName) {
			emit(Status{ID: command.ID, Step: "rejected", Error: ErrNodeNotRunning.Error()})
			return ErrNodeNotRunning
		}
		emit(Status{ID: command.ID, Step: "restarting", Progress: 0.5})
		if err := deps.Svc.Restart(deps.UnitName); err != nil {
			emit(Status{ID: command.ID, Step: "failed", Error: err.Error()})
			return err
		}
		emit(Status{ID: command.ID, Step: "done", Progress: 1})
		return nil
	}
}
