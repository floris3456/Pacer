package pacer

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func usageCall(p *Plugin, method, op, body string) pluginapi.ManagementResponse {
	return p.manage(pluginapi.ManagementRequest{Method: method, Path: usageRoot + op, Body: []byte(body)})
}

func TestUsageRoutesInputsAndRemovedHistorySurface(t *testing.T) {
	p := usageFixture(t)
	registered, err := p.Handle(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(registered)
	for _, route := range usageRoutes() {
		if !bytes.Contains(raw, []byte(`"Path":"`+route["Path"]+`"`)) {
			t.Fatal("route not registered", route)
		}
	}
	if bytes.Contains(raw, []byte("/history/")) {
		t.Fatal("removed history/legacy routes are still registered")
	}
	for _, path := range []string{"/v0/management/plugins/pacer/history/compare", "/v0/management/plugins/pacer/history/report", "/v0/management/plugins/pacer/history/legacy/profiles"} {
		if got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: path, Body: []byte(`{}`)}); got.StatusCode != 404 {
			t.Fatal("removed route still answers", path, got.StatusCode)
		}
	}
	got := usageCall(p, "POST", "profiles", `{}`)
	var view historyreports.CompareView
	if got.StatusCode != 200 || json.Unmarshal(got.Body, &view) != nil || len(view.Profiles) == 0 || view.ExclusionsEditable || view.ExclusionsRevision != 0 {
		t.Fatal(got.StatusCode, string(got.Body))
	}
	var pool *historyreports.ComparePool
	var profile string
	for _, cp := range view.Profiles {
		for i := range cp.Pools {
			if cp.Pools[i].Latest != nil && pool == nil {
				pool, profile = &cp.Pools[i], cp.Reference
			}
		}
	}
	if pool == nil || pool.Latest.Models == nil {
		t.Fatal("no pool with a latest window and model mix")
	}
	// Both report slots busy must not delay the usage page.
	p.reportSlots <- struct{}{}
	p.reportSlots <- struct{}{}
	body, _ := json.Marshal(historyreports.InspectQuery{Profile: profile, Kind: pool.Kind, Scope: pool.Scope, Window: pool.Latest.ID})
	var inspect historyreports.InspectView
	if got = usageCall(p, "POST", "inspect", string(body)); got.StatusCode != 200 || json.Unmarshal(got.Body, &inspect) != nil || inspect.Mode != "window" {
		t.Fatal(got.StatusCode, string(got.Body))
	}
	body, _ = json.Marshal(historyreports.CompareWindowsQuery{Profile: profile, Kind: pool.Kind, Scope: pool.Scope})
	var list historyreports.CompareWindowList
	if got = usageCall(p, "POST", "windows", string(body)); got.StatusCode != 200 || json.Unmarshal(got.Body, &list) != nil || len(list.Windows) != pool.Windows {
		t.Fatal(got.StatusCode, string(got.Body))
	}
	<-p.reportSlots
	<-p.reportSlots
	// Without Pacer's usage store, exclusions are read-only (empty) and edits refuse.
	edit := `{"revision":0,"profile":"` + profile + `","window":"` + pool.Latest.ID + `","excluded":true}`
	if got = usageCall(p, "PATCH", "exclusions", edit); got.StatusCode != 409 {
		t.Fatal("edit without a usage store", got.StatusCode)
	}
	for _, c := range []struct{ method, op, raw string }{
		{"POST", "profiles", ``}, {"POST", "profiles", `null`}, {"POST", "profiles", `{"key":null}`},
		{"POST", "profiles", `{"Key":""}`}, {"POST", "profiles", `{"key":"sk-secret-private"}`},
		{"POST", "profiles", `{"from":"2026-13-01"}`}, {"POST", "profiles", `{"profile":"x"}`},
		{"POST", "windows", `{}`}, {"POST", "inspect", `{"profile":"` + profile + `","kind":"weekly"}`},
		{"POST", "inspect", `{"profile":"` + profile + `","kind":"daily","scope":"all"}`},
		{"PATCH", "exclusions", `{"revision":0,"profile":"x","window":"y","excluded":true,"extra":1}`},
	} {
		if got := usageCall(p, c.method, c.op, c.raw); got.StatusCode != 400 || bytes.Contains(got.Body, []byte("private")) {
			t.Fatal(c, got.StatusCode, string(got.Body))
		}
	}
	for _, request := range []pluginapi.ManagementRequest{
		{Method: "GET", Path: usageRoot + "profiles"},
		{Method: "POST", Path: usageRoot + "exclusions", Body: []byte(`{}`)},
		{Method: "PATCH", Path: usageRoot + "profiles", Body: []byte(`{}`)},
		{Method: "POST", Path: usageRoot + "profiles/", Body: []byte(`{}`)},
		{Method: "POST", Path: usageRoot + "Profiles", Body: []byte(`{}`)},
	} {
		if got := p.manage(request); got.StatusCode != 404 {
			t.Fatal("nonexact usage route", request.Method, request.Path, got.StatusCode)
		}
	}
	if got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: usageRoot + "profiles", Body: []byte(`{}`), Query: url.Values{"key": {"x"}}}); got.StatusCode != 400 {
		t.Fatal("query ignored", got.StatusCode)
	}
	p.mu.Lock()
	p.reports = nil
	p.mu.Unlock()
	if got := usageCall(p, "POST", "profiles", `{}`); got.StatusCode != 409 || !strings.Contains(string(got.Body), "prepared") {
		t.Fatal("missing artifact", got.StatusCode)
	}
}

