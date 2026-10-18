package pacer

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// providerHost lists a Claude credential, two Antigravity credentials and one
// OpenCode Go key, as CPA's auth inventory does.
func providerHost(method string, input any) (json.RawMessage, error) {
	if method == "host.auth.list" {
		return json.Marshal(map[string]any{"files": []map[string]any{
			{"id": "a", "auth_index": "ia", "provider": "claude", "label": "A"},
			{"id": "g1", "auth_index": "ig1", "provider": "antigravity", "label": "G1"},
			{"id": "g2", "auth_index": "ig2", "provider": "antigravity", "label": "G2"},
			{"id": "o1", "auth_index": "io1", "provider": "opencode-go", "label": "O1"},
		}})
	}
	return testHost(method, input)
}

func newProviderFixture(t *testing.T, providers string) *routingFixture {
	t.Helper()
	f := &routingFixture{}
	f.clock.Store(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC).UnixNano())
	f.p = New(providerHost)
	f.p.now = func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\nstate_path: " + filepath.Join(t.TempDir(), "routing.sqlite") + "\ncompact_on_switch: true\nrouting:\n  mode: off\n  providers: [" + providers + "]\n")})
	if _, err := f.p.configure(raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.p.Close)
	f.p.discover()
	f.p.cfg.Routing.Mode = "on"
	return f
}

// meters records an account's windows: scope -> [5-hour %, longer %]; OpenCode
// Go's longer windows are weekly and monthly.
func (f *routingFixture) meters(t *testing.T, id, provider string, scopes map[string][2]float64) {
	t.Helper()
	now := f.p.now()
	windows := []observation.Window{}
	for scope, left := range scopes {
		short, long := left[0], left[1]
		at := observation.Field{At: now, Source: "api"}
		reset := observation.Field{At: now, Source: "api", Quality: "absolute"}
		windows = append(windows,
			observation.Window{Provider: provider, Scope: scope, Seconds: 18000, Remaining: &short, Reset: now.Add(3 * time.Hour), Usage: at, ResetEvidence: reset},
			observation.Window{Provider: provider, Scope: scope, Seconds: 604800, Remaining: &long, Reset: now.Add(96 * time.Hour), Usage: at, ResetEvidence: reset})
		if provider == "opencode-go" {
			windows = append(windows, observation.Window{Provider: provider, Scope: scope, Seconds: observation.MonthSeconds, Remaining: &long, Reset: now.Add(20 * 24 * time.Hour), Usage: at, ResetEvidence: reset})
		}
	}
	if err := f.p.observer.Observe(f.p.usageCreds[id].Account, provider, windows); err != nil {
		t.Fatal(err)
	}
}

func (f *routingFixture) providerRequest(t *testing.T, id, model, first string) pluginapi.RequestInterceptRequest {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "system", "content": "S"}, {"role": "user", "content": first}}})
	r := pluginapi.RequestInterceptRequest{RequestID: id, Model: model, SourceFormat: "openai", Body: body, Headers: http.Header{}, Metadata: map[string]any{"caller_scope": "fixture-caller"}}
	before := invoke(t, f.p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
	for k, v := range before.Headers {
		r.Headers[k] = v
	}
	return r
}

func (f *routingFixture) providerPick(t *testing.T, r pluginapi.RequestInterceptRequest, provider string, ids ...string) pluginapi.SchedulerPickResponse {
	t.Helper()
	req := pluginapi.SchedulerPickRequest{Provider: provider, Model: r.Model, Options: pluginapi.SchedulerOptions{Headers: r.Headers}}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: provider})
	}
	raw, _ := json.Marshal(invoke(t, f.p, pluginabi.MethodSchedulerPick, req))
	var choice pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(raw, &choice); err != nil {
		t.Fatal(err)
	}
	return choice
}

// Antigravity accounts are paced per quota group: a Gemini chat by the Gemini
// meters, a Claude or GPT chat (aliases included) by the Claude and GPT meters.
// A warm chat stays on its account.
func TestAntigravityRoutesEachQuotaGroupByItsOwnPace(t *testing.T) {
	f := newProviderFixture(t, "claude, antigravity")
	f.meters(t, "g1", "antigravity", map[string][2]float64{"gemini": {90, 90}, "claude_gpt": {60, 15}})
	f.meters(t, "g2", "antigravity", map[string][2]float64{"gemini": {60, 40}, "claude_gpt": {90, 85}})

	gemini := f.providerRequest(t, "gemini", "gemini-3.8-flash-high", "summarize this")
	if got := f.providerPick(t, gemini, "antigravity", "g1", "g2"); !got.Handled || got.AuthID != "g1" {
		t.Fatalf("Gemini chat not placed by the Gemini meters: %+v %+v", got, f.p.requests["gemini"].Decision)
	}
	opus := f.providerRequest(t, "opus", "gemini-opus-4-6-thinking", "review this")
	if got := f.providerPick(t, opus, "antigravity", "g1", "g2"); !got.Handled || got.AuthID != "g2" {
		t.Fatalf("Claude alias not placed by the Claude and GPT meters: %+v %+v", got, f.p.requests["opus"].Decision)
	}

	f.after(t, gemini, "g1")
	f.complete(t, gemini.RequestID)
	f.clock.Add(int64(time.Minute))
	f.meters(t, "g1", "antigravity", map[string][2]float64{"gemini": {50, 30}, "claude_gpt": {60, 15}})
	f.meters(t, "g2", "antigravity", map[string][2]float64{"gemini": {95, 95}, "claude_gpt": {90, 85}})
	again := f.providerRequest(t, "again", "gemini-3.8-flash-high", "summarize this")
	if got := f.providerPick(t, again, "antigravity", "g1", "g2"); !got.Handled || got.AuthID != "g1" {
		t.Fatalf("warm Antigravity chat moved: %+v", got)
	}
	if _, captured := f.p.requests["again"]; !captured {
		t.Fatal("fixture: request not tracked")
	}
}

