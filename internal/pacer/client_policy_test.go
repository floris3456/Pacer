package pacer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/policyadapter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Count the requests that cross the modeled host dispatch boundary. A hook
// error is not a substitute for an explicit termination response.
func policyTestDispatch(t *testing.T, p *Plugin, req pluginapi.RequestInterceptRequest, before bool, upstream *int) pluginapi.RequestInterceptResponse {
	t.Helper()
	if before {
		response := invoke(t, p, pluginabi.MethodRequestInterceptBefore, req).(pluginapi.RequestInterceptResponse)
		if response.Terminate {
			return response
		}
		for name, values := range response.Headers {
			req.Headers[name] = values
		}
	}
	response := invoke(t, p, pluginabi.MethodRequestInterceptAfter, req).(pluginapi.RequestInterceptResponse)
	if !response.Terminate {
		*upstream++
	}
	return response
}

func requirePolicyRefusal(t *testing.T, response pluginapi.RequestInterceptResponse) {
	t.Helper()
	var body struct {
		Error struct {
			Code, Action string
			Retryable    bool
		} `json:"error"`
	}
	if !response.Terminate || response.StatusCode != 409 || json.Unmarshal(response.ResponseBody, &body) != nil || body.Error.Code != "pacer_cache_policy_required" || body.Error.Action != "enroll_cache_policy" || body.Error.Retryable {
		t.Fatalf("expected actionable policy-required termination: %#v %s", response, response.ResponseBody)
	}
}

func TestRequiredCachePolicyRejectsUnadmittedRequestsAtBothHooks(t *testing.T) {
	caller := clientCaller("managed-key")
	for _, stage := range []string{"before", "after"} {
		for _, scenario := range []string{"missing selection", "declaration only", "thinking suffix", "requested model", "missing caller", "foreign caller", "wrong route", "compaction", "title", "generate"} {
			t.Run(stage+"/"+scenario, func(t *testing.T) {
				f := newRoutingFixture(t, "off")
				p := f.p
				p.cfg.CachePolicy = PolicyConfig{Enabled: true, RequireManaged: true, Routes: []PolicyRoute{{ID: "claude-main", Models: []string{"claude-opus-5-5"}}}}
				r := grantRequest("unadmitted", caller, "a", nil)
				switch scenario {
				case "declaration only":
					r.Headers.Set("X-Pacer-Cache-Policy", `v=1;route=claude-main;ttl=1h;kind=primary`)
					r.Headers.Set("X-Pacer-Cache-Capability", strings.Repeat("a", 43))
				case "thinking suffix":
					r.Model = "claude-opus-5-5(high)"
				case "requested model":
					r.Model, r.RequestedModel = "helper-model", "claude-opus-5-5(8192)"
				case "missing caller", "foreign caller", "wrong route":
					selected := pluginapi.CachePolicy{Scope: pluginapi.CachePolicyScope{CallerScope: caller, ProviderRoute: "claude-main"}}
					if scenario == "missing caller" {
						r.Metadata["caller_scope"] = ""
					} else if scenario == "foreign caller" {
						selected.Scope.CallerScope = clientCaller("another-caller")
					} else {
						selected.Scope.ProviderRoute = "another-route"
					}
					p.selections[r.RequestID] = selected
				case "compaction", "title", "generate":
					q := f.q
					q.Kind = scenario
					r = grantRequest(r.RequestID, caller, "a", &q)
				}
				if stage == "after" {
					// Before alias resolution the configured upstream model may
					// not be known. The after hook must check it independently.
					initial := r
					initial.Model, initial.RequestedModel = "public-opus-alias", "public-opus-alias"
					grantBefore(t, p, initial)
				}
				upstream := 0
				response := policyTestDispatch(t, p, r, stage == "before", &upstream)
				requirePolicyRefusal(t, response)
				if upstream != 0 {
					t.Fatal("unadmitted request reached upstream")
				}
				if stage == "before" && len(p.requests) != 0 {
					t.Fatal("denied request entered request tracking")
				}
			})
		}
	}
}