func TestUsageExclusionsThroughTheForwardStore(t *testing.T) {
	f := preparedRuntime(t)
	p := forwardWiringOpen(t, f, nil, nil)
	reader := reportFixtureFrom(t, "historical-golden/historical-fixture.sql.gz", 240, 65)
	p.mu.Lock()
	p.reports = reader
	p.mu.Unlock()
	var view historyreports.CompareView
	got := usageCall(p, "POST", "profiles", `{}`)
	if got.StatusCode != 200 || json.Unmarshal(got.Body, &view) != nil || !view.ExclusionsEditable {
		t.Fatal(got.StatusCode, string(got.Body))
	}
	var profile, window string
	var before historyreports.ComparePool
	for _, cp := range view.Profiles {
		for _, pool := range cp.Pools {
			if window == "" && pool.Latest != nil && pool.Average.Status == "available" {
				profile, window, before = cp.Reference, pool.Latest.ID, pool
			}
		}
	}
	if window == "" {
		t.Fatal("fixture has no measured average")
	}
	edit := func(revision int64, excluded bool, note string) pluginapi.ManagementResponse {
		b, _ := json.Marshal(usageExclusionEdit{Revision: revision, Profile: profile, Window: window, Excluded: excluded, Note: note})
		return usageCall(p, "PATCH", "exclusions", string(b))
	}
	var saved historyreports.WindowExclusions
	if got = edit(view.ExclusionsRevision, true, "benchmark day"); got.StatusCode != 200 || json.Unmarshal(got.Body, &saved) != nil || saved.Revision != 1 {
		t.Fatal(got.StatusCode, string(got.Body))
	}
	if got = edit(0, false, ""); got.StatusCode != 409 {
		t.Fatal("stale revision accepted", got.StatusCode)
	}
	view = historyreports.CompareView{}
	if got = usageCall(p, "POST", "profiles", `{}`); json.Unmarshal(got.Body, &view) != nil || view.ExclusionsRevision != 1 {
		t.Fatal(string(got.Body))
	}
	found := false
	for _, cp := range view.Profiles {
		for _, pool := range cp.Pools {
			if cp.Reference == profile && pool.Kind == before.Kind && pool.Scope == before.Scope {
				found = true
				if pool.Excluded != 1 || pool.Latest == nil || !pool.Latest.Excluded || pool.Latest.Note != "benchmark day" || pool.Windows != before.Windows {
					t.Fatalf("excluded window not marked: %+v", pool)
				}
				if pool.Average.Samples == before.Average.Samples && pool.Average.Status == "available" {
					t.Fatal("average still counts the excluded window")
				}
			}
		}
	}
	if !found {
		t.Fatal("pool vanished")
	}
	if got = edit(1, false, ""); got.StatusCode != 200 {
		t.Fatal("restoring the window failed", got.StatusCode)
	}
}
