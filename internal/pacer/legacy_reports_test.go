package pacer

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func legacyHTTPFixture(t *testing.T) *Plugin {
	t.Helper()
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	p.reports = reportFixtureFrom(t, "historical-golden/historical-fixture.sql.gz", 240, 65)
	t.Cleanup(p.Close)
	return p
}
func legacyHTTP(t *testing.T, p *Plugin, op string, body any) pluginapi.ManagementResponse {
	t.Helper()
	b, e := json.Marshal(body)
	if e != nil {
		t.Fatal(e)
	}
	return p.manage(pluginapi.ManagementRequest{Method: "POST", Path: legacyHistoryRoot + op, Body: b})
}
func readLegacyCases(t *testing.T) []struct {
	Name, View, Window, Kind string
	Filter                   historyreports.LegacyPin
	Quota                    *historyreports.EvidencePin
} {
	t.Helper()
	f, e := os.Open("../historyreports/testdata/historical-golden/historical-oracle.json.gz")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	z, e := gzip.NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	var cases []struct {
		Name, View, Window, Kind string
		Filter                   historyreports.LegacyPin
		Quota                    *historyreports.EvidencePin
	}
	if e = json.NewDecoder(z).Decode(&cases); e != nil {
		t.Fatal(e)
	}
	return cases
}
func TestLegacyHTTPDomainAndExportParity(t *testing.T) {
	p := legacyHTTPFixture(t)
	ctx := context.Background()
	responses, exports := 0, 0
	for _, tc := range readLegacyCases(t) {
		t.Run(tc.View+"/"+tc.Name, func(t *testing.T) {
			q := historyreports.LegacyQuery{Pin: &tc.Filter}
			var body, domain any
			var e error
			switch tc.View {
			case "profiles":
				body = legacyProfileRequest{Query: q}
				domain, e = p.reports.ProfileReport(ctx, q)
			case "profile-proof":
				body = legacyProofRequest{Query: q, WindowID: tc.Window}
				domain, e = p.reports.ProfileProof(ctx, q, tc.Window)
			case "history":
				body = legacyHistoryRequest{Query: q, Kind: tc.Kind}
				domain, e = p.reports.HistoryReport(ctx, q, tc.Kind)
			case "history-proof":
				body = legacyProofRequest{Query: q, WindowID: tc.Window}
				domain, e = p.reports.HistoryProof(ctx, q, tc.Window)
			case "quota":
				body = legacyQuotaRequest{Query: historyreports.QuotaQuery{Pin: tc.Quota}}
				domain, e = p.reports.QuotaUsage(ctx, historyreports.QuotaQuery{Pin: tc.Quota})
			case "forecasts":
				body = legacyForecastRequest{Query: q, WindowID: tc.Window, Limit: 100}
				domain, e = p.reports.Forecasts(ctx, historyreports.ForecastQuery{Selection: q, WindowID: tc.Window, Limit: 100})
			default:
				t.Fatal(tc.View)
			}
			if e != nil {
				t.Fatal(e)
			}
			var want bytes.Buffer
			if e = json.NewEncoder(&want).Encode(domain); e != nil {
				t.Fatal(e)
			}
			got := legacyHTTP(t, p, tc.View, body)
			if got.StatusCode != 200 || !bytes.Equal(got.Body, want.Bytes()) || got.Headers.Get("Cache-Control") != "no-store" {
				t.Fatal("domain response changed", got.StatusCode, string(got.Body))
			}
			responses++
			if bytes.Contains(got.Body, []byte(p.reports.ArtifactPath())) {
				t.Fatal("artifact path leaked")
			}
			if tc.View == "forecasts" {
				return
			}
			for _, format := range []string{"json", "csv"} {
				want.Reset()
				op := "export"
				o := historyreports.LegacyExportOptions{View: tc.View, WindowID: tc.Window, Kind: tc.Kind}
				if tc.View == "quota" {
					op = "quota-export"
					q := historyreports.QuotaQuery{Pin: tc.Quota}
					body = legacyQuotaExportRequest{Query: q, Format: format}
					if format == "json" {
						e = p.reports.ExportQuotaJSON(ctx, q, &want)
					} else {
						e = p.reports.ExportQuotaCSV(ctx, q, &want)
					}
				} else {
					body = legacyExportRequest{Query: q, View: tc.View, WindowID: tc.Window, Kind: tc.Kind, Format: format}
					if format == "json" {
						e = p.reports.ExportLegacyJSON(ctx, q, o, &want)
					} else {
						e = p.reports.ExportLegacyCSV(ctx, q, o, &want)
					}
				}
				if e != nil {
					t.Fatal(e)
				}
				got = legacyHTTP(t, p, op, body)
				var envelope map[string]string
				if got.StatusCode != 200 || json.Unmarshal(got.Body, &envelope) != nil || envelope["content"] != want.String() || envelope["filename"] != "pacer-legacy-"+tc.View+"."+format {
					t.Fatal("HTTP export changed exact domain writer bytes", format, got.StatusCode)
				}
				media := "application/json"
				if format == "csv" {
					media = "text/csv"
				}
				if envelope["media_type"] != media {
					t.Fatal(envelope)
				}
				exports++
			}
		})
	}
	if responses != 53 || exports != 104 {
		t.Fatal(responses, exports)
	}
	t.Logf("%d exact domain responses and %d exact domain-writer exports", responses, exports)
}

