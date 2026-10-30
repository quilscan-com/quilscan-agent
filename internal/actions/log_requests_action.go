package actions

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/qclient"
	"reflect"
	"strings"
	"sync"
	"time"
)

// LogRequestState binds a prepared token to the exact reviewed request. It is
// process-local and cleared when a restart makes the Node queue obsolete.
type LogRequestState struct {
	mu            sync.Mutex
	generation    uint64
	preview       *qclient.LogPreview
	previewNode   qclient.RunRequest
	previewNative bool
}

func (s *LogRequestState) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.generation++
	s.preview = nil
	s.mu.Unlock()
}
func (s *LogRequestState) snapshot() (uint64, *qclient.LogPreview) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation, s.preview
}
func (s *LogRequestState) begin(mutation bool) (uint64, *qclient.LogPreview, qclient.RunRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	preview := s.preview
	if mutation {
		s.generation++
		s.preview = nil
	}
	return s.generation, preview, s.previewNode, s.previewNative
}

// publish holds the same lock as restart invalidation through the status
// callback. A completed restart cannot be followed by an older queue/preview.
func (s *LogRequestState) publish(generation uint64, preview *qclient.LogPreview, send func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation != generation {
		return false
	}
	if preview != nil {
		s.preview = preview
	}
	send()
	return true
}

// logPreviewCleanup carries only a native capability captured from Agent state.
// It never obtains cleanup authority from command arguments or a queue response.
type logPreviewCleanup struct {
	request    qclient.LogMachineRequest
	generation uint64
}

func (s *LogRequestState) discardObsolete(obsolete *logPreviewCleanup, node qclient.RunRequest, run func(qclient.LogMachineRequest) (*qclient.LogMachineResult, error)) {
	if obsolete == nil || obsolete.request.RunRequest != node {
		return
	}
	if generation, _ := s.snapshot(); generation != obsolete.generation {
		return
	}
	_, _ = run(obsolete.request)
}

// publishList fences mode observations just like previews. Automatic mode also
// invalidates work in flight, so a concurrent prepare cannot revive a token.
func (s *LogRequestState) publishList(generation uint64, list *qclient.LogRequestsList, send func(), invalidated func()) (bool, *logPreviewCleanup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation != generation {
		return false, nil
	}
	clear := list.Automatic
	if s.preview != nil && !clear {
		current := pendingLogRequest(list, s.preview.Request.RequestID)
		clear = current == nil || !reflect.DeepEqual(*current, s.preview.Request)
	}
	var obsolete *logPreviewCleanup
	if clear {
		s.generation++
		if s.preview != nil && s.previewNative {
			obsolete = &logPreviewCleanup{request: qclient.LogMachineRequest{RunRequest: s.previewNode, Action: "discard", RequestID: s.preview.Request.RequestID, PreparedToken: s.preview.PreparedToken}, generation: s.generation}
		}
		s.preview = nil
		invalidated()
	}
	send()
	return true, obsolete
}

func pendingLogRequest(list *qclient.LogRequestsList, id string) *qclient.LogRequest {
	for i := range list.Requests {
		r := &list.Requests[i]
		if r.RequestID == id && r.Status == "pending" && r.ExpiresUnixMS > uint64(time.Now().UnixMilli()) {
			return r
		}
	}
	return nil
}

type QClientLogDeps struct {
	StatePath         string
	QClientBinaryPath string
	ManagedConfigDir  string
	State             *LogRequestState
	LoadState         func() (*config.State, error)
	Runner            func(context.Context, qclient.LogMachineRequest, time.Duration) (*qclient.LogMachineResult, error)
	PatchNodeStatus   func(map[string]interface{})
}
type LogReadStatus struct {
	CommandID   string      `json:"command_id"`
	ReadState   string      `json:"read_state"`
	Error       string      `json:"error"`
	RefreshedAt string      `json:"refreshed_at"`
	Data        interface{} `json:"data"`
}
type LogResultStatus struct {
	CommandID   string `json:"command_id"`
	Action      string `json:"action"`
	RequestID   string `json:"request_id"`
	Outcome     string `json:"outcome"`
	Error       string `json:"error"`
	RefreshedAt string `json:"refreshed_at"`
}

