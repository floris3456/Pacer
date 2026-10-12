package policyadapter

import (
	"context"
	"reflect"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func auxiliaryFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.opts.AuxiliaryNoCache = true
	f.open()
	return f
}

func (f *fixture) auxiliary(state cachepolicy.State, id string) (pluginapi.CachePolicyResolveRequest, pluginapi.CachePolicy) {
	f.t.Helper()
	r := f.reference(state, id)
	r.Kind = "auxiliary"
	selected := f.resolve(r)
	if selected.Kind != "auxiliary" || selected.TTL != "" || selected.ContextEpoch != state.Active.ContextEpoch || selected.Revision != state.Revision {
		f.t.Fatal("auxiliary inherited a cache write policy", selected)
	}
	return r, selected
}

func (f *fixture) assertAuxiliaryAttempt(r pluginapi.CachePolicyAttemptRequest) {
	f.t.Helper()
	auth, err := credentials(f.scope, f.capability)
	if err != nil {
		f.t.Fatal(err)
	}
	snap, err := f.a.store.GetAttempt(testContext, auth, cachepolicy.Attempt{Scope: scope(f.scope), RequestID: r.RequestID, AttemptID: r.AttemptID})
	if err != nil || snap.Managed || snap.TTL != "" || snap.Kind != cachepolicy.Auxiliary || snap.ContextEpoch != r.Policy.ContextEpoch || !snap.StartedAt.Equal(r.StartedAt) {
		f.t.Fatal("durable auxiliary accounting lost its no-cache selection", snap, err)
	}
}

func TestAuxiliaryExistingHistoryEnrollmentSettlesBeforeClientCommitOrCancel(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			f := auxiliaryFixture(t)
			pending := f.control("POST", f.prepareExisting(), 200, "")
			r, selected := f.auxiliary(pending, "compaction-helper")
			if again := f.resolve(r); again != selected {
				t.Fatal("lost resolve receipt changed policy", again)
			}
			if state := f.read(); state.ActiveAuxiliary != 1 || state.ActivePrimary != 0 || state.ActiveWarming != 0 || state.ActiveAttempts != 0 || state.Active != pending.Active || !reflect.DeepEqual(state.Pending, pending.Pending) {
				t.Fatal("helper changed primary policy or duplicate counts", state)
			}
			primary := f.reference(pending, "must-still-wait")
			_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, primary)
			checkError(t, err, 409, "enrollment_required")
			attempt := f.begin(selected, r.RequestID, "helper-attempt")
			f.assertAuxiliaryAttempt(attempt)
			if state := f.read(); state.ActiveAttempts != 0 || state.ActiveAuxiliary != 1 {
				t.Fatal("auxiliary was counted as a managed cache attempt", state)
			}
			for _, change := range []string{"ttl", "kind"} {
				forged := attempt
				forged.Operation = "validate"
				if change == "ttl" {
					forged.Policy.TTL = "1h"
				} else {
					forged.Policy.Kind = "primary"
				}
				_, err = call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, forged)
				checkError(t, err, 409, "snapshot_conflict")
			}
			f.validate(attempt)
			logicalOutcome := pluginapi.RequestCompletionOutcome(outcome)
			checkError(t, f.complete(r.RequestID, logicalOutcome), 409, "inflight")
			// Host cancellation must still durably settle the real stopped helper.
			ctx, cancel := context.WithCancel(testContext)
			cancel()
			finish := attempt
			finish.Operation, finish.Outcome, finish.Dispatched = "finish", outcome, true
			if _, err = call[pluginapi.CachePolicyAttemptResponse](ctx, f.a, pluginabi.MethodCachePolicyAttempt, finish); err != nil {
				t.Fatal("canceled context prevented settlement", err)
			}
			if err = f.finish(attempt, outcome, true); err != nil {
				t.Fatal("physical settlement was not idempotent", err)
			}
			if err = f.complete(r.RequestID, logicalOutcome); err != nil {
				t.Fatal(err)
			}
			if state := f.read(); state.ActiveAuxiliary != 0 || state.Active != pending.Active || state.Pending == nil || !state.EnrollmentRequired {
				t.Fatal("helper settlement published a checkpoint", state)
			}
			if outcome == "succeeded" {
				commit := f.commit(pending, "new-durable-checkpoint")
				active := f.control("POST", commit, 200, "")
				f.restart()
				if got := f.control("POST", commit, 200, ""); !reflect.DeepEqual(got, active) || got.Active.Checkpoint != "new-durable-checkpoint" || got.EnrollmentRequired {
					t.Fatal("durable checkpoint acknowledgement lost on restart", got)
				}
			} else {
				cancel := f.operation("cancel", pending, string(pending.Pending.TTL))
				cancel.TransitionID = pending.Pending.ID
				canceled := f.control("POST", cancel, 200, "")
				f.restart()
				if got := f.read(); !reflect.DeepEqual(got, canceled) || !got.EnrollmentRequired || got.Active.TTL != "" || got.Active.Checkpoint != pending.Active.Checkpoint {
					t.Fatal("failed helper changed the durable checkpoint or TTL", got)
				}
				// A later retry needs a fresh transition and a newly settled helper.
				retryPending := f.control("POST", f.operation("prepare", canceled, "1h"), 200, "")
				_, retryPolicy := f.auxiliary(retryPending, "retry-helper")
				retryAttempt := f.begin(retryPolicy, "retry-helper", "retry-attempt")
				f.validate(retryAttempt)
				if err = f.finish(retryAttempt, "succeeded", true); err != nil {
					t.Fatal(err)
				}
				if err = f.complete("retry-helper", pluginapi.RequestCompletionSucceeded); err != nil {
					t.Fatal(err)
				}
				active := f.control("POST", f.commit(retryPending, "recovered-checkpoint"), 200, "")
				f.restart()
				if got := f.read(); !reflect.DeepEqual(got, active) || got.Active.TTL != cachepolicy.TTL1h || got.EnrollmentRequired {
					t.Fatal("retry checkpoint was not durable", got)
				}
			}
			f.assertAuxiliaryAttempt(attempt)
			if outstanding, err := f.a.Outstanding(testContext, f.scope, f.capability); err != nil || len(outstanding) != 0 {
				t.Fatal("settled helper left active guards", outstanding, err)
			}
		})
	}
}

