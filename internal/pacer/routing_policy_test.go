package pacer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/policyadapter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHandleRoutingActionOrFallbackCannotBypassManagedPolicy(t *testing.T) {
	for _, tc := range []struct {
		mode string
		kind string
	}{{"off", "primary"}, {"dry-run", "primary"}, {"on", "primary"}, {"on", "warming"}, {"on", "compaction"}} {
		t.Run(tc.mode+"/"+tc.kind, func(t *testing.T) {
			f := newRoutingFixture(t, tc.mode)
			opts := policyadapter.Options{Clock: f.p.now, Routes: []policyadapter.Route{{ProviderRoute: "claude-main", Models: []string{"claude-opus-5-5"}, TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}}}}
			policyOpts, err := policyadapter.PolicyOptions(opts)
			if err != nil {
				t.Fatal(err)
			}
			// Explicit fixture first installation, never a scheduler fallback.
			store, err := cachepolicy.Create(context.Background(), f.p.db, policyOpts)
			if err != nil {
				t.Fatal(err)
			}
			f.p.policy, err = policyadapter.Open(context.Background(), f.p.db, opts)
			if err != nil {
				t.Fatal(err)
			}
			scope := pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "fixture", SessionID: "chat", ProviderRoute: "claude-main"}
			capability := strings.Repeat("a", 42) + "A"
			control := func(op pluginapi.CachePolicyOperation, want int) cachepolicy.State {
				op.Protocol, op.InstallationID, op.SessionID, op.ProviderRoute = 1, scope.InstallationID, scope.SessionID, scope.ProviderRoute
				resp := invoke(t, f.p, pluginabi.MethodCachePolicyHandle, pluginapi.CachePolicyRequest{Method: "POST", Scope: scope, Capability: capability, Operation: op}).(pluginapi.CachePolicyResponse)
				if resp.StatusCode != want {
					t.Fatalf("%d %s", resp.StatusCode, resp.Body)
				}
				var state cachepolicy.State
				if want == 200 && json.Unmarshal(resp.Body, &state) != nil {
					t.Fatal(string(resp.Body))
				}
				return state
			}
			pending := control(pluginapi.CachePolicyOperation{Operation: "prepare", ContextEpoch: "old", Enrollment: "existing_history", TTL: "1h", OperationID: "prepare"}, 200)
			active := control(pluginapi.CachePolicyOperation{Operation: "commit", ContextEpoch: "checkpoint", Checkpoint: "checkpoint", TTL: "1h", ExpectedRevision: pending.Revision, OperationID: "commit", TransitionID: pending.Pending.ID, CheckpointReceipt: &pluginapi.CachePolicyCheckpointReceipt{Version: 1, TransitionID: pending.Pending.ID, PreviousCheckpoint: pending.Pending.FromCheckpoint, Checkpoint: "checkpoint", Committed: true}}, 200)
			resolve := pluginapi.CachePolicyResolveRequest{Protocol: 1, Scope: scope, Capability: capability, ContextEpoch: active.Active.ContextEpoch, Revision: active.Revision, Kind: tc.kind, RequestID: "managed", Model: "claude-opus-5-5", SourceFormat: "claude"}
			if tc.kind == "compaction" {
				raw, _ := json.Marshal(resolve)
				_, err = f.p.Handle(pluginabi.MethodCachePolicyResolve, raw)
				var coded *pluginabi.Error
				if !errors.As(err, &coded) || coded.HTTPStatus != 422 {
					t.Fatal("summary request entered managed primary dispatch", err)
				}
				return
			}
			selected := invoke(t, f.p, pluginabi.MethodCachePolicyResolve, resolve).(pluginapi.CachePolicy)
			r := f.before(t, "managed", resolve.Model)
			choice := f.pick(t, r, "a", "b")
			if choice.Handled != (tc.mode == "on" && tc.kind == "primary") {
				t.Fatal(choice)
			}
			if got := f.after(t, r, "b"); got.Terminate {
				t.Fatal(string(got.ResponseBody))
			}
			if tc.kind != "primary" && f.p.requests["managed"].Binding.Key != "" {
				t.Fatal("auxiliary policy created a primary routing binding")
			}
			// Routing cannot change the frozen policy selection, even under a handled choice.
			physical := pluginapi.CachePolicyAttemptRequest{Operation: "begin", RequestID: "managed", AttemptID: "physical", Capability: capability, Policy: selected}
			started := invoke(t, f.p, pluginabi.MethodCachePolicyAttempt, physical).(pluginapi.CachePolicyAttemptResponse)
			if started.Policy != selected || selected.TTL != "1h" {
				t.Fatal(started)
			}
			physical.Operation, physical.StartedAt = "validate", started.StartedAt
			bad := physical
			bad.Policy.TTL = "5m"
			raw, _ := json.Marshal(bad)
			_, err = f.p.Handle(pluginabi.MethodCachePolicyAttempt, raw)
			var coded *pluginabi.Error
			if !errors.As(err, &coded) || coded.HTTPStatus != 409 {
				t.Fatal("scheduler action bypassed frozen TTL", err)
			}
			pending = control(pluginapi.CachePolicyOperation{Operation: "prepare", ContextEpoch: active.Active.ContextEpoch, ExpectedRevision: active.Revision, TTL: "5m", OperationID: "switch"}, 200)
			control(pluginapi.CachePolicyOperation{Operation: "commit", ContextEpoch: "next", Checkpoint: "next", TTL: "5m", ExpectedRevision: pending.Revision, OperationID: "blocked", TransitionID: pending.Pending.ID, CheckpointReceipt: &pluginapi.CachePolicyCheckpointReceipt{Version: 1, TransitionID: pending.Pending.ID, PreviousCheckpoint: pending.Pending.FromCheckpoint, Checkpoint: "next", Committed: true}}, 409)
			// Old started work still validates under its pinned 1h policy.
			invoke(t, f.p, pluginabi.MethodCachePolicyAttempt, physical)
			physical.Operation, physical.Outcome, physical.Dispatched = "finish", "succeeded", true
			invoke(t, f.p, pluginabi.MethodCachePolicyAttempt, physical)
			f.complete(t, "managed")
			outstanding, err := store.Outstanding(context.Background(), cachepolicy.Credentials{CallerDigest: scope.CallerScope, Capability: capability}, cachepolicy.Scope{InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute})
			if err != nil || len(outstanding) != 0 {
				t.Fatal(outstanding, err)
			}
			resolve.RequestID = "must-pause"
			raw, _ = json.Marshal(resolve)
			if _, err = f.p.Handle(pluginabi.MethodCachePolicyResolve, raw); err == nil {
				t.Fatal("routing mode bypassed pending policy CAS")
			}
		})
	}
}