func parseLogAction(c Command) (qclient.LogMachineRequest, error) {
	r := qclient.LogMachineRequest{}
	known := map[string]bool{}
	switch c.Action {
	case "qclient_log_requests":
		r.Action = "list"
	case "qclient_log_preview":
		r.Action = "prepare"
		known["request_id"] = true
	case "qclient_log_approve":
		r.Action = "approve"
		known["request_id"] = true
		known["prepared_token"] = true
		known["confirmed"] = true
	case "qclient_log_deny":
		r.Action = "deny"
		known["request_id"] = true
		known["confirmed"] = true
	case "qclient_log_discard":
		r.Action = "discard"
		known["request_id"] = true
		known["prepared_token"] = true
	default:
		return r, errors.New("unsupported log request action")
	}
	for key := range c.Args {
		if !known[key] {
			return r, errors.New("unknown log request argument")
		}
	}
	if known["request_id"] {
		r.RequestID, _ = c.Args["request_id"].(string)
		if !qclient.ValidLogRequestID(r.RequestID) {
			return r, errors.New("request_id must be full lowercase 32 hexadecimal characters")
		}
	}
	if known["prepared_token"] {
		r.PreparedToken, _ = c.Args["prepared_token"].(string)
		if !qclient.ValidLogPreparedToken(r.PreparedToken) {
			return r, errors.New("prepared_token must be full lowercase 64 hexadecimal characters")
		}
	}
	if known["confirmed"] {
		if confirmed, ok := c.Args["confirmed"].(bool); !ok || !confirmed {
			return r, errors.New("confirmed must be true")
		}
	}
	return r, nil
}
func safeLogRunError(err error) error {
	if err != nil && strings.Contains(err.Error(), "does not support the log request machine interface") {
		return errors.New("installed qclient does not support the log request machine interface; update qclient")
	}
	if err != nil && strings.Contains(err.Error(), "timed out or was cancelled") {
		return errors.New("qclient log request timed out or was cancelled; refresh the request queue before any further action")
	}
	return errors.New("qclient log request unavailable; refresh the request queue and check that qclient supports the machine interface")
}

