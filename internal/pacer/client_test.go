package pacer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestClientDeclarationIsPerRequestAndNeverRefuses(t *testing.T) {
	for raw, want := range map[string]bool{
		"v=1; caps=quota":       true,
		"V=1;caps=QUOTA,future": true,
		" v=1 ; caps = quota ":  true,
		"v=1; caps=future":      false,
		"v=2; caps=quota":       false,
		"caps=quota":            false,
		"quota":                 false,
		"":                      false,
	} {
		if got := clientDeclaration(http.Header{clientHeader: {raw}}).Quota; got != want {
			t.Errorf("%q: quota=%v, want %v", raw, got, want)
		}
	}
}

// reply runs one request through Pacer's hooks the way CPA does and returns
// what reached the upstream and the headers Pacer added to the reply.
func reply(t *testing.T, p *Plugin, id, auth, model string, declared string, upstream http.Header) (http.Header, http.Header) {
	t.Helper()
	headers := http.Header{"X-Session-Affinity": {"chat-" + id}}
	if declared != "" {
		headers.Set(clientHeader, declared)
	}
	req := pluginapi.RequestInterceptRequest{RequestID: id, Model: model, Headers: headers, Metadata: map[string]any{"caller_scope": "test-caller", "selected_auth_id": auth}}
	before := invoke(t, p, pluginabi.MethodRequestInterceptBefore, req).(pluginapi.RequestInterceptResponse)
	if before.Terminate {
		t.Fatal("refused before", before.StatusCode, string(before.ResponseBody))
	}
	sent := headers.Clone()
	for k, v := range before.Headers {
		sent[k] = v
	}
	after := invoke(t, p, pluginabi.MethodRequestInterceptAfter, req).(pluginapi.RequestInterceptResponse)
	if after.Terminate {
		t.Fatal("refused after", after.StatusCode, string(after.ResponseBody))
	}
	for _, k := range after.ClearHeaders {
		sent.Del(k)
	}
	defer invoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: id})
	r := invoke(t, p, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: id, Metadata: req.Metadata, ResponseHeaders: upstream}).(pluginapi.ResponseInterceptResponse)
	if !slices.Contains(quotaAckHeaders(), quotaReplyHeader) {
		t.Fatal("an upstream could set X-Pacer-Quota itself")
	}
	return sent, r.Headers
}

func decodeQuota(t *testing.T, h http.Header) quotaReply {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(h.Get(quotaReplyHeader))
	if err != nil {
		t.Fatal(err)
	}
	var q quotaReply
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQuotaReplyOnlyForDeclaredClients(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	at := p.now()
	c := p.creds["a"]
	weekly := 70.0
	if err := p.observer.Observe(c.Account, "claude", []observation.Window{
		{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &weekly, Reset: at.Add(72 * time.Hour), Usage: observation.Field{At: at, Source: "header", Quality: "measured"}, ResetEvidence: observation.Field{At: at, Source: "header", Quality: "absolute"}},
		{Provider: "claude", Scope: "opus", Seconds: 604800, NotApplicable: true, StatusEvidence: observation.Field{At: at, Source: "api", Quality: "measured"}},
	}); err != nil {
		t.Fatal(err)
	}
	reset := at.Add(2 * time.Hour).Truncate(time.Second)
	upstream := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(reset.Unix(), 10)}}

	// Without a declaration the reply is CPA's own, and nothing is refused.
	sent, added := reply(t, p, "plain", "a", "claude-opus-5-5", "", upstream)
	if added.Get(quotaReplyHeader) != "" || sent.Get(clientHeader) != "" {
		t.Fatal("quota sent to a client that did not ask", added)
	}
	_, added = reply(t, p, "unknown-version", "a", "claude-opus-5-5", "v=9; caps=quota", upstream)
	if added.Get(quotaReplyHeader) != "" {
		t.Fatal("an unknown declaration version got quota")
	}

	// Declared: the reply carries the serving account's windows, including
	// the ones this very response reported, and the upstream never sees the
	// declaration.
	sent, added = reply(t, p, "declared", "a", "claude-opus-5-5", "v=1; caps=quota", upstream)
	if sent.Get(clientHeader) != "" {
		t.Fatal("X-Pacer-Client reached the upstream")
	}
	q := decodeQuota(t, added)
	if q.V != 1 || q.Provider != "claude" || q.Auth != "a" || q.Alias != "A" || len(q.Windows) != 2 {
		t.Fatalf("%+v", q)
	}
	five, week := q.Windows[0], q.Windows[1]
	if five.Seconds != 18000 || five.Left == nil || *five.Left != 60 || five.ResetMS != reset.UnixMilli() || five.ResetQuality != "absolute" || five.State != "current" || five.Source != "header" || !five.Applies {
		t.Fatalf("5h from this response: %+v", five)
	}
	if week.Seconds != 604800 || *week.Left != 70 || week.ReadMS != at.UnixMilli() {
		t.Fatalf("weekly: %+v", week)
	}
}

