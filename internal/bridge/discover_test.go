package bridge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeAuths struct {
	mu    sync.Mutex
	files []pluginapi.HostAuthFileEntry
	fail  bool
	lists int
}

func (f *fakeAuths) host(method string, _ any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case pluginabi.MethodHostAuthList:
		f.lists++
		if f.fail {
			return nil, errors.New("host unavailable")
		}
		return json.Marshal(map[string]any{"files": f.files})
	case pluginabi.MethodHostLog:
		return json.RawMessage(`{}`), nil
	}
	// Quota sampling (get_runtime/get/http) is outside this test.
	return nil, errors.New("not in fixture")
}

func (f *fakeAuths) set(files ...pluginapi.HostAuthFileEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = files
}

func ids(cs []handoff.Credential) string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.ID+"="+c.Alias)
	}
	return strings.Join(out, ",")
}

// The deployed failure: a credential added in CPA after configuration served the
// chat, shadow observation ignored it, no binding existed and the usage view
// answered 401. With discovery the chat binds to the credential actually in use.
func TestDiscoveryBindsChatToCredentialAddedInCPA(t *testing.T) {
	auths := &fakeAuths{}
	auths.set(
		pluginapi.HostAuthFileEntry{ID: "claude-old", AuthIndex: "i-old", Provider: "claude", Label: "old@example.test"},
		pluginapi.HostAuthFileEntry{ID: "codex-c", AuthIndex: "i-c", Provider: "codex", Label: "c@example.test"},
		pluginapi.HostAuthFileEntry{ID: "gemini-x", AuthIndex: "i-x", Provider: "gemini", Label: "x"},
	)
	p := New(auths.host)
	var err error
	p.Engine, err = handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"), DiscoverCredentials: true,
		Adaptive: adaptive.Config{Mode: "shadow"}, Credentials: []handoff.Credential{
			{ID: "claude-old", Index: "i-old", Provider: "claude", Account: "old", Alias: "Claude primary"},
		}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.discover(true)
	if got := ids(p.Engine.Credentials()); got != "claude-old=Claude primary,codex-c=c@example.test" {
		t.Fatal("initial discovery", got)
	}
	// New account added in CPA; the periodic sync has not run yet.
	auths.set(append(auths.files, pluginapi.HostAuthFileEntry{ID: "claude-new", AuthIndex: "i-new", Provider: "claude", Label: "new@example.test"})...)
	p.lastDiscover = time.Now().Add(-time.Minute)
	invoke := func(method string, request any) any {
		t.Helper()
		raw, _ := json.Marshal(request)
		result, err := p.Handle(method, raw)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	token := strings.Repeat("t", 48)
	claim := handoff.Claim{Protocol: 1, Installation: "install", Session: "chat", Route: "proxy/opus", Request: "r1", Kind: "primary", Capability: handoff.Digest(token), Auto: true, Fresh: true, Portable: true}
	encoded, _ := json.Marshal(claim)
	header := http.Header{ControlHeader: {base64.RawURLEncoding.EncodeToString(encoded)}}
	invoke(pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: "r1", Model: "claude-opus", Body: []byte(`{"input":"dummy"}`), Headers: header, Metadata: map[string]any{"caller_scope": "caller"}})
	invoke(pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: "r1", Model: "claude-opus", Metadata: map[string]any{"selected_auth_id": "claude-new"}})
	response := invoke(pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/events", Headers: http.Header{"Authorization": {"Bearer " + token}},
		Query: url.Values{"view": {"usage"}, "all": {"false"}, "model": {claim.Route}}}).(pluginapi.ManagementResponse)
	var report handoff.Usage
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &report) != nil {
		t.Fatal("chat did not bind to the CPA-selected credential", response.StatusCode, string(response.Body))
	}
	if report.Current != "claude-new" || len(report.Active) != 1 || report.Active[0] != "claude-new" {
		t.Fatal("usage does not show the credential in use", report)
	}
	got := []string{}
	for _, c := range report.Credentials {
		got = append(got, c.ID)
	}
	if strings.Join(got, ",") != "claude-old,claude-new" { // sorted by alias
		t.Fatal("model scope should list both Claude credentials", got)
	}
}