// validateLogResult also validates injected runners: their typed values cannot
// bypass response identity, schema, bounds or outcome checks.
func validateLogResult(r *qclient.LogMachineResult, req qclient.LogMachineRequest) (*qclient.LogMachineResult, error) {
	if r == nil {
		return nil, errors.New("missing qclient response")
	}
	var value interface{}
	count := 0
	for _, present := range []bool{r.List != nil, r.Preview != nil, r.Approve != nil, r.Deny != nil, r.Discard != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return nil, errors.New("unexpected qclient response")
	}
	switch req.Action {
	case "list":
		value = r.List
	case "prepare":
		value = r.Preview
	case "approve":
		value = r.Approve
	case "deny":
		value = r.Deny
	case "discard":
		value = r.Discard
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("invalid qclient response")
	}
	return qclient.ParseLogMachine(raw, req)
}
func NewQClientLogHandler(d QClientLogDeps) Handler {
	state := d.State
	if state == nil {
		state = &LogRequestState{}
	}
	return func(c Command, emit Emitter) error {
		now := func() string { return time.Now().UTC().Format(time.RFC3339) }
		patch := func(key string, value interface{}) {
			if d.PatchNodeStatus != nil {
				d.PatchNodeStatus(map[string]interface{}{key: value})
			}
		}
		read := func(key string, data interface{}, err error) {
			s := LogReadStatus{CommandID: c.ID, ReadState: "ok", RefreshedAt: now(), Data: data}
			if err != nil {
				s.ReadState = "unavailable"
				s.Error = err.Error()
				s.Data = nil
			}
			patch(key, s)
		}
		result := func(req qclient.LogMachineRequest, outcome string, err error) {
			s := LogResultStatus{CommandID: c.ID, Action: c.Action, RequestID: req.RequestID, Outcome: outcome, RefreshedAt: now()}
			if err != nil {
				s.Error = err.Error()
			}
			patch("qclient_log_result", s)
		}
		req, err := parseLogAction(c)
		terminal := c.Action == "qclient_log_approve" || c.Action == "qclient_log_deny" || c.Action == "qclient_log_discard"
		generation, preview, previewNode, previewNative := state.begin(c.Action == "qclient_log_preview" || terminal)
		if c.Action == "qclient_log_preview" || terminal {
			read("qclient_log_preview", nil, errors.New("Preview invalidated; prepare a fresh preview before approval"))
		}
		fail := func(err error) error {
			if c.Action == "qclient_log_requests" {
				read("qclient_log_requests", nil, err)
			} else if c.Action == "qclient_log_preview" {
				read("qclient_log_preview", nil, err)
			} else if terminal {
				result(req, "failed", err)
			}
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		if err != nil {
			return fail(err)
		}
		if req.Action == "approve" && (preview == nil || preview.Request.RequestID != req.RequestID || preview.PreparedToken != req.PreparedToken || preview.ExpiresUnixMS <= uint64(time.Now().UnixMilli())) {
			return fail(errors.New("approval requires the current unexpired preview for this request and token; prepare again"))
		}
		if req.Action == "discard" && preview != nil && (preview.Request.RequestID != req.RequestID || preview.PreparedToken != req.PreparedToken) {
			return fail(errors.New("discard token does not match the current preview"))
		}
		load := d.LoadState
		if load == nil {
			load = func() (*config.State, error) { return config.LoadState(d.StatePath) }
		}
		cfg, err := load()
		if err != nil || cfg == nil {
			return fail(errors.New("cannot load Agent state"))
		}
		req.RunRequest = qclient.RunRequest{BinaryPath: firstNonEmpty(d.QClientBinaryPath, cfg.QClientBinaryPath, "/usr/local/bin/qclient"), ConfigPath: firstNonEmpty(cfg.ConfigPath, d.ManagedConfigDir), WorkDir: refreshQClientWorkDir(cfg.ConfigPath, d.ManagedConfigDir)}
		runner := d.Runner
		if runner == nil {
			runner = qclient.RunLogMachine
		}
		budget := time.Minute
		manual := req.Action == "prepare" || req.Action == "approve"
		if manual {
			budget = 5 * time.Minute
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		run := func(r qclient.LogMachineRequest) (*qclient.LogMachineResult, error) {
			if ctx.Err() != nil {
				return nil, safeLogRunError(errors.New("timed out or was cancelled"))
			}
			timeout := time.Minute
			if r.Action == "prepare" || r.Action == "approve" {
				timeout = 5 * time.Minute
			}
			deadline, _ := ctx.Deadline()
			if remaining := time.Until(deadline); timeout > remaining {
				timeout = remaining
			}
			v, err := runner(ctx, r, timeout)
			if err != nil {
				return nil, safeLogRunError(err)
			}
			if ctx.Err() != nil {
				return nil, safeLogRunError(errors.New("timed out or was cancelled"))
			}
			v, err = validateLogResult(v, r)
			if err != nil {
				return nil, errors.New("invalid qclient log request response; update qclient or refresh the queue")
			}
			return v, nil
		}
		invalidated := func() {
			read("qclient_log_preview", nil, errors.New("Preview invalidated; prepare a fresh preview before approval"))
		}
		refresh := func() {
			refreshLogList(req, generation, state, run, func(data interface{}, err error) { read("qclient_log_requests", data, err) }, invalidated)
		}
		attempted := false
		var nativePrepared *qclient.LogPreview
		previewPublished := false
		// Only the opaque token produced by our native runner is eligible for cleanup.
		// Never derive paths or cleanup authority from command arguments.
		defer func() {
			cleanup := func(obsolete *qclient.LogPreview) {
				request := req
				request.Action, request.RequestID, request.PreparedToken = "discard", obsolete.Request.RequestID, obsolete.PreparedToken
				_, _ = run(request)
			}
			if manual && !attempted && preview != nil && previewNative && previewNode == req.RunRequest && !(previewPublished && nativePrepared != nil && nativePrepared.PreparedToken == preview.PreparedToken) {
				cleanup(preview)
			}
			if nativePrepared != nil && !previewPublished {
				cleanup(nativePrepared)
			}
		}()
		var currentRequest *qclient.LogRequest
		if manual {
			listReq := req
			listReq.Action, listReq.RequestID, listReq.PreparedToken = "list", "", ""
			fresh, err := run(listReq)
			if err != nil {
				state.publish(generation, nil, func() { read("qclient_log_requests", nil, err) })
				return fail(err)
			}
			published, obsolete := state.publishList(generation, fresh.List, func() { read("qclient_log_requests", fresh.List, nil) }, invalidated)
			if !published {
				return fail(errors.New("Node restarted while checking log request mode; refresh the queue"))
			}
			state.discardObsolete(obsolete, req.RunRequest, run)
			if fresh.List.Automatic {
				return fail(errors.New("automatic log sharing is enabled; manual preview and approval are unavailable"))
			}
			currentRequest = pendingLogRequest(fresh.List, req.RequestID)
			if currentRequest == nil {
				return fail(errors.New("manual action requires a current unexpired pending request; refresh the queue"))
			}
			if req.Action == "approve" && preview.ExpiresUnixMS <= uint64(time.Now().UnixMilli()) {
				return fail(errors.New("preview expired while checking log request mode; prepare again"))
			}
			if req.Action == "approve" && ((previewNode != (qclient.RunRequest{}) && previewNode != req.RunRequest) || !reflect.DeepEqual(preview.Request, *currentRequest)) {
				return fail(errors.New("request changed since preview; prepare again"))
			}
			currentGeneration, _ := state.snapshot()
			if currentGeneration != generation {
				return fail(errors.New("Node changed while checking log request mode; refresh the queue"))
			}
		}

		emit(Status{ID: c.ID, Step: "running_qclient", Progress: 0.2, Message: "Waiting for qclient log request command to finish"})
		attempted = req.Action == "approve"
		v, err := run(req)
		if err != nil {
			if manual || req.Action == "deny" {
				refresh()
			}
			return fail(err)
		}
		if req.Action == "prepare" && d.Runner == nil {
			nativePrepared = v.Preview
		}
		if req.Action == "approve" && (v.Approve.BundleBytes != preview.SealedBytes || v.Approve.ExpiresUnixMS <= uint64(time.Now().UnixMilli())) {
			refresh()
			return fail(errors.New("qclient approval response does not match the reviewed encrypted bundle"))
		}
		if req.Action == "prepare" {
			if !reflect.DeepEqual(v.Preview.Request, *currentRequest) {
				refresh()
				return fail(errors.New("prepared request changed since the queue read; refresh the queue"))
			}
			nowMS := uint64(time.Now().UnixMilli())
			if v.Preview.ExpiresUnixMS <= nowMS || v.Preview.ExpiresUnixMS > uint64(time.Now().Add(10*time.Minute).UnixMilli()) {
				return fail(errors.New("qclient prepared token has expired or has an invalid lifetime; prepare again"))
			}
		}
		currentGeneration, _ := state.snapshot()
		if currentGeneration != generation && !terminal {
			return fail(errors.New("Node restarted while reading log requests; refresh the queue"))
		}
		switch req.Action {
		case "list":
			published, obsolete := state.publishList(generation, v.List, func() { read("qclient_log_requests", v.List, nil) }, invalidated)
			if !published {
				return fail(errors.New("Node restarted while reading log requests; refresh the queue"))
			}
			state.discardObsolete(obsolete, req.RunRequest, run)
		case "prepare":
			if !state.publish(generation, v.Preview, func() {
				previewPublished = true
				state.previewNode, state.previewNative = req.RunRequest, d.Runner == nil
				read("qclient_log_preview", v.Preview, nil)
			}) {
				return fail(errors.New("Node restarted while preparing preview; refresh the queue"))
			}
		case "approve", "deny", "discard":
			outcome := map[string]string{"approve": "approved", "deny": "denied", "discard": "discarded"}[req.Action]
			result(req, outcome, nil)
			if req.Action != "discard" {
				refresh()
			}
		}
		emit(Status{ID: c.ID, Step: "done", Progress: 1, Message: "qclient log request command completed"})
		return nil
	}
}
func refreshLogList(req qclient.LogMachineRequest, generation uint64, state *LogRequestState, run func(qclient.LogMachineRequest) (*qclient.LogMachineResult, error), publish func(interface{}, error), invalidated func()) {
	if current, _ := state.snapshot(); current != generation {
		return
	}
	req.Action = "list"
	req.RequestID = ""
	req.PreparedToken = ""
	v, err := run(req)
	current, _ := state.snapshot()
	if current != generation {
		publish(nil, errors.New("Node restarted; refresh log requests"))
		return
	}
	if err != nil {
		publish(nil, err)
		return
	}
	published, obsolete := state.publishList(generation, v.List, func() { publish(v.List, nil) }, invalidated)
	if !published {
		publish(nil, errors.New("Node restarted; refresh log requests"))
		return
	}
	state.discardObsolete(obsolete, req.RunRequest, run)
}
