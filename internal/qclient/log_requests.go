package qclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/quilscan-com/quilscan-agent/internal/rpcconfig"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const LogMachineOutputLimit = 128 * 1024
const LogBundleMaxBytes = 268435456

var logRequestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var logPreparedTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var logFilterPattern = regexp.MustCompile(`^(?:[0-9a-f]{2}){32,64}$`)

func ValidLogRequestID(s string) bool     { return logRequestIDPattern.MatchString(s) }
func ValidLogPreparedToken(s string) bool { return logPreparedTokenPattern.MatchString(s) }

type LogRequest struct {
	RequestID       string   `json:"request_id"`
	RequesterPeerID string   `json:"requester_peer_id"`
	Status          string   `json:"status"`
	ReceivedUnixMS  uint64   `json:"received_unix_ms"`
	ExpiresUnixMS   uint64   `json:"expires_unix_ms"`
	SinceUnix       uint64   `json:"since_unix"`
	UntilUnix       uint64   `json:"until_unix"`
	Filters         []string `json:"filters"`
	BundleBytes     uint64   `json:"bundle_bytes"`
}
type LogRequestsList struct {
	SchemaVersion  int          `json:"schema_version"`
	Requests       []LogRequest `json:"requests"`
	MaxBundleBytes uint64       `json:"max_bundle_bytes"`
}
type LogRedactedPreview struct {
	LinesRead         uint64            `json:"lines_read"`
	LinesKept         uint64            `json:"lines_kept"`
	SourceKind        string            `json:"source_kind"`
	SourceCount       uint64            `json:"source_count"`
	Redactions        map[string]uint64 `json:"redactions"`
	RedactedSamples   []string          `json:"redacted_samples"`
	SamplesTruncated  bool              `json:"samples_truncated"`
	UnreadableSources uint64            `json:"unreadable_sources"`
	Note              string            `json:"note"`
}
type LogPreview struct {
	SchemaVersion int                `json:"schema_version"`
	Request       LogRequest         `json:"request"`
	PreparedToken string             `json:"prepared_token"`
	ExpiresUnixMS uint64             `json:"expires_unix_ms"`
	SealedBytes   uint64             `json:"sealed_bytes"`
	Preview       LogRedactedPreview `json:"preview"`
}
type LogApprove struct {
	SchemaVersion int    `json:"schema_version"`
	RequestID     string `json:"request_id"`
	Outcome       string `json:"outcome"`
	BundleBytes   uint64 `json:"bundle_bytes"`
	ExpiresUnixMS uint64 `json:"expires_unix_ms"`
}
type LogDeny struct {
	SchemaVersion  int    `json:"schema_version"`
	RequestID      string `json:"request_id"`
	Outcome        string `json:"outcome"`
	PreviousStatus string `json:"previous_status"`
}
type LogDiscard struct {
	SchemaVersion int    `json:"schema_version"`
	RequestID     string `json:"request_id"`
	Outcome       string `json:"outcome"`
}
type LogMachineRequest struct {
	RunRequest
	Action, RequestID, PreparedToken string
}
type LogMachineResult struct {
	List    *LogRequestsList
	Preview *LogPreview
	Approve *LogApprove
	Deny    *LogDeny
	Discard *LogDiscard
}

func (r LogMachineRequest) args() ([]string, error) {
	args := []string{"node", "diagnostics", "machine", "--config-dir", r.ConfigPath, r.Action}
	switch r.Action {
	case "list":
		if r.RequestID != "" || r.PreparedToken != "" {
			return nil, errors.New("list accepts no request or token")
		}
		return args, nil
	case "prepare", "approve", "deny", "discard":
		if !ValidLogRequestID(r.RequestID) {
			return nil, errors.New("request_id must be full lowercase 32 hexadecimal characters")
		}
		args = append(args, r.RequestID)
	default:
		return nil, errors.New("unsupported log request action")
	}
	if r.Action == "approve" || r.Action == "discard" {
		if !ValidLogPreparedToken(r.PreparedToken) {
			return nil, errors.New("prepared_token must be full lowercase 64 hexadecimal characters")
		}
		args = append(args, "--token", r.PreparedToken)
	} else if r.PreparedToken != "" {
		return nil, errors.New("unexpected prepared token")
	}
	return args, nil
}

type boundedLogOutput struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (w *boundedLogOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - w.buf.Len()
	if n > remaining {
		w.overflow = true
		if remaining > 0 {
			w.buf.Write(p[:remaining])
		}
	} else {
		w.buf.Write(p)
	}
	return n, nil
}
func RunLogMachine(ctx context.Context, req LogMachineRequest, timeout time.Duration) (*LogMachineResult, error) {
	if !filepath.IsAbs(req.ConfigPath) || filepath.Clean(req.ConfigPath) == string(filepath.Separator) {
		return nil, errors.New("log request config directory must be a trusted absolute non-root path")
	}
	req.ConfigPath = filepath.Clean(req.ConfigPath)
	args, err := req.args()
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = time.Minute
		if req.Action == "prepare" || req.Action == "approve" {
			timeout = 5 * time.Minute
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := newCommand(ctx, req.BinaryPath, args...)
	cmd.Dir = req.WorkDir
	cmd.Stdin = nil
	cmd.WaitDelay = time.Second
	stdout := &boundedLogOutput{limit: LogMachineOutputLimit}
	stderr := &boundedLogOutput{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("qclient log request timed out or was cancelled; refresh the request queue before any further action")
		}
		diagnostic := stderr.buf.String()
		if strings.Contains(diagnostic, "machine") && (strings.Contains(diagnostic, "unrecognized subcommand") || strings.Contains(diagnostic, "unknown subcommand") || strings.Contains(diagnostic, "unexpected argument")) {
			return nil, errors.New("installed qclient does not support the log request machine interface; update qclient")
		}
		return nil, errors.New("qclient log request command failed; refresh the request queue before any further action")
	}
	if stdout.overflow {
		return nil, errors.New("qclient log request JSON exceeds 128 KiB")
	}
	return ParseLogMachine(stdout.buf.Bytes(), req)
}