func TestRequiredCachePolicyLeavesOptOutAndUnlistedHelpersAllowed(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		required    bool
	}{{"default opt-out", "claude-opus-5-5", false}, {"unlisted helper", "helper-model(high)", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRoutingFixture(t, "off").p
			p.cfg.CachePolicy = PolicyConfig{Enabled: true, RequireManaged: tc.required, Routes: []PolicyRoute{{ID: "claude-main", Models: []string{"claude-opus-5-5"}}}}
			r := grantRequest(tc.name, clientCaller("managed-key"), "a", nil)
			r.Model = tc.model
			upstream := 0
			if response := policyTestDispatch(t, p, r, true, &upstream); response.Terminate || upstream != 1 {
				t.Fatal("unrequired model was blocked", response, upstream)
			}
		})
	}
}

func resolvedPolicyFixture(t *testing.T) (*routingFixture, pluginapi.CachePolicyResolveRequest) {
	t.Helper()
	f := newRoutingFixture(t, "off")
	p := f.p
	scope := pluginapi.CachePolicyScope{CallerScope: clientCaller("managed-key"), InstallationID: "required-policy-test", SessionID: "session", ProviderRoute: "claude-main"}
	p.cfg.CachePolicy = PolicyConfig{Enabled: true, RequireManaged: true, Routes: []PolicyRoute{{ID: scope.ProviderRoute, Models: []string{"claude-opus-5-5"}}}, TrustedEmptyInstallations: []TrustedEmptyInstallation{{CallerDigest: scope.CallerScope, Installation: scope.InstallationID}}}
	opts, err := p.policyOptions(p.cfg.CachePolicy)
	if err != nil {
		t.Fatal(err)
	}
	storeOpts, err := policyadapter.PolicyOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cachepolicy.Create(context.Background(), p.db, storeOpts); err != nil {
		t.Fatal(err)
	}
	p.policy, err = policyadapter.Open(context.Background(), p.db, opts)
	if err != nil {
		t.Fatal(err)
	}
	capability := strings.Repeat("a", 42) + "A"
	command := pluginapi.CachePolicyRequest{Method: "POST", Scope: scope, Capability: capability, Operation: pluginapi.CachePolicyOperation{Protocol: 1, Operation: "ensure", InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute, ContextEpoch: "empty", TTL: "1h", OperationID: "enroll", Enrollment: "new_empty"}}
	response := invoke(t, p, pluginabi.MethodCachePolicyHandle, command).(pluginapi.CachePolicyResponse)
	var state cachepolicy.State
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &state) != nil {
		t.Fatal(response.StatusCode, string(response.Body))
	}
	return f, pluginapi.CachePolicyResolveRequest{Protocol: 1, Scope: scope, Capability: capability, ContextEpoch: state.Active.ContextEpoch, Revision: state.Revision, Kind: "primary", RequestID: "resolved", Model: "claude-opus-5-5", SourceFormat: "claude"}
}

func TestRequiredCachePolicyAcceptsResolvedSelectionAndRechecksRetries(t *testing.T) {
	for _, kind := range []string{"primary", "warming"} {
		t.Run(kind, func(t *testing.T) {
			f, resolve := resolvedPolicyFixture(t)
			resolve.Kind = kind
			selected := invoke(t, f.p, pluginabi.MethodCachePolicyResolve, resolve).(pluginapi.CachePolicy)
			if f.p.selections[resolve.RequestID] != selected {
				t.Fatal("policy RPC did not retain admitted selection")
			}
			r := grantRequest(resolve.RequestID, resolve.Scope.CallerScope, "a", nil)
			upstream := 0
			if response := policyTestDispatch(t, f.p, r, true, &upstream); response.Terminate || upstream != 1 {
				t.Fatal("resolved policy was denied", response, upstream)
			}
			// A different lifecycle ID cannot borrow the preceding selection.
			retry := r
			retry.RequestID = "new-retry-id"
			requirePolicyRefusal(t, policyTestDispatch(t, f.p, retry, true, &upstream))
			requirePolicyRefusal(t, policyTestDispatch(t, f.p, retry, false, &upstream))
			// Even with the original authenticated before hook still tracked,
			// every retry's after hook requires its process-owned selection.
			delete(f.p.selections, r.RequestID)
			requirePolicyRefusal(t, policyTestDispatch(t, f.p, r, false, &upstream))
			if upstream != 1 {
				t.Fatal("retry without admission reached upstream", upstream)
			}
		})
	}
}

