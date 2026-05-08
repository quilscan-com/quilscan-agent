package actions

// ServiceController is the narrow contract start/stop handlers use.
type ServiceController interface {
	Start(unit string) error
	Stop(unit string) error
}

// NewStartHandler returns a Handler that starts the given unit.
func NewStartHandler(ctl ServiceController, unit string) Handler {
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "starting", Progress: 0.50})
		if err := ctl.Start(unit); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
		return nil
	}
}

// NewStopHandler returns a Handler that stops the given unit.
func NewStopHandler(ctl ServiceController, unit string) Handler {
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "stopping", Progress: 0.50})
		if err := ctl.Stop(unit); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
		return nil
	}
}
