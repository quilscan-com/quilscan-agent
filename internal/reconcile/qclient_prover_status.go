package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/qclient"
)

// RefreshQClientStatus performs a focused status read without inventory,
// disk usage, manage, or config/state writes.
func (l *Loop) RefreshQClientStatus(commandID string) error {
	l.qclientStatusMu.Lock()
	defer l.qclientStatusMu.Unlock()
	state, err := config.LoadState(l.StatePath)
	if err != nil {
		patch := qclient.StatusPatch(nil, fmt.Errorf("load state: %w", err), time.Now())
		patch["qclient_prover_status_command_id"] = commandID
		l.PatchNodeStatus(patch)
		return err
	}
	_, err = l.refreshQClientStatusLocked(state, commandID)
	return err
}

func (l *Loop) refreshQClientStatusLocked(state *config.State, commandID string) (*qclient.ProverStatus, error) {
	if !l.proverStatusSourceCurrent(state) {
		return nil, config.ErrStaleState
	}
	runner := l.QClientStatusRunner
	if runner == nil {
		runner = qclient.Run
	}
	cfg := l.nodeInfoConfigPath(state)
	req := qclient.RunRequest{BinaryPath: l.qclientBinaryPath(), ConfigPath: cfg, WorkDir: nodeCommandWorkDir(cfg, l.managedConfigDir())}
	status, err := runner(context.Background(), req, 30*time.Second)
	if err == nil && status == nil {
		err = fmt.Errorf("qclient prover status returned no snapshot")
	}
	// A remove/reinstall or config switch during the read invalidates its result.
	if !l.proverStatusSourceCurrent(state) || l.qclientBinaryPath() != req.BinaryPath {
		return nil, config.ErrStaleState
	}
	patch := qclient.StatusPatch(status, err, time.Now())
	if commandID != "" {
		patch["qclient_prover_status_command_id"] = commandID
	}
	l.PatchNodeStatus(patch)
	return status, err
}

func (l *Loop) proverStatusSourceCurrent(source *config.State) bool {
	if !config.StateIsCurrent(l.StatePath, source) {
		return false
	}
	current, err := config.LoadState(l.StatePath)
	return err == nil && current.ConfigPath == source.ConfigPath &&
		current.PeerID == source.PeerID && current.QClientBinaryPath == source.QClientBinaryPath &&
		current.NodeSource == source.NodeSource && current.LastStartedAt.Equal(source.LastStartedAt)
}
