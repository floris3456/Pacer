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

func compareHTTP(p *Plugin, op, body string) pluginapi.ManagementResponse {
	return p.manage(pluginapi.ManagementRequest{Method: "POST", Path: historyRoot + op, Body: []byte(body)})
}

func TestCompareHTTPRoutesInputsAndAdmission(t *testing.T) {
	p := legacyHTTPFixture(t)
	registered, e := p.Handle(pluginabi.MethodManagementRegister, nil)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(registered)
	for _, op := range []string{"compare", "compare-windows"} {
		if !bytes.Contains(raw, []byte(`"Path":"/plugins/pacer/history/`+op+`"`)) {
			t.Fatal("route not registered", op)
		}
	}
	got := compareHTTP(p, "compare", `{}`)
	var view historyreports.CompareView
	if got.StatusCode != 200 || json.Unmarshal(got.Body, &view) != nil || view.Version != historyreports.CompareVersion || len(view.Profiles) == 0 {
		t.Fatal(got.StatusCode, string(got.Body))
	}
	// Both slots held by full calculations must not delay Compare.
	p.reportSlots <- struct{}{}
	p.reportSlots <- struct{}{}
	if got = compareHTTP(p, "compare", `{"key":"unattributed","from":"2026-09-01","through":"2026-10-01"}`); got.StatusCode != 200 {
		t.Fatal("compare waited for report slots", got.StatusCode, string(got.Body))
	}
	var pool *historyreports.ComparePool
	var profile string
	for _, cp := range view.Profiles {
		if len(cp.Pools) > 0 {
			pool, profile = &cp.Pools[0], cp.Reference
			break
		}
	}
	if pool == nil {
		t.Fatal("fixture has no pools")
	}
	body, _ := json.Marshal(historyreports.CompareWindowsQuery{Profile: profile, Kind: pool.Kind, Scope: pool.Scope})
	got = compareHTTP(p, "compare-windows", string(body))
	var list historyreports.CompareWindowList
	if got.StatusCode != 200 || json.Unmarshal(got.Body, &list) != nil || len(list.Windows) != pool.Windows {
		t.Fatal(got.StatusCode, string(got.Body))
	}
	<-p.reportSlots
	<-p.reportSlots
	for _, c := range []struct{ op, raw string }{
		{"compare", ``}, {"compare", `null`}, {"compare", `[]`}, {"compare", `{} {}`},
		{"compare", `{"key":null}`}, {"compare", `{"Key":""}`}, {"compare", `{"key":"a","key":"b"}`},
		{"compare", `{"key":"sk-secret-private"}`}, {"compare", `{"from":"2026-13-01"}`},
		{"compare", `{"from":"2026-10-02","through":"2026-10-01"}`}, {"compare", `{"profile":"x"}`},
		{"compare", `{"path":"/private/sqlite"}`}, {"compare-windows", `{}`},
		{"compare-windows", `{"profile":"` + profile + `","kind":"monthly","scope":"all"}`},
		{"compare-windows", `{"profile":"profile:` + strings.Repeat("0", 64) + `","kind":"weekly","scope":"all"}`},
	} {
		got := compareHTTP(p, c.op, c.raw)
		if got.StatusCode != 400 {
			t.Fatal(c, got.StatusCode, string(got.Body))
		}
		if bytes.Contains(got.Body, []byte("private")) {
			t.Fatal("input echoed")
		}
	}
	for _, request := range []pluginapi.ManagementRequest{
		{Method: "GET", Path: historyRoot + "compare"},
		{Method: "PATCH", Path: historyRoot + "compare", Body: []byte(`{}`)},
		{Method: "POST", Path: historyRoot + "compare/", Body: []byte(`{}`)},
		{Method: "POST", Path: historyRoot + "Compare", Body: []byte(`{}`)},
	} {
		if got := p.manage(request); got.StatusCode != 404 {
			t.Fatal("nonexact compare route", request.Method, request.Path, got.StatusCode)
		}
	}
	if got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: historyRoot + "compare", Body: []byte(`{}`), Query: url.Values{"key": {"x"}}}); got.StatusCode != 400 {
		t.Fatal("query ignored", got.StatusCode)
	}
	if got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: historyRoot + "compare", Body: append([]byte(`{}`), bytes.Repeat([]byte(" "), 64<<10)...)}); got.StatusCode != 413 {
		t.Fatal("oversized body", got.StatusCode)
	}
	p.mu.Lock()
	p.reports = nil
	p.mu.Unlock()
	if got := compareHTTP(p, "compare", `{}`); got.StatusCode != 409 {
		t.Fatal("missing artifact", got.StatusCode)
	}
}
