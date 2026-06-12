package actions

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/qclient"
)

type QClientManageActionDeps struct {
	StatePath         string
	QClientBinaryPath string
	ManagedConfigDir  string
	LoadState         func() (*config.State, error)
	Runner            func(context.Context, qclient.ManageActionRequest, time.Duration) (*qclient.ManageActionResult, error)
	AllocationsRunner func(context.Context, qclient.RunRequest, time.Duration) ([]qclient.Allocation, error)
	PatchNodeStatus   func(map[string]interface{})
}

func NewQClientManageActionHandler(d QClientManageActionDeps) Handler {
	return func(c Command, emit Emitter) error {
		action, err := manageActionArg(c.Args)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		filters, err := stringSliceArg(c.Args, "filters")
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		workers, err := workerSliceArg(c.Args, "workers")
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		if requiresManageFilters(action) && len(filters) == 0 {
			err := fmt.Errorf("%s requires at least one filter", action)
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		if requiresManageWorkers(action) && len(workers) == 0 {
			err := fmt.Errorf("%s requires at least one worker", action)
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		state, err := loadRefreshQClientState(RefreshQClientAllocationsDeps{
			StatePath: d.StatePath,
			LoadState: d.LoadState,
		})
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: fmt.Sprintf("load state: %v", err)})
			return err
		}

		cfg := firstNonEmpty(state.ConfigPath, d.ManagedConfigDir)
		baseReq := qclient.RunRequest{
			BinaryPath: firstNonEmpty(d.QClientBinaryPath, state.QClientBinaryPath, "/usr/local/bin/qclient"),
			ConfigPath: cfg,
			WorkDir:    refreshQClientWorkDir(cfg, d.ManagedConfigDir),
		}
		runner := d.Runner
		if runner == nil {
			runner = qclient.RunManageAction
		}

		emit(Status{ID: c.ID, Step: "running_qclient", Progress: 0.20, Message: fmt.Sprintf("%s requested...", qclientActionLabel(action))})
		result, err := runner(context.Background(), qclient.ManageActionRequest{
			RunRequest: baseReq,
			Action:     action,
			Filters:    filters,
			Workers:    workers,
		}, 2*time.Minute)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		if d.PatchNodeStatus != nil {
			if rows := refreshAllocationsAfterManageAction(d, baseReq); rows != nil {
				d.PatchNodeStatus(map[string]interface{}{
					"qclient_allocations":              rows,
					"qclient_allocations_refreshed_at": time.Now().UTC().Format(time.RFC3339),
				})
			}
		}
		message := strings.TrimSpace(result.Output)
		if message == "" {
			message = fmt.Sprintf("%s completed.", qclientActionLabel(action))
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0, Message: message})
		return nil
	}
}

func refreshAllocationsAfterManageAction(d QClientManageActionDeps, req qclient.RunRequest) []qclient.Allocation {
	runner := d.AllocationsRunner
	if runner == nil {
		runner = qclient.RunManageOnce
	}
	rows, err := runner(context.Background(), req, 60*time.Second)
	if err != nil {
		return nil
	}
	return rows
}

func manageActionArg(args map[string]interface{}) (string, error) {
	raw, _ := args["action"].(string)
	action := strings.ToLower(strings.TrimSpace(raw))
	switch action {
	case "join", "leave", "confirm", "reject", "pause", "resume", "manual", "auto":
		return action, nil
	default:
		return "", fmt.Errorf("unsupported qclient manage action %q", raw)
	}
}

func requiresManageFilters(action string) bool {
	switch action {
	case "join", "leave", "confirm", "reject", "pause", "resume":
		return true
	default:
		return false
	}
}

func requiresManageWorkers(action string) bool {
	return action == "manual" || action == "auto"
}

func qclientActionLabel(action string) string {
	switch strings.ToLower(action) {
	case "join":
		return "Join"
	case "leave":
		return "Leave"
	case "confirm":
		return "Confirm"
	case "reject":
		return "Reject"
	case "pause":
		return "Pause"
	case "resume":
		return "Resume"
	case "manual":
		return "Manual"
	case "auto":
		return "Auto"
	default:
		return action
	}
}

func stringSliceArg(args map[string]interface{}, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	values, ok := raw.([]interface{})
	if !ok {
		if s, ok := raw.([]string); ok {
			return compactStringSlice(s), nil
		}
		return nil, fmt.Errorf("%s must be an array", key)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s must contain strings", key)
		}
		out = append(out, s)
	}
	return compactStringSlice(out), nil
}

func compactStringSlice(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func workerSliceArg(args map[string]interface{}, key string) ([]uint32, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	values, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	out := make([]uint32, 0, len(values))
	for _, value := range values {
		switch v := value.(type) {
		case float64:
			if v < 0 || v > float64(^uint32(0)) || v != float64(uint32(v)) {
				return nil, fmt.Errorf("invalid worker id %v", value)
			}
			out = append(out, uint32(v))
		case int:
			if v < 0 {
				return nil, fmt.Errorf("invalid worker id %v", value)
			}
			out = append(out, uint32(v))
		default:
			return nil, fmt.Errorf("%s must contain numbers", key)
		}
	}
	return out, nil
}
