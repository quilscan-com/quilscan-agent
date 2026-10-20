package rpcconfig

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// PeerPreset is a named peer an operator can allow by label instead of
// pasting its peer ID. Add a trusted diagnostics peer with one entry.
type PeerPreset struct {
	Label  string `json:"label"`
	PeerID string `json:"peer_id"`
}

// DiagnosticsPeerPresets lists the bundled diagnostics peers.
var DiagnosticsPeerPresets = []PeerPreset{
	{Label: "Black Swan", PeerID: "Qmdb8jLVCw4jdgtnWzLuUn28ejW4YPbjU5ULVVc2tkXz6D"},
}

// DiagnosticsSetting is one numeric key of a diagnostics section: its
// action argument, its config.yml key and the bounds the node clamps it to.
type DiagnosticsSetting struct {
	Arg, Key string
	Min, Max int64
}

// DiagnosticsSections maps each editable section under the node's
// config.yml `diagnostics` key to its numeric settings. Every section also
// has `enabled` and `allowedPeers`.
var DiagnosticsSections = map[string][]DiagnosticsSetting{
	"healthReport": {
		{Arg: "min_interval_secs", Key: "minIntervalSecs", Min: 10, Max: 3600},
	},
	"logRequests": {
		{Arg: "requests_per_day", Key: "requestsPerDay", Min: 1, Max: 24},
		{Arg: "max_pending", Key: "maxPending", Min: 1, Max: 16},
		{Arg: "ttl_secs", Key: "ttlSecs", Min: 600, Max: 604800},
		{Arg: "max_bundle_bytes", Key: "maxBundleBytes", Min: 65536, Max: 268435456},
	},
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// ValidPeerID reports whether id looks like a base58 libp2p peer ID.
func ValidPeerID(id string) bool {
	return len(id) >= 46 && len(id) <= 64 && strings.Trim(id, base58Alphabet) == ""
}

// DiagnosticsEdit sets keys of one diagnostics section. Peers nil leaves
// allowedPeers as it is; a non-nil slice replaces it. Settings maps YAML
// keys to values.
type DiagnosticsEdit struct {
	Section  string
	Enabled  bool
	Peers    []string
	Settings map[string]int64
}

// ApplyDiagnostics returns raw with edit applied, keeping every other key
// and comment. changed is false when the file already holds the values.
func ApplyDiagnostics(raw []byte, edit DiagnosticsEdit) ([]byte, bool, error) {
	doc, root, err := parseConfigYAML(raw)
	if err != nil {
		return nil, false, err
	}
	diagnostics, err := childMapping(root, "diagnostics")
	if err != nil {
		return nil, false, err
	}
	section, err := childMapping(diagnostics, edit.Section)
	if err != nil {
		return nil, false, err
	}
	changed := setValue(section, "enabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(edit.Enabled)})
	if edit.Peers != nil {
		changed = setValue(section, "allowedPeers", peerListNode(edit.Peers)) || changed
	}
	for _, setting := range DiagnosticsSections[edit.Section] {
		if value, ok := edit.Settings[setting.Key]; ok {
			changed = setValue(section, setting.Key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(value)}) || changed
		}
	}
	if !changed {
		return raw, false, nil
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, false, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, false, fmt.Errorf("encode config: %w", err)
	}
	return out.Bytes(), true, nil
}

// childMapping returns parent[key], creating an empty mapping when it is
// missing or null (the node reads null as the default).
func childMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		value := parent.Content[i+1]
		if value.Kind == yaml.ScalarNode && value.ShortTag() == "!!null" {
			value.Kind, value.Tag, value.Value, value.Style = yaml.MappingNode, "!!map", "", 0
		}
		if value.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("config.yml %s is not a mapping", key)
		}
		return value, nil
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, scalarStringNode(key), child)
	return child, nil
}

// setValue sets mapping[key] to desired unless it already decodes to the
// same value, keeping the existing node's comments. It reports a change.
func setValue(mapping *yaml.Node, key string, desired *yaml.Node) bool {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		current := mapping.Content[i+1]
		var have, want interface{}
		if current.Decode(&have) == nil && desired.Decode(&want) == nil && reflect.DeepEqual(have, want) {
			return false
		}
		current.Kind, current.Tag, current.Value, current.Style, current.Content =
			desired.Kind, desired.Tag, desired.Value, desired.Style, desired.Content
		return true
	}
	mapping.Content = append(mapping.Content, scalarStringNode(key), desired)
	return true
}

// peerListNode lists peers, labelling each preset with a line comment.
func peerListNode(peers []string) *yaml.Node {
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{}}
	for _, peer := range peers {
		item := scalarStringNode(peer)
		if label := presetLabel(peer); label != "" {
			item.LineComment = "# " + label
		}
		list.Content = append(list.Content, item)
	}
	return list
}

func presetLabel(peerID string) string {
	for _, preset := range DiagnosticsPeerPresets {
		if preset.PeerID == peerID {
			return preset.Label
		}
	}
	return ""
}

// DiagnosticsStatus summarises the configured diagnostics for node_status:
// the preset table, and per section whether it is on, how many peers it
// allows and which presets are among them. It reflects config.yml, which
// the node applies on its next start.
func DiagnosticsStatus(raw []byte) map[string]interface{} {
	var cfg struct {
		Diagnostics map[string]struct {
			Enabled      bool     `yaml:"enabled"`
			AllowedPeers []string `yaml:"allowedPeers"`
		} `yaml:"diagnostics"`
	}
	_ = yaml.Unmarshal(raw, &cfg)
	status := map[string]interface{}{"peer_presets": DiagnosticsPeerPresets}
	for name := range DiagnosticsSections {
		section := cfg.Diagnostics[name]
		presets := []string{}
		for _, peer := range section.AllowedPeers {
			if label := presetLabel(peer); label != "" {
				presets = append(presets, label)
			}
		}
		status[name] = map[string]interface{}{
			"enabled":       section.Enabled,
			"allowed_peers": len(section.AllowedPeers),
			"presets":       presets,
		}
	}
	return status
}
