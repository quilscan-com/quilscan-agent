package reconcile

import (
	"path/filepath"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/rpcconfig"
)

// refreshNodeDiagnostics publishes a fresh, limited view of the actual config.
// Failed reads deliberately replace old values, including a stale enabled flag.
func (l *Loop) refreshNodeDiagnostics() {
	rpcconfig.DiagnosticsMu.Lock()
	defer rpcconfig.DiagnosticsMu.Unlock()
	status := rpcconfig.UnavailableDiagnostics()
	state, err := config.LoadState(l.StatePath)
	if err == nil {
		dir := l.managedConfigDir()
		if state.ConfigPath != "" {
			dir = state.ConfigPath
		}
		if filepath.IsAbs(dir) {
			status = rpcconfig.ReadDiagnosticsFile(filepath.Join(filepath.Clean(dir), "config.yml"))
		}
	}
	l.updateNodeStatus(map[string]interface{}{"node_diagnostics": status})
}
