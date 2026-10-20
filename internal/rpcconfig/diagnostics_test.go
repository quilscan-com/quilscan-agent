package rpcconfig

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const blackSwan = "Qmdb8jLVCw4jdgtnWzLuUn28ejW4YPbjU5ULVVc2tkXz6D"

var testPeer = "Qm" + strings.Repeat("a", 44)

func section(t *testing.T, out []byte, name string) map[string]interface{} {
	t.Helper()
	var cfg struct {
		Diagnostics map[string]map[string]interface{} `yaml:"diagnostics"`
	}
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, out)
	}
	return cfg.Diagnostics[name]
}

func TestApplyDiagnostics(t *testing.T) {
	existing := "p2p:\n  network: 1 # keep me\ndiagnostics:\n  healthReport:\n    # interval comment\n    enabled: false\n    allowedPeers: [" + testPeer + "]\n    minIntervalSecs: 60\n  logRequests:\n    enabled: true\n"
	cases := []struct {
		name  string
		input string
		edit  DiagnosticsEdit
		check func(t *testing.T, out []byte)
	}{
		{"creates the section", "p2p:\n  network: 1 # keep me\n", DiagnosticsEdit{Section: "healthReport", Enabled: true},
			func(t *testing.T, out []byte) {
				if !strings.HasPrefix(string(out), "p2p:\n  network: 1 # keep me\ndiagnostics:\n  healthReport:\n    enabled: true\n") {
					t.Fatalf("unexpected output:\n%s", out)
				}
			}},
		{"replaces a null section", "diagnostics: null\n", DiagnosticsEdit{Section: "logRequests", Enabled: true},
			func(t *testing.T, out []byte) {
				if got := section(t, out, "logRequests")["enabled"]; got != true {
					t.Fatalf("enabled = %v\n%s", got, out)
				}
			}},
		{"updates an existing section", existing, DiagnosticsEdit{Section: "healthReport", Enabled: true, Settings: map[string]int64{"minIntervalSecs": 120}},
			func(t *testing.T, out []byte) {
				got := section(t, out, "healthReport")
				want := map[string]interface{}{"enabled": true, "allowedPeers": []interface{}{testPeer}, "minIntervalSecs": 120}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("healthReport = %v", got)
				}
				for _, keep := range []string{"# keep me", "# interval comment", "logRequests:\n    enabled: true"} {
					if !strings.Contains(string(out), keep) {
						t.Fatalf("lost %q:\n%s", keep, out)
					}
				}
			}},
		{"replaces peers and labels presets", existing, DiagnosticsEdit{Section: "healthReport", Peers: []string{blackSwan, testPeer}},
			func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), "- "+blackSwan+" # Black Swan\n") {
					t.Fatalf("preset not labelled:\n%s", out)
				}
				if got := section(t, out, "healthReport")["allowedPeers"]; !reflect.DeepEqual(got, []interface{}{blackSwan, testPeer}) {
					t.Fatalf("allowedPeers = %v", got)
				}
			}},
		{"clears peers", existing, DiagnosticsEdit{Section: "healthReport", Peers: []string{}},
			func(t *testing.T, out []byte) {
				if got := section(t, out, "healthReport")["allowedPeers"]; !reflect.DeepEqual(got, []interface{}{}) {
					t.Fatalf("allowedPeers = %v", got)
				}
			}},
		{"writes settings in table order", "", DiagnosticsEdit{Section: "logRequests", Settings: map[string]int64{"maxBundleBytes": 65536, "requestsPerDay": 2, "ttlSecs": 600, "maxPending": 3}},
			func(t *testing.T, out []byte) {
				want := "diagnostics:\n  logRequests:\n    enabled: false\n    requestsPerDay: 2\n    maxPending: 3\n    ttlSecs: 600\n    maxBundleBytes: 65536\n"
				if string(out) != want {
					t.Fatalf("got:\n%s", out)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, err := ApplyDiagnostics([]byte(tc.input), tc.edit)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			tc.check(t, out)
			again, changed, err := ApplyDiagnostics(out, tc.edit)
			if err != nil || changed || string(again) != string(out) {
				t.Fatalf("second apply changed=%v err=%v\n%s", changed, err, again)
			}
		})
	}
}

func TestApplyDiagnosticsRefuses(t *testing.T) {
	for _, input := range []string{"p2p: [", "- a\n", "diagnostics: 5\n", "diagnostics:\n  healthReport: [1]\n", "x: &a {}\ndiagnostics: *a\n"} {
		if _, _, err := ApplyDiagnostics([]byte(input), DiagnosticsEdit{Section: "healthReport", Enabled: true}); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestDiagnosticsStatus(t *testing.T) {
	status := DiagnosticsStatus([]byte("diagnostics:\n  healthReport:\n    enabled: true\n    allowedPeers: [" + blackSwan + ", " + testPeer + "]\n"))
	want := map[string]interface{}{
		"peer_presets": DiagnosticsPeerPresets,
		"healthReport": map[string]interface{}{"enabled": true, "allowed_peers": 2, "presets": []string{"Black Swan"}},
		"logRequests":  map[string]interface{}{"enabled": false, "allowed_peers": 0, "presets": []string{}},
	}
	if !reflect.DeepEqual(status, want) {
		t.Fatalf("status = %#v", status)
	}
	if got := DiagnosticsStatus([]byte("diagnostics: [")); !reflect.DeepEqual(got["healthReport"], want["logRequests"]) {
		t.Fatalf("unparseable config status = %#v", got)
	}
}
