package pacer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestClientAPIDiscoveryAddsVersionedFeaturesWithoutChangingLegacyFields(t *testing.T) {
	for _, tc := range []struct {
		schema  int
		managed bool
	}{{6, false}, {7, false}, {7, true}} {
		t.Run(fmt.Sprintf("schema-%d/managed-%v", tc.schema, tc.managed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			config := "enabled: true\nstate_path: " + path + "\ncompact_on_switch: true\n"
			if tc.managed {
				db, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = cachepolicy.Create(context.Background(), db, cachepolicy.Options{Clock: func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) }, Routes: []cachepolicy.Route{{ProviderRoute: "claude-main", TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}}}})
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
				config += "cache_policy:\n  enabled: true\n  routes:\n    - id: claude-main\n      models: [claude-opus-5-5]\n"
			}
			var hostCalls atomic.Int64
			p := New(func(method string, input any) (json.RawMessage, error) {
				hostCalls.Add(1)
				return testHost(method, input)
			})
			defer p.Close()
			raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config), "schema_version": tc.schema})
			if _, err := p.configure(raw); err != nil {
				t.Fatal(err)
			}
			before := hostCalls.Load()
			response := p.events(pluginapi.ManagementRequest{Method: "GET"})
			var discovery struct {
				Protocol    int             `json:"protocol"`
				Mode        string          `json:"mode"`
				Pacer       bool            `json:"pacer"`
				CachePolicy json.RawMessage `json:"cache_policy"`
				Client      struct {
					Version int      `json:"version"`
					Views   []string `json:"views"`
				} `json:"client"`
				API clientAPI `json:"api"`
			}
			if response.StatusCode != 200 || json.Unmarshal(response.Body, &discovery) != nil {
				t.Fatal(response.StatusCode, string(response.Body))
			}
			if discovery.Protocol != 1 || discovery.Mode != "off" || !discovery.Pacer || discovery.Client.Version != 1 {
				t.Fatal("legacy discovery changed", string(response.Body))
			}
			for _, view := range []string{"usage", "prices", "usage-watch", "capabilities"} {
				if !slices.Contains(discovery.Client.Views, view) {
					t.Fatal("missing client view", view)
				}
			}
			api := discovery.API
			if api.Version != 1 || api.HostSchema != tc.schema || api.Build.Version == "" || api.Build.Revision == "" || !strings.Contains(string(response.Body), `"dirty":`) {
				t.Fatal("missing version/build identity", string(response.Body))
			}
			policy := api.Features["cache_policy"]
			if policy.Supported != (tc.schema >= 7) || policy.Enabled != tc.managed || policy.ContractRevision != 2 {
				t.Fatal("host support was confused with configured policy", policy)
			}
			if !tc.managed && string(discovery.CachePolicy) != "false" {
				t.Fatal("legacy policy field overclaimed support", string(discovery.CachePolicy))
			}
			for _, feature := range []string{"quota", "watch", "account_switch"} {
				if value, ok := api.Features[feature]; !ok || !value.Supported || !value.Enabled {
					t.Fatal("configured feature missing", feature, value)
				}
			}
			for _, feature := range []string{"keepalive", "native_compaction"} {
				if value := api.Features[feature]; value.Supported || value.Enabled || value.Reason != "host_integration_required" {
					t.Fatal("unimplemented operation advertised", feature, value)
				}
			}
			if api.Access["mode"] != "shared" || api.Access["enforcement"] != "optional-interceptors" || api.Features["account_access"].Enabled || api.Features["account_access"].Supported {
				t.Fatal("unattested host advertised account enforcement", api.Access, api.Features["account_access"])
			}
			if hostCalls.Load() != before || len(p.requests) != 0 || len(p.selections) != 0 {
				t.Fatal("public discovery caused host work or request state")
			}
			for _, private := range []string{"caller_scope", "auth_ids", "credentials", path} {
				if strings.Contains(string(response.Body), private) {
					t.Fatal("public discovery leaked private state", private)
				}
			}
		})
	}
}

