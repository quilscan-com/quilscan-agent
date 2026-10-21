package rpcconfig

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"gopkg.in/yaml.v3"
)

// DiagnosticsMu serializes health config edits with read-and-publish operations,
// so an older reconcile read cannot replace a command's final readback.
var DiagnosticsMu sync.Mutex

const DiagnosticsReadLimit = 256 * 1024
const MaxHealthPeers = 32

// DiagnosticsStatus describes config on disk, never the running Node's health.
// Missing values in an existing mapping stay null, rather than assuming defaults.
type DiagnosticsStatus struct {
	ReadState    string             `json:"read_state"`
	Error        string             `json:"error"`
	HealthReport HealthReportStatus `json:"healthReport"`
	LogRequests  LogRequestsStatus  `json:"logRequests"`
}
type HealthReportStatus struct {
	Configured       bool      `json:"configured"`
	Enabled          *bool     `json:"enabled"`
	AllowedPeers     *[]string `json:"allowed_peers"`
	MinIntervalSecs  *int64    `json:"min_interval_secs"`
	MissingFields    []string  `json:"missing_fields"`
	ValidationErrors []string  `json:"validation_errors"`
	ReadState        string    `json:"read_state,omitempty"`
}
type DiagnosticsEdit struct {
	Section         string
	RequestsPerDay  *int64
	MaxPending      *int64
	TTLSecs         *int64
	MaxBundleBytes  *int64
	Enabled         bool
	AllowedPeers    *[]string
	MinIntervalSecs *int64
}

func UnavailableDiagnostics() DiagnosticsStatus {
	s := emptyDiagnostics()
	s.ReadState = "unavailable"
	s.Error = "Cannot read Node diagnostics configuration"
	return s
}
func emptyDiagnostics() DiagnosticsStatus {
	return DiagnosticsStatus{ReadState: "ok", HealthReport: HealthReportStatus{MissingFields: []string{}, ValidationErrors: []string{}}, LogRequests: LogRequestsStatus{MissingFields: []string{}, ValidationErrors: []string{}}}
}

