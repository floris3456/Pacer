package pacer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestClientAccessValidatesOperatorGrants(t *testing.T) {
	caller := clientCaller("caller-one")
	grant := func(scope string, ids ...string) *ClientAccess {
		return &ClientAccess{Grants: []ClientGrant{{CallerScope: scope, AuthIDs: ids}}}
	}
	tooManyIDs := make([]string, 257)
	tooManyCallers := make([]ClientGrant, 257)
	for i := range tooManyIDs {
		tooManyIDs[i] = fmt.Sprintf("auth-%d", i)
		tooManyCallers[i] = ClientGrant{CallerScope: tracker.Digest(tooManyIDs[i]), AuthIDs: []string{"a"}}
	}
	for _, tc := range []struct {
		name  string
		value *ClientAccess
		valid bool
	}{
		{"omitted shared pool", nil, true},
		{"explicit deny all", &ClientAccess{}, true},
		{"multiple credentials", grant(caller, "a", "b"), true},
		{"same credential different callers", &ClientAccess{Grants: tooManyCallers[:2]}, true},
		{"maximum grants", &ClientAccess{Grants: tooManyCallers[:256]}, true},
		{"maximum credentials", grant(caller, tooManyIDs[:256]...), true},
		{"missing caller", grant("", "a"), false},
		{"short caller", grant(caller[:62], "a"), false},
		{"nonhex caller", grant(strings.Repeat("z", 64), "a"), false},
		{"uppercase caller", grant(strings.ToUpper(caller), "a"), false},
		{"duplicate caller", &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a"}}, {CallerScope: caller, AuthIDs: []string{"b"}}}}, false},
		{"missing credentials", grant(caller), false},
		{"blank credential", grant(caller, ""), false},
		{"whitespace credential", grant(caller, " a"), false},
		{"newline credential", grant(caller, "a\nb"), false},
		{"nul credential", grant(caller, "a\x00b"), false},
		{"oversize credential", grant(caller, strings.Repeat("a", 513)), false},
		{"duplicate credential", grant(caller, "a", "a"), false},
		{"too many credentials", grant(caller, tooManyIDs...), false},
		{"too many callers", &ClientAccess{Grants: tooManyCallers}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateClientAccess(tc.value); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
	// The caller digest must match the ordinary CPA authentication provider.
	if caller != tracker.Digest("cli-proxy-api:caller-scope:v1\x00caller-one") || clientCaller(" caller-one ") != caller {
		t.Fatal("API-key reads use a different caller namespace from inference")
	}
}

func TestRoutingAndPinningCannotRestoreARevokedAccount(t *testing.T) {
	for _, mode := range []string{"off", "on"} {
		for _, kind := range []string{"primary", "generate"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				f := newRoutingFixture(t, mode)
				f.seed(t, 5*time.Minute)
				f.p.hostGuard = requiredInterceptors{Version: 1, PluginID: "pacer", Required: true, RequestInterceptors: true, NestedModelInterceptors: true, RetryInterceptors: true, AuthPoolFiltering: true}
				f.p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: "fixture-caller", AuthIDs: []string{"b"}}}}
				f.q.Kind = kind
				r := f.before(t, "revoked-affinity", "claude-opus-5-5")
				choice := f.pick(t, r, "a", "b")
				if choice.AuthID == "a" || choice.Reject {
					t.Fatal("revoked account was selected or treated as temporarily unavailable", choice)
				}
				for _, candidate := range f.p.requests[r.RequestID].Decision.Candidates {
					if candidate.Auth != "b" {
						t.Fatal("routing exposed a foreign candidate", candidate)
					}
				}
				if view := f.p.nextUse(f.binding(t)); view.Auth == "a" || view.ForceAuth == "a" {
					t.Fatal("preview restored a revoked account", view)
				}
			})
		}
	}
}

func grantRequest(id, caller, auth string, q *tracker.Claim) pluginapi.RequestInterceptRequest {
	r := pluginapi.RequestInterceptRequest{RequestID: id, Model: "claude-opus-5-5", Headers: http.Header{}, Metadata: map[string]any{"caller_scope": caller, "selected_auth_id": auth}}
	if q != nil {
		raw, _ := json.Marshal(q)
		r.Headers.Set(controlHeader, base64.RawURLEncoding.EncodeToString(raw))
	}
	return r
}

