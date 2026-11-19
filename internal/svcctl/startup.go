package svcctl

import (
	"fmt"
	"time"
)

// CheckReleaseStartup waits for the launched master process to survive the
// startup window. The service manager may acknowledge Start before the
// binary has completed its signature check.
func CheckReleaseStartup(unit string, mainPID func() (int, error)) error {
	return waitForStableProcess(unit, mainPID, 25*time.Second, 15*time.Second, 250*time.Millisecond)
}

func waitForStableProcess(unit string, mainPID func() (int, error), startTimeout, stableFor, interval time.Duration) error {
	deadline := time.Now().Add(startTimeout)
	var originalPID int
	var stableUntil time.Time
	for {
		pid, err := mainPID()
		if err != nil {
			return fmt.Errorf("%s failed during startup: %w", unit, err)
		}
		now := time.Now()
		if originalPID == 0 {
			if pid > 0 {
				originalPID = pid
				stableUntil = now.Add(stableFor)
			} else if !now.Before(deadline) {
				return fmt.Errorf("%s did not launch a process within %s", unit, startTimeout)
			}
		} else {
			if pid != originalPID {
				return fmt.Errorf("%s process %d exited during startup (current PID %d)", unit, originalPID, pid)
			}
			if !now.Before(stableUntil) {
				return nil
			}
		}
		time.Sleep(interval)
	}
}