// ReadDiagnosticsFile bounds reads and rejects links/nonregular files. Callers
// that publish a snapshot hold DiagnosticsMu through publication.
func ReadDiagnosticsFile(path string) DiagnosticsStatus {
	raw, _, err := ReadDiagnosticsConfigFile(path)
	if err != nil {
		return UnavailableDiagnostics()
	}
	return ReadDiagnostics(raw)
}
func ReadDiagnosticsConfigFile(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, errors.New("cannot inspect config.yml")
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("config.yml must be a regular file, not a symlink")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, errors.New("cannot open config.yml")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, nil, errors.New("config.yml changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, DiagnosticsReadLimit+1))
	if err != nil {
		return nil, nil, errors.New("cannot read config.yml")
	}
	if len(raw) > DiagnosticsReadLimit {
		return nil, nil, errors.New("config.yml exceeds size limit")
	}
	final, err := f.Stat()
	if err != nil || final.Size() != opened.Size() || !final.ModTime().Equal(opened.ModTime()) {
		return nil, nil, errors.New("config.yml changed while reading")
	}
	return raw, final, nil
}
func ReadDiagnostics(raw []byte) DiagnosticsStatus {
	s := emptyDiagnostics()
	_, root, err := diagnosticsDocument(raw)
	if err != nil {
		s.ReadState = "invalid"
		s.Error = err.Error()
		return s
	}
	readLogRequests(root, &s)
	d := mappingValue(root, "diagnostics")
	if d == nil || isNull(d) {
		v := false
		s.HealthReport.Enabled = &v
		return s
	}
	if d.Kind != yaml.MappingNode {
		s.ReadState = "invalid"
		s.Error = "diagnostics must be a YAML mapping"
		return s
	}
	h := mappingValue(d, "healthReport")
	if h == nil || isNull(h) {
		v := false
		s.HealthReport.Enabled = &v
		return s
	}
	s.HealthReport.Configured = true
	report := &s.HealthReport
	if h.Kind != yaml.MappingNode {
		report.ReadState = "invalid"
		report.ValidationErrors = append(report.ValidationErrors, "healthReport must be a YAML mapping")
		return s
	}
	for _, key := range []string{"enabled", "allowedPeers", "minIntervalSecs"} {
		node := mappingValue(h, key)
		if node == nil || isNull(node) {
			report.MissingFields = append(report.MissingFields, key)
			continue
		}
		switch key {
		case "enabled":
			var v bool
			if node.Kind == yaml.ScalarNode && node.Tag == "!!bool" && node.Decode(&v) == nil {
				report.Enabled = &v
			} else {
				report.ValidationErrors = append(report.ValidationErrors, "enabled must be a boolean")
			}
		case "allowedPeers":
			report.AllowedPeers = readDiagnosticsPeers(node, &report.ValidationErrors)
		case "minIntervalSecs":
			var v int64
			readable := node.Kind == yaml.ScalarNode && node.Tag == "!!int" && node.Decode(&v) == nil
			if readable {
				report.MinIntervalSecs = &v
			}
			if !readable || v < 10 || v > 3600 {
				report.ValidationErrors = append(report.ValidationErrors, "minIntervalSecs must be an integer from 10 to 3600")
			}
		}
	}
	return s
}

// Reads retain configured string values for display, while static errors keep
// ApplyDiagnostics strict. Never serialize arbitrary non-string YAML entries.
func readDiagnosticsPeers(node *yaml.Node, validationErrors *[]string) *[]string {
	if node.Kind != yaml.SequenceNode {
		*validationErrors = append(*validationErrors, "allowedPeers must be a list of PeerID strings")
		return nil
	}
	peers := []string{}
	validTypes := true
	for _, p := range node.Content {
		if p.Kind != yaml.ScalarNode || p.Tag != "!!str" {
			validTypes = false
			continue
		}
		peers = append(peers, p.Value)
	}
	if !validTypes {
		*validationErrors = append(*validationErrors, "allowedPeers must be a list of PeerID strings")
		if len(peers) == 0 {
			return nil
		}
	}
	if err := ValidateHealthPeers(peers); err != nil {
		*validationErrors = append(*validationErrors, err.Error())
	}
	return &peers
}
func diagnosticsDocument(raw []byte) (*yaml.Node, *yaml.Node, error) {
	if len(raw) > DiagnosticsReadLimit {
		return nil, nil, errors.New("config.yml exceeds size limit")
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, errors.New("config.yml must contain a YAML mapping")
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, nil, errors.New("config.yml must contain exactly one YAML document")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("config.yml must contain a YAML mapping")
	}
	count := 0
	var check func(*yaml.Node, int) error
	check = func(n *yaml.Node, depth int) error {
		count++
		if depth > 64 || count > 65536 {
			return errors.New("config.yml YAML structure exceeds limits")
		}
		if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!merge" {
			return errors.New("config.yml aliases, anchors and merge keys are unsupported")
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				key := n.Content[i]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
					return errors.New("config.yml has duplicate or unsupported mapping keys")
				}
				seen[key.Value] = true
			}
		}
		for _, child := range n.Content {
			if err := check(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(&doc, 0); err != nil {
		return nil, nil, err
	}
	return &doc, doc.Content[0], nil
}
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }
func putDiagnosticsNode(n *yaml.Node, key string, value *yaml.Node) {
	if old := mappingValue(n, key); old != nil {
		head, line, foot := old.HeadComment, old.LineComment, old.FootComment
		*old = *value
		old.HeadComment = head
		old.LineComment = line
		old.FootComment = foot
		return
	}
	n.Content = append(n.Content, scalarStringNode(key), value)
}
func diagnosticsMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	n := mappingValue(parent, key)
	if n == nil || isNull(n) {
		putDiagnosticsNode(parent, key, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		return mappingValue(parent, key), nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a YAML mapping", key)
	}
	return n, nil
}
func ApplyDiagnostics(raw []byte, edit DiagnosticsEdit) ([]byte, bool, error) {
	if edit.Section == "" {
		edit.Section = "healthReport"
	}
	if edit.Section != "healthReport" && edit.Section != "logRequests" {
		return nil, false, errors.New("unsupported diagnostics section")
	}
	if err := validateLogEdit(edit); err != nil {
		return nil, false, err
	}
	if edit.AllowedPeers != nil {
		peers, err := NormalizeHealthPeers(*edit.AllowedPeers)
		if err != nil {
			return nil, false, err
		}
		edit.AllowedPeers = &peers
	}
	if edit.MinIntervalSecs != nil && (*edit.MinIntervalSecs < 10 || *edit.MinIntervalSecs > 3600) {
		return nil, false, errors.New("minIntervalSecs must be an integer from 10 to 3600")
	}
	if edit.Section == "healthReport" && edit.Enabled && (edit.AllowedPeers == nil || edit.MinIntervalSecs == nil) {
		return nil, false, errors.New("enabling healthReport requires allowed_peers and min_interval_secs")
	}
	doc, root, err := diagnosticsDocument(raw)
	if err != nil {
		return nil, false, err
	}
	d, err := diagnosticsMapping(root, "diagnostics")
	if err != nil {
		return nil, false, err
	}
	h, err := diagnosticsMapping(d, edit.Section)
	if err != nil {
		return nil, false, err
	}
	before, err := yaml.Marshal(doc)
	if err != nil {
		return nil, false, errors.New("cannot encode config.yml")
	}
	putDiagnosticsNode(h, "enabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(edit.Enabled)})
	if edit.AllowedPeers != nil {
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, p := range *edit.AllowedPeers {
			n.Content = append(n.Content, scalarStringNode(p))
		}
		putDiagnosticsNode(h, "allowedPeers", n)
	}
	if edit.Section == "healthReport" && edit.MinIntervalSecs != nil {
		putDiagnosticsNode(h, "minIntervalSecs", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(*edit.MinIntervalSecs, 10)})
	}
	if edit.Section == "logRequests" {
		for _, field := range logEditFields(edit) {
			if field.value != nil {
				putDiagnosticsNode(h, field.key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(*field.value, 10)})
			}
		}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, false, errors.New("cannot encode config.yml")
	}
	status := ReadDiagnostics(out)
	validationErrors, missing := status.HealthReport.ValidationErrors, status.HealthReport.MissingFields
	if edit.Section == "logRequests" {
		validationErrors, missing = status.LogRequests.ValidationErrors, status.LogRequests.MissingFields
	}
	if status.ReadState != "ok" || status.HealthReport.ReadState == "invalid" || status.LogRequests.ReadState == "invalid" || len(validationErrors) > 0 {
		return nil, false, errors.New("edited diagnostics section contains invalid fields")
	}
	if edit.Enabled && len(missing) > 0 {
		return nil, false, errors.New("enabled diagnostics section contains missing fields")
	}
	if bytes.Equal(before, out) {
		return raw, false, nil
	}
	return out, true, nil
}