func grantBefore(t *testing.T, p *Plugin, r pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptRequest {
	t.Helper()
	response := invoke(t, p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
	if response.Terminate {
		t.Fatalf("authorized caller was rejected: %d %s", response.StatusCode, response.ResponseBody)
	}
	for name, values := range response.Headers {
		r.Headers[name] = values
	}
	return r
}

func requireGrantRefusal(t *testing.T, result any, err error, status int) {
	t.Helper()
	response, ok := result.(pluginapi.RequestInterceptResponse)
	if err != nil || !ok || !response.Terminate || response.StatusCode != status {
		t.Fatalf("expected explicit Terminate %d, got %#v, error=%v", status, result, err)
	}
	code := "pacer_account_access_denied"
	if status == 503 {
		code = "pacer_account_access_unavailable"
	}
	if !strings.Contains(string(response.ResponseBody), code) {
		t.Fatalf("missing error code %s: %s", code, response.ResponseBody)
	}
}

func TestClientAccessRefusesBeforeUpstreamAcrossRequestPaths(t *testing.T) {
	caller := clientCaller("permitted-key")
	for _, path := range []string{"empty grants", "missing caller", "foreign caller", "header spoof", "untracked", "missing selected account", "foreign account", "compaction", "title", "generate", "managed auxiliary", "retry"} {
		t.Run(path, func(t *testing.T) {
			f := newRoutingFixture(t, "off")
			p := f.p
			p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a"}}}}
			var upstreamCalls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.WriteHeader(200)
			}))
			defer upstream.Close()
			r := grantRequest("denied-"+path, caller, "b", nil)
			method := pluginabi.MethodRequestInterceptAfter
			switch path {
			case "empty grants":
				p.cfg.ClientAccess = &ClientAccess{}
				method = pluginabi.MethodRequestInterceptBefore
			case "missing caller", "header spoof":
				delete(r.Metadata, "caller_scope")
				r.Headers.Set("X-Pacer-Caller-Scope", caller)
				r.Headers.Set("X-Pacer-Allowed-Auth-IDs", "b")
				method = pluginabi.MethodRequestInterceptBefore
			case "foreign caller":
				r.Metadata["caller_scope"] = clientCaller("other-key")
				method = pluginabi.MethodRequestInterceptBefore
			case "untracked":
				// An after hook without its authenticated before hook must deny.
			case "compaction", "title", "generate":
				q := f.q
				q.Kind = path
				r = grantBefore(t, p, grantRequest(r.RequestID, caller, "b", &q))
			default:
				r = grantBefore(t, p, r)
				if path == "missing selected account" {
					delete(r.Metadata, "selected_auth_id")
				}
				if path == "managed auxiliary" {
					p.selections[r.RequestID] = pluginapi.CachePolicy{Kind: "compaction"}
				}
				if path == "retry" {
					r.Metadata["selected_auth_id"] = "a"
					allowed := invoke(t, p, method, r).(pluginapi.RequestInterceptResponse)
					if allowed.Terminate {
						t.Fatal("first permitted attempt denied", string(allowed.ResponseBody))
					}
					r.Metadata["selected_auth_id"] = "b"
				}
			}
			raw, _ := json.Marshal(r)
			result, err := p.Handle(method, raw)
			// Model the host's dispatch boundary; only a returned Terminate
			// prevents forwarding, while a plugin error alone is insufficient.
			if response, ok := result.(pluginapi.RequestInterceptResponse); !ok || !response.Terminate {
				response, requestErr := upstream.Client().Get(upstream.URL)
				if requestErr != nil {
					t.Fatal(requestErr)
				}
				response.Body.Close()
			}
			requireGrantRefusal(t, result, err, 403)
			if upstreamCalls.Load() != 0 {
				t.Fatal("denied account reached upstream")
			}
			if method == pluginabi.MethodRequestInterceptBefore && len(p.requests) != 0 {
				t.Fatal("denied caller was enrolled")
			}
		})
	}
}