func TestQuotaReplyForUsageOnlyProvider(t *testing.T) {
	host := func(method string, input any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.Marshal(map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "ia", "provider": "claude", "label": "A"}, {"id": "antigravity-x.json", "auth_index": "iag", "provider": "antigravity", "label": "x"}}})
		}
		return testHost(method, input)
	}
	p := New(host)
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("enabled: true\nstate_path: " + filepath.Join(t.TempDir(), "state.sqlite") + "\n")})
	defer p.Close()
	p.mu.Lock()
	ag, found := p.usageCreds["antigravity-x.json"]
	p.mu.Unlock()
	if !found {
		t.Fatal("antigravity login not discovered")
	}
	at := p.now()
	left := 55.0
	if err := p.observer.Observe(ag.Account, "antigravity", []observation.Window{{Provider: "antigravity", Scope: "gemini", Seconds: 18000, Remaining: &left, Reset: at.Add(time.Hour), Usage: observation.Field{At: at, Source: "api", Quality: "measured"}, ResetEvidence: observation.Field{At: at, Source: "api", Quality: "absolute"}}}); err != nil {
		t.Fatal(err)
	}
	_, added := reply(t, p, "ag", ag.ID, "gemini-3.8-pro-high", "v=1; caps=quota", nil)
	q := decodeQuota(t, added)
	if q.Provider != "antigravity" || len(q.Windows) != 1 || q.Windows[0].Scope != "gemini" || *q.Windows[0].Left != 55 || q.Windows[0].Source != "api" {
		t.Fatalf("%+v", q)
	}
	// The usage view lists usage-only accounts too.
	key := "client-key-0123456789"
	cpa := fakeModelList(t, key, `{"object":"list","data":[{"id":"m"}]}`)
	p.mu.Lock()
	p.cfg.UsageAuthURL = cpa
	p.mu.Unlock()
	r := p.events(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/pacer/events", Headers: http.Header{"Authorization": {"Bearer " + key}}, Query: url.Values{"auth": {"api-key"}, "view": {"usage"}, "all": {"true"}}})
	var view struct {
		Credentials []struct {
			ID       string           `json:"id"`
			Provider string           `json:"provider"`
			Windows  []map[string]any `json:"windows"`
		} `json:"credentials"`
	}
	if r.StatusCode != 200 || json.Unmarshal(r.Body, &view) != nil || !slices.ContainsFunc(view.Credentials, func(c struct {
		ID       string           `json:"id"`
		Provider string           `json:"provider"`
		Windows  []map[string]any `json:"windows"`
	}) bool {
		return c.ID == ag.ID && c.Provider == "antigravity" && len(c.Windows) == 1
	}) {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	// v=1: the client format, including windows the older format hides (no
	// reset yet, stale).
	weekly := 100.0
	if err := p.observer.Observe(ag.Account, "antigravity", []observation.Window{{Provider: "antigravity", Scope: "gemini", Seconds: 604800, Remaining: &weekly, Usage: observation.Field{At: at, Source: "api", Quality: "measured"}}}); err != nil {
		t.Fatal(err)
	}
	r = p.events(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/pacer/events", Headers: http.Header{"Authorization": {"Bearer " + key}}, Query: url.Values{"auth": {"api-key"}, "view": {"usage"}, "all": {"true"}, "v": {"1"}}})
	var v1 struct {
		Credentials []struct {
			ID      string        `json:"id"`
			Windows []quotaWindow `json:"windows"`
		} `json:"credentials"`
	}
	if r.StatusCode != 200 || json.Unmarshal(r.Body, &v1) != nil {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	listed := false
	for _, c := range v1.Credentials {
		if c.ID == ag.ID {
			listed = true
			if len(c.Windows) != 2 || c.Windows[1].State != "partial" || c.Windows[1].ResetMS != 0 || *c.Windows[1].Left != 100 {
				t.Fatalf("client usage view: %+v", c.Windows)
			}
		}
	}
	if !listed {
		t.Fatal("antigravity login missing from the client usage view", string(r.Body))
	}
}

// fakeModelList is CPA's /v1/models on loopback: it refuses requests without
// the key and answers the key with body.
func fakeModelList(t *testing.T, key, body string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	endpoint, err := validateUsageURL(s.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func TestDiscoveryNamesClientDeclaration(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	var d struct {
		Client struct {
			Version       int      `json:"version"`
			RequestHeader string   `json:"request_header"`
			Caps          []string `json:"caps"`
			QuotaHeader   string   `json:"quota_header"`
			Views         []string `json:"views"`
		} `json:"client"`
	}
	r := p.events(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/pacer/events", Query: url.Values{}})
	if json.Unmarshal(r.Body, &d) != nil || d.Client.Version != 1 || d.Client.RequestHeader != clientHeader || d.Client.QuotaHeader != quotaReplyHeader || !slices.Contains(d.Client.Caps, "quota") || !slices.Contains(d.Client.Views, "prices") {
		t.Fatal(string(r.Body))
	}
}

// Clients get Pacer's price for each model their key can use, with its source.
func TestClientPricesFollowModelSettings(t *testing.T) {
	ctx := context.Background()
	f := preparedRuntime(t)
	p := f.open(t, nil)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	reader := reportFixtureFrom(t, "historical-golden/historical-fixture.sql.gz", 240, 65)
	attachRecord(t, p, reader, nil)
	if err := p.refreshUsageLive(ctx); err != nil {
		t.Fatal(err)
	}
	manage := func(method, op, body string) pluginapi.ManagementResponse {
		return invoke(t, p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: method, Path: "/v0/management/plugins/pacer/" + op, Body: []byte(body)}).(pluginapi.ManagementResponse)
	}
	var prices modelPricesView
	json.Unmarshal(manage("POST", "models/prices", `{"models":[]}`).Body, &prices)
	body, _ := json.Marshal(manualPriceEdit{Revision: prices.Revision, Model: "claude-new-9", Input: 3, Output: 15})
	if r := manage("PATCH", "models/prices", string(body)); r.StatusCode != 200 {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	key := "client-key-0123456789"
	p.mu.Lock()
	p.cfg.UsageAuthURL = fakeModelList(t, key, `{"object":"list","schema":"cliproxyapi.model-details/1","data":[{"id":"claude-opus-5-5","providers":["claude"]},{"id":"claude-new-9","providers":["claude"]},{"id":"unpriced-model-x","providers":["openai-compatible"]}]}`)
	p.mu.Unlock()
	view := func(token string) (int, map[string]clientPrice) {
		r := p.events(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/pacer/events", Headers: http.Header{"Authorization": {"Bearer " + token}}, Query: url.Values{"auth": {"api-key"}, "view": {"prices"}}})
		var out struct {
			Unit   string                 `json:"unit"`
			Models map[string]clientPrice `json:"models"`
		}
		json.Unmarshal(r.Body, &out)
		if r.StatusCode == 200 && out.Unit != "usd_per_million_tokens" {
			t.Fatal(string(r.Body))
		}
		return r.StatusCode, out.Models
	}
	code, models := view(key)
	if code != 200 || len(models) != 2 {
		t.Fatal(code, models)
	}
	if m := models["claude-new-9"]; m.Source != "manual" || m.Input != 3 || m.Output != 15 {
		t.Fatal("manual price", m)
	}
	if m := models["claude-opus-5-5"]; m.Source != "imported" || m.Input <= 0 {
		t.Fatal("imported price", m)
	}
	if code, _ = view("wrong-key-0123456789"); code != 401 {
		t.Fatal("prices for a refused key", code)
	}
}
