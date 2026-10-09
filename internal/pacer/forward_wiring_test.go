package pacer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func forwardWiringOpen(t *testing.T, f forwardFixture, edit func(*forwardOptions), hook func(context.Context)) *Plugin {
	t.Helper()
	p := New(f.h.call)
	p.now = f.h.now
	p.workerOnce.Do(func() {}) // no periodic host activity in source-only fixtures
	p.forwardManagementLeaseHook = hook
	opts := defaultForwardOptions()
	opts.ticks, opts.yield, opts.retry = f.ticks, 0, time.Hour
	if edit != nil {
		edit(&opts)
	}
	p.forwardOptions = &opts
	result := invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": f.config(), "schema_version": 7}).(map[string]any)
	if result["schema_version"] != 7 || result["capabilities"].(map[string]bool)["cache_policy"] || p.policy != nil || p.cfg.CachePolicy.Enabled || p.routingEligible {
		t.Fatal("forward registration enabled policy/routing or omitted schema 7", result)
	}
	t.Cleanup(p.Close)
	return p
}

func forwardWiringRegistration() pluginapi.ManagementRegistrationRequest {
	return pluginapi.ManagementRegistrationRequest{Plugin: pluginapi.Metadata{Name: "pacer", Version: "0.2.0-dev", Author: "floris3456", GitHubRepository: "https://github.com/floris3456/cliproxyapi-quota-handoff"}, BasePath: "/v0/management", ResourceBasePath: "/v0/resource/plugins/pacer", PluginManagementBasePath: forwardV8Base}
}