func TestClientAccessUnavailableHooksExplicitlyTerminate(t *testing.T) {
	for _, state := range []string{"uninitialized", "stopped", "malformed"} {
		t.Run(state, func(t *testing.T) {
			p := New(testHost)
			if state != "uninitialized" {
				p = newRoutingFixture(t, "off").p
			}
			p.cfg.ClientAccess = &ClientAccess{}
			if state == "stopped" {
				p.Close()
			}
			defer p.Close()
			for _, method := range []string{pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter} {
				raw := []byte(`{"request_id":"unavailable"}`)
				if state == "malformed" {
					raw = []byte(`{"request_id":`)
				}
				result, err := p.Handle(method, raw)
				requireGrantRefusal(t, result, err, 503)
			}
		})
	}
}

type grantUsageView struct {
	Current     string   `json:"current"`
	Active      []string `json:"active"`
	Credentials []struct {
		ID         string          `json:"id"`
		Alias      string          `json:"alias"`
		SharedWith []string        `json:"shared_with"`
		Windows    json.RawMessage `json:"windows"`
	} `json:"credentials"`
	Next nextUseView `json:"next_use"`
}

func TestClientAccessScopesThreeCallersUsageAndAccountAliases(t *testing.T) {
	f := newRoutingFixture(t, "on")
	p := f.p
	callers := []struct {
		key, auth, token string
		ids              []string
	}{
		{"alice-key", "a", strings.Repeat("alice-cap-", 6), []string{"a", "a-shared", "usage-a"}},
		{"bob-key", "a-hidden", strings.Repeat("bob-cap-", 6), []string{"a-hidden", "b"}},
		{"carol-key", "c", strings.Repeat("carol-cap-", 6), []string{"c", "usage-c"}},
	}
	for _, id := range []string{"a-shared", "a-hidden", "c"} {
		c := p.creds["a"]
		c.ID, c.Index, c.Alias = id, "index-"+id, "private-alias-"+id
		if id == "c" {
			c.Account = "private-account-c"
		}
		p.creds[id] = c
	}
	p.usageCreds = map[string]credential{}
	for _, id := range []string{"usage-a", "usage-c"} {
		p.usageCreds[id] = credential{ID: id, Account: "private-account-" + id, Alias: "private-alias-" + id, Provider: "antigravity"}
	}
	for _, c := range []credential{p.creds["c"], p.usageCreds["usage-a"], p.usageCreds["usage-c"]} {
		left := 60.0
		now := p.now()
		w := observation.Window{Provider: c.Provider, Scope: "private-scope-" + c.ID, Seconds: 18000, Remaining: &left, Reset: now.Add(time.Hour), Usage: observation.Field{At: now, Source: "header"}, ResetEvidence: observation.Field{At: now, Source: "header", Quality: "absolute"}}
		if err := p.observer.Observe(c.Account, c.Provider, []observation.Window{w}); err != nil {
			t.Fatal(err)
		}
	}
	p.cfg.ClientAccess = &ClientAccess{}
	for _, caller := range callers {
		p.cfg.ClientAccess.Grants = append(p.cfg.ClientAccess.Grants, ClientGrant{CallerScope: clientCaller(caller.key), AuthIDs: caller.ids})
	}
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, caller := range callers {
			if r.Header.Get("Authorization") == "Bearer "+caller.key {
				_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
				return
			}
		}
		w.WriteHeader(401)
	}))
	defer auth.Close()
	p.cfg.UsageAuthURL = auth.URL + "/v1/models"
	for _, caller := range callers {
		q := f.q
		q.Session, q.Capability = caller.key, tracker.Digest(caller.token)
		r := grantBefore(t, p, grantRequest("seed-"+caller.key, clientCaller(caller.key), caller.auth, &q))
		if after := f.after(t, r, caller.auth); after.Terminate {
			t.Fatal("permitted binding denied", string(after.ResponseBody))
		}
		if err := p.chats.CacheActivityForAuth(r.RequestID, caller.auth, p.now(), true, true, time.Minute); err != nil {
			t.Fatal(err)
		}
		f.complete(t, r.RequestID)
		binding, err := p.chats.ByCapability(caller.token, q.Route)
		if err != nil || binding.Caller != clientCaller(caller.key) {
			t.Fatal("caller ownership not recorded", binding, err)
		}
		preview := grantBefore(t, p, grantRequest("preview-"+caller.key, clientCaller(caller.key), caller.auth, &q))
		f.pick(t, preview, "a", "a-shared", "a-hidden", "b", "c")
		f.complete(t, preview.RequestID)
		// Include a foreign account in the same chat's in-memory requests:
		// a stale attempt must not restore access removed by a grant change.
		p.requests["active-"+caller.key] = attempt{Key: binding.Key, Auth: caller.auth}
		foreign := "b"
		if slices.Contains(caller.ids, foreign) {
			foreign = "c"
		}
		p.requests["foreign-"+caller.key] = attempt{Key: binding.Key, Auth: foreign}
	}
	f.clock.Add(int64(2 * time.Minute))
	for _, caller := range callers {
		for _, mode := range []string{"api-key", "capability", "legacy-windows"} {
			t.Run(caller.key+"/"+mode, func(t *testing.T) {
				query := url.Values{"view": {"usage"}, "all": {"true"}, "v": {"1"}, "model": {f.q.Route}}
				token := caller.token
				if mode == "api-key" {
					query.Set("auth", "api-key")
					token = caller.key
				} else if mode == "legacy-windows" {
					query.Del("v")
				}
				response := p.events(pluginapi.ManagementRequest{Method: "GET", Headers: http.Header{"Authorization": {"Bearer " + token}}, Query: query})
				var view grantUsageView
				if response.StatusCode != 200 || json.Unmarshal(response.Body, &view) != nil {
					t.Fatal(response.StatusCode, string(response.Body))
				}
				ids := []string{}
				for _, row := range view.Credentials {
					ids = append(ids, row.ID)
					wantShared := []string{}
					if caller.key == "alice-key" && (row.ID == "a" || row.ID == "a-shared") {
						wantShared = []string{"a"}
						if row.ID == "a" {
							wantShared = []string{"a-shared"}
						}
					}
					if !reflect.DeepEqual(row.SharedWith, wantShared) {
						t.Fatal("shared credential aliases escaped grant scope", row)
					}
				}
				if !reflect.DeepEqual(ids, caller.ids) {
					t.Fatalf("credentials=%v, want %v", ids, caller.ids)
				}
				if mode == "api-key" {
					if view.Current != "" || len(view.Active) != 0 || view.Next.Auth != "" {
						t.Fatal("API-key read leaked a chat binding", string(response.Body))
					}
				} else if view.Current != caller.auth || !reflect.DeepEqual(view.Active, []string{caller.auth}) {
					t.Fatal("current/active escaped caller scope", string(response.Body))
				}
				if view.Next.Auth != "" && !slices.Contains(caller.ids, view.Next.Auth) {
					t.Fatal("next-use escaped caller scope", string(response.Body))
				}
				if mode == "capability" && caller.key == "bob-key" && view.Next.Auth != "b" {
					t.Fatal("fixture did not exercise an authorized next-use choice", string(response.Body))
				}
				for id, c := range p.creds {
					if !slices.Contains(caller.ids, id) && (strings.Contains(string(response.Body), `"`+id+`"`) || strings.Contains(string(response.Body), `"`+c.Alias+`"`)) {
						t.Fatalf("foreign credential or alias %s leaked: %s", id, response.Body)
					}
				}
				for id := range p.usageCreds {
					if !slices.Contains(caller.ids, id) && strings.Contains(string(response.Body), id) {
						t.Fatalf("foreign usage-only account/window leaked: %s", response.Body)
					}
				}
			})
		}
	}
	// Removing the current credential also denies the legacy window endpoint;
	// ownership of a capability is not authority over a revoked account grant.
	p.cfg.ClientAccess.Grants[0].AuthIDs = []string{"usage-a"}
	for _, query := range []url.Values{{"model": {f.q.Route}}, {"view": {"usage"}, "v": {"1"}, "model": {f.q.Route}}} {
		response := p.events(pluginapi.ManagementRequest{Headers: http.Header{"Authorization": {"Bearer " + callers[0].token}}, Query: query})
		if response.StatusCode != 403 || strings.Contains(string(response.Body), "windows") || strings.Contains(string(response.Body), "current") {
			t.Fatal("revoked binding remained readable", response.StatusCode, string(response.Body))
		}
	}
}