// strictLogJSON rejects missing/null fields, duplicate/unknown keys and trailing
// documents. This independently checks the subprocess boundary before publishing.
func strictLogJSON(raw []byte, out interface{}) error {
	if len(raw) > LogMachineOutputLimit || !utf8.Valid(raw) {
		return errors.New("invalid or oversized qclient log request JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	if err := requiredLogFields(raw, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errors.New("invalid qclient log request JSON schema")
	}
	return nil
}
func uniqueJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return errors.New("invalid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return errors.New("invalid JSON object")
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errors.New("duplicate JSON key")
			}
			seen[s] = true
			if err := uniqueJSONValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := uniqueJSONValue(dec); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = dec.Token()
	return err
}
func requiredLogFields(raw []byte, t reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("null qclient log request field")
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return errors.New("expected JSON object")
		}
		if len(fields) != t.NumField() {
			return errors.New("missing or unknown qclient log request field")
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			v, ok := fields[f.Tag.Get("json")]
			if !ok {
				return errors.New("missing qclient log request field")
			}
			if err := requiredLogFields(v, f.Type); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil || items == nil {
			return errors.New("expected JSON array")
		}
		for _, v := range items {
			if err := requiredLogFields(v, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var items map[string]json.RawMessage
		if json.Unmarshal(raw, &items) != nil || items == nil {
			return errors.New("expected JSON object")
		}
		for _, v := range items {
			if err := requiredLogFields(v, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
func validLogStatus(s string) bool {
	switch s {
	case "pending", "ready", "served", "unknown":
		return true
	}
	return false
}
func validLogRequest(r LogRequest) bool {
	if !ValidLogRequestID(r.RequestID) || strings.TrimSpace(r.RequesterPeerID) != r.RequesterPeerID || rpcconfig.ValidateHealthPeers([]string{r.RequesterPeerID}) != nil || !validLogStatus(r.Status) || r.ExpiresUnixMS < r.ReceivedUnixMS || r.UntilUnix < r.SinceUnix || r.UntilUnix-r.SinceUnix > 86400 || len(r.Filters) > 16 || r.BundleBytes > LogBundleMaxBytes {
		return false
	}
	for _, f := range r.Filters {
		if !logFilterPattern.MatchString(f) {
			return false
		}
	}
	return true
}
func ParseLogMachine(raw []byte, req LogMachineRequest) (*LogMachineResult, error) {
	if _, err := req.args(); err != nil {
		return nil, err
	}
	result := &LogMachineResult{}
	invalid := errors.New("invalid qclient log request response; update qclient or refresh the queue")
	switch req.Action {
	case "list":
		var v LogRequestsList
		if err := strictLogJSON(raw, &v); err != nil {
			return nil, invalid
		}
		if v.SchemaVersion != 1 || v.MaxBundleBytes < 65536 || v.MaxBundleBytes > LogBundleMaxBytes || len(v.Requests) > 16 {
			return nil, invalid
		}
		seen := map[string]bool{}
		for _, r := range v.Requests {
			if !validLogRequest(r) || r.BundleBytes > v.MaxBundleBytes || seen[r.RequestID] {
				return nil, invalid
			}
			seen[r.RequestID] = true
		}
		result.List = &v
	case "prepare":
		var v LogPreview
		if err := strictLogJSON(raw, &v); err != nil {
			return nil, invalid
		}
		p := v.Preview
		if v.SchemaVersion != 1 || !validLogRequest(v.Request) || v.Request.RequestID != req.RequestID || v.Request.Status != "pending" || !ValidLogPreparedToken(v.PreparedToken) || v.ExpiresUnixMS == 0 || v.ExpiresUnixMS > v.Request.ExpiresUnixMS || v.SealedBytes == 0 || v.SealedBytes > LogBundleMaxBytes || p.LinesKept > p.LinesRead || len(p.RedactedSamples) > 20 || len(p.Redactions) > 32 || len(p.Note) > 4096 || (p.SourceKind != "files" && p.SourceKind != "journal") {
			return nil, invalid
		}
		for k := range p.Redactions {
			if len(k) == 0 || len(k) > 64 {
				return nil, invalid
			}
		}
		for _, s := range p.RedactedSamples {
			if len(s) > 2048 {
				return nil, invalid
			}
		}
		result.Preview = &v
	case "approve":
		var v LogApprove
		if err := strictLogJSON(raw, &v); err != nil {
			return nil, invalid
		}
		if v.SchemaVersion != 1 || v.RequestID != req.RequestID || v.Outcome != "approved" || v.BundleBytes == 0 || v.BundleBytes > LogBundleMaxBytes || v.ExpiresUnixMS == 0 {
			return nil, invalid
		}
		result.Approve = &v
	case "deny":
		var v LogDeny
		if err := strictLogJSON(raw, &v); err != nil {
			return nil, invalid
		}
		if v.SchemaVersion != 1 || v.RequestID != req.RequestID || v.Outcome != "denied" || !validLogStatus(v.PreviousStatus) {
			return nil, invalid
		}
		result.Deny = &v
	case "discard":
		var v LogDiscard
		if err := strictLogJSON(raw, &v); err != nil {
			return nil, invalid
		}
		if v.SchemaVersion != 1 || v.RequestID != req.RequestID || v.Outcome != "discarded" {
			return nil, invalid
		}
		result.Discard = &v
	default:
		return nil, fmt.Errorf("unsupported log action")
	}
	return result, nil
}