func TestAuxiliaryRequiresTrustedHostAndReplaysOriginalAdmissionReference(t *testing.T) {
	f := newFixture(t)
	active := f.enroll()
	r := f.reference(active, "helper")
	r.Kind = "auxiliary"
	_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, r)
	checkError(t, err, 422, "unsupported")
	if state := f.read(); state.ActiveAuxiliary != 0 {
		t.Fatal("unsupported host acquired an auxiliary guard", state)
	}
	f.opts.AuxiliaryNoCache = true
	f.open()
	for _, change := range []string{"model", "format", "kind"} {
		unsupported := r
		switch change {
		case "model":
			unsupported.Model = "claude-haiku-4-5"
		case "format":
			unsupported.SourceFormat = "websocket"
		case "kind":
			unsupported.Kind = "compaction"
		}
		_, err = call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, unsupported)
		checkError(t, err, 422, "unsupported")
	}
	selected := f.resolve(r)
	if selected.TTL != "" || f.resolve(r) != selected || f.read().ActiveAuxiliary != 1 {
		t.Fatal("auxiliary replay reused its empty wire TTL as the active admission TTL")
	}
	pending := f.control("POST", f.operation("prepare", f.read(), "1h"), 200, "")
	if f.resolve(r) != selected {
		t.Fatal("pending transition rewrote an admitted auxiliary")
	}
	// Auxiliary work does not block a separately proven durable checkpoint.
	// Its snapshot remains no-cache even after another helper commits a new TTL.
	f.control("POST", f.commit(pending, "another-checkpoint"), 200, "")
	if f.resolve(r) != selected {
		t.Fatal("new active TTL changed the original auxiliary admission fingerprint")
	}
	attempt := f.begin(selected, r.RequestID, "late-helper-attempt")
	f.validate(attempt)
	f.assertAuxiliaryAttempt(attempt)
	if err = f.finish(attempt, "succeeded", true); err != nil {
		t.Fatal(err)
	}
	if err = f.complete(r.RequestID, pluginapi.RequestCompletionSucceeded); err != nil {
		t.Fatal(err)
	}
}

func TestAuxiliaryRestartRetainsGuardUntilStoppedDispatchRecovery(t *testing.T) {
	f := auxiliaryFixture(t)
	pending := f.control("POST", f.prepareExisting(), 200, "")
	r, selected := f.auxiliary(pending, "orphan-helper")
	attempt := f.begin(selected, r.RequestID, "orphan-attempt")
	f.validate(attempt)
	f.restart()
	next := r
	next.RequestID = "replacement-helper"
	_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, next)
	checkError(t, err, 409, "recovery_required")
	if err = f.complete(r.RequestID, pluginapi.RequestCompletionCanceled); err != nil || f.read().ActiveAuxiliary != 1 {
		t.Fatal("unknown completion released an orphan guard", err)
	}
	if err = f.a.FinishStoppedRequest(testContext, f.scope, f.capability, r.RequestID, cachepolicy.Canceled); err != nil {
		t.Fatal(err)
	}
	if state := f.read(); state.ActiveAuxiliary != 0 || !state.EnrollmentRequired || state.Pending == nil {
		t.Fatal("recovery erased the enrollment transition", state)
	}
	if selected := f.resolve(next); selected.TTL != "" || selected.Kind != "auxiliary" {
		t.Fatal("recovered scope could not admit a new helper", selected)
	}
}