func TestClientAccessHeldUsageRechecksOrdinaryAPIKey(t *testing.T) {
	p := newRoutingFixture(t, "off").p
	key := "revoked-during-watch"
	p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: clientCaller(key), AuthIDs: []string{"a"}}}}
	var revoked atomic.Bool
	var authenticated atomic.Int64
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+key {
			authenticated.Add(1)
			if !revoked.Load() {
				_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
				return
			}
		}
		w.WriteHeader(401)
	}))
	defer auth.Close()
	p.cfg.UsageAuthURL = auth.URL + "/v1/models"
	cursor, _ := p.usageChanges.wait("", 0, p.workerDone)
	request := usageWatchRequest(key, "", cursor, "20000")
	request.Query.Set("auth", "api-key")
	request.Query.Set("all", "true")
	results := make(chan pluginapi.ManagementResponse, 1)
	go func() { results <- p.events(request) }()
	awaitUsageWaiters(t, &p.usageChanges, 1)
	revoked.Store(true)
	p.usageChanges.notify()
	response := awaitUsageResponse(t, results)
	if response.StatusCode != 401 || strings.Contains(string(response.Body), "credentials") || authenticated.Load() != 2 {
		t.Fatal("held usage survived API-key revocation", response.StatusCode, string(response.Body), authenticated.Load())
	}
}