func forwardWiringRegister(t *testing.T, p *Plugin, raw []byte) map[string]any {
	t.Helper()
	result, err := p.Handle(pluginabi.MethodManagementRegister, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result.(map[string]any)
}

func forwardWiringCall(t *testing.T, p *Plugin, req pluginapi.ManagementRequest, want int) pluginapi.ManagementResponse {
	t.Helper()
	return forwardWiringRaw(t, p, hookRaw(t, req), want)
}

func forwardWiringRaw(t *testing.T, p *Plugin, raw []byte, want int) pluginapi.ManagementResponse {
	t.Helper()
	result, err := p.Handle(pluginabi.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := result.(pluginapi.ManagementResponse)
	if !ok || r.StatusCode != want {
		t.Fatalf("status: %d %s; want %d", r.StatusCode, r.Body, want)
	}
	forwardHTTPCheckHeaders(t, r)
	for _, secret := range []string{"private-secret", "private-SQL", "Bearer ", "/private/path"} {
		if bytes.Contains(r.Body, []byte(secret)) {
			t.Fatal("raw input/error leaked")
		}
	}
	return r
}

func forwardWiringRequest(t *testing.T, op string, body any) pluginapi.ManagementRequest {
	t.Helper()
	req := pluginapi.ManagementRequest{Method: "POST", Path: forwardHTTPRoot + op}
	if body != nil {
		req.Body = hookRaw(t, body)
	}
	if op == "status" {
		req.Method = "GET"
	}
	return req
}

func TestForwardWiringRegistrationCapabilityAndLegacyPreservation(t *testing.T) {
	f := preparedRuntime(t)
	p := forwardWiringOpen(t, f, func(o *forwardOptions) { o.beforeTurn = func(ctx context.Context) { <-ctx.Done() } }, nil)
	legacy := forwardWiringRegister(t, p, nil)
	legacyJSON := hookRaw(t, legacy)
	forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 404)
	reg := forwardWiringRegistration()
	raw := hookRaw(t, reg)
	full := forwardWiringRegister(t, p, raw)
	routes := full["routes"].([]map[string]string)
	if len(routes) != len(legacy["routes"].([]map[string]string))+9 || !reflect.DeepEqual(routes[len(routes)-9:], forwardV8Routes(reg, true)) {
		t.Fatal("explicit routes not appended exactly", routes)
	}
	full["routes"] = routes[:len(routes)-9]
	if !bytes.Equal(hookRaw(t, full), legacyJSON) {
		t.Fatal("legacy resources/routes changed")
	}
	usageCount := 0
	for _, r := range routes {
		if strings.Contains(r["Path"], "/history/") {
			t.Fatal("removed history route still registered", r)
		}
		if strings.Contains(r["Path"], "/usage/") {
			usageCount++
		}
		if strings.Contains(r["Path"], "forward") && !strings.HasPrefix(r["Path"], forwardHTTPRoot) {
			t.Fatal("forward legacy alias")
		}
	}
	if usageCount != 4 {
		t.Fatal("the four usage routes are missing")
	}
	r := forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 200)
	var status forwardWiredStatus
	if json.Unmarshal(r.Body, &status) != nil || status.Publisher.State != "initializing" || status.Runtime.State != "unready" || status.Latest != "" || status.Publisher.Latest != "" {
		t.Fatal("startup invented readiness", string(r.Body))
	}
	for _, bad := range [][]byte{nil, []byte(`{}`), []byte(`null`), []byte(`{"BasePath":"` + forwardV8Base + `"}`), []byte(`{"PluginManagementBasePath":"` + forwardV8Base + `/"}`), []byte(`{"PluginManagementBasePath":"/v8/management/plugins/foreign"}`), []byte(`{"pluginmanagementbasepath":"` + forwardV8Base + `"}`), []byte(`{"PluginManagementBasePath":"` + forwardV8Base + `","PluginManagementBasePath":"` + forwardV8Base + `"}`), []byte(`{"Plugin":{"Name":"pacer","Name":"other"},"PluginManagementBasePath":"` + forwardV8Base + `"}`), append(append([]byte{}, raw...), []byte(` {}`)...), []byte(`{"PluginManagementBasePath":"` + forwardV8Base + `","SchemaVersion":7}`)} {
		forwardWiringRegister(t, p, raw)
		got := forwardWiringRegister(t, p, bad)
		if !bytes.Equal(hookRaw(t, got), legacyJSON) {
			t.Fatal("bad capability changed old registration", string(bad))
		}
		forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 404)
	}
	// Client body, BasePath, private capability or inference credentials cannot
	// stand in for the host-owned registration advertisement.
	req := forwardWiringRequest(t, "pin", map[string]any{"PluginManagementBasePath": forwardV8Base})
	req.Headers = http.Header{"Authorization": {"Bearer private-secret"}}
	forwardWiringCall(t, p, req, 404)
	forwardWiringRegister(t, p, raw)
	forwardWiringCall(t, p, req, 400)
	for _, path := range []string{forwardHTTPRoot + "status/", forwardHTTPRoot + "%73tatus", forwardHTTPRoot + "../status", forwardHTTPRoot + "status?x=1", strings.Replace(forwardHTTPRoot, "pacer/", "foreign/", 1) + "status"} {
		forwardWiringCall(t, p, pluginapi.ManagementRequest{Method: "GET", Path: path}, 404)
	}
	for _, path := range []string{"/v0/management/plugins/pacer/reports/forward/status", "/v0/resource/plugins/pacer/reports/forward/status", "/plugins/pacer/reports/forward/status"} {
		result, err := p.Handle(pluginabi.MethodManagementHandle, hookRaw(t, pluginapi.ManagementRequest{Method: "GET", Path: path}))
		if err != nil || result.(pluginapi.ManagementResponse).StatusCode != 404 {
			t.Fatal("legacy alias accepted", err)
		}
	}
	// Standalone legacy and unconfigured plugins retain schema 6/no forward.
	if registration().(map[string]any)["schema_version"] != 6 {
		t.Fatal("legacy schema changed")
	}
	legacyPlugin := openCoveragePlugin(t, newCoverageHost(1), f.state+"-legacy")
	if got := forwardWiringRegister(t, legacyPlugin, raw); !bytes.Equal(hookRaw(t, got), legacyJSON) {
		t.Fatal("unconfigured plugin registered forward")
	}
	forwardWiringCall(t, legacyPlugin, forwardWiringRequest(t, "status", nil), 404)
	unloaded := New(nil)
	if _, err := unloaded.Handle(pluginabi.MethodManagementRegister, raw); err == nil {
		t.Fatal("unloaded plugin registered")
	}
	forwardWiringCall(t, unloaded, forwardWiringRequest(t, "status", nil), 404)
}

