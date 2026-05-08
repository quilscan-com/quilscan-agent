// Package actions implements the agent's hardcoded whitelist of allowed
// commands. Everything outside the switch is rejected with ErrForbidden.
package actions

// Dispatcher routes Command.Action to a registered Handler, rejecting anything
// not pre-registered. The caller wires the allowed handlers at startup.
type Dispatcher struct {
	Handlers map[string]Handler
}

// Dispatch validates and runs a command. Dispatch fires one emit for every
// status transition the handler produces.
func (d *Dispatcher) Dispatch(c Command, emit Emitter) error {
	h, ok := d.Handlers[c.Action]
	if !ok {
		emit(Status{ID: c.ID, Step: "rejected", Error: "forbidden"})
		return ErrForbidden
	}
	return h(c, emit)
}
