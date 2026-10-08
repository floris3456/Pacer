package pacer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var forwardHTTPAt = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

type forwardHTTPFixture struct {
	store   *historyreports.ForwardStore
	feed    *measurement.Store
	book    string
	pin     historyreports.ForwardPin
	slots   chan struct{}
	adapter *ForwardHTTP
	records []measurement.Record
}

func forwardHTTPNative(t *testing.T, id string, at time.Time) measurement.Record {
	t.Helper()
	wire := map[string]any{"RequestID": id, "LifecycleRequestID": "shared-logical", "Provider": "claude", "AuthID": "synthetic-auth", "AuthIndex": "index", "Model": "claude-opus-5-5", "ResponseServiceTier": "default", "RequestedAt": at, "Latency": int64(time.Second), "Failed": false, "AccountingVersion": 2,
		"CachePolicy":    map[string]any{"dispatched": true, "upstream_started_at": at},
		"TokenBreakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 1000000, "unclassified_tokens": 0, "input": map[string]any{"total_tokens": 1000000, "uncached_tokens": 0, "cache_read_tokens": 0, "cache_write_tokens": 1000000}, "output": map[string]any{"total_tokens": 0, "non_reasoning_tokens": 0, "reasoning_tokens": 0}},
		"Detail":         map[string]any{"InputTokens": 0, "CachedTokens": 0, "CacheReadTokens": 0, "CacheCreationTokens": 1000000, "OutputTokens": 0, "ReasoningTokens": 0, "TotalTokens": 1000000, "CacheCreation": map[string]any{"quality": "complete", "total_tokens": 1000000, "ephemeral_5m_input_tokens": 250000, "ephemeral_1h_input_tokens": 750000, "unknown_tokens": 0}},
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	r, err := measurement.FromNative(raw, measurement.NativeCredential{Provider: "claude", AccountID: "native-account", CredentialRef: "credential", AuthID: "synthetic-auth", AuthIndex: "index"}, "producer")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newForwardHTTPFixture(t *testing.T) *forwardHTTPFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "measurement.sqlite")+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	feed, err := measurement.Open(ctx, db, measurement.Options{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := historyreports.OpenForward(ctx, filepath.Join(dir, "forward.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	book, err := store.RegisterForwardBook(ctx, historyreports.ForwardBook{ID: "synthetic-opus-v1", Book: historyreports.ForwardComparisonBook{ID: "synthetic-opus-v1", Policy: "profile-api-value-v3", Prices: map[string]historyreports.ForwardModelPrice{"claude-opus-5-5": {Prompt: 4, Completion: 20, CacheRead: .2}}}, Qualifications: []historyreports.ForwardPriceQualification{{SourceID: "producer", Provider: "claude", Model: "claude-opus-5-5", AccountID: "native-account", CredentialRef: "credential"}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &forwardHTTPFixture{store: store, feed: feed, book: book, slots: make(chan struct{}, 2)}
	for i := 0; i < 2; i++ {
		r := forwardHTTPNative(t, fmt.Sprint(i), forwardHTTPAt.Add(time.Duration(i)*time.Nanosecond))
		if _, err = feed.PutRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
		f.records = append(f.records, r)
	}
	f.pin = f.publish(t, 0)
	f.adapter = NewForwardHTTP(store, f.slots, ForwardHTTPOptions{Enabled: true, Registration: pluginapi.ManagementRegistrationRequest{PluginManagementBasePath: forwardV8Base}})
	return f
}

func (f *forwardHTTPFixture) publish(t *testing.T, revision int64) historyreports.ForwardPin {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		done, err := f.feed.BootstrapReportFeed(ctx, measurement.ReportBudget{Rows: 3})
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	head, err := f.feed.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := f.store.AdvanceForward(ctx, f.feed, head, measurement.ReportBudget{Rows: 3})
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	pin, err := f.store.SealForward(ctx, historyreports.ForwardSeal{Head: head, BookDigest: f.book, ProfileRevision: revision, At: forwardHTTPAt.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

func forwardHTTPCall(t *testing.T, a *ForwardHTTP, method, op string, value any, want int) pluginapi.ManagementResponse {
	t.Helper()
	var body []byte
	if value != nil {
		var err error
		body, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := a.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: method, Path: forwardHTTPRoot + op, Body: body})
	if err != nil || r.StatusCode != want {
		t.Fatalf("%s %s: %d %s %v", method, op, r.StatusCode, r.Body, err)
	}
	forwardHTTPCheckHeaders(t, r)
	return r
}

func forwardHTTPCheckHeaders(t *testing.T, r pluginapi.ManagementResponse) {
	t.Helper()
	want := http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}
	if !reflect.DeepEqual(r.Headers, want) {
		t.Fatalf("unexpected headers: %v", r.Headers)
	}
}

func TestForwardHTTPAdvertisementSDKProvenanceAndNoImplicitRegistration(t *testing.T) {
	for _, base := range []string{"", "/v0/management/plugins/pacer", "/v8/management/plugins/other", forwardV8Base + "/", forwardV8Base + "..suffix"} {
		reg := pluginapi.ManagementRegistrationRequest{BasePath: forwardV8Base, PluginManagementBasePath: base}
		if len(forwardV8Routes(reg, true)) != 0 {
			t.Fatal("unadvertised/wrong capability accepted", base)
		}
		a := NewForwardHTTP(nil, make(chan struct{}, 2), ForwardHTTPOptions{Enabled: true, Registration: reg})
		forwardHTTPCall(t, a, "GET", "status", nil, 404)
	}
	reg := pluginapi.ManagementRegistrationRequest{PluginManagementBasePath: forwardV8Base}
	if forwardV8Routes(reg, false) != nil {
		t.Fatal("disabled routes")
	}
	forwardHTTPCall(t, NewForwardHTTP(nil, make(chan struct{}, 2), ForwardHTTPOptions{Registration: reg}), "GET", "status", nil, 404)
	routes := forwardV8Routes(reg, true)
	if len(routes) != 9 {
		t.Fatal(routes)
	}
	seen := map[string]bool{}
	for _, route := range routes {
		key := route["Method"] + " " + route["Path"]
		if seen[key] || !strings.HasPrefix(route["Path"], forwardHTTPRoot) || route["Menu"] != "" {
			t.Fatal(route)
		}
		seen[key] = true
	}
	for _, route := range reportRoutes() {
		if strings.Contains(route["Path"], "/v8/") || strings.Contains(route["Path"], "forward") {
			t.Fatal("legacy registration acquired new route", route)
		}
	}
	for _, value := range []pluginapi.ManagementRegistrationRequest{{}, reg} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("PluginManagementBasePath")) != (value.PluginManagementBasePath != "") {
			t.Fatal("omitempty failure")
		}
		var round pluginapi.ManagementRegistrationRequest
		if err = json.Unmarshal(raw, &round); err != nil || round.PluginManagementBasePath != value.PluginManagementBasePath {
			t.Fatal(round, err)
		}
	}
	var legacy pluginapi.ManagementRegistrationRequest
	if err := json.Unmarshal([]byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/pacer"}`), &legacy); err != nil || len(forwardV8Routes(legacy, true)) != 0 {
		t.Fatal("old host inferred v8")
	}
	root := "../../third_party/cpa-sdk"
	var provenance struct {
		Commit    string            `json:"commit"`
		Files     map[string]string `json:"files"`
		Preserved map[string]string `json:"preserved_manifests"`
	}
	raw, err := os.ReadFile(filepath.Join(root, "V8-PROVENANCE.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.Commit != "9af81eadc59475eba69de16f586b4fb555791515" || len(provenance.Files) != 1 || len(provenance.Preserved) != 2 || provenance.Files["sdk/pluginapi/types.go"] != "1c72f5459f04d9d9f81c25686999e7022b7d9935731259f4cb2e9ada2d5ac2de" {
		t.Fatal("incorrect SDK source attestation")
	}
	for path, want := range provenance.Files {
		data, e := os.ReadFile(filepath.Join(root, path))
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatal("SDK provenance mismatch", path)
		}
	}
	for path, want := range provenance.Preserved {
		data, e := os.ReadFile(filepath.Join(root, path))
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatal("historical manifest changed", path)
		}
	}
}

func TestForwardHTTPNativeLibraryPinsReportsEvidenceDownloadsAndCABI(t *testing.T) {
	f := newForwardHTTPFixture(t)
	a := f.adapter
	response := forwardHTTPCall(t, a, "POST", "pin", map[string]any{}, 200)
	var pin historyreports.ForwardPin
	if err := json.Unmarshal(response.Body, &pin); err != nil || pin != f.pin {
		t.Fatal(pin, err)
	}
	report := forwardHTTPCall(t, a, "POST", "report", map[string]any{"pin": pin}, 200)
	var dto historyreports.ForwardReport
	if err := json.Unmarshal(report.Body, &dto); err != nil {
		t.Fatal(err)
	}
	// The original CPAMP split valuation is $7.25 per synthetic attempt; this
	// adapter transports library results and never computes that formula itself.
	if dto.Totals.Rows != 2 || dto.Totals.SelectionValue == nil || dto.Totals.SelectionValue.Low != 14500000000 || dto.Totals.SelectionValue.High != 14500000000 || dto.Capacity != "unavailable" || dto.Totals.CoverageReasons["physical_coverage_unverified"] != 2 {
		t.Fatalf("value/unknown authority lost: %+v", dto)
	}
	pageResponse := forwardHTTPCall(t, a, "POST", "requests", map[string]any{"pin": pin, "limit": 1}, 200)
	var page historyreports.ForwardPage
	if err := json.Unmarshal(pageResponse.Body, &page); err != nil || len(page.Rows) != 1 || page.Next == "" {
		t.Fatal(page, err)
	}
	proof := forwardHTTPCall(t, a, "POST", "evidence", map[string]any{"pin": pin, "id": page.Rows[0].ID}, 200)
	var evidence historyreports.ForwardEvidence
	if err := json.Unmarshal(proof.Body, &evidence); err != nil || evidence.ID != page.Rows[0].ID {
		t.Fatal(evidence, err)
	}
	var goldenJSON, goldenCSV bytes.Buffer
	if err := f.store.ExportForwardJSON(context.Background(), pin, &goldenJSON); err != nil {
		t.Fatal(err)
	}
	next, err := f.store.ExportForwardCSV(context.Background(), pin, 1, "", &goldenCSV)
	if err != nil {
		t.Fatal(err)
	}
	jsonResponse := forwardHTTPCall(t, a, "POST", "export-json", map[string]any{"pin": pin}, 200)
	csvResponse := forwardHTTPCall(t, a, "POST", "export-csv", map[string]any{"pin": pin, "limit": 1}, 200)
	for _, tc := range []struct {
		response          pluginapi.ManagementResponse
		bytes             []byte
		next, name, media string
	}{{jsonResponse, goldenJSON.Bytes(), "", "pacer-forward.json", "application/json"}, {csvResponse, goldenCSV.Bytes(), next, "pacer-forward.csv", "text/csv"}} {
		var download forwardHTTPDownload
		if err = json.Unmarshal(tc.response.Body, &download); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(download.Content, tc.bytes) || download.NextCursor != tc.next || download.Encoding != "base64" || download.Filename != tc.name || download.MediaType != tc.media {
			t.Fatal("download/original-writer/continuation changed")
		}
	}
	if next != page.Next {
		t.Fatal("CSV and requests cursor disagree")
	}
	forwardHTTPCall(t, a, "POST", "export-csv", map[string]any{"pin": pin, "limit": 1, "cursor": next}, 200)
	// The actual SDK byte-array field uses base64 in the C ABI RPC envelope.
	body, _ := json.Marshal(map[string]any{"pin": pin})
	request := pluginapi.ManagementRequest{Method: "POST", Path: forwardHTTPRoot + "export-json", Body: body}
	raw, _ := json.Marshal(request)
	rpc, err := a.HandleRPC(context.Background(), pluginabi.MethodManagementHandle, raw)
	if err != nil || !bytes.Equal(rpc.Body, jsonResponse.Body) {
		t.Fatal("native request/response bytes changed", err)
	}
	result, _ := json.Marshal(rpc)
	envelope, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: result})
	var decoded pluginabi.Envelope
	if err = json.Unmarshal(envelope, &decoded); err != nil {
		t.Fatal(err)
	}
	var transported pluginapi.ManagementResponse
	if err = json.Unmarshal(decoded.Result, &transported); err != nil || !bytes.Equal(transported.Body, jsonResponse.Body) {
		t.Fatal("C ABI result body changed", err)
	}
	// Nanosecond-aligned selection survives JSON strings without millisecond rounding.
	selection := forwardHTTPCall(t, a, "POST", "pin", map[string]any{"generation": pin.Generation, "filter": map[string]any{"from": forwardHTTPAt.Add(time.Nanosecond).Format(time.RFC3339Nano), "to": forwardHTTPAt.Add(2 * time.Nanosecond).Format(time.RFC3339Nano)}}, 200)
	var nanos historyreports.ForwardPin
	_ = json.Unmarshal(selection.Body, &nanos)
	r := forwardHTTPCall(t, a, "POST", "report", map[string]any{"pin": nanos}, 200)
	_ = json.Unmarshal(r.Body, &dto)
	if dto.Totals.Rows != 1 {
		t.Fatal("nanosecond boundary rounded")
	}
	// A new accepted unknown-price record changes latest but never the saved export.
	unknown := forwardHTTPNative(t, "unknown", forwardHTTPAt.Add(time.Second))
	unknown.Attribution.Model = "unpriced"
	if _, err = f.feed.PutRecord(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	newPin := f.publish(t, 0)
	if newPin.Generation == pin.Generation {
		t.Fatal("new generation not sealed")
	}
	r = forwardHTTPCall(t, a, "POST", "report", map[string]any{"pin": newPin}, 200)
	dto = historyreports.ForwardReport{}
	_ = json.Unmarshal(r.Body, &dto)
	if dto.Totals.Rows != 3 || dto.Totals.ValuedCount != 2 || dto.Totals.SelectionValue != nil || dto.Totals.Reasons["missing_price_qualification"] != 1 || dto.Capacity != "unavailable" {
		t.Fatal("unknown price/coverage disappeared")
	}
	if after := forwardHTTPCall(t, a, "POST", "export-json", map[string]any{"pin": pin}, 200); !bytes.Equal(after.Body, jsonResponse.Body) {
		t.Fatal("old pin JSON changed after publication")
	}
	if after := forwardHTTPCall(t, a, "POST", "export-csv", map[string]any{"pin": pin, "limit": 1}, 200); !bytes.Equal(after.Body, csvResponse.Body) {
		t.Fatal("old pin CSV changed after publication")
	}
	for _, tc := range []struct {
		pin    historyreports.ForwardPin
		limit  int
		cursor string
	}{{newPin, 1, next}, {pin, 2, next}, {pin, 1, next + "bad"}, {nanos, 1, next}} {
		forwardHTTPCall(t, a, "POST", "requests", map[string]any{"pin": tc.pin, "limit": tc.limit, "cursor": tc.cursor}, 409)
	}
}

func TestForwardHTTPStrictInputAndDirectNativeRefusals(t *testing.T) {
	f := newForwardHTTPFixture(t)
	pin, _ := json.Marshal(f.pin)
	valid := `{"pin":` + string(pin) + `}`
	for _, raw := range []string{
		``, `[]`, `null`, `{}`, `{"Pin":` + string(pin) + `}`, valid + ` {}`, valid + ` false`,
		`{"pin":null}`, `{"pin":` + string(pin) + `,"pin":` + string(pin) + `}`, `{"pin":` + string(pin) + `,"\u0070in":` + string(pin) + `}`,
		`{"pin":` + string(pin) + `,"filter":{}}`, `{"pin":` + string(pin) + `,"revision":1}`, `{"pin":` + string(pin) + `,"complete":true}`,
		strings.Replace(valid, `"version":`, `"Version":`, 1), strings.Replace(valid, `"filter":{`, `"filter":{"domain":"native",`, 1),
		strings.Replace(valid, historyreports.ForwardVersion, "unsupported-version", 1),
		strings.Replace(valid, `"domain":"combined"`, `"domain":null`, 1), strings.Replace(valid, `"domain":"combined"`, `"domain":"future"`, 1),
		strings.Replace(valid, `"domain":"combined"`, `"domain":"combined","sql":"private SELECT"`, 1),
		strings.Replace(valid, `"domain":"combined"`, `"domain":"combined","from":1790000000000000001`, 1),
		strings.Replace(valid, `"domain":"combined"`, `"domain":"combined","from":"2263-01-01T00:00:00Z"`, 1),
		strings.Replace(valid, `"domain":"combined"`, `"domain":"combined","model":"`+strings.Repeat("x", 257)+`"`, 1),
	} {
		r, err := f.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: "POST", Path: forwardHTTPRoot + "report", Body: []byte(raw)})
		if err != nil || r.StatusCode != 400 || bytes.Contains(r.Body, []byte("SELECT")) {
			t.Fatalf("accepted/unsanitized input %q -> %d %s %v", raw, r.StatusCode, r.Body, err)
		}
		forwardHTTPCheckHeaders(t, r)
	}
	for _, path := range []string{"/v0/management/plugins/pacer/reports/forward/report", "/v0/resource/plugins/pacer/reports/forward/report", "/reports/forward/report", forwardHTTPRoot + "%72eport", forwardHTTPRoot + "./report", forwardHTTPRoot + "../report", forwardHTTPRoot + "report/", forwardHTTPRoot + "report?x=1", forwardHTTPRoot + "report#x", forwardHTTPRoot + "report.json", forwardHTTPRoot + "//report", strings.Replace(forwardHTTPRoot, "pacer/", "other/", 1) + "report"} {
		r, _ := f.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: "POST", Path: path, Body: []byte(valid)})
		if r.StatusCode != 404 {
			t.Fatalf("alias %s accepted: %d", path, r.StatusCode)
		}
	}
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "TRACE", "post", "PATCH", "DELETE"} {
		forwardHTTPCall(t, f.adapter, method, "report", map[string]any{"pin": f.pin}, 404)
	}
	for _, req := range []pluginapi.ManagementRequest{
		{Method: "POST", Path: forwardHTTPRoot + "report", Body: []byte(valid), Query: url.Values{"pin": {"x"}}},
		{Method: "GET", Path: forwardHTTPRoot + "status", Body: []byte(" ")},
		{Method: "POST", Path: forwardHTTPRoot + "report", Body: []byte(valid), Headers: http.Header{"Authorization": {"Bearer private-secret"}}},
		{Method: "POST", Path: forwardHTTPRoot + "report", Body: []byte(valid), Headers: http.Header{"X-Pacer-Cache-Capability": {"private-secret"}}},
		{Method: "POST", Path: forwardHTTPRoot + "report", Body: []byte(valid), Headers: http.Header{"If-Match": {"1"}}},
	} {
		r, _ := f.adapter.HandleManagement(context.Background(), req)
		if r.StatusCode != 400 || bytes.Contains(r.Body, []byte("private")) {
			t.Fatal(r)
		}
	}
	for _, size := range []int{64 << 10, (64 << 10) + 1} {
		body := append([]byte(valid), bytes.Repeat([]byte(" "), size-len(valid))...)
		want := 200
		if size > 64<<10 {
			want = 413
		}
		req := pluginapi.ManagementRequest{Method: "POST", Path: forwardHTTPRoot + "report", Body: body}
		r, _ := f.adapter.HandleManagement(context.Background(), req)
		if r.StatusCode != want {
			t.Fatalf("body size=%d status=%d", size, r.StatusCode)
		}
		raw, _ := json.Marshal(req)
		r, _ = f.adapter.HandleRPC(context.Background(), pluginabi.MethodManagementHandle, raw)
		if r.StatusCode != want {
			t.Fatalf("native body size=%d status=%d", size, r.StatusCode)
		}
	}
	largeQuery := url.Values{"revision": {strings.Repeat("1", 4090)}}
	r, _ := f.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: "GET", Path: forwardHTTPRoot + "profiles", Query: largeQuery})
	if r.StatusCode != 413 {
		t.Fatal("query not bounded", r.StatusCode)
	}
	for _, op := range []string{"requests", "export-csv"} {
		for _, limit := range []any{0, 101, 1.5, "1"} {
			forwardHTTPCall(t, f.adapter, "POST", op, map[string]any{"pin": f.pin, "limit": limit}, 400)
		}
	}
	wrong := f.pin
	wrong.Generation = strings.Repeat("f", 64)
	forwardHTTPCall(t, f.adapter, "POST", "report", map[string]any{"pin": wrong}, 404)
	for _, raw := range [][]byte{[]byte(`{"Method":"GET","Method":"POST","Path":"` + forwardHTTPRoot + `status"}`), []byte(`{"Method":"GET","Path":"` + forwardHTTPRoot + `status","Headers":{"Accept":["a"],"Accept":["b"]}}`), []byte(`{"method":"GET","Path":"` + forwardHTTPRoot + `status"}`), []byte(`{"Method":"GET","Path":"` + forwardHTTPRoot + `status","Query":{"a":["b"],"a":["c"]}}`)} {
		r, _ := f.adapter.HandleRPC(context.Background(), pluginabi.MethodManagementHandle, raw)
		if r.StatusCode != 400 {
			t.Fatal("ambiguous native envelope", r)
		}
	}
	r, _ = f.adapter.HandleRPC(context.Background(), pluginabi.MethodManagementRegister, nil)
	if r.StatusCode != 404 {
		t.Fatal("adapter installed itself")
	}
}