func TestForwardWiringNativeUsageProfilesPublisherAndPinnedTransport(t *testing.T) {
	f := preparedRuntime(t)
	p := forwardWiringOpen(t, f, nil, nil)
	forwardWiringRegister(t, p, hookRaw(t, forwardWiringRegistration()))
	first := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "wired-logical", c.ID)
	u := forwardMixed(c, "wired-logical", "wired-physical", hookEpoch.Add(time.Nanosecond))
	f.h.clock.Add(int64(3 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	head, err := p.measurement.ReportFeedHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ready := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Projected == head.Through && v.Latest != first.Latest
	})
	statusResponse := forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 200)
	var status forwardWiredStatus
	_ = json.Unmarshal(statusResponse.Body, &status)
	if status.Publisher.Latest != ready.Latest || status.Publisher.Projected != head.Through || status.Publisher.Capacity != "unavailable" || status.Publisher.State != "ready" || status.Runtime.State != "ready" {
		t.Fatal(status)
	}
	response := forwardWiringCall(t, p, forwardWiringRequest(t, "pin", map[string]any{}), 200)
	var pin historyreports.ForwardPin
	if err = json.Unmarshal(response.Body, &pin); err != nil || pin.Generation != ready.Latest {
		t.Fatal(pin, err)
	}
	reportResponse := forwardWiringCall(t, p, forwardWiringRequest(t, "report", map[string]any{"pin": pin}), 200)
	var report historyreports.ForwardReport
	_ = json.Unmarshal(reportResponse.Body, &report)
	if report.Totals.Rows != 1 || report.Totals.SelectionValue == nil || report.Totals.SelectionValue.Low != 7250000000 || report.Totals.SelectionValue.High != 7250000000 || report.Capacity != "unavailable" {
		t.Fatal("original value/unknown capacity changed", report)
	}
	pageResponse := forwardWiringCall(t, p, forwardWiringRequest(t, "requests", map[string]any{"pin": pin, "limit": 1}), 200)
	var page historyreports.ForwardPage
	_ = json.Unmarshal(pageResponse.Body, &page)
	if len(page.Rows) != 1 {
		t.Fatal(page)
	}
	forwardWiringCall(t, p, forwardWiringRequest(t, "evidence", map[string]any{"pin": pin, "id": page.Rows[0].ID}), 200)
	var goldenJSON, goldenCSV bytes.Buffer
	err = p.WithForwardStore(context.Background(), func(ctx context.Context, s *historyreports.ForwardStore) error {
		if err := s.ExportForwardJSON(ctx, pin, &goldenJSON); err != nil {
			return err
		}
		_, err := s.ExportForwardCSV(ctx, pin, 1, "", &goldenCSV)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string][]byte{}
	for _, tc := range []struct {
		op   string
		want []byte
	}{{"export-json", goldenJSON.Bytes()}, {"export-csv", goldenCSV.Bytes()}} {
		q := map[string]any{"pin": pin}
		if tc.op == "export-csv" {
			q["limit"] = 1
		}
		r := forwardWiringCall(t, p, forwardWiringRequest(t, tc.op, q), 200)
		var download forwardHTTPDownload
		_ = json.Unmarshal(r.Body, &download)
		if !bytes.Equal(download.Content, tc.want) {
			t.Fatal("writer bytes changed", tc.op)
		}
		saved[tc.op] = append([]byte{}, r.Body...)
		encoded := hookRaw(t, pluginabi.Envelope{OK: true, Result: hookRaw(t, r)})
		var envelope pluginabi.Envelope
		_ = json.Unmarshal(encoded, &envelope)
		var decoded pluginapi.ManagementResponse
		_ = json.Unmarshal(envelope.Result, &decoded)
		if !bytes.Equal(decoded.Body, r.Body) || len(encoded) > forwardHTTPOutputLimit {
			t.Fatal("native envelope changed/oversized")
		}
	}
	profile := map[string]any{"expected_revision": "0", "profile": map[string]any{"id": "wired-profile", "name": "<exact & dated>", "provider": c.Provider, "memberships": []map[string]string{{"account": historyreports.ForwardAccountRef(f.source, c.Provider, c.Account), "start_ms": fmt.Sprint(hookEpoch.UnixMilli())}}}}
	req := forwardWiringRequest(t, "profiles", profile)
	req.Method = "PATCH"
	committed := forwardWiringCall(t, p, req, 202)
	if !bytes.Contains(committed.Body, []byte(`"revision":"1"`)) || !bytes.Contains(committed.Body, []byte(`"publication":"pending"`)) {
		t.Fatal(string(committed.Body))
	}
	forwardWiringCall(t, p, req, 409)
	// No manual WakeForward/tick: the committed PATCH wakes the owned publisher.
	updated := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active && v.Latest != ready.Latest })
	response = forwardWiringCall(t, p, forwardWiringRequest(t, "pin", map[string]any{}), 200)
	var latest historyreports.ForwardPin
	_ = json.Unmarshal(response.Body, &latest)
	if latest.Generation != updated.Latest {
		t.Fatal("profile publication not visible")
	}
	response = forwardWiringCall(t, p, forwardWiringRequest(t, "report", map[string]any{"pin": latest}), 200)
	_ = json.Unmarshal(response.Body, &report)
	if report.Profiles["wired-profile"].Rows != 1 {
		t.Fatal("dated profile not published", report.Profiles)
	}
	forwardWiringCall(t, p, pluginapi.ManagementRequest{Method: "GET", Path: forwardHTTPRoot + "profiles", Query: url.Values{"revision": {"1"}}}, 200)
	for op, want := range saved {
		q := map[string]any{"pin": pin}
		if op == "export-csv" {
			q["limit"] = 1
		}
		if replay := forwardWiringCall(t, p, forwardWiringRequest(t, op, q), 200); !bytes.Equal(replay.Body, want) {
			t.Fatal("old pin refreshed", op)
		}
	}
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	if len(f.h.checkpointCalls) != 0 {
		t.Fatal("forward management called a source/provider checkpoint")
	}
}