func TestRequiredCachePolicyRegistrationRequiresEnabledPolicyAndHostEnforcement(t *testing.T) {
	complete := requiredInterceptors{Version: 1, PluginID: "pacer", Required: true, RequestInterceptors: true, NestedModelInterceptors: true, RetryInterceptors: true}
	partial := complete
	partial.RetryInterceptors = false
	for _, tc := range []struct {
		name    string
		enabled bool
		schema  int
		guard   requiredInterceptors
		message string
	}{
		{"disabled policy", false, 7, complete, "require_managed needs"},
		{"missing host guard", true, 7, requiredInterceptors{}, "require_managed needs"},
		{"missing retry guard", true, 7, partial, "require_managed needs"},
		{"old host schema", true, 6, complete, "requires CPA schema 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(testHost)
			defer p.Close()
			path := filepath.Join(t.TempDir(), "uninitialized.sqlite")
			config := fmt.Sprintf("enabled: true\nstate_path: %s\ncache_policy:\n  enabled: %v\n  require_managed: true\n  routes:\n    - id: claude-main\n      models: [claude-opus-5-5]\n", path, tc.enabled)
			raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config), "schema_version": tc.schema, "host_capabilities": map[string]any{"required_interceptors": tc.guard}})
			if _, err := p.Handle(pluginabi.MethodPluginRegister, raw); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatal("unsafe required-policy configuration accepted", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("rejected configuration touched policy state", err)
			}
		})
	}
}

func TestRequiredCachePolicyDiscoveryListsSortedUniqueModelsAndActualAvailability(t *testing.T) {
	f, _ := resolvedPolicyFixture(t)
	p := f.p
	complete := requiredInterceptors{Version: 1, PluginID: "pacer", Required: true, RequestInterceptors: true, NestedModelInterceptors: true, RetryInterceptors: true}
	p.cfg.CachePolicy.Routes = []PolicyRoute{{ID: "main", Models: []string{"z-model", "a-model", "z-model"}}, {ID: "other", Models: []string{"m-model", "a-model"}}}
	for _, tc := range []struct {
		name                         string
		schema                       int
		guard                        requiredInterceptors
		required, supported, enabled bool
	}{
		{"enforced", 7, complete, true, true, true},
		{"opted out", 7, complete, false, true, false},
		{"no host enforcement", 7, requiredInterceptors{}, true, false, false},
		{"schema six", 6, complete, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.hostSchema, p.hostGuard, p.cfg.CachePolicy.RequireManaged = tc.schema, tc.guard, tc.required
			response := p.events(pluginapi.ManagementRequest{Method: http.MethodGet})
			var discovery struct {
				API clientAPI `json:"api"`
			}
			if response.StatusCode != 200 || json.Unmarshal(response.Body, &discovery) != nil {
				t.Fatal(response.StatusCode, string(response.Body))
			}
			feature, exists := discovery.API.Features["required_cache_policy"]
			if !exists || feature.Supported != tc.supported || feature.Enabled != tc.enabled {
				t.Fatal("incorrect required-policy availability", feature)
			}
			models := []string{}
			if tc.required {
				models = []string{"a-model", "m-model", "z-model"}
			}
			if !reflect.DeepEqual(discovery.API.Requirements.ManagedCacheModels, models) || !reflect.DeepEqual(discovery.API.Requirements.ManagedKinds, []string{"primary", "warming"}) {
				t.Fatal("unstable public policy requirements", string(response.Body))
			}
			for _, private := range []string{"caller_scope", "auth_ids", "required-policy-test", "managed-key"} {
				if strings.Contains(string(response.Body), private) {
					t.Fatal("policy requirements leaked private configuration", private)
				}
			}
		})
	}
}
