package pacer

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHistoryKeyFollowsTheStartOfTheHistoryPerFormat(t *testing.T) {
	for _, tc := range []struct {
		format, start, later, otherFirst, otherSystem string
	}{
		{"claude",
			`{"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"t"}],"messages":[{"role":"user","content":"hello"}]}`,
			`{"system":[{"type":"text","text":"S"}],"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":[{"type":"text","text":"next","cache_control":{"type":"ephemeral"}}]}]}`,
			`{"system":[{"type":"text","text":"S"}],"tools":[{"name":"t"}],"messages":[{"role":"user","content":"bye"}]}`,
			`{"system":[{"type":"text","text":"T"}],"tools":[{"name":"t"}],"messages":[{"role":"user","content":"hello"}]}`},
		{"openai",
			`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"hello"}]}`,
			`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`,
			`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"bye"}]}`,
			`{"messages":[{"role":"developer","content":"T"},{"role":"user","content":"hello"}]}`},
		{"openai-response",
			`{"instructions":"S","input":"hello"}`,
			`{"instructions":"S","input":[{"role":"user","content":"hello"},{"type":"function_call","name":"f"},{"role":"user","content":"next"}]}`,
			`{"instructions":"S","input":"bye"}`,
			`{"instructions":"T","input":"hello"}`},
		{"gemini",
			`{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			`{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":"hi"}]},{"role":"user","parts":[{"text":"next"}]}]}`,
			`{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"bye"}]}]}`,
			`{"systemInstruction":{"parts":[{"text":"T"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
	} {
		start := historyKey(tc.format, []byte(tc.start))
		if start == "" {
			t.Fatalf("%s: no history key", tc.format)
		}
		if got := historyKey(tc.format, []byte(tc.later)); got != start {
			t.Fatalf("%s: a later request of the chat has another key", tc.format)
		}
		if historyKey(tc.format, []byte(tc.otherFirst)) == start || historyKey(tc.format, []byte(tc.otherSystem)) == start {
			t.Fatalf("%s: another chat shares the key", tc.format)
		}
	}
	if historyKey("claude", []byte(`{"messages":[{"role":"user","content":"hello"}]}`)) == historyKey("openai", []byte(`{"messages":[{"role":"user","content":"hello"}]}`)) {
		t.Fatal("formats share keys")
	}
	for _, tc := range []struct{ format, body string }{
		{"openai-response", `{"previous_response_id":"resp_1","input":"hello"}`},
		{"openai", `{"messages":[{"role":"system","content":"S"}]}`},
		{"claude", `not json`},
		{"interactions", `{"messages":[{"role":"user","content":"hello"}]}`},
	} {
		if got := historyKey(tc.format, []byte(tc.body)); got != "" {
			t.Fatalf("%s %s: key %q, want none", tc.format, tc.body, got)
		}
	}
}

// historyRequest is a request with a history body and, when session is set, a
// claim naming the chat.
func (f *routingFixture) historyRequest(t *testing.T, id, session, body string) pluginapi.RequestInterceptRequest {
	t.Helper()
	r := pluginapi.RequestInterceptRequest{RequestID: id, Model: "claude-opus-5-5", SourceFormat: "claude", Body: []byte(body), Headers: http.Header{}, Metadata: map[string]any{"caller_scope": "fixture-caller"}}
	if session != "" {
		q := f.q
		q.Session = session
		raw, _ := json.Marshal(q)
		r.Headers.Set(controlHeader, base64.RawURLEncoding.EncodeToString(raw))
	}
	before := invoke(t, f.p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
	if before.Terminate {
		t.Fatal(string(before.ResponseBody))
	}
	for k, v := range before.Headers {
		r.Headers[k] = v
	}
	return r
}

// weekly sets each account's weekly remaining percentage, as observe does.
func (f *routingFixture) weekly(t *testing.T, left map[string]float64) {
	t.Helper()
	for id, remaining := range left {
		short, weekly, now := 80.0, remaining, f.p.now()
		windows := []observation.Window{
			{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &short, Reset: now.Add(3 * time.Hour), Usage: observation.Field{At: now, Source: "header"}, ResetEvidence: observation.Field{At: now, Source: "header", Quality: "absolute"}},
			{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &weekly, Reset: now.Add(96 * time.Hour), Usage: observation.Field{At: now, Source: "header"}, ResetEvidence: observation.Field{At: now, Source: "header", Quality: "absolute"}},
		}
		if err := f.p.observer.Observe(f.p.creds[id].Account, "claude", windows); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	historyStart = `{"system":"S","messages":[{"role":"user","content":"hello"}]}`
	historyLater = `{"system":"S","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`
	historyOther = `{"system":"S","messages":[{"role":"user","content":"another chat"}]}`
)

// A request that names no chat is placed by pace when new and then stays on its
// account while warm, recognized by its history.
func TestHistoryRecognizesAChatThatNamesNone(t *testing.T) {
	f := newRoutingFixture(t, "on")
	r := f.historyRequest(t, "first", "", historyStart)
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "b" {
		t.Fatalf("new chat not placed by pace: %+v", got)
	}
	f.after(t, r, "b")
	f.complete(t, r.RequestID)

	// Account a looks better now; a new chat goes there, the warm chat stays on b.
	f.clock.Add(int64(time.Minute))
	f.weekly(t, map[string]float64{"a": 90, "b": 40})
	if got := f.pick(t, f.historyRequest(t, "new", "", historyOther), "a", "b"); !got.Handled || got.AuthID != "a" {
		t.Fatalf("fixture: pace does not prefer a: %+v %+v", got, f.p.requests["new"].Decision)
	}
	r = f.historyRequest(t, "second", "", historyLater)
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "b" {
		t.Fatalf("warm chat left its account: %+v", got)
	}
	if f.p.requests["second"].Decision.Account != f.p.creds["b"].Account {
		t.Fatalf("decision: %+v", f.p.requests["second"].Decision)
	}

	// Another caller (CPA API key) with the same history is another chat.
	key := historyChatKey("fixture-caller", "claude", "claude-opus-5-5", historyKey("claude", []byte(historyStart)))
	if key == historyChatKey("other-caller", "claude", "claude-opus-5-5", historyKey("claude", []byte(historyStart))) {
		t.Fatal("history shared across callers")
	}
	if b, _, err := f.p.chats.Get(key); err != nil || b.Account != f.p.creds["b"].Account {
		t.Fatalf("history binding: %+v %v", b, err)
	}
}

// A chat whose name is new but whose history is warm on an account (a fork, a
// client restart) stays on that account; a cold history or another history is
// placed by pace.
func TestHistoryKeepsAChatUnderANewNameOnItsWarmAccount(t *testing.T) {
	f := newRoutingFixture(t, "on")
	r := f.historyRequest(t, "named", "chat", historyStart)
	f.after(t, r, "a") // Pace prefers b; the chat runs on a.
	f.complete(t, r.RequestID)
	if b, _, err := f.p.chats.Get(historyChatKey("fixture-caller", "claude", "claude-opus-5-5", historyKey("claude", []byte(historyStart)))); err != nil || b.Account != f.p.creds["a"].Account {
		t.Fatalf("history did not follow the chat: %+v %v", b, err)
	}

	fork := f.historyRequest(t, "fork", "fork", historyLater)
	if got := f.pick(t, fork, "a", "b"); !got.Handled || got.AuthID != "a" {
		t.Fatalf("renamed warm chat moved: %+v", got)
	}
	if b, _, err := f.p.chats.Get(tracker.Key("fixture-caller", "claude", tracker.Claim{Installation: f.q.Installation, Session: "fork", Route: f.q.Route})); err != nil || b.Account != f.p.creds["a"].Account {
		t.Fatalf("renamed chat not seeded: %+v %v", b, err)
	}

	other := f.historyRequest(t, "other", "other", historyOther)
	if got := f.pick(t, other, "a", "b"); !got.Handled || got.AuthID != "b" {
		t.Fatalf("another history not placed by pace: %+v", got)
	}

	f.clock.Add(int64(2 * time.Hour))
	f.observe(t)
	late := f.historyRequest(t, "late", "late", historyLater)
	if got := f.pick(t, late, "a", "b"); !got.Handled || got.AuthID != "b" {
		t.Fatalf("cold history kept its account: %+v", got)
	}
}