func TestForwardWiringLeaseCancellationJoinsCloseQuiesceAndRevocation(t *testing.T) {
	for _, action := range []string{"close", "quiesce", "reregister", "reconfigure", "changed-config"} {
		t.Run(action, func(t *testing.T) {
			f := preparedRuntime(t)
			entered := make(chan context.Context, 1)
			release := make(chan struct{})
			var once sync.Once
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			p := forwardWiringOpen(t, f, func(o *forwardOptions) { o.beforeTurn = func(ctx context.Context) { <-ctx.Done() } }, func(ctx context.Context) { once.Do(func() { entered <- ctx; <-release }) })
			reg := hookRaw(t, forwardWiringRegistration())
			forwardWiringRegister(t, p, reg)
			request := hookRaw(t, forwardWiringRequest(t, "status", nil))
			result := make(chan any, 1)
			go func() {
				v, e := p.Handle(pluginabi.MethodManagementHandle, request)
				if e != nil {
					result <- e
				} else {
					result <- v
				}
			}()
			var leaseCtx context.Context
			select {
			case leaseCtx = <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not enter lease")
			}
			if deadline, ok := leaseCtx.Deadline(); !ok || time.Until(deadline) > 31*time.Second {
				t.Fatal("missing inherited HTTP deadline")
			}
			done := make(chan error, 1)
			go func() {
				switch action {
				case "close":
					p.Close()
					done <- nil
				case "quiesce":
					_, e := p.Handle(pluginabi.MethodPluginQuiesce, []byte(`{}`))
					done <- e
				case "reregister":
					_, e := p.Handle(pluginabi.MethodManagementRegister, []byte(`{}`))
					done <- e
				default:
					cfg := f.config()
					if action == "changed-config" {
						cfg = bytes.ReplaceAll(cfg, []byte(f.book), []byte(strings.Repeat("a", 64)))
					}
					_, e := p.Handle(pluginabi.MethodPluginReconfigure, hookRaw(t, map[string]any{"config_yaml": cfg, "schema_version": 7}))
					done <- e
				}
			}()
			select {
			case <-leaseCtx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("lease not revoked/cancelled")
			}
			// Shutdown must not close the database underneath a still-running lease.
			if _, e := p.forward.store.Status(context.Background()); e != nil {
				t.Fatal("store closed before borrower returned", e)
			}
			if action == "close" || action == "quiesce" {
				select {
				case <-done:
					t.Fatal("shutdown failed to join borrower")
				default:
				}
			}
			close(release)
			select {
			case v := <-result:
				r, ok := v.(pluginapi.ManagementResponse)
				if !ok || r.StatusCode != 503 {
					t.Fatal("cancelled request returned false success", v)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request did not return")
			}
			select {
			case e := <-done:
				if (e != nil) != (action == "changed-config") {
					t.Fatal("lifecycle outcome", e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not join")
			}
			want := 404
			if action == "close" || action == "quiesce" {
				want = 503
			}
			forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), want)
			if action == "close" || action == "quiesce" {
				if _, e := p.Handle(pluginabi.MethodPluginReconfigure, hookRaw(t, map[string]any{"config_yaml": f.config()})); e == nil {
					t.Fatal("quiesced instance restarted")
				}
				p.Close()
				if _, e := p.forward.store.Status(context.Background()); e == nil {
					t.Fatal("Close retained store")
				}
			} else {
				forwardWiringRegister(t, p, reg)
				forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 200)
			}
		})
	}
}