func TestForwardHTTPSharedAdmissionCancellationAndHonestStatus(t *testing.T) {
	f := newForwardHTTPFixture(t)
	f.slots <- struct{}{}
	f.slots <- struct{}{}
	forwardHTTPCall(t, f.adapter, "GET", "status", nil, 503)
	if len(f.slots) != 2 {
		t.Fatal("other owner's slots stolen")
	}
	<-f.slots
	<-f.slots
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ := f.adapter.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "GET", Path: forwardHTTPRoot + "status"})
	if r.StatusCode != 503 || len(f.slots) != 0 {
		t.Fatal("cancellation leaked admission")
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	a := NewForwardHTTP(f.store, f.slots, ForwardHTTPOptions{Enabled: true, Registration: pluginapi.ManagementRegistrationRequest{PluginManagementBasePath: forwardV8Base}, Status: func(ctx context.Context) (ForwardHTTPRuntimeStatus, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 31*time.Second {
			return ForwardHTTPRuntimeStatus{}, errors.New("missing budget")
		}
		entered <- struct{}{}
		select {
		case <-release:
			return ForwardHTTPRuntimeStatus{State: "lagging"}, nil
		case <-ctx.Done():
			return ForwardHTTPRuntimeStatus{}, ctx.Err()
		}
	}})
	results := make(chan pluginapi.ManagementResponse, 2)
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	for i := 0; i < 2; i++ {
		go func() {
			r, _ := a.HandleManagement(runCtx, pluginapi.ManagementRequest{Method: "GET", Path: forwardHTTPRoot + "status"})
			results <- r
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("status did not enter shared slots")
		}
	}
	forwardHTTPCall(t, a, "GET", "status", nil, 503)
	stop()
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.StatusCode != 503 {
				t.Fatal("cancel became success")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancellation hung")
		}
	}
	if len(f.slots) != 0 {
		t.Fatal("slots leaked")
	}
	close(release)
	r = forwardHTTPCall(t, f.adapter, "GET", "status", nil, 200)
	var status struct {
		State   string                   `json:"state"`
		Runtime ForwardHTTPRuntimeStatus `json:"runtime"`
		Latest  string                   `json:"latest_generation"`
	}
	_ = json.Unmarshal(r.Body, &status)
	if status.State != "snapshot_available" || status.Runtime.State != "unknown" || status.Latest != f.pin.Generation {
		t.Fatal("store status invented runtime readiness", status)
	}
	for _, state := range []string{"unready", "ready", "lagging", "error", "disabled"} {
		a := NewForwardHTTP(f.store, f.slots, ForwardHTTPOptions{Enabled: true, Registration: pluginapi.ManagementRegistrationRequest{PluginManagementBasePath: forwardV8Base}, Status: func(context.Context) (ForwardHTTPRuntimeStatus, error) {
			return ForwardHTTPRuntimeStatus{State: state}, nil
		}})
		r := forwardHTTPCall(t, a, "GET", "status", nil, 200)
		_ = json.Unmarshal(r.Body, &status)
		if status.Runtime.State != state {
			t.Fatal("runtime status laundered")
		}
	}
	for _, callback := range []func(context.Context) (ForwardHTTPRuntimeStatus, error){func(context.Context) (ForwardHTTPRuntimeStatus, error) {
		return ForwardHTTPRuntimeStatus{}, errors.New("SQL /private/path bearer-secret")
	}, func(context.Context) (ForwardHTTPRuntimeStatus, error) {
		return ForwardHTTPRuntimeStatus{State: "SQL /private/path"}, nil
	}} {
		a := NewForwardHTTP(f.store, f.slots, ForwardHTTPOptions{Enabled: true, Registration: pluginapi.ManagementRegistrationRequest{PluginManagementBasePath: forwardV8Base}, Status: callback})
		r := forwardHTTPCall(t, a, "GET", "status", nil, 503)
		if bytes.Contains(r.Body, []byte("SQL")) || bytes.Contains(r.Body, []byte("private")) {
			t.Fatal("raw status error leaked")
		}
	}
	for _, slots := range []chan struct{}{nil, make(chan struct{}, 1), make(chan struct{}, 3)} {
		a := NewForwardHTTP(f.store, slots, ForwardHTTPOptions{Enabled: true, Registration: pluginapi.ManagementRegistrationRequest{PluginManagementBasePath: forwardV8Base}})
		forwardHTTPCall(t, a, "GET", "status", nil, 503)
	}
	empty, err := historyreports.OpenForward(context.Background(), filepath.Join(t.TempDir(), "empty.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	a = NewForwardHTTP(empty, f.slots, ForwardHTTPOptions{Enabled: true, Registration: pluginapi.ManagementRegistrationRequest{PluginManagementBasePath: forwardV8Base}})
	r = forwardHTTPCall(t, a, "GET", "status", nil, 200)
	_ = json.Unmarshal(r.Body, &status)
	if status.State != "unready" || status.Latest != "" {
		t.Fatal("empty store fabricated latest")
	}
	forwardHTTPCall(t, a, "POST", "pin", map[string]any{}, 503)
	a = NewForwardHTTP(nil, f.slots, ForwardHTTPOptions{Enabled: true, Registration: pluginapi.ManagementRegistrationRequest{PluginManagementBasePath: forwardV8Base}})
	forwardHTTPCall(t, a, "GET", "status", nil, 503)
}

func TestForwardHTTPProfilesDatedCASAndNonblockingPublicationNotice(t *testing.T) {
	f := newForwardHTTPFixture(t)
	notice := make(chan struct{}, 1)
	f.adapter.notify = notice
	ref := historyreports.ForwardAccountRef("producer", "claude", "native-account")
	profile := map[string]any{"id": "dated", "name": "<Dated & bounded>", "provider": "claude", "memberships": []any{map[string]string{"account": ref, "start_ms": fmt.Sprint(forwardHTTPAt.UnixMilli()), "end_ms": fmt.Sprint(forwardHTTPAt.Add(time.Hour).UnixMilli())}}}
	edit := map[string]any{"expected_revision": "0", "profile": profile}
	r := forwardHTTPCall(t, f.adapter, "PATCH", "profiles", edit, 202)
	if !bytes.Contains(r.Body, []byte(`"revision":"1"`)) || !bytes.Contains(r.Body, []byte(`"publication":"pending"`)) || !bytes.Contains(r.Body, []byte(`"notification_enqueued":true`)) || len(notice) != 1 {
		t.Fatal("CAS/notice semantics", string(r.Body))
	}
	forwardHTTPCall(t, f.adapter, "PATCH", "profiles", edit, 409)
	for _, revision := range []any{1, 1.0, 9007199254740992.0, "01", "+1", "1.0", "9223372036854775808"} {
		forwardHTTPCall(t, f.adapter, "PATCH", "profiles", map[string]any{"expected_revision": revision, "archive_id": "dated"}, 400)
	}
	forwardHTTPCall(t, f.adapter, "PATCH", "profiles", map[string]any{"expected_revision": "9007199254740993", "archive_id": "dated"}, 409)
	forwardHTTPCall(t, f.adapter, "PATCH", "profiles", map[string]any{"expected_revision": "9007199254740993", "profile": profile}, 409)
	read, err := f.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: "GET", Path: forwardHTTPRoot + "profiles", Query: url.Values{"revision": {"1"}}})
	if err != nil || read.StatusCode != 200 || !bytes.Contains(read.Body, []byte(`"revision":"1"`)) {
		t.Fatal(read, err)
	}
	var profileJSON struct {
		Revision string `json:"revision"`
		Config   struct {
			Revision string `json:"revision"`
		} `json:"config"`
	}
	if err = json.Unmarshal(read.Body, &profileJSON); err != nil || profileJSON.Revision != "1" || profileJSON.Config.Revision != "1" {
		t.Fatal("profile revisions were not both quoted", err)
	}
	for _, raw := range []string{
		`{"expected_revision":"1","expected_revision":"0","archive_id":"dated"}`,
		`{"expected_revision":"1","profile":{"id":"x","name":"x","provider":"claude","memberships":[{"account":"` + ref + `","account":"other","start_ms":"1"}]}}`,
		`{"expected_revision":"1","profile":{"id":"x","name":"x","provider":"claude","memberships":null}}`,
		`{"expected_revision":"1","profile":{"id":"x","name":"x","provider":"claude","memberships":[],"complete":true}}`,
	} {
		r, _ := f.adapter.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: "PATCH", Path: forwardHTTPRoot + "profiles", Body: []byte(raw)})
		if r.StatusCode != 400 {
			t.Fatal("invalid profile accepted", string(r.Body))
		}
	}
	for _, start := range []any{"0", "-1", "1.1", 1.1, "9223372036855"} {
		p := map[string]any{"id": "invalid", "name": "invalid", "provider": "claude", "memberships": []any{map[string]any{"account": ref, "start_ms": start}}}
		forwardHTTPCall(t, f.adapter, "PATCH", "profiles", map[string]any{"expected_revision": "1", "profile": p}, 400)
	}
	tooMany := make([]any, 101)
	for i := range tooMany {
		tooMany[i] = map[string]string{"account": ref, "start_ms": fmt.Sprint(i + 1), "end_ms": fmt.Sprint(i + 2)}
	}
	forwardHTTPCall(t, f.adapter, "PATCH", "profiles", map[string]any{"expected_revision": "1", "profile": map[string]any{"id": "invalid", "name": "invalid", "provider": "claude", "memberships": tooMany}}, 400)
	for _, memberships := range [][]any{{map[string]string{"account": "unknown-account", "start_ms": "1"}}, {map[string]string{"account": ref, "start_ms": "2", "end_ms": "1"}}, {map[string]string{"account": ref, "start_ms": "1"}, map[string]string{"account": ref, "start_ms": "2"}}} {
		p := map[string]any{"id": "invalid", "name": "invalid", "provider": "claude", "memberships": memberships}
		forwardHTTPCall(t, f.adapter, "PATCH", "profiles", map[string]any{"expected_revision": "1", "profile": p}, 400)
	}
	// Full/closed channels never turn an already committed mutation into failure.
	r = forwardHTTPCall(t, f.adapter, "PATCH", "profiles", map[string]any{"expected_revision": "1", "archive_id": "dated"}, 202)
	if !bytes.Contains(r.Body, []byte(`"revision":"2"`)) || !bytes.Contains(r.Body, []byte(`"notification_enqueued":false`)) {
		t.Fatal(string(r.Body))
	}
	close(notice)
	r = forwardHTTPCall(t, f.adapter, "PATCH", "profiles", map[string]any{"expected_revision": "2", "archive_id": "dated"}, 202)
	if !bytes.Contains(r.Body, []byte(`"revision":"3"`)) {
		t.Fatal("closed notice lost committed CAS")
	}
	config, _, err := f.store.ProfilesAt(context.Background(), 3)
	if err != nil || !config.Profiles[0].Archived {
		t.Fatal(config, err)
	}
	status, err := f.store.Status(context.Background())
	if err != nil || status.Latest != f.pin.Generation {
		t.Fatal("profile edit fabricated publication")
	}
	old, err := f.store.ForwardReport(context.Background(), f.pin)
	if err != nil || len(old.Profiles) != 1 || old.Profiles["unassigned"].Rows != 2 {
		t.Fatal("old pin memberships changed", old.Profiles, err)
	}
}

