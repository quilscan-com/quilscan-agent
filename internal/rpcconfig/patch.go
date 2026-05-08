// Package rpcconfig waits for the Quilibrium node's auto-generated config.yml
// to appear after first start, then non-destructively sets the listen
// multiaddrs to localhost so the agent can talk to the node's REST/gRPC API
// without exposing it publicly. Existing user values are preserved.
package rpcconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Default loopback ports the agent expects. Kept here as constants so other
// packages can import them when wiring up RPC clients.
const (
	GRPCPort = 8337
	RESTPort = 8338

	GRPCMultiaddr = "/ip4/127.0.0.1/tcp/8337"
	RESTMultiaddr = "/ip4/127.0.0.1/tcp/8338"

	configFilename = "config.yml"
)

// SystemdController is the narrow service-control contract the patcher needs
// to bounce the node service after editing its config. The name is historical;
// macOS callers provide the same Stop/Start shape via launchd.
type SystemdController interface {
	Stop(unit string) error
	Start(unit string) error
}

// PatchResult reports what the patcher actually did, so callers can decide
// whether to record state and emit telemetry.
type PatchResult struct {
	ConfigFile     string
	GRPCWasEmpty   bool // true if we wrote (i.e. user value was empty/missing)
	RESTWasEmpty   bool
	GRPCFinalValue string
	RESTFinalValue string
	Restarted      bool
}

// WaitAndPatch polls cfgDir/config.yml until it exists (up to timeout), then
// applies the loopback listen addresses to any field that's empty. Existing
// non-empty values are left alone so a user who already exposed the node
// publicly doesn't have it silently re-bound.
//
// If at least one field was actually changed, the node service is bounced
// (stop → 1.5s → start) so the new config takes effect.
func WaitAndPatch(ctl SystemdController, unit, cfgDir string, timeout time.Duration) (*PatchResult, error) {
	cfgFile := filepath.Join(cfgDir, configFilename)

	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(cfgFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for %s", cfgFile)
		}
		time.Sleep(2 * time.Second)
	}

	res := &PatchResult{ConfigFile: cfgFile}
	changed, err := patchYAMLListenAddrs(cfgFile, res)
	if err != nil {
		return res, err
	}

	if !changed {
		return res, nil
	}

	if ctl != nil {
		if err := ctl.Stop(unit); err != nil {
			return res, fmt.Errorf("stop %s: %w", unit, err)
		}
		time.Sleep(1500 * time.Millisecond)
		if err := ctl.Start(unit); err != nil {
			return res, fmt.Errorf("start %s: %w", unit, err)
		}
		res.Restarted = true
	}
	return res, nil
}

// patchYAMLListenAddrs reads cfgFile, fills in the two listen multiaddrs only
// when they are absent or empty, and atomically writes the result. Any other
// field in the file is preserved byte-for-byte (subject to YAML normalisation).
//
// Returns true if at least one field was modified.
func patchYAMLListenAddrs(cfgFile string, res *PatchResult) (bool, error) {
	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}

	// Decode into a generic map so we don't lose unknown fields.
	root := map[string]interface{}{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &root); err != nil {
			return false, fmt.Errorf("parse config: %w", err)
		}
	}

	changed := false

	if applied, current := applyMultiaddr(root, "listenGrpcMultiaddr", GRPCMultiaddr); applied {
		changed = true
		res.GRPCWasEmpty = true
		res.GRPCFinalValue = GRPCMultiaddr
	} else {
		res.GRPCFinalValue = current
	}

	if applied, current := applyMultiaddr(root, "listenRESTMultiaddr", RESTMultiaddr); applied {
		changed = true
		res.RESTWasEmpty = true
		res.RESTFinalValue = RESTMultiaddr
	} else {
		res.RESTFinalValue = current
	}

	if !changed {
		return false, nil
	}

	out, err := yaml.Marshal(root)
	if err != nil {
		return false, fmt.Errorf("encode config: %w", err)
	}
	tmp := cfgFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return false, fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmp, cfgFile); err != nil {
		return false, fmt.Errorf("rename: %w", err)
	}
	return true, nil
}

// applyMultiaddr writes desired to root[key] iff the key is missing or its
// current value is empty. Returns (modified, currentString).
func applyMultiaddr(root map[string]interface{}, key, desired string) (bool, string) {
	cur, ok := root[key]
	if !ok || cur == nil {
		root[key] = desired
		return true, desired
	}
	switch v := cur.(type) {
	case string:
		if v == "" {
			root[key] = desired
			return true, desired
		}
		return false, v
	default:
		// Unexpected type: stringify and treat as a populated user value.
		return false, fmt.Sprintf("%v", v)
	}
}
