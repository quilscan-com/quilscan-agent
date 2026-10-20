package actions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/quilscan-com/quilscan-agent/internal/config"
)

type fakeRestarter struct {
	active   bool
	err      error
	restarts int
}

func (f *fakeRestarter) IsActive(string) bool { return f.active }
func (f *fakeRestarter) RestartAndWait(string) error {
	f.restarts++
	return f.err
}

func TestSetNodeDiagnosticsHandler(t *testing.T) {
	peer := "Qm" + strings.Repeat("b", 44)
	base := func(extra map[string]interface{}) map[string]interface{} {
		args := map[string]interface{}{"section": "healthReport", "enabled": true}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	tooMany := []interface{}{}
	for i := 0; i < 33; i++ {
		tooMany = append(tooMany, peer)
	}
	original := "p2p:\n  network: 1 # keep me\n"
	cases := []struct {
		name        string
		config      string
		args        map[string]interface{}
		svc         fakeRestarter
		wantErr     string
		wantMessage string
		wantWrite   bool
		restarts    int
	}{
		{name: "writes without restart", config: original, args: base(map[string]interface{}{"presets": []interface{}{"Black Swan"}, "allowed_peers": []interface{}{peer, peer}, "min_interval_secs": float64(120)}),
			svc: fakeRestarter{active: true}, wantMessage: "restart the node to apply", wantWrite: true},
		{name: "restarts a running node", config: original, args: base(map[string]interface{}{"restart": true}),
			svc: fakeRestarter{active: true}, wantMessage: "node restarted", wantWrite: true, restarts: 1},
		{name: "does not start a stopped node", config: original, args: base(map[string]interface{}{"restart": true}),
			wantMessage: "not running", wantWrite: true},
		{name: "reports a failed restart", config: original, args: base(map[string]interface{}{"restart": true}),
			svc: fakeRestarter{active: true, err: errors.New("boom")}, wantErr: "restart failed", wantWrite: true, restarts: 1},
		{name: "leaves a matching config alone", config: "diagnostics:\n  healthReport:\n    enabled: true\n", args: base(map[string]interface{}{"restart": true}),
			svc: fakeRestarter{active: true}, wantMessage: "unchanged"},
		{name: "refuses unparseable yaml", config: "p2p: [", args: base(nil), wantErr: "parse config"},
		{name: "refuses a non-mapping section", config: "diagnostics:\n  healthReport: true\n", args: base(nil), wantErr: "not a mapping"},
		{name: "rejects an unknown section", config: original, args: base(map[string]interface{}{"section": "other"}), wantErr: "section must be"},
		{name: "rejects a missing enabled", config: original, args: map[string]interface{}{"section": "healthReport"}, wantErr: "enabled must be"},
		{name: "rejects a string enabled", config: original, args: base(map[string]interface{}{"enabled": "true"}), wantErr: "enabled must be"},
		{name: "rejects a string restart", config: original, args: base(map[string]interface{}{"restart": "yes"}), wantErr: "restart must be"},
		{name: "rejects an unknown argument", config: original, args: base(map[string]interface{}{"path": "/x"}), wantErr: "unknown argument"},
		{name: "rejects another section's setting", config: original, args: base(map[string]interface{}{"ttl_secs": float64(600)}), wantErr: "unknown argument"},
		{name: "rejects an interval below the node bound", config: original, args: base(map[string]interface{}{"min_interval_secs": float64(5)}), wantErr: "from 10 to 3600"},
		{name: "rejects a fractional interval", config: original, args: base(map[string]interface{}{"min_interval_secs": 60.5}), wantErr: "from 10 to 3600"},
		{name: "rejects a log setting out of range", config: original, args: base(map[string]interface{}{"section": "logRequests", "max_pending": float64(17)}), wantErr: "from 1 to 16"},
		{name: "rejects an unknown preset", config: original, args: base(map[string]interface{}{"presets": []interface{}{"Someone"}}), wantErr: "unknown peer preset"},
		{name: "rejects a non-base58 peer", config: original, args: base(map[string]interface{}{"allowed_peers": []interface{}{"Qm" + strings.Repeat("0", 44)}}), wantErr: "not a base58"},
		{name: "rejects a short peer", config: original, args: base(map[string]interface{}{"allowed_peers": []interface{}{"QmShort"}}), wantErr: "not a base58"},
		{name: "rejects a peer string instead of a list", config: original, args: base(map[string]interface{}{"allowed_peers": peer}), wantErr: "must be a list"},
		{name: "rejects too many peers", config: original, args: base(map[string]interface{}{"allowed_peers": tooMany}), wantErr: "must be a list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir, backupRoot := t.TempDir(), filepath.Join(t.TempDir(), "backups")
			cfgFile := filepath.Join(cfgDir, "config.yml")
			if err := os.WriteFile(cfgFile, []byte(tc.config), 0o640); err != nil {
				t.Fatal(err)
			}
			if os.Geteuid() == 0 {
				if err := os.Chown(cfgFile, 1234, 2345); err != nil {
					t.Fatal(err)
				}
			}
			var last Status
			handler := NewSetNodeDiagnosticsHandler(SetNodeDiagnosticsDeps{
				BackupRootDir: backupRoot,
				Svc:           &tc.svc,
				LoadState:     func() (*config.State, error) { return &config.State{ConfigPath: cfgDir}, nil },
			})
			err := handler(Command{ID: "cmd-1", Action: "set_node_diagnostics", Args: tc.args}, func(s Status) { last = s })

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || last.Step != "failed" {
					t.Fatalf("err=%v last=%+v, want %q", err, last, tc.wantErr)
				}
			} else if err != nil || last.Step != "done" || !strings.Contains(last.Message, tc.wantMessage) {
				t.Fatalf("err=%v last=%+v, want message %q", err, last, tc.wantMessage)
			}
			if tc.svc.restarts != tc.restarts {
				t.Fatalf("restarts = %d, want %d", tc.svc.restarts, tc.restarts)
			}
			got, _ := os.ReadFile(cfgFile)
			backups, _ := filepath.Glob(filepath.Join(backupRoot, "node-config-*", "config.yml"))
			if !tc.wantWrite {
				if string(got) != tc.config || len(backups) != 0 {
					t.Fatalf("config rewritten or backed up: %d backups\n%s", len(backups), got)
				}
				return
			}
			if !strings.Contains(string(got), "# keep me") || !strings.Contains(string(got), "enabled: true") {
				t.Fatalf("config not updated:\n%s", got)
			}
			if tc.name == "writes without restart" && strings.Count(string(got), peer) != 1 {
				t.Fatalf("peer not deduped:\n%s", got)
			}
			if len(backups) != 1 {
				t.Fatalf("backups = %v", backups)
			}
			if backup, _ := os.ReadFile(backups[0]); string(backup) != tc.config {
				t.Fatalf("backup = %q", backup)
			}
			info, err := os.Stat(cfgFile)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("mode = %v, err = %v", info.Mode(), err)
			}
			if st := info.Sys().(*syscall.Stat_t); os.Geteuid() == 0 && (st.Uid != 1234 || st.Gid != 2345) {
				t.Fatalf("owner = %d:%d", st.Uid, st.Gid)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(cfgDir, ".config.yml.tmp-*")); len(leftovers) != 0 {
				t.Fatalf("temp files left: %v", leftovers)
			}
		})
	}
}

func TestSetNodeDiagnosticsIsMutating(t *testing.T) {
	if !IsMutatingAction("set_node_diagnostics") {
		t.Fatal("set_node_diagnostics must take the mutation gate")
	}
}
