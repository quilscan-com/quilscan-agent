package rpcconfig

import (
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
)

type LogRequestsStatus struct {
	Configured       bool      `json:"configured"`
	Enabled          *bool     `json:"enabled"`
	AllowedPeers     *[]string `json:"allowed_peers"`
	RequestsPerDay   *int64    `json:"requests_per_day"`
	MaxPending       *int64    `json:"max_pending"`
	TTLSecs          *int64    `json:"ttl_secs"`
	MaxBundleBytes   *int64    `json:"max_bundle_bytes"`
	MissingFields    []string  `json:"missing_fields"`
	ValidationErrors []string  `json:"validation_errors"`
	ReadState        string    `json:"read_state,omitempty"`
}
type logEditField struct {
	key      string
	value    *int64
	min, max int64
}

func logEditFields(e DiagnosticsEdit) []logEditField {
	return []logEditField{{"requestsPerDay", e.RequestsPerDay, 1, 24}, {"maxPending", e.MaxPending, 1, 16}, {"ttlSecs", e.TTLSecs, 600, 604800}, {"maxBundleBytes", e.MaxBundleBytes, 65536, 268435456}}
}
func validateLogEdit(e DiagnosticsEdit) error {
	if e.Section != "logRequests" {
		if e.RequestsPerDay != nil || e.MaxPending != nil || e.TTLSecs != nil || e.MaxBundleBytes != nil {
			return errors.New("log request fields require logRequests section")
		}
		return nil
	}
	if e.MinIntervalSecs != nil {
		return errors.New("minIntervalSecs requires healthReport section")
	}
	if e.Enabled && e.AllowedPeers == nil {
		return errors.New("enabling logRequests requires allowed_peers")
	}
	for _, f := range logEditFields(e) {
		if f.value == nil {
			if e.Enabled {
				return fmt.Errorf("enabling logRequests requires %s", f.key)
			}
			continue
		}
		if *f.value < f.min || *f.value > f.max {
			return fmt.Errorf("%s must be an integer from %d to %d", f.key, f.min, f.max)
		}
	}
	return nil
}
func readLogRequests(root *yaml.Node, s *DiagnosticsStatus) {
	r := &s.LogRequests
	absent := false
	d := mappingValue(root, "diagnostics")
	if d == nil || isNull(d) {
		absent = true
	} else if d.Kind != yaml.MappingNode {
		return
	}
	var n *yaml.Node
	if !absent {
		n = mappingValue(d, "logRequests")
		absent = n == nil || isNull(n)
	}
	if absent {
		v := false
		r.Enabled = &v
		return
	}
	r.Configured = true
	if n.Kind != yaml.MappingNode {
		r.ReadState = "invalid"
		r.ValidationErrors = append(r.ValidationErrors, "logRequests must be a YAML mapping")
		return
	}
	for _, key := range []string{"enabled", "allowedPeers", "requestsPerDay", "maxPending", "ttlSecs", "maxBundleBytes"} {
		v := mappingValue(n, key)
		if v == nil || isNull(v) {
			r.MissingFields = append(r.MissingFields, key)
			continue
		}
		switch key {
		case "enabled":
			var b bool
			if v.Kind != yaml.ScalarNode || v.Tag != "!!bool" || v.Decode(&b) != nil {
				r.ValidationErrors = append(r.ValidationErrors, "enabled must be a boolean")
			} else {
				r.Enabled = &b
			}
		case "allowedPeers":
			r.AllowedPeers = readDiagnosticsPeers(v, &r.ValidationErrors)
		default:
			for _, f := range logEditFields(DiagnosticsEdit{}) {
				if f.key != key {
					continue
				}
				var value int64
				readable := v.Kind == yaml.ScalarNode && v.Tag == "!!int" && v.Decode(&value) == nil
				if !readable || value < f.min || value > f.max {
					r.ValidationErrors = append(r.ValidationErrors, fmt.Sprintf("%s must be an integer from %d to %d", key, f.min, f.max))
				}
				if !readable {
					break
				}
				switch key {
				case "requestsPerDay":
					r.RequestsPerDay = &value
				case "maxPending":
					r.MaxPending = &value
				case "ttlSecs":
					r.TTLSecs = &value
				case "maxBundleBytes":
					r.MaxBundleBytes = &value
				}
			}
		}
	}
}
