package actions

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/qclient"
)

type RefreshQClientAllocationsDeps struct {
	StatePath         string
	QClientBinaryPath string
	ManagedConfigDir  string
	LoadState         func() (*config.State, error)
	Runner            func(context.Context, qclient.RunRequest, time.Duration) ([]qclient.Allocation, error)
	PatchNodeStatus   func(map[string]interface{})
}

func NewRefreshQClientAllocationsHandler(d RefreshQClientAllocationsDeps) Handler {
	return func(c Command, emit Emitter) error {
		emit(Status{ID: c.ID, Step: "refreshing_allocations", Progress: 0.20})

		state, err := loadRefreshQClientState(d)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("load state: %v", err)})
			return err
		}

		cfg := firstNonEmpty(state.ConfigPath, d.ManagedConfigDir)
		req := qclient.RunRequest{
			BinaryPath: firstNonEmpty(d.QClientBinaryPath, state.QClientBinaryPath, "/usr/local/bin/qclient"),
			ConfigPath: cfg,
			WorkDir:    refreshQClientWorkDir(cfg, d.ManagedConfigDir),
		}
		runner := d.Runner
		if runner == nil {
			runner = qclient.RunManageOnce
		}
		rows, err := runner(context.Background(), req, 60*time.Second)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		if d.PatchNodeStatus != nil {
			d.PatchNodeStatus(map[string]interface{}{
				"qclient_allocations":              rows,
				"qclient_allocations_refreshed_at": time.Now().UTC().Format(time.RFC3339),
			})
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
		return nil
	}
}

func loadRefreshQClientState(d RefreshQClientAllocationsDeps) (*config.State, error) {
	if d.LoadState != nil {
		return d.LoadState()
	}
	return config.LoadState(d.StatePath)
}

func refreshQClientWorkDir(configPath, managedConfigDir string) string {
	if configPath != "" {
		return filepath.Dir(filepath.Clean(configPath))
	}
	if managedConfigDir != "" {
		return filepath.Dir(filepath.Clean(managedConfigDir))
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
