package pacer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestClaimedAutomaticProviderSwitchStopsBeforeUpstream(t *testing.T) {
	for _, scenario := range []string{"same-request-retry", "next-request"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRoutingFixture(t, "off")
			p := f.p
			caller := clientCaller("provider-bound-caller")
			p.creds["codex-c"] = credential{ID: "codex-c", Index: "ic", Provider: "codex", Account: tracker.Digest("codex-account"), Alias: "Codex C"}
			p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a", "codex-c"}}}}
			q := f.q
			q.Route = "shared-model-alias"
			var calls, codexCalls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/codex" {
					codexCalls.Add(1)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			dispatch := func(r pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
				t.Helper()
				response := invoke(t, p, pluginabi.MethodRequestInterceptAfter, r).(pluginapi.RequestInterceptResponse)
				if !response.Terminate {
					provider := p.creds[text(r.Metadata, "selected_auth_id")].Provider
					result, err := upstream.Client().Get(upstream.URL + "/" + provider)
					if err != nil {
						t.Fatal(err)
					}
					result.Body.Close()
				}
				return response
			}
			first := grantBefore(t, p, grantRequest("bound-claude", caller, "a", &q))
			if response := dispatch(first); response.Terminate {
				t.Fatalf("initial Claude request denied: %s", response.ResponseBody)
			}
			invoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: first.RequestID})
			next := grantBefore(t, p, grantRequest(scenario, caller, "a", &q))
			pick := invoke(t, p, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
				Provider: "mixed", Providers: []string{"claude", "codex"}, Model: "shared-model-alias",
				Options:    pluginapi.SchedulerOptions{Headers: next.Headers},
				Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "claude"}, {ID: "codex-c", Provider: "codex"}},
			}).(pluginapi.SchedulerPickResponse)
			if pick.Handled {
				t.Fatalf("fixture must exercise the host's mixed-provider fallback: %+v", pick)
			}
			if scenario == "same-request-retry" {
				if response := dispatch(next); response.Terminate {
					t.Fatalf("same-provider attempt denied: %s", response.ResponseBody)
				}
			}
			next.Metadata["selected_auth_id"], next.Model = "codex-c", "gpt-target"
			response := dispatch(next)
			var problem struct {
				Error struct {
					Code, Action string
					Retryable    *bool
				}
			}
			if json.Unmarshal(response.ResponseBody, &problem) != nil || !response.Terminate || response.StatusCode != http.StatusConflict ||
				problem.Error.Code != "quota_provider_switch_forbidden" || problem.Error.Action != "select_same_provider" || problem.Error.Retryable == nil || *problem.Error.Retryable {
				t.Fatalf("expected actionable provider refusal: %+v %s", response, response.ResponseBody)
			}
			if calls.Load() == 0 || codexCalls.Load() != 0 {
				t.Fatalf("upstream calls=%d foreign-provider calls=%d", calls.Load(), codexCalls.Load())
			}
			if binding, _, err := p.chats.Get(tracker.Key(caller, "codex", q)); err != nil || binding.Key != "" {
				t.Fatal("refused provider acquired a binding", binding, err)
			}
			if binding, _, err := p.chats.Get(tracker.Key(caller, "claude", q)); err != nil || binding.Auth != "a" || binding.Generation != 1 {
				t.Fatal("refusal changed original binding", binding, err)
			}
		})
	}
}

func TestClaimedFallbackToUsageOnlyProviderIsRefused(t *testing.T) {
	for _, provider := range []string{"deepseek", "qwen"} {
		t.Run(provider, func(t *testing.T) {
			f := newRoutingFixture(t, "off")
			p := f.p
			p.Host = func(method string, input any) (json.RawMessage, error) {
				if method == "host.auth.list" {
					return json.Marshal(map[string]any{"files": []map[string]any{
						{"id": "a", "auth_index": "ia", "provider": "claude"},
						{"id": "other", "auth_index": "io", "provider": provider},
					}})
				}
				return testHost(method, input)
			}
			p.discover(true)
			if _, known := p.creds["other"]; known || p.usageCreds["other"].Provider != provider {
				t.Fatal("usage-only provider entered the binding inventory")
			}
			caller := clientCaller("fallback-caller")
			p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: caller, AuthIDs: []string{"a", "other"}}}}
			q := f.q
			first := grantBefore(t, p, grantRequest("first", caller, "a", &q))
			if response := invoke(t, p, pluginabi.MethodRequestInterceptAfter, first).(pluginapi.RequestInterceptResponse); response.Terminate {
				t.Fatalf("initial request denied: %s", response.ResponseBody)
			}
			next := grantBefore(t, p, grantRequest("fallback", caller, "other", &q))
			response := invoke(t, p, pluginabi.MethodRequestInterceptAfter, next).(pluginapi.RequestInterceptResponse)
			var problem struct{ Error struct{ Code string } }
			if json.Unmarshal(response.ResponseBody, &problem) != nil || !response.Terminate || problem.Error.Code != "quota_state_unavailable" {
				t.Fatalf("claimed unsupported provider fallback admitted: %+v %s", response, response.ResponseBody)
			}
		})
	}
}