func TestLegacyHTTPExactManagementRoutesAndRefusals(t *testing.T) {
	p := legacyHTTPFixture(t)
	registered, e := p.Handle(pluginabi.MethodManagementRegister, nil)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(registered)
	var manifest struct {
		Resources []map[string]string
		Routes    []map[string]string
	}
	if json.Unmarshal(raw, &manifest) != nil {
		t.Fatal("registration")
	}
	want := map[string]bool{"GET /plugins/pacer/history/legacy/status": true}
	for _, op := range []string{"profiles", "profile-proof", "history", "history-proof", "forecasts", "quota", "export", "quota-export"} {
		want["POST /plugins/pacer/history/legacy/"+op] = true
	}
	for _, r := range manifest.Resources {
		if strings.Contains(r["Path"], "history") {
			t.Fatal("history advertised as resource", r)
		}
	}
	for _, r := range manifest.Routes {
		key := r["Method"] + " " + r["Path"]
		if strings.Contains(r["Path"], "/legacy/") {
			if !want[key] {
				t.Fatal("unexpected/duplicate route", key)
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Fatal("routes missing", want)
	}
	for _, op := range []string{"status", "profiles", "profile-proof", "history", "history-proof", "forecasts", "quota", "export", "quota-export"} {
		for _, method := range []string{"PUT", "PATCH", "DELETE", "HEAD"} {
			got := p.manage(pluginapi.ManagementRequest{Method: method, Path: legacyHistoryRoot + op, Body: []byte(`{}`)})
			if got.StatusCode != 404 {
				t.Fatal("mutation/method exposed", op, method, got.StatusCode)
			}
		}
		for _, path := range []string{"/v0/resource/plugins/pacer/history/legacy/" + op, legacyHistoryRoot + op + "/", legacyHistoryRoot + "../" + op, legacyHistoryRoot + op + "?x=1"} {
			if got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: path, Body: []byte(`{}`)}); got.StatusCode != 404 {
				t.Fatal("nonexact route", path, got.StatusCode)
			}
		}
	}
	for _, op := range []string{"prepare", "import", "sqlite", "refresh", "forecast-tick", "profile-mutate", "Profiles"} {
		if got := legacyHTTP(t, p, op, map[string]any{}); got.StatusCode != 404 {
			t.Fatal(op, got.StatusCode)
		}
	}
	for _, request := range []pluginapi.ManagementRequest{{Method: "GET", Path: legacyHistoryRoot + "profiles"}, {Method: "POST", Path: legacyHistoryRoot + "status", Body: []byte(`{}`)}} {
		if got := p.manage(request); got.StatusCode != 404 {
			t.Fatal("wrong method", got.StatusCode)
		}
	}
	for _, request := range []pluginapi.ManagementRequest{{Method: "GET", Path: legacyHistoryRoot + "status", Body: []byte(`{}`)}, {Method: "GET", Path: legacyHistoryRoot + "status", Query: url.Values{"revision": {"0"}}}, {Method: "POST", Path: legacyHistoryRoot + "profiles", Body: []byte(`{}`), Query: url.Values{"path": {"private"}}}} {
		if got := p.manage(request); got.StatusCode != 400 {
			t.Fatal("query/body ignored", got.StatusCode)
		}
	}
}

