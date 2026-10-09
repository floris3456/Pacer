package pacer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Antigravity and OpenCode Go are observed and recorded for the usage page
// only: never routed, never in the measurement report or forward ledger.
func TestUsageOnlyProvidersAreObservedAndRecordedNotRouted(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var mu sync.Mutex
	requests := map[string]string{}
	host := func(method string, input any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": []map[string]any{
				{"id": "a", "auth_index": "ia", "provider": "claude", "label": "A"},
				{"id": "antigravity-x@example.invalid.json", "auth_index": "iag", "provider": "antigravity", "label": "x@example.invalid"},
				{"id": "opencode-go-key-abc", "auth_index": "iog", "provider": "opencode-go"},
				{"id": "off", "auth_index": "ioff", "provider": "antigravity", "disabled": true},
			}})
		case "host.auth.get":
			switch input.(map[string]string)["auth_index"] {
			case "iag":
				return json.Marshal(map[string]any{"json": map[string]string{"access_token": "dummy-ag", "project_id": "proj-1"}})
			case "iog":
				return json.Marshal(map[string]any{"json": map[string]string{"api_key": "dummy-og"}})
			}
			return json.Marshal(map[string]any{"json": map[string]string{"account_uuid": "u-a", "access_token": "dummy-a"}})
		case "host.auth.get_runtime":
			ids := map[string]string{"ia": "a", "iag": "antigravity-x@example.invalid.json", "iog": "opencode-go-key-abc"}
			return json.Marshal(pluginapi.HostAuthGetRuntimeResponse{Auth: pluginapi.HostAuthFileEntry{ID: ids[input.(map[string]string)["auth_index"]]}})
		case "host.http.operation_open":
			return json.Marshal(map[string]string{"operation_id": "op"})
		case "host.http.do":
			call := input.(map[string]any)
			url := call["url"].(string)
			auth := call["headers"].(map[string][]string)["Authorization"][0]
			body, _ := call["body"].([]byte)
			mu.Lock()
			requests[url] = call["method"].(string) + " " + auth + " " + string(body)
			mu.Unlock()
			at := now()
			switch {
			case strings.Contains(url, "retrieveUserQuotaSummary"):
				return json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"groups":[{"displayName":"Gemini","buckets":[{"remainingFraction":0.6,"window":"5h","resetTime":"` + at.Add(3*time.Hour).Format(time.RFC3339) + `"}]}]}`)})
			case strings.Contains(url, "opencode.ai"):
				return json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"usage":{"monthly":{"percent":30,"resetsAt":"` + at.Add(20*24*time.Hour).Format(time.RFC3339) + `"}}}`)})
			}
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)})
		}
		return testHost(method, input)
	}
	p := New(host)
	p.now = now
	defer p.Close()
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("state_path: " + filepath.Join(t.TempDir(), "state.sqlite") + "\n")})
	store, err := historyreports.OpenRecordStore(context.Background(), filepath.Join(t.TempDir(), "usage-record.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p.mu.Lock()
	p.recordStore = store
	if len(p.creds) != 1 || len(p.usageCreds) != 2 {
		t.Fatal("discovery", p.creds, p.usageCreds)
	}
	ag := p.usageCreds["antigravity-x@example.invalid.json"]
	og := p.usageCreds["opencode-go-key-abc"]
	p.mu.Unlock()
	if ag.Alias != "x@example.invalid" || !strings.HasPrefix(ag.Account, "antigravity:") || og.Provider != "opencode-go" {
		t.Fatal(ag, og)
	}
	for i := 0; i < 3; i++ {
		clock.Add(int64(6 * time.Minute))
		p.reconcile()
	}
	mu.Lock()
	agCall := requests["https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"]
	ogCall := requests["https://opencode.ai/zen/go/v1/usage"]
	mu.Unlock()
	if agCall != `POST Bearer dummy-ag {"project":"proj-1"}` || ogCall != "GET Bearer dummy-og " {
		t.Fatalf("meter reads: %q %q", agCall, ogCall)
	}
	rows, err := p.observer.ReadWindows(ag.Account)
	if err != nil || len(rows) != 1 || rows[0].Scope != "gemini" || *rows[0].Remaining != 60 {
		t.Fatal("antigravity meter", rows, err)
	}
	rows, err = p.observer.ReadWindows(og.Account)
	if err != nil || len(rows) != 1 || rows[0].Seconds != 30*86400 || *rows[0].Remaining != 70 {
		t.Fatal("opencode go meter", rows, err)
	}
	// A request of a usage-only provider goes to the usage record only.
	at := now()
	envelope := map[string]any{"Provider": "antigravity", "AuthID": ag.ID, "AuthIndex": ag.Index, "LifecycleRequestID": "l1", "RequestID": "r1", "RequestedAt": at, "Latency": int64(time.Second), "Failed": false, "Model": "gemini-3.8-flash-high", "AccountingVersion": 2,
		"TokenBreakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 300, "input": map[string]any{"total_tokens": 200, "uncached_tokens": 200, "cache_read_tokens": 0, "cache_write_tokens": 0}, "output": map[string]any{"total_tokens": 100, "non_reasoning_tokens": 100, "reasoning_tokens": 0}, "unclassified_tokens": 0}}
	invoke(t, p, pluginabi.MethodUsageHandle, envelope)
	st, err := store.Stats(context.Background())
	if err != nil || st.Events != 1 {
		t.Fatal("usage-only request not recorded", st, err)
	}
	ref := historyreports.ForwardAccountRef(p.measurementSource, "antigravity", ag.Account)
	events, err := store.Events(context.Background(), []string{ref}, 0, at.Add(time.Hour).UnixMilli())
	if err != nil || len(events) != 1 || events[0].Event.Model != "gemini-3.8-flash-high" || events[0].Event.Tokens.Total != 300 || events[0].Origin != "usage" {
		t.Fatal(events, err)
	}
	if n := hookCount(t, p, `SELECT count(*) FROM measurement_records`); n != 0 {
		t.Fatal("a usage-only request entered the measurement report", n)
	}
	if changes := p.usageLive.take(); changes[ref] != at.UnixMilli() {
		t.Fatal("the usage loop was not told", changes)
	}
}

// The settings routes: accounts with opaque references, a re-login link, a
// profile edit and manual prices.
func TestUsageSettingsRoutes(t *testing.T) {
	ctx := context.Background()
	f := preparedRuntime(t)
	p := f.open(t, nil)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	reader := reportFixtureFrom(t, "historical-golden/historical-fixture.sql.gz", 240, 65)
	attachRecord(t, p, reader, nil)
	if err := p.refreshUsageLive(ctx); err != nil || p.usageLiveStatus().State != "live" {
		t.Fatal(err, p.usageLiveStatus())
	}
	call := func(method, op, body string) pluginapi.ManagementResponse {
		return invoke(t, p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: method, Path: "/v0/management/plugins/pacer/" + op, Body: []byte(body)}).(pluginapi.ManagementResponse)
	}
	var view usageAccountsView
	r := call("POST", "usage/accounts", `{}`)
	if r.StatusCode != 200 || json.Unmarshal(r.Body, &view) != nil || !view.Editable || len(view.Accounts) == 0 || len(view.Profiles) == 0 {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	if bytes.Contains(r.Body, []byte("usage-account-history")) {
		t.Fatal("raw account IDs reached the page")
	}
	// Link two claude logins: the later one disappears into the earlier.
	var claude []historyreports.UsageAccount
	for _, a := range view.Accounts {
		if a.Provider == "claude" {
			claude = append(claude, a)
		}
	}
	if len(claude) < 2 {
		t.Fatal("fixture: two claude accounts needed")
	}
	body, _ := json.Marshal(usageLinkEdit{Revision: view.LinksRevision, Login: claude[1].Logins[0].Ref, SameAs: claude[0].Ref, Linked: true, Note: "re-login"})
	if r = call("PATCH", "usage/links", string(body)); r.StatusCode != 200 {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	if r = call("PATCH", "usage/links", string(body)); r.StatusCode != 409 {
		t.Fatal("stale revision accepted", r.StatusCode)
	}
	if err := p.refreshUsageLive(ctx); err != nil {
		t.Fatal(err)
	}
	var linked usageAccountsView
	json.Unmarshal(call("POST", "usage/accounts", `{}`).Body, &linked)
	merged := false
	for _, a := range linked.Accounts {
		if a.Ref == claude[1].Ref {
			t.Fatal("the linked login is still its own account")
		}
		if a.Ref == claude[0].Ref && len(a.Logins) == len(claude[0].Logins)+len(claude[1].Logins) {
			merged = true
		}
	}
	if !merged {
		t.Fatal("logins not merged")
	}
	// A profile edit: create a profile and add the merged account to it.
	body, _ = json.Marshal(usageProfileEdit{Revision: linked.ProfilesRevision, Op: "create", Name: "Claude Max 5x", Provider: "claude"})
	r = call("PATCH", "usage/profile", string(body))
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	var rev struct{ Revision int64 }
	json.Unmarshal(r.Body, &rev)
	if err := p.refreshUsageLive(ctx); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(call("POST", "usage/accounts", `{}`).Body, &view)
	ref := ""
	for _, prof := range view.Profiles {
		if prof.Name == "Claude Max 5x" {
			ref = prof.Reference
		}
	}
	body, _ = json.Marshal(usageProfileEdit{Revision: rev.Revision, Op: "add", Profile: ref, Account: claude[0].Ref})
	if r = call("PATCH", "usage/profile", string(body)); r.StatusCode != 200 || ref == "" {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	if err := p.refreshUsageLive(ctx); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(call("POST", "usage/accounts", `{}`).Body, &view)
	for _, prof := range view.Profiles {
		if prof.Reference == ref && (len(prof.Accounts) != 1 || prof.Accounts[0].Account != claude[0].Ref || prof.Accounts[0].FromMS != 0) {
			t.Fatal("added account", prof)
		}
	}
	// The usage page shows the new profile's card with the account's history.
	var cards historyreports.CompareView
	json.Unmarshal(usageCall(p, "POST", "profiles", `{}`).Body, &cards)
	found := false
	for _, c := range cards.Profiles {
		if c.Reference == ref && c.Lifetime.Requests > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("the new profile has no card with the account's requests")
	}
	// Prices: the imported price is used; a manual price replaces it.
	var prices modelPricesView
	r = call("POST", "models/prices", `{"models":[{"provider":"claude","model":"claude-opus-5-5"},{"provider":"claude","model":"claude-new-9"}]}`)
	if r.StatusCode != 200 || json.Unmarshal(r.Body, &prices) != nil || len(prices.Models) != 2 || prices.Models[1].Price != nil {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	body, _ = json.Marshal(manualPriceEdit{Revision: prices.Revision, Model: "claude-new-9", Input: 3, Output: 15})
	if r = call("PATCH", "models/prices", string(body)); r.StatusCode != 200 {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	json.Unmarshal(call("POST", "models/prices", `{"models":[{"provider":"claude","model":"claude-new-9"}]}`).Body, &prices)
	if prices.Models[0].Source != historyreports.PriceManual || prices.Models[0].Price.Input != 3 {
		t.Fatal("manual price", prices.Models[0])
	}
	if r = call("PATCH", "models/prices", `{"revision":1,"model":"x","input":-1,"output":1}`); r.StatusCode != 400 {
		t.Fatal("negative price accepted", r.StatusCode)
	}
	_ = http.StatusOK
}