// ValidateHealthPeers mirrors official .96 PeerId::from_bytes: base58 multihash,
// identity digest <=42 bytes, or SHA256 code with digest <=64 bytes. Even empty
// digests are accepted upstream. This intentionally adds no presets or defaults.
func ValidateHealthPeers(peers []string) error {
	for _, p := range peers {
		if !validDiagnosticsPeerID(strings.TrimSpace(p)) {
			return errors.New("allowedPeers contains an invalid base58 PeerID")
		}
	}
	return nil
}

// NormalizeHealthPeers validates the raw entry bound before trimming and
// deduplicating an explicitly submitted list. Reads preserve disk list values.
func NormalizeHealthPeers(peers []string) ([]string, error) {
	if len(peers) > MaxHealthPeers {
		return nil, fmt.Errorf("allowedPeers supports at most %d entries", MaxHealthPeers)
	}
	if err := ValidateHealthPeers(peers); err != nil {
		return nil, err
	}
	out := []string{}
	seen := map[string]bool{}
	for _, p := range peers {
		p = strings.TrimSpace(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}
func validDiagnosticsPeerID(p string) bool {
	if len(p) == 0 || len(p) > 128 {
		return false
	}
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	n := new(big.Int)
	base := big.NewInt(58)
	for _, c := range p {
		idx := strings.IndexRune(alphabet, c)
		if idx < 0 {
			return false
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(idx)))
	}
	leading := 0
	for leading < len(p) && p[leading] == '1' {
		leading++
	}
	raw := append(make([]byte, leading), n.Bytes()...)
	code, cn := binary.Uvarint(raw)
	if cn <= 0 {
		return false
	}
	length, ln := binary.Uvarint(raw[cn:])
	if ln <= 0 {
		return false
	}
	var buf [10]byte
	if binary.PutUvarint(buf[:], code) != cn || binary.PutUvarint(buf[:], length) != ln {
		return false
	}
	if length > 64 || uint64(len(raw)-cn-ln) != length {
		return false
	}
	return code == 0 && length <= 42 || code == 0x12
}