func TestForwardWiringSharedHistoricalPublisherAdmission(t *testing.T) {
	f := preparedRuntime(t)
	sealEntered, sealRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := forwardWiringOpen(t, f, func(o *forwardOptions) {
		o.seal = func(ctx context.Context, s *historyreports.ForwardStore, q historyreports.ForwardSeal) (historyreports.ForwardPin, error) {
			once.Do(func() { close(sealEntered) })
			select {
			case <-sealRelease:
				return s.SealForward(ctx, q)
			case <-ctx.Done():
				return historyreports.ForwardPin{}, ctx.Err()
			}
		}
	}, nil)
	select {
	case <-sealEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not take shared slot")
	}
	reader := reportFixture(t)
	p.mu.Lock()
	p.reports = reader
	p.mu.Unlock()
	forwardWiringRegister(t, p, hookRaw(t, forwardWiringRegistration()))
	if p.forward.slots != p.reportSlots || cap(p.reportSlots) != 2 || len(p.reportSlots) != 1 {
		t.Fatal("independent publisher pool")
	}
	// One available slot succeeds: a store lease must not acquire a second slot.
	forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 200)
	// Another report computation (e.g. the dry-run report) holds the second slot.
	p.reportSlots <- struct{}{}
	r := forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 503)
	if string(r.Body) != `{"error":"busy"}` || len(p.reportSlots) != 2 {
		t.Fatal("busy acquired/stole slot", string(r.Body))
	}
	<-p.reportSlots
	forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 200)
	close(sealRelease)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	if len(p.reportSlots) != 0 {
		t.Fatal("shared slot leaked")
	}
}