func TestClientAPIAuthenticatesCapabilitiesAndDeniesUngrantedPrices(t *testing.T) {
	p := newRoutingFixture(t, "off", false).p
	p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: clientCaller("granted-key"), AuthIDs: []string{"a"}}}}
	var hostCalls, wrongPath atomic.Int64
	p.Host = func(method string, input any) (json.RawMessage, error) {
		hostCalls.Add(1)
		return testHost(method, input)
	}
	var anonymous atomic.Bool
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/models" {
			wrongPath.Add(1)
			w.WriteHeader(500)
			return
		}
		if anonymous.Load() {
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer granted-key", "Bearer ungranted-key":
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		case "Bearer unavailable":
			w.WriteHeader(503)
		default:
			w.WriteHeader(401)
		}
	}))
	defer auth.Close()
	p.cfg.UsageAuthURL = auth.URL + "/v1/models"
	for _, tc := range []struct {
		name, key, view, version string
		status                   int
		code, action             string
	}{
		{"authorized", "granted-key", "capabilities", "1", 200, "", ""},
		{"wrong key", "wrong-key", "capabilities", "1", 401, "unauthorized", "authenticate"},
		{"ungranted caller", "ungranted-key", "capabilities", "1", 403, "account_access_denied", "contact_operator"},
		{"ungranted prices", "ungranted-key", "prices", "1", 403, "account_access_denied", "contact_operator"},
		{"auth unavailable", "unavailable", "capabilities", "1", 503, "authentication_unavailable", "wait"},
		{"unsupported version", "granted-key", "capabilities", "2", 422, "unsupported_version", "upgrade"},
		{"missing version", "granted-key", "capabilities", "", 422, "unsupported_version", "upgrade"},
		{"anonymous boundary", "granted-key", "capabilities", "1", 503, "authentication_unavailable", "wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anonymous.Store(tc.name == "anonymous boundary")
			response := p.events(pluginapi.ManagementRequest{Method: "GET", Headers: http.Header{"Authorization": {"Bearer " + tc.key}}, Query: url.Values{"auth": {"api-key"}, "view": {tc.view}, "v": {tc.version}}})
			if response.StatusCode != tc.status {
				t.Fatal(response.StatusCode, string(response.Body))
			}
			if tc.status == 200 {
				var api clientAPI
				if json.Unmarshal(response.Body, &api) != nil || api.Version != 1 || api.Access["mode"] != "granted" {
					t.Fatal("capability contract missing", string(response.Body))
				}
			} else {
				var problem struct {
					Error struct {
						Code, Action string
						Retryable    bool
					} `json:"error"`
				}
				if json.Unmarshal(response.Body, &problem) != nil || problem.Error.Code != tc.code || problem.Error.Action != tc.action || problem.Error.Retryable != (tc.status == 503) {
					t.Fatal("unstable client error contract", string(response.Body))
				}
			}
			for _, private := range []string{"caller_scope", "credentials", "auth_ids", "granted-key"} {
				if strings.Contains(string(response.Body), private) {
					t.Fatal("capabilities leaked caller/account secrets", private)
				}
			}
		})
	}
	if hostCalls.Load() != 0 || wrongPath.Load() != 0 || len(p.requests) != 0 || len(p.selections) != 0 {
		t.Fatal("client discovery/prices denial caused inference or mutation")
	}
}

func TestClientAccessPolicyRPCUsesAuthenticatedScope(t *testing.T) {
	p := newRoutingFixture(t, "off", false).p
	caller := clientCaller("granted-key")
	p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a"}}}}
	for _, method := range []string{pluginabi.MethodCachePolicyHandle, pluginabi.MethodCachePolicyResolve, pluginabi.MethodCachePolicyAttempt} {
		t.Run(method, func(t *testing.T) {
			for _, foreign := range []string{"", clientCaller("ungranted-key")} {
				// Put the granted caller in the other shape as a spoof attempt.
				request := map[string]any{"scope": pluginapi.CachePolicyScope{CallerScope: foreign}, "policy": pluginapi.CachePolicy{Scope: pluginapi.CachePolicyScope{CallerScope: caller}}}
				if method == pluginabi.MethodCachePolicyAttempt {
					request["scope"] = pluginapi.CachePolicyScope{CallerScope: caller}
					request["policy"] = pluginapi.CachePolicy{Scope: pluginapi.CachePolicyScope{CallerScope: foreign}}
				}
				raw, _ := json.Marshal(request)
				_, err := p.Handle(method, raw)
				var refusal *pluginabi.Error
				if !errors.As(err, &refusal) || refusal.HTTPStatus != 403 || refusal.Code != "pacer_account_access_denied" {
					t.Fatal("policy RPC bypassed grants", err)
				}
			}
		})
	}
}
