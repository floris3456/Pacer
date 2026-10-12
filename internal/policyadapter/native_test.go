package policyadapter

import (
	"reflect"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func nativeFixture(t *testing.T) (*fixture, cachepolicy.State) {
	t.Helper()
	f := newFixture(t)
	f.opts.NativeCompaction = true
	f.open()
	active := f.enroll()
	pending := f.control("POST", f.operation("prepare", active, "1h"), 200, "")
	return f, pending
}

func nativeReference(f *fixture, state cachepolicy.State, requestID string) pluginapi.CachePolicyResolveRequest {
	r := f.reference(state, requestID)
	r.Kind, r.NativeReplayAuthorized = "native_compaction", true
	return r
}

func TestNativeResolveRequiresHostOptionAuthorizationAndClaudeTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*fixture, *pluginapi.CachePolicyResolveRequest)
	}{
		{"host not attested", func(f *fixture, _ *pluginapi.CachePolicyResolveRequest) { f.opts.NativeCompaction = false; f.open() }},
		{"no replay authority", func(_ *fixture, r *pluginapi.CachePolicyResolveRequest) { r.NativeReplayAuthorized = false }},
		{"OpenAI transport", func(_ *fixture, r *pluginapi.CachePolicyResolveRequest) { r.SourceFormat = "openai-response" }},
		{"unknown transport", func(_ *fixture, r *pluginapi.CachePolicyResolveRequest) { r.SourceFormat = "websocket" }},
		{"unlisted model", func(_ *fixture, r *pluginapi.CachePolicyResolveRequest) { r.Model = "claude-haiku-4-5" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, pending := nativeFixture(t)
			r := nativeReference(f, pending, "native")
			tc.edit(f, &r)
			_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, r)
			checkError(t, err, 422, "unsupported")
			if after := f.read(); !reflect.DeepEqual(after, pending) || len(f.a.requests) != 0 {
				t.Fatal("unsupported native request changed durable/process state", after)
			}
			if outstanding, err := f.a.Outstanding(testContext, f.scope, f.capability); err != nil || len(outstanding) != 0 {
				t.Fatal("denied request created a dispatch guard", outstanding, err)
			}
		})
	}
}

func TestNativeResolveRequiresEstablishedPreparedContext(t *testing.T) {
	f := newFixture(t)
	f.opts.NativeCompaction = true
	f.open()
	pending := f.control("POST", f.prepareExisting(), 200, "")
	_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, nativeReference(f, pending, "initial-history"))
	checkError(t, err, 409, "enrollment_required")
	active := f.control("POST", f.commit(pending, "initial-checkpoint"), 200, "")
	_, err = call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, nativeReference(f, active, "no-transition"))
	checkError(t, err, 409, "transition_required")
	if state := f.read(); state.ActiveNative != 0 || len(f.a.requests) != 0 {
		t.Fatal("native replay enrolled history or prepared implicitly", state)
	}
}

func TestNativeLifecycleRetainsOldTTLAndFencesCommitCancelUntilSettlement(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			f, pending := nativeFixture(t)
			r := nativeReference(f, pending, "native")
			selected := f.resolve(r)
			if selected.Kind != "native_compaction" || selected.TTL != "5m" || selected.ContextEpoch != pending.Active.ContextEpoch || selected.Revision != pending.Revision || f.resolve(r) != selected {
				t.Fatal("native replay did not pin original TTL and policy", selected)
			}
			if state := f.read(); state.ActiveNative != 1 || state.ActiveAttempts != 0 || state.Active != pending.Active || !reflect.DeepEqual(state.Pending, pending.Pending) {
				t.Fatal("native resolve changed context or inflated active counts", state)
			}
			// The logical guard alone blocks transition changes before physical IO.
			f.control("POST", f.commit(pending, "too-early-checkpoint"), 409, "inflight")
			cancel := f.operation("cancel", pending, "1h")
			cancel.TransitionID = pending.Pending.ID
			f.control("POST", cancel, 409, "inflight")
			second := nativeReference(f, pending, "second-native")
			_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, second)
			checkError(t, err, 409, "inflight")
			attempt := f.begin(selected, r.RequestID, "native-attempt")
			if state := f.read(); state.ActiveAttempts != 1 || state.ActiveNative != 1 {
				t.Fatal("physical native attempt lost its managed cache guard", state)
			}
			forged := attempt
			forged.Operation, forged.Policy.TTL = "validate", "1h"
			_, err = call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, forged)
			checkError(t, err, 409, "snapshot_conflict")
			f.validate(attempt)
			f.control("POST", f.commit(pending, "still-too-early"), 409, "inflight")
			checkError(t, f.complete(r.RequestID, pluginapi.RequestCompletionOutcome(outcome)), 409, "inflight")
			if err = f.finish(attempt, outcome, true); err != nil {
				t.Fatal(err)
			}
			if err = f.complete(r.RequestID, pluginapi.RequestCompletionOutcome(outcome)); err != nil {
				t.Fatal(err)
			}
			settled := f.read()
			if settled.ActiveNative != 0 || settled.ActiveAttempts != 0 || settled.Active != pending.Active || !reflect.DeepEqual(settled.Pending, pending.Pending) {
				t.Fatal("native settlement committed policy without a receipt", settled)
			}
			if outcome == "succeeded" {
				committed := f.control("POST", f.commit(settled, "native-checkpoint"), 200, "")
				if committed.Active.TTL != cachepolicy.TTL1h || committed.Pending != nil || committed.ActiveNative != 0 {
					t.Fatal("settled native checkpoint failed to activate target TTL", committed)
				}
			} else {
				cancel = f.operation("cancel", settled, "1h")
				cancel.TransitionID = pending.Pending.ID
				canceled := f.control("POST", cancel, 200, "")
				if canceled.Active != pending.Active || canceled.Pending != nil || canceled.ActiveNative != 0 {
					t.Fatal("failed native replay rewrote existing context", canceled)
				}
			}
			if outstanding, err := f.a.Outstanding(testContext, f.scope, f.capability); err != nil || len(outstanding) != 0 {
				t.Fatal("settled native replay left orphan guards", outstanding, err)
			}
		})
	}
}

func TestNativeResolveCannotChangeCallerOrScopeOnReplay(t *testing.T) {
	for _, change := range []string{"caller", "session", "capability"} {
		t.Run(change, func(t *testing.T) {
			f, pending := nativeFixture(t)
			r := nativeReference(f, pending, "native")
			f.resolve(r)
			before := f.read()
			switch change {
			case "caller":
				r.Scope.CallerScope = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "session":
				r.Scope.SessionID = "foreign-session"
			case "capability":
				r.Capability = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA"
			}
			_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, r)
			checkError(t, err, 403, "scope_denied")
			if !reflect.DeepEqual(f.read(), before) || len(f.a.requests) != 1 {
				t.Fatal("foreign replay changed native ownership")
			}
		})
	}
}
