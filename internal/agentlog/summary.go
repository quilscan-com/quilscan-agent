// Package agentlog builds short, safe log lines for agent traffic.
package agentlog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SummarizeInbound returns a compact one-line summary for a backend frame.
func SummarizeInbound(raw []byte) string {
	m := map[string]interface{}{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Sprintf("recv invalid_json bytes=%d", len(raw))
	}
	return summarize("recv", m)
}

// SummarizeOutbound returns a compact one-line summary for an agent report.
// Routine log-stream payloads return an empty string to avoid amplifying node
// logs into the agent journal.
func SummarizeOutbound(v interface{}) string {
	m, ok := toMap(v)
	if !ok {
		return "send frame"
	}
	if stringField(m, "type") == "logs" && stringField(m, "error") == "" {
		return ""
	}
	return summarize("send", m)
}

func summarize(direction string, m map[string]interface{}) string {
	typ := stringField(m, "type")
	if typ == "" {
		typ = "unknown"
	}
	parts := []string{direction, "type=" + typ}
	for _, key := range fieldsForType(typ) {
		if val, ok := fieldValue(m, key); ok {
			parts = append(parts, key+"="+val)
		}
	}
	return strings.Join(parts, " ")
}

func fieldsForType(typ string) []string {
	switch typ {
	case "auth":
		return []string{"version", "os", "has_node"}
	case "cmd":
		return []string{"id", "action"}
	case "cmd_status":
		return []string{"id", "action", "step", "progress", "error"}
	case "stream_on", "stream_off":
		return nil
	case "logs_on", "logs_off", "logs":
		return []string{"target", "lines", "error"}
	case "metrics":
		return []string{"node_running", "uptime_sec"}
	case "meta_update":
		return []string{"has_node", "node_version", "peer_id", "node_residues"}
	case "node_status":
		return []string{"has_node", "installed", "node_running", "peer_id", "node_version", "current_node_version", "latest_node_version", "node_update_available"}
	case "rpc_patched", "rpc_patch_failed":
		return []string{"grpc_multiaddr", "rest_multiaddr", "restarted", "error"}
	default:
		return []string{"id", "action", "target", "error"}
	}
}

func toMap(v interface{}) (map[string]interface{}, bool) {
	if m, ok := v.(map[string]interface{}); ok {
		return m, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	m := map[string]interface{}{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
}

func fieldValue(m map[string]interface{}, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return "", false
		}
		return x, true
	case bool:
		return fmt.Sprintf("%t", x), true
	case float64:
		return trimFloat(x), true
	case float32:
		return trimFloat(float64(x)), true
	case int:
		return fmt.Sprintf("%d", x), true
	case int64:
		return fmt.Sprintf("%d", x), true
	case []interface{}:
		return fmt.Sprintf("%d", len(x)), true
	case []string:
		return fmt.Sprintf("%d", len(x)), true
	case map[string]interface{}:
		return summarizeKeys(x), true
	default:
		return fmt.Sprintf("%v", x), true
	}
}

func stringField(m map[string]interface{}, key string) string {
	v, ok := fieldValue(m, key)
	if !ok {
		return ""
	}
	return v
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func summarizeKeys(m map[string]interface{}) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("%d:%s", len(keys), strings.Join(keys, ","))
}
