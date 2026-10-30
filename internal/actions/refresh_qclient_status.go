package actions

import "fmt"

// NewRefreshQClientStatusHandler reads only prover status. The reconcile loop
// owns the query and publication lock shared with periodic verification.
func NewRefreshQClientStatusHandler(refresh func(string) error) Handler {
	return func(c Command, emit Emitter) error {
		fail := func(err error) error { emit(Status{ID: c.ID, Step: "failed", Error: err.Error()}); return err }
		if len(c.Args) != 0 {
			return fail(fmt.Errorf("refresh_qclient_status accepts no arguments"))
		}
		emit(Status{ID: c.ID, Step: "refreshing_status", Progress: 0.2})
		if refresh == nil {
			return fail(fmt.Errorf("status sampler unavailable"))
		}
		if err := refresh(c.ID); err != nil {
			return fail(err)
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1})
		return nil
	}
}