func TestForwardWiringStatusErrorsStrictRPCAndNativeEnvelopeLimit(t *testing.T) {
	f := preparedRuntime(t)
	var fail atomic.Bool
	p := forwardWiringOpen(t, f, func(o *forwardOptions) {
		seal := o.seal
		o.seal = func(ctx context.Context, s *historyreports.ForwardStore, q historyreports.ForwardSeal) (historyreports.ForwardPin, error) {
			if fail.Load() {
				return historyreports.ForwardPin{}, errors.New("private-SQL /private/path")
			}
			return seal(ctx, s, q)
		}
	}, nil)
	forwardWiringRegister(t, p, hookRaw(t, forwardWiringRegistration()))
	ready := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	fail.Store(true)
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "fault-logical", c.ID)
	f.h.clock.Add(int64(3 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "fault-logical", "fault-physical", hookEpoch.Add(time.Second)))
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "error" && !v.Active })
	r := forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 200)
	var status forwardWiredStatus
	_ = json.Unmarshal(r.Body, &status)
	if status.Publisher.ErrorCode != "forward_seal_failed" || status.Publisher.Latest != ready.Latest || status.Runtime.State != "error" || status.Latest != ready.Latest {
		t.Fatal("error status upgraded/lost old publication", status)
	}
	forwardWiringCall(t, p, forwardWiringRequest(t, "pin", map[string]any{"generation": ready.Latest}), 200)
	for _, raw := range []string{
		`{"Method":"GET","Path":"` + forwardHTTPRoot + `status","Path":"/v0/management/plugins/pacer/state"}`,
		`{"Method":"GET","path":"` + forwardHTTPRoot + `status"}`,
		`{"Method":"GET","Path":"` + forwardHTTPRoot + `status","Headers":{"Accept":["one"],"Accept":["two"]}}`,
		`{"Method":"GET","Path":"` + forwardHTTPRoot + `status"} {}`,
	} {
		forwardWiringRaw(t, p, []byte(raw), 400)
	}
	req := forwardWiringRequest(t, "pin", map[string]any{})
	req.Body = append(req.Body, bytes.Repeat([]byte(" "), (64<<10)+1-len(req.Body))...)
	forwardWiringCall(t, p, req, 413)
	for _, headers := range []http.Header{{"Authorization": {"Bearer private-secret"}}, {"X-Pacer-Cache-Capability": {"private-secret"}}, {"Cookie": {"private-secret"}}} {
		req := forwardWiringRequest(t, "status", nil)
		req.Headers = headers
		forwardWiringCall(t, p, req, 400)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := p.handleForwardManagement(ctx, hookRaw(t, forwardWiringRequest(t, "status", nil))); r.StatusCode != 503 {
		t.Fatal("caller cancellation lost")
	}
	// Bound the actual raw C ABI serialization, including BOTH levels of base64.
	for _, size := range []int{(9 << 19) - 1024, 9 << 19} {
		body := forwardHTTPDownload{Filename: "pacer-forward.csv", MediaType: "text/csv", Encoding: "base64", Content: bytes.Repeat([]byte("x"), size)}
		r := forwardNativeResponseBound(forwardHTTPJSON(200, body))
		encoded := hookRaw(t, pluginabi.Envelope{OK: true, Result: hookRaw(t, r)})
		want := 200
		if size == 9<<19 {
			want = 413
		}
		if r.StatusCode != want || len(encoded) > forwardHTTPOutputLimit {
			t.Fatal("raw serialized native limit", size, r.StatusCode, len(encoded))
		}
		if want == 200 {
			var download forwardHTTPDownload
			_ = json.Unmarshal(r.Body, &download)
			if !bytes.Equal(download.Content, body.Content) {
				t.Fatal("successful output truncated")
			}
		}
	}
}