func TestForwardHTTPWholeEnvelopeBoundsWriterFailureAndSafeErrors(t *testing.T) {
	f := newForwardHTTPFixture(t)
	// Exactly 6 MiB of file bytes becomes 8 MiB of base64 BEFORE envelope fields.
	d := forwardHTTPDownload{Filename: "pacer-forward.csv", MediaType: "text/csv", Encoding: "base64", Content: bytes.Repeat([]byte("x"), 6<<20), NextCursor: "cursor"}
	r := forwardHTTPJSON(200, d)
	if r.StatusCode != 413 || len(r.Body) > 100 {
		t.Fatal("envelope overhead ignored or partial success")
	}
	d.Content = d.Content[:len(d.Content)-1024]
	r = forwardHTTPJSON(200, d)
	if r.StatusCode != 200 || len(r.Body) > 8<<20 {
		t.Fatal("bounded envelope refused")
	}
	var restored forwardHTTPDownload
	if err := json.Unmarshal(r.Body, &restored); err != nil || !bytes.Equal(restored.Content, d.Content) || restored.NextCursor != "cursor" {
		t.Fatal("bounded download truncated", err)
	}
	var limited forwardHTTPBuffer
	limited.Buffer.Write(bytes.Repeat([]byte("x"), 8<<20))
	before := limited.Len()
	if _, err := f.store.ExportForwardCSV(context.Background(), f.pin, 1, "", &limited); !errors.Is(err, errForwardHTTPOutput) || limited.Len() != before {
		t.Fatal("writer overflow swallowed/partial", err)
	}
	if err := f.store.ExportForwardJSON(context.Background(), f.pin, &limited); !errors.Is(err, errForwardHTTPOutput) || limited.Len() != before {
		t.Fatal("JSON writer overflow swallowed/partial", err)
	}
	for _, err := range []error{errors.New("SQL select /private/path Bearer secret"), fmt.Errorf("private: %w", sql.ErrNoRows), io.ErrShortWrite, context.DeadlineExceeded} {
		r := forwardHTTPError(err)
		forwardHTTPCheckHeaders(t, r)
		if r.StatusCode == 200 || bytes.Contains(r.Body, []byte("private")) || bytes.Contains(r.Body, []byte("SQL")) || bytes.Contains(r.Body, []byte("secret")) {
			t.Fatal("raw error escaped")
		}
	}
}