// Disabling or deleting an auth in CPA removes it from the set but keeps its
// quota history; re-enabling restores it with that history. Host failures keep
// the current set.
func TestDiscoveryRemovalKeepsQuotaState(t *testing.T) {
	auths := &fakeAuths{}
	keep := pluginapi.HostAuthFileEntry{ID: "claude-a", AuthIndex: "i-a", Provider: "claude", Label: "a"}
	dead := pluginapi.HostAuthFileEntry{ID: "claude-dead", AuthIndex: "i-d", Type: "claude", Email: "dead@example.test"}
	auths.set(keep, dead)
	p := New(auths.host)
	var err error
	p.Engine, err = handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"), DiscoverCredentials: true,
		Adaptive: adaptive.Config{Mode: "shadow"}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.discover(true)
	if got := ids(p.Engine.Credentials()); got != "claude-a=a,claude-dead=dead@example.test" {
		t.Fatal(got)
	}
	now := time.Now().Unix()
	if err := p.Engine.Observe("claude-dead", []quota.Window{{Scope: "all", Seconds: 604800, Remaining: 13, Observed: now, Reset: now + 3600}}); err != nil {
		t.Fatal(err)
	}
	dead.Disabled = true
	auths.set(keep, dead)
	p.lastDiscover = time.Time{}
	p.discover(false)
	if got := ids(p.Engine.Credentials()); got != "claude-a=a" {
		t.Fatal("disabled auth still listed", got)
	}
	all, _ := p.Engine.AllUsage()
	if len(all.Credentials) != 1 {
		t.Fatal("usage still shows disabled auth", all)
	}
	raw, _ := p.Engine.MarshalState()
	if !strings.Contains(string(raw), `"claude:claude-i-d"`) {
		t.Fatal("quota history of the removed credential was deleted", string(raw))
	}
	auths.mu.Lock()
	auths.fail = true
	auths.mu.Unlock()
	p.lastDiscover = time.Time{}
	p.discover(false)
	if got := ids(p.Engine.Credentials()); got != "claude-a=a" {
		t.Fatal("host failure changed the set", got)
	}
	auths.mu.Lock()
	auths.fail = false
	auths.mu.Unlock()
	dead.Disabled = false
	auths.set(keep, dead)
	p.lastDiscover = time.Time{}
	p.discover(false)
	all, _ = p.Engine.AllUsage()
	if len(all.Credentials) != 2 || all.Credentials[1].ID != "claude-dead" || len(all.Credentials[1].Windows) != 1 || all.Credentials[1].Windows[0].Remaining != 13 {
		t.Fatal("re-enabled credential lost its history", all)
	}
	// Periodic calls are rate limited.
	before := auths.lists
	p.discover(false)
	p.discover(false)
	if auths.lists != before {
		t.Fatal("periodic discovery not rate limited")
	}
}

// Without discover_credentials the configured set is authoritative (old behavior).
func TestDiscoveryOffKeepsConfiguredSet(t *testing.T) {
	auths := &fakeAuths{}
	auths.set(pluginapi.HostAuthFileEntry{ID: "claude-new", AuthIndex: "i-new", Provider: "claude"})
	p := New(auths.host)
	var err error
	p.Engine, err = handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"),
		Adaptive: adaptive.Config{Mode: "shadow"}, Credentials: []handoff.Credential{{ID: "claude-old", Index: "1", Provider: "claude", Account: "old", Alias: "Old"}}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.discover(true)
	if got := ids(p.Engine.Credentials()); got != "claude-old=Old" || auths.lists != 0 {
		t.Fatal(got, auths.lists)
	}
	if _, err := handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "s.sqlite"), Adaptive: adaptive.Config{Mode: "shadow"}}, time.Now); err == nil {
		t.Fatal("empty credentials without discovery must be refused")
	}
}