func TestLegacyHTTPStrictInputAndPinBounds(t *testing.T) {
	p := legacyHTTPFixture(t)
	for _, raw := range []string{`null`, `[]`, `{} {}`, `{"query":null}`, `{"query":{"pin":null}}`, `{"Query":{}}`, `{"query":{},"query":{}}`, `{"query":{"selection":{"Model":"x"}}}`, `{"query":{"selection":{"model":"a","model":"b"}}}`, `{"query":{"selection":{"model":null}}}`, `{"query":{"selection":{"key_hashes":["secret-raw-key"]}}}`, `{"query":{"selection":{"path":"/private/sqlite"}}}`, `{"archive_path":"/private/sqlite"}`, `{"query":{},"limit":0}`, `{"query":{},"kind":""}`, `{"query":{"selection":{"pin":{}}}}`, `{"query":{"pin":{"at":1790812800000,"page":0}}}`} {
		got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: legacyHistoryRoot + "profiles", Body: []byte(raw)})
		if got.StatusCode != 400 {
			t.Fatal(raw, got.StatusCode, string(got.Body))
		}
		if bytes.Contains(got.Body, []byte("private")) || bytes.Contains(got.Body, []byte("secret-raw-key")) {
			t.Fatal("input echoed")
		}
	}
	for _, c := range []struct{ op, raw string }{{"quota", `{"query":{"capacity_ratio":null}}`}, {"quota", `{"query":{"capacity_ratio":1,"capacity_ratio":2}}`}, {"quota", `{"query":{"pin":{"At":1}}}`}, {"quota", `{"query":{"basis":"later"}}`}, {"quota", `{"query":{"capacity_ratio":10001}}`}, {"quota", `{"query":{"capacity_ratio":1}}`}, {"history", `{"query":{"selection":{"model":"x"}}}`}, {"history-proof", `{"window_id":"forecast-window","query":{"selection":{"from":1}}}`}, {"forecasts", `{"window_id":"forecast-window","limit":101}`}, {"forecasts", `{"window_id":"forecast-window","limit":-1}`}, {"forecasts", `{"window_id":"forecast-window","query":{"selection":{"profile_reference":"all"}}}`}, {"forecasts", `{"window_id":"forecast-window","format":""}`}, {"profile-proof", `{}`}, {"export", `{"view":"profiles","format":"zip"}`}, {"export", `{"view":"sqlite","format":"json"}`}, {"quota-export", `{"format":"json","view":"quota"}`}, {"quota-export", `{"format":"json","query":{"selection":{"model":false}}}`}} {
		got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: legacyHistoryRoot + c.op, Body: []byte(c.raw)})
		if got.StatusCode != 400 {
			t.Fatal(c, got.StatusCode, string(got.Body))
		}
	}
	base := p.reports.DefaultPin().ProfileFilter
	base.ValuationRule = ""
	for _, mutate := range []func(*historyreports.LegacyPin){func(p *historyreports.LegacyPin) { p.At++ }, func(p *historyreports.LegacyPin) { p.Through++ }, func(p *historyreports.LegacyPin) { p.ObservationsThrough++ }, func(p *historyreports.LegacyPin) { p.ForecastsThrough++ }, func(p *historyreports.LegacyPin) { p.VerificationsThrough++ }, func(p *historyreports.LegacyPin) { p.TokenEvidenceThrough++ }} {
		pin := base
		mutate(&pin)
		if got := legacyHTTP(t, p, "profiles", legacyProfileRequest{Query: historyreports.LegacyQuery{Pin: &pin}}); got.StatusCode != 400 {
			t.Fatal("future pin", pin, got.StatusCode)
		}
	}
	if got := legacyHTTP(t, p, "profiles", legacyProfileRequest{Query: historyreports.LegacyQuery{Pin: &base, Selection: historyreports.Query{Model: "x"}}}); got.StatusCode != 400 {
		t.Fatal("pin overridden", got.StatusCode)
	}
	if got := legacyHTTP(t, p, "forecasts", legacyForecastRequest{WindowID: "forecast-window", Cursor: strings.Repeat("x", 4097)}); got.StatusCode != 400 {
		t.Fatal("oversized cursor", got.StatusCode)
	}
	if got := legacyHTTP(t, p, "profile-proof", legacyProofRequest{WindowID: strings.Repeat("x", 257)}); got.StatusCode != 400 {
		t.Fatal("oversized window", got.StatusCode)
	}
}

