package systemd

import (
	"os"
	"path/filepath"
)

// FSOps implements install's SystemdOps with real file + systemctl calls.
type FSOps struct {
	UnitDir string // e.g. "/etc/systemd/system"
}

func (f FSOps) WriteUnit(name, content string) error {
	return os.WriteFile(filepath.Join(f.UnitDir, name), []byte(content), 0o644)
}
func (f FSOps) DaemonReload() error     { return Ctl{}.Reload() }
func (f FSOps) Start(unit string) error { return Ctl{}.Start(unit) }
