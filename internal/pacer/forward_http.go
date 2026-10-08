package pacer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	forwardV8Base          = "/v8/management/plugins/pacer"
	forwardHTTPRoot        = forwardV8Base + "/reports/forward/"
	forwardHTTPInputLimit  = 64 << 10
	forwardHTTPOutputLimit = 8 << 20
	forwardHTTPDeadline    = 30 * time.Second
)

var errForwardHTTPInput = errors.New("invalid forward HTTP input")
var errForwardHTTPOutput = errors.New("forward HTTP output limit")

// forwardV8Routes is a pure declaration helper, not registration. Only the
// literal host advertisement is a capability; legacy BasePath/schema is not.
func forwardV8Routes(hostreg pluginapi.ManagementRegistrationRequest, enabled bool) []map[string]string {
	if !enabled || hostreg.PluginManagementBasePath != forwardV8Base {
		return nil
	}
	out := []map[string]string{{"Method": "GET", "Path": forwardHTTPRoot + "status"}, {"Method": "GET", "Path": forwardHTTPRoot + "profiles"}, {"Method": "PATCH", "Path": forwardHTTPRoot + "profiles"}}
	for _, op := range []string{"pin", "report", "requests", "evidence", "export-json", "export-csv"} {
		out = append(out, map[string]string{"Method": "POST", "Path": forwardHTTPRoot + op})
	}
	return out
}

// ForwardHTTPRuntimeStatus is supplied by the lifecycle owner. State must be one
// of unknown/unready/ready/lagging/error/disabled. No raw error text is accepted.
type ForwardHTTPRuntimeStatus struct {
	State         string `json:"state"`
	TargetThrough *int64 `json:"target_through,string,omitempty"`
}

type ForwardHTTPOptions struct {
	Registration pluginapi.ManagementRegistrationRequest
	Enabled      bool
	Status       func(context.Context) (ForwardHTTPRuntimeStatus, error)
	// Notify is caller-owned, never closed here, and sent nonblocking after CAS.
	Notify chan<- struct{}
}

// ForwardHTTP holds no publisher, Plugin fields, private auth or owned capacity.
// Its caller must already be authenticated by CPA's scoped v8 host dispatch.
type ForwardHTTP struct {
	store   *historyreports.ForwardStore
	slots   chan struct{}
	enabled bool
	status  func(context.Context) (ForwardHTTPRuntimeStatus, error)
	notify  chan<- struct{}
}

func NewForwardHTTP(store *historyreports.ForwardStore, sharedSlots chan struct{}, options ForwardHTTPOptions) *ForwardHTTP {
	return &ForwardHTTP{store: store, slots: sharedSlots, enabled: len(forwardV8Routes(options.Registration, options.Enabled)) != 0, status: options.Status, notify: options.Notify}
}

var _ pluginapi.ManagementHandler = (*ForwardHTTP)(nil)

// HandleRPC accepts the actual native management.handle JSON envelope. The
// integration owner decides where to call it; no existing dispatcher is changed.
func (a *ForwardHTTP) HandleRPC(ctx context.Context, method string, raw []byte) (pluginapi.ManagementResponse, error) {
	if method != pluginabi.MethodManagementHandle {
		return forwardHTTPFailure(404, "not_found"), nil
	}
	if len(raw) > 128<<10 {
		return forwardHTTPFailure(413, "request_too_large"), nil
	}
	var wire struct {
		Method         string      `json:"Method"`
		Path           string      `json:"Path"`
		Headers        http.Header `json:"Headers"`
		Query          url.Values  `json:"Query"`
		Body           []byte      `json:"Body"`
		HostCallbackID string      `json:"host_callback_id,omitempty"`
	}
	if forwardHTTPDecode(raw, &wire, true) != nil {
		return forwardHTTPFailure(400, "invalid_request"), nil
	}
	return a.HandleManagement(ctx, pluginapi.ManagementRequest{Method: wire.Method, Path: wire.Path, Headers: wire.Headers, Query: wire.Query, Body: wire.Body})
}