func TestLegacyHTTPPagingAdmissionSizesAndReadiness(t *testing.T) {
	p := legacyHTTPFixture(t)
	ctx := context.Background()
	status := p.manage(pluginapi.ManagementRequest{Method: "GET", Path: legacyHistoryRoot + "status"})
	var ready struct {
		Ready bool                     `json:"historical_ready"`
		Pin   historyreports.LegacyPin `json:"default_pin"`
	}
	if status.StatusCode != 200 || json.Unmarshal(status.Body, &ready) != nil || !ready.Ready || ready.Pin.At != 1790812800000 || ready.Pin.Through != 240 || ready.Pin.ValuationRule != "" {
		t.Fatal(string(status.Body))
	}
	body := legacyForecastRequest{Query: historyreports.LegacyQuery{Pin: &ready.Pin}, WindowID: "forecast-window", Limit: 1}
	first := legacyHTTP(t, p, "forecasts", body)
	var page historyreports.ForecastPage
	if first.StatusCode != 200 || json.Unmarshal(first.Body, &page) != nil || page.Total != 2 || len(page.Forecasts) != 1 || page.NextCursor == "" {
		t.Fatal(string(first.Body))
	}
	if page.Forecasts[0].Forecast.ID != "saved-0" || len(page.Forecasts[0].Results) != 1 {
		t.Fatal("outside-H/cursor evidence leaked")
	}
	body.Cursor = page.NextCursor
	next := legacyHTTP(t, p, "forecasts", body)
	page = historyreports.ForecastPage{}
	if next.StatusCode != 200 || json.Unmarshal(next.Body, &page) != nil || page.NextCursor != "" || page.Forecasts[0].Forecast.ID != "saved-1" || len(page.Forecasts[0].Results) != 2 || page.Forecasts[0].Results[1].State != "unscorable" {
		t.Fatal(string(next.Body))
	}
	body.Limit = 2
	if got := legacyHTTP(t, p, "forecasts", body); got.StatusCode != 409 {
		t.Fatal("cursor not filter-bound")
	}
	body.Limit = 1
	body.Cursor += "x"
	if got := legacyHTTP(t, p, "forecasts", body); got.StatusCode != 409 {
		t.Fatal("tampered cursor")
	}
	p.reportSlots <- struct{}{}
	p.reportSlots <- struct{}{}
	for _, path := range []string{legacyHistoryRoot + "profiles", historyRoot + "report"} {
		got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: path, Body: []byte(`{}`)})
		if got.StatusCode != 429 || got.Headers.Get("Retry-After") != "1" {
			t.Fatal("gate not shared", got.StatusCode)
		}
	}
	<-p.reportSlots
	<-p.reportSlots
	pad := append([]byte(`{}`), bytes.Repeat([]byte(" "), (64<<10)-2)...)
	for _, n := range []int{0, 1} {
		got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: legacyHistoryRoot + "profiles", Body: append(append([]byte{}, pad...), bytes.Repeat([]byte(" "), n)...)})
		want := 200
		if n == 1 {
			want = 413
		}
		if got.StatusCode != want {
			t.Fatal("input byte boundary", n, got.StatusCode)
		}
	}
	var b boundedReport
	if _, e := b.Write(make([]byte, maxHistoryOutput)); e != nil {
		t.Fatal(e)
	}
	if _, e := b.Write([]byte{0}); !errors.Is(e, errReportSize) || b.Len() != maxHistoryOutput {
		t.Fatal("writer limit", e)
	}
	// Output bounding includes JSON escaping and the export envelope itself.
	if got := boundedReportResponse(legacyDownload("profiles", "json", strings.Repeat("<", maxHistoryOutput/6))); got.StatusCode != 413 {
		t.Fatal("encoded envelope limit", got.StatusCode)
	}
	if got := reportError(errors.New("sqlite /private/source secret")); got.StatusCode != 503 || bytes.Contains(got.Body, []byte("private")) {
		t.Fatal("source error exposed")
	}
	// Restore old preparation metadata in a disposable fixture only.
	path := p.reports.ArtifactPath()
	if e := p.reports.Close(); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(path, 0600); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite3", path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`UPDATE pacer_report_preparation SET payload=json_remove(payload,'$.HistoricalReads'); DROP TABLE pacer_historical_reports;`); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(path, 0400); e != nil {
		t.Fatal(e)
	}
	p.reports, e = historyreports.Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	status = p.manage(pluginapi.ManagementRequest{Method: "GET", Path: legacyHistoryRoot + "status"})
	if status.StatusCode != 200 || json.Unmarshal(status.Body, &ready) != nil || ready.Ready {
		t.Fatal("old readiness", string(status.Body))
	}
	if got := legacyHTTP(t, p, "profiles", legacyProfileRequest{}); got.StatusCode != 409 || !bytes.Contains(got.Body, []byte("offline re-preparation")) {
		t.Fatal("old artifact silently initialized", got.StatusCode)
	}
	if got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: historyRoot + "report", Body: []byte(`{}`)}); got.StatusCode != 200 {
		t.Fatal("stable report broken", got.StatusCode)
	}
	old := p.reports
	p.reports = nil
	if got := legacyHTTP(t, p, "profiles", legacyProfileRequest{}); got.StatusCode != 409 {
		t.Fatal("no artifact", got.StatusCode)
	}
	p.reports = old
}
