package pacer

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestClientAccessPublishesDisjointPoolsBeforeSelection(t *testing.T) {
	p := newRoutingFixture(t, "off", false).p
	callers := []ClientGrant{
		{CallerScope: clientCaller("alice-key"), AuthIDs: []string{"alice-claude", "alice-codex"}},
		{CallerScope: clientCaller("bob-key"), AuthIDs: []string{"bob-claude", "bob-codex"}},
		{CallerScope: clientCaller("carol-key"), AuthIDs: []string{"carol-claude", "carol-codex"}},
	}
	p.cfg.ClientAccess = &ClientAccess{Grants: callers}
	for i, caller := range callers {
		r := grantRequest(caller.CallerScope, caller.CallerScope, "", nil)
		// Grants use stable IDs, even when inventory discovery has not loaded
		// them yet. The host resolves provider/model eligibility, not Pacer.
		r.Model = "operator-model-alias"
		delete(r.Metadata, "selected_auth_id")
		r.Headers.Set("X-Pacer-Caller-Scope", callers[(i+1)%len(callers)].CallerScope)
		r.Headers.Set("X-Pacer-Allowed-Auth-IDs", "*")
		r.Headers.Set("X-Pacer-Auth-Pool", `{"version":1,"auth_ids":["foreign"]}`)
		r.Metadata["auth_pool"] = map[string]any{"version": 1, "auth_ids": []string{"foreign"}}
		result := invoke(t, p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
		if result.Terminate || result.AuthPool == nil || result.AuthPool.Version != 1 || !reflect.DeepEqual(result.AuthPool.AuthIDs, caller.AuthIDs) {
			t.Fatalf("caller %d received wrong pool: %+v", i, result)
		}
		if len(result.Headers) != 1 || result.Headers.Get(requestHeader) != r.RequestID {
			t.Fatalf("pool authority escaped into request headers: %v", result.Headers)
		}
		if p.requests[r.RequestID].Caller != caller.CallerScope {
			t.Fatal("client headers replaced the host principal")
		}
		wire, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			AuthPool *pluginapi.AuthPool `json:"auth_pool"`
		}
		if err := json.Unmarshal(wire, &decoded); err != nil || decoded.AuthPool == nil || !reflect.DeepEqual(decoded.AuthPool.AuthIDs, caller.AuthIDs) {
			t.Fatalf("native response lost pool restriction: %s, error=%v", wire, err)
		}
		// Returned restrictions and operator state must not alias each other.
		want := slices.Clone(caller.AuthIDs)
		result.AuthPool.AuthIDs[0] = "response-mutated"
		if !reflect.DeepEqual(p.cfg.ClientAccess.Grants[i].AuthIDs, want) {
			t.Fatal("response mutation changed operator grants")
		}
		result.AuthPool.AuthIDs[0] = want[0]
		p.cfg.ClientAccess.Grants[i].AuthIDs[0] = "later-grant"
		if !reflect.DeepEqual(result.AuthPool.AuthIDs, want) {
			t.Fatal("later grant edit changed an in-flight pool")
		}
	}
}

func TestClientAccessPoolDefaultsAndTrustedCaller(t *testing.T) {
	caller := clientCaller("allowed-key")
	for _, tc := range []struct {
		name   string
		access *ClientAccess
		caller string
		deny   bool
	}{
		{name: "omitted is shared"},
		{name: "empty denies", access: &ClientAccess{}, caller: caller, deny: true},
		{name: "unknown caller", access: &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a"}}}}, caller: clientCaller("foreign-key"), deny: true},
		{name: "header-only caller", access: &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a"}}}}, deny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRoutingFixture(t, "off", false).p
			p.cfg.ClientAccess = tc.access
			r := grantRequest(tc.name, tc.caller, "", nil)
			r.Headers.Set("X-Pacer-Caller-Scope", caller)
			r.Headers.Set("X-Pacer-Allowed-Auth-IDs", "a")
			r.Headers.Set("X-Pacer-Auth-Pool", `{"version":1,"auth_ids":["a"]}`)
			result := invoke(t, p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
			if tc.deny {
				requireGrantRefusal(t, result, nil, http.StatusForbidden)
				if _, tracked := p.requests[r.RequestID]; tracked {
					t.Fatal("denied caller acquired an in-flight request")
				}
				return
			}
			if result.Terminate || result.AuthPool != nil {
				t.Fatalf("shared pool was restricted by a client header: %+v", result)
			}
			wire, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["auth_pool"]; present {
				t.Fatalf("shared mode emitted a pool restriction: %s", wire)
			}
		})
	}
}

func TestClientAccessRechecksRevokedPoolOnRetries(t *testing.T) {
	p := newRoutingFixture(t, "off", false).p
	caller := clientCaller("retry-key")
	p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a", "b"}}}}
	r := grantRequest("retry", caller, "a", nil)
	before := invoke(t, p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
	if before.Terminate || before.AuthPool == nil || !reflect.DeepEqual(before.AuthPool.AuthIDs, []string{"a", "b"}) {
		t.Fatalf("missing initial pool: %+v", before)
	}
	for _, id := range []string{"a", "b"} {
		r.Metadata["selected_auth_id"] = id
		result := invoke(t, p, pluginabi.MethodRequestInterceptAfter, r).(pluginapi.RequestInterceptResponse)
		if result.Terminate {
			t.Fatalf("granted retry %s denied: %s", id, result.ResponseBody)
		}
	}
	// Production grant changes require an idle reload. Mutating this fixture
	// exercises the dispatch defense against stale host selections as well.
	p.cfg.ClientAccess.Grants[0].AuthIDs = []string{"b"}
	r.Metadata["selected_auth_id"] = "a"
	result := invoke(t, p, pluginabi.MethodRequestInterceptAfter, r)
	requireGrantRefusal(t, result, nil, http.StatusForbidden)
	if !reflect.DeepEqual(before.AuthPool.AuthIDs, []string{"a", "b"}) {
		t.Fatal("revocation mutated the original restriction")
	}
	next := grantRequest("after-revocation", caller, "b", nil)
	newBefore := invoke(t, p, pluginabi.MethodRequestInterceptBefore, next).(pluginapi.RequestInterceptResponse)
	if newBefore.Terminate || newBefore.AuthPool == nil || !reflect.DeepEqual(newBefore.AuthPool.AuthIDs, []string{"b"}) {
		t.Fatalf("new request reused revoked membership: %+v", newBefore)
	}
	r.Metadata["selected_auth_id"] = "b"
	if result := invoke(t, p, pluginabi.MethodRequestInterceptAfter, r).(pluginapi.RequestInterceptResponse); result.Terminate {
		t.Fatal("remaining granted account was denied")
	}
	p.cfg.ClientAccess.Grants = nil
	requireGrantRefusal(t, invoke(t, p, pluginabi.MethodRequestInterceptAfter, r), nil, http.StatusForbidden)
	requireGrantRefusal(t, invoke(t, p, pluginabi.MethodRequestInterceptBefore, grantRequest("caller-revoked", caller, "", nil)), nil, http.StatusForbidden)
}
