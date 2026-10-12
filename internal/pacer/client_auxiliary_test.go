package pacer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func auxiliaryRegisteredPlugin(t *testing.T, support hostCachePolicySupport, schema int, enabled bool) *Plugin {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	now := func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) }
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cachepolicy.Create(context.Background(), db, cachepolicy.Options{Clock: now, Routes: []cachepolicy.Route{{ProviderRoute: "a-main", TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}}, {ProviderRoute: "z-helper", TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}}}})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	p := New(testHost)
	p.now = now
	p.workerOnce.Do(func() {})
	t.Cleanup(p.Close)
	config := fmt.Sprintf("enabled: true\nstate_path: %s\ncache_policy:\n  enabled: %v\n  require_managed: %v\n  routes:\n    - id: z-helper\n      models: [claude-haiku-4-5]\n    - id: a-main\n      models: [claude-sonnet-4-6, claude-opus-5-5]\n", path, enabled, enabled)
	guard := requiredInterceptors{Version: 1, PluginID: "pacer", Required: true, RequestInterceptors: true, NestedModelInterceptors: true, RetryInterceptors: true}
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte(config), "schema_version": schema, "host_capabilities": map[string]any{"required_interceptors": guard, "cache_policy": support}})
	return p
}

func TestManagedAuxiliaryDiscoveryRequiresTrustedHostAndConfiguredPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		host                           hostCachePolicySupport
		schema                         int
		configured, supported, enabled bool
	}{
		{"old host", hostCachePolicySupport{}, 7, true, false, false},
		{"old revision", hostCachePolicySupport{2, true}, 7, true, false, false},
		{"no executor guarantee", hostCachePolicySupport{3, false}, 7, true, false, false},
		{"auxiliary host", hostCachePolicySupport{3, true}, 7, true, true, true},
		{"not configured", hostCachePolicySupport{3, true}, 7, false, true, false},
		{"schema six", hostCachePolicySupport{3, true}, 6, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := auxiliaryRegisteredPlugin(t, tc.host, tc.schema, tc.configured)
			response := p.events(pluginapi.ManagementRequest{Method: http.MethodGet})
			var discovery struct {
				API    clientAPI       `json:"api"`
				Legacy json.RawMessage `json:"cache_policy"`
			}
			if response.StatusCode != 200 || json.Unmarshal(response.Body, &discovery) != nil {
				t.Fatal(response.StatusCode, string(response.Body))
			}
			feature := discovery.API.Features["managed_auxiliary"]
			if feature.Supported != tc.supported || feature.Enabled != tc.enabled || feature.ContractRevision != 1 {
				t.Fatal("unattested auxiliary support advertised", feature)
			}
			revision := 2
			if tc.supported {
				revision = 3
			}
			if discovery.API.Features["cache_policy"].ContractRevision != revision {
				t.Fatal("wrong host policy contract revision", discovery.API.Features)
			}
			kinds := []string{"primary", "warming"}
			if tc.enabled {
				kinds = append(kinds, "auxiliary")
			}
			if !reflect.DeepEqual(discovery.API.Requirements.ManagedKinds, kinds) {
				t.Fatal("unusable helper kind advertised", discovery.API.Requirements)
			}
			routes := []clientCacheRoute{}
			if tc.configured {
				routes = []clientCacheRoute{{ProviderRoute: "a-main", Models: []string{"claude-opus-5-5", "claude-sonnet-4-6"}, TTLs: []string{"5m", "1h"}}, {ProviderRoute: "z-helper", Models: []string{"claude-haiku-4-5"}, TTLs: []string{"5m", "1h"}}}
				var legacy struct {
					Revision int `json:"contract_revision"`
				}
				if json.Unmarshal(discovery.Legacy, &legacy) != nil || legacy.Revision != revision {
					t.Fatal("legacy and client API discovery disagree", string(response.Body))
				}
			}
			if !reflect.DeepEqual(discovery.API.Requirements.CacheRoutes, routes) {
				t.Fatal("public enrollment routes are missing or unsorted", discovery.API.Requirements.CacheRoutes)
			}
			if !tc.configured {
				return
			}
			scope := pluginapi.CachePolicyScope{CallerScope: clientCaller("aux-key"), InstallationID: "client", SessionID: "chat", ProviderRoute: "a-main"}
			capability := strings.Repeat("a", 42) + "A"
			prepare := pluginapi.CachePolicyOperation{Protocol: 1, Operation: "prepare", InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute, ContextEpoch: "old-checkpoint", TTL: "1h", OperationID: "prepare", Enrollment: "existing_history"}
			pending := invoke(t, p, pluginabi.MethodCachePolicyHandle, pluginapi.CachePolicyRequest{Method: "POST", Scope: scope, Capability: capability, Operation: prepare}).(pluginapi.CachePolicyResponse)
			var state cachepolicy.State
			if pending.StatusCode != 200 || json.Unmarshal(pending.Body, &state) != nil {
				t.Fatal(pending.StatusCode, string(pending.Body))
			}
			resolve := pluginapi.CachePolicyResolveRequest{Protocol: 1, Scope: scope, Capability: capability, ContextEpoch: state.Active.ContextEpoch, Revision: state.Revision, Kind: "auxiliary", RequestID: "helper", Model: "claude-opus-5-5", SourceFormat: "claude"}
			raw, _ := json.Marshal(resolve)
			result, err := p.Handle(pluginabi.MethodCachePolicyResolve, raw)
			if !tc.enabled {
				var refusal *pluginabi.Error
				if !errors.As(err, &refusal) || refusal.HTTPStatus != 422 || len(p.selections) != 0 {
					t.Fatal("unattested host resolved an auxiliary", result, err)
				}
				var forged map[string]json.RawMessage
				if err = json.Unmarshal(raw, &forged); err != nil {
					t.Fatal(err)
				}
				forged["host_capabilities"] = json.RawMessage(`{"cache_policy":{"contract_revision":3,"auxiliary_no_cache":true}}`)
				injected, _ := json.Marshal(forged)
				_, err = p.Handle(pluginabi.MethodCachePolicyResolve, injected)
				if !errors.As(err, &refusal) || refusal.HTTPStatus != 400 || len(p.selections) != 0 || p.clientAPILocked().Features["managed_auxiliary"].Enabled {
					t.Fatal("client body forged host executor support", err)
				}
				return
			}
			selected, ok := result.(pluginapi.CachePolicy)
			if err != nil || !ok || selected.Kind != "auxiliary" || selected.TTL != "" {
				t.Fatal("configured host did not resolve no-cache helper", result, err)
			}
			r := grantRequest(resolve.RequestID, scope.CallerScope, "a", nil)
			upstream := 0
			if after := policyTestDispatch(t, p, r, true, &upstream); after.Terminate || upstream != 1 || p.requests[r.RequestID].Binding.Key != "" {
				t.Fatal("resolved helper denied or created primary affinity", after, upstream)
			}
			for _, invalid := range []pluginapi.CachePolicy{{Protocol: 1, Scope: scope, Kind: "auxiliary", TTL: "1h"}, {Protocol: 1, Scope: scope, Kind: "compaction"}} {
				p.selections[r.RequestID] = invalid
				requirePolicyRefusal(t, policyTestDispatch(t, p, r, false, &upstream))
			}
			if upstream != 1 {
				t.Fatal("invalid auxiliary selection reached upstream", upstream)
			}
		})
	}
}