func (a *ForwardHTTP) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if a == nil || !a.enabled {
		return forwardHTTPFailure(404, "not_found"), nil
	}
	op := strings.TrimPrefix(req.Path, forwardHTTPRoot)
	valid := req.Path == forwardHTTPRoot+op && (req.Method == "GET" && (op == "status" || op == "profiles") || req.Method == "PATCH" && op == "profiles" || req.Method == "POST" && (op == "pin" || op == "report" || op == "requests" || op == "evidence" || op == "export-json" || op == "export-csv"))
	if !valid {
		return forwardHTTPFailure(404, "not_found"), nil
	}
	if len(req.Body) > forwardHTTPInputLimit || len(req.Query.Encode()) > 4096 {
		return forwardHTTPFailure(413, "request_too_large"), nil
	}
	if !forwardHTTPHeaders(req.Headers) || (op != "profiles" || req.Method != "GET") && len(req.Query) != 0 || req.Method == "GET" && len(req.Body) != 0 {
		return forwardHTTPFailure(400, "invalid_request"), nil
	}
	if a.store == nil || cap(a.slots) != 2 {
		return forwardHTTPFailure(503, "store_unavailable"), nil
	}
	if ctx == nil || ctx.Err() != nil {
		return forwardHTTPFailure(503, "request_cancelled"), nil
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		return forwardHTTPFailure(503, "busy"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, forwardHTTPDeadline)
	defer cancel()
	return a.serve(ctx, req, op), nil
}

func forwardHTTPHeaders(headers http.Header) bool {
	for key, values := range headers {
		name := http.CanonicalHeaderKey(key)
		if (name != "Content-Type" && name != "Accept") || len(values) != 1 || len(values[0]) > 4096 {
			return false
		}
		for _, r := range values[0] {
			if r < 32 || r == 127 {
				return false
			}
		}
		if name == "Content-Type" {
			kind, _, err := mime.ParseMediaType(values[0])
			if err != nil || kind != "application/json" {
				return false
			}
		}
	}
	return true
}

type forwardHTTPFilter struct {
	Domain   string `json:"domain,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Profile  string `json:"profile,omitempty"`
	Account  string `json:"account,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	KeyHash  string `json:"key_hash,omitempty"`
}

type forwardHTTPPin struct {
	Version    string             `json:"version"`
	Generation string             `json:"generation"`
	Filter     *forwardHTTPFilter `json:"filter"`
}

func forwardHTTPHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func forwardHTTPText(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func (f forwardHTTPFilter) typed(explicit bool) (out historyreports.ForwardFilter, err error) {
	out = historyreports.ForwardFilter{Domain: f.Domain, Profile: f.Profile, Account: f.Account, Provider: f.Provider, Model: f.Model, KeyHash: f.KeyHash}
	if out.Domain == "" && !explicit {
		out.Domain = "combined"
	}
	if out.Domain != "combined" && out.Domain != "native" && out.Domain != "baseline" || !forwardHTTPText(f.Account, 1024) || !forwardHTTPText(f.Profile, 256) || !forwardHTTPText(f.Provider, 256) || !forwardHTTPText(f.Model, 256) || f.KeyHash != "" && !forwardHTTPHash(f.KeyHash) {
		return out, errForwardHTTPInput
	}
	for _, pair := range []struct {
		raw  string
		dest *time.Time
	}{{f.From, &out.From}, {f.To, &out.To}} {
		if pair.raw == "" {
			continue
		}
		t, e := time.Parse(time.RFC3339Nano, pair.raw)
		if e != nil || t.UnixNano() <= 0 || !time.Unix(0, t.UnixNano()).Equal(t) || t.UTC().Format(time.RFC3339Nano) != pair.raw {
			return out, errForwardHTTPInput
		}
		*pair.dest = t
	}
	if !out.From.IsZero() && !out.To.IsZero() && !out.From.Before(out.To) {
		return out, errForwardHTTPInput
	}
	return out, nil
}

func (p *forwardHTTPPin) typed() (historyreports.ForwardPin, error) {
	if p == nil || p.Filter == nil || p.Version != historyreports.ForwardVersion || !forwardHTTPHash(p.Generation) {
		return historyreports.ForwardPin{}, errForwardHTTPInput
	}
	f, err := p.Filter.typed(true)
	return historyreports.ForwardPin{Version: p.Version, Generation: p.Generation, Filter: f}, err
}

func (a *ForwardHTTP) serve(ctx context.Context, req pluginapi.ManagementRequest, op string) pluginapi.ManagementResponse {
	var value any
	var err error
	switch op {
	case "status":
		return a.readStatus(ctx)
	case "profiles":
		if req.Method == "PATCH" {
			return a.editProfiles(ctx, req.Body)
		}
		return a.readProfiles(ctx, req.Query)
	case "pin":
		var q struct {
			Generation string            `json:"generation,omitempty"`
			Filter     forwardHTTPFilter `json:"filter"`
		}
		if forwardHTTPDecode(req.Body, &q, false) != nil || q.Generation != "" && !forwardHTTPHash(q.Generation) {
			return forwardHTTPFailure(400, "invalid_request")
		}
		var f historyreports.ForwardFilter
		f, err = q.Filter.typed(false)
		if err == nil && q.Generation == "" {
			var status historyreports.ForwardStatus
			status, err = a.store.Status(ctx)
			q.Generation = status.Latest // Capture latest once; no per-page refresh.
			if err == nil && q.Generation == "" {
				err = historyreports.ErrForwardPending
			}
		}
		if err == nil {
			value, err = a.store.PinForward(ctx, q.Generation, f)
		}
	case "report", "export-json":
		var q struct {
			Pin *forwardHTTPPin `json:"pin"`
		}
		if forwardHTTPDecode(req.Body, &q, false) != nil {
			return forwardHTTPFailure(400, "invalid_request")
		}
		var pin historyreports.ForwardPin
		pin, err = q.Pin.typed()
		if err == nil {
			if op == "report" {
				value, err = a.store.ForwardReport(ctx, pin)
			} else {
				var out forwardHTTPBuffer
				err = a.store.ExportForwardJSON(ctx, pin, &out)
				value = forwardHTTPDownload{Filename: "pacer-forward.json", MediaType: "application/json", Encoding: "base64", Content: out.Bytes()}
			}
		}
	case "requests", "export-csv":
		var q struct {
			Pin    *forwardHTTPPin `json:"pin"`
			Limit  int             `json:"limit"`
			Cursor string          `json:"cursor,omitempty"`
		}
		if forwardHTTPDecode(req.Body, &q, false) != nil || q.Limit < 1 || q.Limit > 100 || len(q.Cursor) > 4096 {
			return forwardHTTPFailure(400, "invalid_request")
		}
		var pin historyreports.ForwardPin
		pin, err = q.Pin.typed()
		if err == nil {
			if op == "requests" {
				value, err = a.store.ForwardRequests(ctx, pin, q.Limit, q.Cursor)
			} else {
				var out forwardHTTPBuffer
				var next string
				next, err = a.store.ExportForwardCSV(ctx, pin, q.Limit, q.Cursor, &out)
				value = forwardHTTPDownload{Filename: "pacer-forward.csv", MediaType: "text/csv", Encoding: "base64", Content: out.Bytes(), NextCursor: next}
			}
		}
	case "evidence":
		var q struct {
			Pin *forwardHTTPPin `json:"pin"`
			ID  string          `json:"id"`
		}
		if forwardHTTPDecode(req.Body, &q, false) != nil || q.ID == "" || !forwardHTTPText(q.ID, 2048) {
			return forwardHTTPFailure(400, "invalid_request")
		}
		var pin historyreports.ForwardPin
		pin, err = q.Pin.typed()
		if err == nil {
			value, err = a.store.ForwardEvidence(ctx, pin, q.ID)
		}
	}
	if err != nil {
		return forwardHTTPError(err)
	}
	if err = ctx.Err(); err != nil {
		return forwardHTTPError(err)
	}
	return forwardHTTPJSON(200, value)
}

type forwardHTTPDownload struct {
	Filename   string `json:"filename"`
	MediaType  string `json:"media_type"`
	Encoding   string `json:"encoding"`
	Content    []byte `json:"content"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func (a *ForwardHTTP) readStatus(ctx context.Context) pluginapi.ManagementResponse {
	s, err := a.store.Status(ctx)
	if err != nil {
		return forwardHTTPError(err)
	}
	if s.FeedID != "" && !forwardHTTPHash(s.FeedID) || s.Latest != "" && !forwardHTTPHash(s.Latest) || s.Through < 0 || s.BootstrapThrough < 0 {
		return forwardHTTPFailure(503, "store_unavailable")
	}
	runtime := ForwardHTTPRuntimeStatus{State: "unknown"}
	if a.status != nil {
		runtime, err = a.status(ctx)
		if err != nil {
			return forwardHTTPFailure(503, "runtime_unavailable")
		}
	}
	switch runtime.State {
	case "unknown", "unready", "ready", "lagging", "error", "disabled":
	default:
		return forwardHTTPFailure(503, "runtime_unavailable")
	}
	if runtime.TargetThrough != nil && *runtime.TargetThrough < 0 {
		return forwardHTTPFailure(503, "runtime_unavailable")
	}
	if err = ctx.Err(); err != nil {
		return forwardHTTPError(err)
	}
	state := "unready"
	if s.Latest != "" {
		state = "snapshot_available"
	}
	return forwardHTTPJSON(200, struct {
		Version          string                   `json:"version"`
		State            string                   `json:"state"`
		FeedID           string                   `json:"feed_id"`
		ProjectedThrough int64                    `json:"projected_through,string"`
		BootstrapThrough int64                    `json:"bootstrap_through,string"`
		Latest           string                   `json:"latest_generation"`
		Runtime          ForwardHTTPRuntimeStatus `json:"runtime"`
	}{historyreports.ForwardVersion, state, s.FeedID, s.Through, s.BootstrapThrough, s.Latest, runtime})
}

func forwardHTTPDecimal(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != s {
		return 0, errForwardHTTPInput
	}
	return n, nil
}

func (a *ForwardHTTP) readProfiles(ctx context.Context, query url.Values) pluginapi.ManagementResponse {
	values, ok := query["revision"]
	if !ok || len(query) != 1 || len(values) != 1 {
		return forwardHTTPFailure(400, "invalid_request")
	}
	revision, err := forwardHTTPDecimal(values[0])
	if err != nil {
		return forwardHTTPError(err)
	}
	config, digest, err := a.store.ProfilesAt(ctx, revision)
	if err != nil {
		return forwardHTTPError(err)
	}
	if err = ctx.Err(); err != nil {
		return forwardHTTPError(err)
	}
	// Keep original profile/binding/plan fields; quote the potentially wide revision.
	type configWire struct {
		historyreports.ForwardProfileConfig
		Revision int64 `json:"revision,string"`
	}
	return forwardHTTPJSON(200, struct {
		Revision int64      `json:"revision,string"`
		Digest   string     `json:"digest"`
		Config   configWire `json:"config"`
	}{revision, digest, configWire{config, config.Revision}})
}

func (a *ForwardHTTP) editProfiles(ctx context.Context, raw []byte) pluginapi.ManagementResponse {
	var q struct {
		Expected  string `json:"expected_revision"`
		ArchiveID string `json:"archive_id,omitempty"`
		Profile   *struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Provider    string `json:"provider"`
			Selection   bool   `json:"selection,omitempty"`
			Memberships []struct {
				Account string `json:"account"`
				Start   string `json:"start_ms"`
				End     string `json:"end_ms,omitempty"`
			} `json:"memberships"`
		} `json:"profile,omitempty"`
	}
	if forwardHTTPDecode(raw, &q, false) != nil || (q.ArchiveID == "") == (q.Profile == nil) || !forwardHTTPText(q.ArchiveID, 256) {
		return forwardHTTPFailure(400, "invalid_request")
	}
	expected, err := forwardHTTPDecimal(q.Expected)
	if err != nil {
		return forwardHTTPError(err)
	}
	edit := historyreports.ForwardProfileEdit{ArchiveID: q.ArchiveID}
	if p := q.Profile; p != nil {
		if p.ID == "" || p.Name == "" || p.Provider == "" || !forwardHTTPText(p.ID, 256) || !forwardHTTPText(p.Name, 256) || !forwardHTTPText(p.Provider, 256) || p.Memberships == nil || len(p.Memberships) > 100 {
			return forwardHTTPFailure(400, "invalid_request")
		}
		// Preserve retained creation/archive metadata when editing an existing ID.
		config, _, e := a.store.ProfilesAt(ctx, expected)
		if errors.Is(e, sql.ErrNoRows) {
			e = historyreports.ErrForwardConflict
		}
		if e != nil {
			return forwardHTTPError(e)
		}
		for _, old := range config.Profiles {
			if old.ID == p.ID {
				edit.Profile = old
				break
			}
		}
		edit.Profile.ID, edit.Profile.Name, edit.Profile.Provider, edit.Profile.Selection = p.ID, p.Name, p.Provider, p.Selection
		edit.Profile.Accounts, edit.Profile.ActiveAccount = nil, ""
		edit.Profile.Memberships = make([]historyreports.ForwardMembership, 0, len(p.Memberships))
		for _, m := range p.Memberships {
			start, e := forwardHTTPDecimal(m.Start)
			if e != nil || start <= 0 || start > math.MaxInt64/int64(time.Millisecond) || m.Account == "" || !forwardHTTPText(m.Account, 1024) {
				return forwardHTTPFailure(400, "invalid_request")
			}
			var end int64
			if m.End != "" {
				end, e = forwardHTTPDecimal(m.End)
				if e != nil || end <= start || end > math.MaxInt64/int64(time.Millisecond) {
					return forwardHTTPFailure(400, "invalid_request")
				}
			}
			edit.Profile.Memberships = append(edit.Profile.Memberships, historyreports.ForwardMembership{Account: m.Account, Start: start, End: end})
		}
	}
	revision, err := a.store.EditProfiles(ctx, expected, edit)
	if err != nil {
		return forwardHTTPError(err)
	}
	// A missed/closed notification or cancellation after a successful commit must
	// not report a failed write. Durable CAS is authority; publication stays pending.
	queued := forwardHTTPNotify(a.notify)
	return forwardHTTPJSON(202, struct {
		Revision     int64  `json:"revision,string"`
		Publication  string `json:"publication"`
		Notification bool   `json:"notification_enqueued"`
	}{revision, "pending", queued})
}

func forwardHTTPNotify(ch chan<- struct{}) (queued bool) {
	defer func() {
		if recover() != nil {
			queued = false
		}
	}()
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func forwardHTTPFailure(status int, code string) pluginapi.ManagementResponse {
	body, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{code})
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}, Body: body}
}

func forwardHTTPError(err error) pluginapi.ManagementResponse {
	switch {
	case errors.Is(err, errForwardHTTPInput), errors.Is(err, historyreports.ErrInput), errors.Is(err, historyreports.ErrBoundary):
		return forwardHTTPFailure(400, "invalid_request")
	case errors.Is(err, sql.ErrNoRows):
		return forwardHTTPFailure(404, "reference_not_found")
	case errors.Is(err, historyreports.ErrCursor):
		return forwardHTTPFailure(409, "cursor_conflict")
	case errors.Is(err, historyreports.ErrForwardConflict):
		return forwardHTTPFailure(409, "reference_conflict")
	case errors.Is(err, historyreports.ErrForwardPending):
		return forwardHTTPFailure(503, "projection_pending")
	case errors.Is(err, errForwardHTTPOutput), errors.Is(err, historyreports.ErrLimit), errors.Is(err, io.ErrShortWrite):
		return forwardHTTPFailure(413, "response_too_large")
	case errors.Is(err, context.Canceled):
		return forwardHTTPFailure(503, "request_cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return forwardHTTPFailure(503, "deadline_exceeded")
	default:
		return forwardHTTPFailure(503, "store_unavailable")
	}
}

type forwardHTTPBuffer struct{ bytes.Buffer }

func (b *forwardHTTPBuffer) Write(p []byte) (int, error) {
	if len(p) > forwardHTTPOutputLimit-b.Len() {
		return 0, errForwardHTTPOutput
	}
	return b.Buffer.Write(p)
}
func forwardHTTPJSON(status int, value any) pluginapi.ManagementResponse {
	var b forwardHTTPBuffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return forwardHTTPError(err)
	}
	r := forwardHTTPFailure(status, "")
	r.Body = b.Bytes()
	return r
}

// Scan before typed decoding: encoding/json alone accepts duplicate keys,
// case aliases and scalar nulls. Limits apply at every nested schema node.
func forwardHTTPDecode(raw []byte, out any, rpc bool) error {
	if !utf8.Valid(raw) {
		return errForwardHTTPInput
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	if err := forwardHTTPInputValue(d, reflect.TypeOf(out).Elem(), 0, &nodes, rpc); err != nil {
		return errForwardHTTPInput
	}
	if _, err := d.Token(); err != io.EOF {
		return errForwardHTTPInput
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errForwardHTTPInput
	}
	return nil
}

func forwardHTTPInputValue(d *json.Decoder, t reflect.Type, depth int, nodes *int, rpc bool) error {
	*nodes++
	if depth > 12 || *nodes > 4096 {
		return errForwardHTTPInput
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		if rpc && (t.Kind() == reflect.Map || t.Kind() == reflect.Slice) {
			return nil
		}
		return errForwardHTTPInput
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		s, ok := token.(string)
		if !rpc || !ok || len(s) > 128<<10 {
			return errForwardHTTPInput
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		if token != json.Delim('{') {
			return errForwardHTTPInput
		}
		fields := map[string]reflect.Type{}
		if t.Kind() == reflect.Struct {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				if name != "" && name != "-" {
					fields[name] = f.Type
				}
			}
		}
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			name, ok := key.(string)
			if !ok || seen[name] || len(name) > 256 || len(seen) >= 64 {
				return errForwardHTTPInput
			}
			seen[name] = true
			child := fields[name]
			if t.Kind() == reflect.Map {
				child = t.Elem()
			}
			if child == nil {
				return errForwardHTTPInput
			}
			if e = forwardHTTPInputValue(d, child, depth+1, nodes, rpc); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return errForwardHTTPInput
		}
	case reflect.Slice:
		if token != json.Delim('[') {
			return errForwardHTTPInput
		}
		n := 0
		for d.More() {
			n++
			if n > 128 {
				return errForwardHTTPInput
			}
			if e := forwardHTTPInputValue(d, t.Elem(), depth+1, nodes, rpc); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return errForwardHTTPInput
		}
	case reflect.String:
		s, ok := token.(string)
		if !ok || len(s) > 4096 {
			return errForwardHTTPInput
		}
	default:
		if _, ok := token.(json.Delim); ok {
			return errForwardHTTPInput
		}
	}
	return nil
}
