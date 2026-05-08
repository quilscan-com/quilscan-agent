package actions

// NewRescanHandler triggers an immediate node-state re-detection on the
// agent. The trigger argument is a closure rather than an interface
// because the production caller (cmd/agent/main.go) builds the action
// map before the reconcile.Loop is constructed; passing a closure lets
// it forward-declare the reference and resolve at call time.
//
// This exists so the browser refresh button reflects out-of-band
// changes (someone running remove-node.sh in the terminal) without
// waiting up to 60s for the next reconcile tick. Read-only with
// respect to managed state — the only side effect is bumping
// last_verified_at on state.yaml. The bool return is unused: debounce
// is silent because it isn't an error from the user's perspective.
func NewRescanHandler(trigger func() bool) Handler {
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "scanning", Progress: 0.50})
		if trigger != nil {
			_ = trigger()
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
		return nil
	}
}
