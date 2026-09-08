package actions

import (
	"errors"
	"time"
)

// SelfRestarter is the narrow contract restart_agent needs: a single
// Restart(name) call that bounces the supplied service. svcctl.Ctl
// satisfies this directly so cmd/agent wires the platform-appropriate
// implementation in (systemd: --no-block restart; launchd: kickstart -k).
type SelfRestarter interface {
	Restart(name string) error
}

// RestartAgentDeps wires the dependencies for the restart_agent handler.
type RestartAgentDeps struct {
	SelfServiceUnit string        // systemd unit name OR launchd label
	Svc             SelfRestarter // service controller
	Sleep           func(time.Duration)
}

// NewRestartAgentHandler returns a handler that restarts the quilscan-agent
// systemd unit (Linux) or launchd job (macOS). The agent restarts
// itself, so cmd_status `done` is emitted before the restart is scheduled;
// once the service manager kills this PID the WS connection dies and the
// frontend will see a brief offline window before the new process
// reconnects under the same token.
func NewRestartAgentHandler(d RestartAgentDeps) Handler {
	sleep := d.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "preparing", Progress: 0.1})
		emit(Status{ID: c.ID, Step: "restarting", Progress: 0.5})
		// Emit done first so the frame can leave before the service
		// manager SIGTERMs us.
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})

		// Keep the worker occupied through the delay and service-manager
		// invocation so its mutation gate cannot be released prematurely.
		sleep(500 * time.Millisecond)
		if d.Svc == nil {
			return errors.New("Agent restart service is unavailable")
		}
		return d.Svc.Restart(d.SelfServiceUnit)
	}
}