func TestClientAccessRestartPreservesOwnerAndDeniesOldOwnerlessCapabilities(t *testing.T) {
	f := newRoutingFixture(t, "off")
	p := f.p
	caller := clientCaller("owned-api-key")
	ownedToken, oldToken := strings.Repeat("owned-cap-", 6), strings.Repeat("legacy-cap-", 6)
	q := f.q
	q.Capability = tracker.Digest(ownedToken)
	r := grantBefore(t, p, grantRequest("owned", caller, "a", &q))
	if response := f.after(t, r, "a"); response.Terminate {
		t.Fatal(string(response.ResponseBody))
	}
	f.complete(t, r.RequestID)
	// Old persisted bindings did not contain caller_scope. Keep one in the
	// database while reopening it with grants enabled after the upgrade.
	old := q
	old.Session, old.Capability = "legacy", tracker.Digest(oldToken)
	c := p.creds["a"]
	if _, err := p.chats.Admit("legacy", tracker.Key(caller, c.Provider, old), tracker.Selection{Auth: c.ID, Account: c.Account, Alias: c.Alias, Provider: c.Provider, Model: r.Model}, &old, false); err != nil {
		t.Fatal(err)
	}
	path := p.cfg.StatePath
	p.Close()
	p = New(testHost)
	p.now = f.p.now
	defer p.Close()
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\nstate_path: " + path + "\n")})
	if _, err := p.configure(raw); err != nil {
		t.Fatal(err)
	}
	p.discover()
	p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a"}}}}
	if persisted, err := p.chats.ByCapability(ownedToken, q.Route); err != nil || persisted.Caller != caller {
		t.Fatal("binding lost its caller after restart", persisted, err)
	}
	for _, tc := range []struct {
		token  string
		status int
	}{{ownedToken, 200}, {oldToken, 403}} {
		for _, query := range []url.Values{{"model": {q.Route}}, {"view": {"usage"}, "v": {"1"}, "all": {"true"}, "model": {q.Route}}} {
			response := p.events(pluginapi.ManagementRequest{Headers: http.Header{"Authorization": {"Bearer " + tc.token}}, Query: query})
			if response.StatusCode != tc.status {
				t.Fatalf("capability status=%d want=%d: %s", response.StatusCode, tc.status, response.Body)
			}
			if tc.status == 403 && (strings.Contains(string(response.Body), "windows") || strings.Contains(string(response.Body), "credentials")) {
				t.Fatal("ownerless capability disclosed quota", string(response.Body))
			}
		}
	}
}