// For an alias, CPA picks by the routed name and its after-hook carries the
// upstream model. The chat is bound under the name it was placed by, so its
// next turn stays on its account although another one has the better pace.
func TestAliasChatStaysOnItsAccount(t *testing.T) {
	f := newProviderFixture(t, "claude, antigravity")
	f.meters(t, "g1", "antigravity", map[string][2]float64{"claude_gpt": {60, 40}})
	f.meters(t, "g2", "antigravity", map[string][2]float64{"claude_gpt": {90, 85}})
	first := f.providerRequest(t, "first", "gemini-opus-4-6-thinking", "review this")
	if got := f.providerPick(t, first, "antigravity", "g1", "g2"); !got.Handled || got.AuthID != "g2" {
		t.Fatalf("alias chat not placed: %+v", got)
	}
	upstream := first
	upstream.Model, upstream.RequestedModel = "claude-opus-4-6-thinking", "gemini-opus-4-6-thinking"
	if got := f.after(t, upstream, "g2"); got.Terminate {
		t.Fatalf("alias chat refused: %s", got.ResponseBody)
	}
	f.complete(t, first.RequestID)
	f.clock.Add(int64(time.Minute))
	f.meters(t, "g1", "antigravity", map[string][2]float64{"claude_gpt": {95, 95}})
	f.meters(t, "g2", "antigravity", map[string][2]float64{"claude_gpt": {70, 60}})
	next := f.providerRequest(t, "next", "gemini-opus-4-6-thinking", "review this")
	if got := f.providerPick(t, next, "antigravity", "g1", "g2"); !got.Handled || got.AuthID != "g2" || f.p.requests["next"].Decision.Reason != "warm" {
		t.Fatalf("alias chat moved: %+v %+v", got, f.p.requests["next"].Decision)
	}
}

// A provider that is not routed stays CPA's: no placement, no binding.
func TestUnroutedUsageOnlyProviderStaysCPAs(t *testing.T) {
	f := newProviderFixture(t, "claude")
	f.meters(t, "g1", "antigravity", map[string][2]float64{"gemini": {90, 90}})
	f.meters(t, "g2", "antigravity", map[string][2]float64{"gemini": {60, 40}})
	r := f.providerRequest(t, "r", "gemini-3.8-flash-high", "summarize this")
	if got := f.providerPick(t, r, "antigravity", "g1", "g2"); got.Handled {
		t.Fatalf("unrouted provider placed: %+v", got)
	}
	if got := f.after(t, r, "g2"); got.Terminate {
		t.Fatalf("unrouted provider refused: %s", got.ResponseBody)
	}
	_, history := f.p.requests["r"].keys("antigravity", "gemini-3.8-flash-high")
	if b, _, err := f.p.chats.Get(history); err != nil || b.Key != "" {
		t.Fatalf("unrouted provider bound: %+v %v", b, err)
	}
}

// OpenCode Go has one key today: its chats are still tracked and kept there.
func TestOpenCodeGoChatsAreRoutedAndBound(t *testing.T) {
	f := newProviderFixture(t, "claude, opencode-go")
	f.meters(t, "o1", "opencode-go", map[string][2]float64{"all": {70, 60}})
	r := f.providerRequest(t, "go", "deepseek-v4.1-flash", "hello")
	if got := f.providerPick(t, r, "opencode-go", "o1"); !got.Handled || got.AuthID != "o1" {
		t.Fatalf("OpenCode Go chat not placed: %+v %+v", got, f.p.requests["go"].Decision)
	}
	f.after(t, r, "o1")
	_, history := f.p.requests["go"].keys("opencode-go", "deepseek-v4.1-flash")
	if b, _, err := f.p.chats.Get(history); err != nil || b.Account != f.p.usageCreds["o1"].Account {
		t.Fatalf("OpenCode Go chat not bound: %+v %v", b, err)
	}
}

func TestRoutedProvidersAreValidated(t *testing.T) {
	s := defaultSettings()
	for _, providers := range [][]string{{"claude", "codex", "antigravity", "opencode-go"}, {"antigravity"}} {
		s.Providers = providers
		if err := s.validate(); err != nil {
			t.Fatalf("%v: %v", providers, err)
		}
	}
	for _, providers := range [][]string{{"gemini"}, {"claude", "claude"}} {
		s.Providers = providers
		if err := s.validate(); err == nil {
			t.Fatalf("%v accepted", providers)
		}
	}
}
