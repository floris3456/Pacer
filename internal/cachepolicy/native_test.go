package cachepolicy

import "testing"

func TestNativeCompactionRetainsOldCacheAndBlocksTransitionUntilSettled(t *testing.T) {
	f := newFixture(t)
	initial := f.apply(f.initial())
	f.request(initial, "primary", Primary)
	a, _ := f.attempt("primary", "primary-1")
	f.settle(a, Succeeded)
	used := f.get()
	_, err := f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(used), RequestID: "too-early", Kind: NativeCompaction})
	requireCode(t, err, 409, "transition_required")
	pending := f.apply(f.command("prepare", used, TTL1h))
	native := f.request(pending, "native", NativeCompaction)
	if !native.Managed || native.TTL != TTL5m || f.get().ActiveNative != 1 {
		t.Fatal("native dispatch lost the original cache policy", native, f.get())
	}
	physical, snap := f.attempt("native", "native-1")
	if snap.TTL != TTL5m || f.get().ActiveAttempts != 1 {
		t.Fatal("native physical attempt lost its durable guard", snap)
	}
	_, err = f.s.Apply(ctx, f.auth, f.commit(pending, "checkpoint"), Evidence{})
	requireCode(t, err, 409, "inflight")
	cancel := f.command("cancel", pending, pending.Pending.TTL)
	cancel.TransitionID = pending.Pending.ID
	_, err = f.s.Apply(ctx, f.auth, cancel, Evidence{})
	requireCode(t, err, 409, "inflight")
	f.settle(physical, Succeeded)
	settled := f.get()
	if settled.ActiveNative != 0 || settled.ActiveAttempts != 0 || settled.Pending == nil || settled.Active.TTL != TTL5m {
		t.Fatal("native settlement committed or lost the pending transition", settled)
	}
	committed := f.apply(f.commit(settled, "checkpoint"))
	if committed.Active.TTL != TTL1h || committed.ActiveNative != 0 || committed.Pending != nil {
		t.Fatal("checkpoint did not commit the requested policy", committed)
	}
}

func TestNativeCompactionFailureAllowsCancelButNeverRewritesContext(t *testing.T) {
	f := newFixture(t)
	initial := f.apply(f.initial())
	f.request(initial, "primary", Primary)
	a, _ := f.attempt("primary", "primary-1")
	f.settle(a, Succeeded)
	pending := f.apply(f.command("prepare", f.get(), TTL1h))
	f.request(pending, "native", NativeCompaction)
	a, _ = f.attempt("native", "native-1")
	f.settle(a, Failed)
	cancel := f.command("cancel", f.get(), pending.Pending.TTL)
	cancel.TransitionID = pending.Pending.ID
	canceled := f.apply(cancel)
	if canceled.Active.ContextEpoch != initial.Active.ContextEpoch || canceled.Active.TTL != TTL5m || canceled.ActiveNative != 0 || canceled.Pending != nil {
		t.Fatal("failed native request rewrote context", canceled)
	}
}
