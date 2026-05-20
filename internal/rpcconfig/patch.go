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
// when they are absent or empty, and atomically writes the result. Other YAML
// fields are preserved, and top-level keys keep their original order.
//
// Returns true if at least one field was modified.
func patchYAMLListenAddrs(cfgFile string, res *PatchResult) (bool, error) {
	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}

	doc, root, err := parseConfigYAML(raw)
	if err != nil {
		return false, err
	}

	changed := false

	if applied, current := applyMultiaddrNode(root, "listenGrpcMultiaddr", GRPCMultiaddr); applied {
		changed = true
		res.GRPCWasEmpty = true
		res.GRPCFinalValue = GRPCMultiaddr
	} else {
		res.GRPCFinalValue = current
	}

	if applied, current := applyMultiaddrNode(root, "listenRESTMultiaddr", RESTMultiaddr); applied {
		changed = true
		res.RESTWasEmpty = true
		res.RESTFinalValue = RESTMultiaddr
	} else {
		res.RESTFinalValue = current
	}

	if !changed {
		return false, nil
	}

	out, err := yaml.Marshal(doc)
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

func parseConfigYAML(raw []byte) (*yaml.Node, *yaml.Node, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode}
	if len(raw) == 0 {
		root := &yaml.Node{Kind: yaml.MappingNode}
		doc.Content = []*yaml.Node{root}
		return doc, root, nil
	}

	if err := yaml.Unmarshal(raw, doc); err != nil {
		return nil, nil, fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 || doc.Content[0] == nil {
		root := &yaml.Node{Kind: yaml.MappingNode}
		doc.Content = []*yaml.Node{root}
		return doc, root, nil
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("parse config: expected YAML mapping at document root")
	}
	return doc, root, nil
}

// applyMultiaddrNode writes desired to root[key] iff the key is missing or its
// current value is empty. Existing keys are updated in place; missing keys are
// appended so the generated config's original top-level order remains stable.
func applyMultiaddrNode(root *yaml.Node, key, desired string) (bool, string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		valueNode := root.Content[i+1]
		if keyNode == nil || keyNode.Value != key {
			continue
		}
		if valueNode == nil {
			root.Content[i+1] = scalarStringNode(desired)
			return true, desired
		}
		if valueNode.Kind == yaml.ScalarNode && valueNode.Value == "" {
			setScalarStringNode(valueNode, desired)
			return true, desired
		}
		if valueNode.Kind == yaml.ScalarNode {
			return false, valueNode.Value
		}
		return false, valueNode.Value
	}

	root.Content = append(root.Content, scalarStringNode(key), scalarStringNode(desired))
	return true, desired
}

func scalarStringNode(value string) *yaml.Node {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: value,
	}
}

func setScalarStringNode(node *yaml.Node, value string) {
	node.Kind = yaml.ScalarNode
	node.Tag = "!!str"
	node.Value = value
	node.Style = 0
}
