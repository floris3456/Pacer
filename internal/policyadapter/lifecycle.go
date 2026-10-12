package policyadapter

import (
	"context"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func selection(s cachepolicy.Snapshot, caller string) pluginapi.CachePolicy {
	return pluginapi.CachePolicy{Protocol: s.Protocol,
		Scope:        pluginapi.CachePolicyScope{CallerScope: caller, InstallationID: s.InstallationID, SessionID: s.SessionID, ProviderRoute: s.ProviderRoute},
		ContextEpoch: s.ContextEpoch, Revision: s.Revision, TTL: string(s.TTL), Kind: string(s.Kind)}
}

func snapshot(req pluginapi.CachePolicyAttemptRequest) cachepolicy.Snapshot {
	p := req.Policy
	return cachepolicy.Snapshot{Protocol: p.Protocol, Scope: scope(p.Scope), ContextEpoch: p.ContextEpoch, Revision: p.Revision,
		TTL: cachepolicy.TTL(p.TTL), Kind: cachepolicy.Kind(p.Kind), RequestID: req.RequestID, AttemptID: req.AttemptID, Managed: p.Kind == "primary" || p.Kind == "warming" || p.Kind == "native_compaction", StartedAt: req.StartedAt.UTC()}
}

func sameSnapshot(a, b cachepolicy.Snapshot) bool {
	return a.Protocol == b.Protocol && a.Scope == b.Scope && a.ContextEpoch == b.ContextEpoch && a.Revision == b.Revision && a.TTL == b.TTL && a.Kind == b.Kind && a.RequestID == b.RequestID && a.AttemptID == b.AttemptID && a.Managed == b.Managed && a.StartedAt.Equal(b.StartedAt)
}

func (a *Adapter) ResolveCachePolicy(ctx context.Context, req pluginapi.CachePolicyResolveRequest) (pluginapi.CachePolicy, error) {
	auth, err := credentials(req.Scope, req.Capability)
	if err != nil {
		return pluginapi.CachePolicy{}, err
	}
	if req.Protocol != cachepolicy.Protocol || !id(req.ContextEpoch) || req.Revision < 1 || !id(req.RequestID) {
		return pluginapi.CachePolicy{}, reject(400, "invalid", "A complete protocol-1 lifecycle policy reference is required.")
	}
	if (req.Kind != "primary" && req.Kind != "warming" && !(req.Kind == "auxiliary" && a.auxiliary) && !(req.Kind == "native_compaction" && a.native && req.NativeReplayAuthorized && req.SourceFormat == "claude")) || (req.SourceFormat != "claude" && req.SourceFormat != "openai-response") || !a.models[req.Scope.ProviderRoute][req.Model] {
		return pluginapi.CachePolicy{}, reject(422, "unsupported", "The model, transport or request kind is not supported by this host's cache policy integration.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, completed := a.completed[req.RequestID]; completed {
		return pluginapi.CachePolicy{}, reject(409, "request_settled", "This lifecycle request has settled.")
	}
	clean := req
	clean.Capability = ""
	owned := a.requests[req.RequestID]
	if owned != nil {
		if !sameCredentials(owned.auth, auth) || owned.policy.Scope != req.Scope {
			return pluginapi.CachePolicy{}, reject(403, "scope_denied", "Lifecycle scope or capability does not match its owner.")
		}
		if owned.resolve != clean {
			return pluginapi.CachePolicy{}, reject(409, "request_reused", "Lifecycle ID was reused with a different reference, model or transport.")
		}
		if owned.closing != "" || owned.stopped {
			return pluginapi.CachePolicy{}, reject(409, "request_closing", "The lifecycle request is closing; no new dispatch is allowed.")
		}
	}
	state, err := a.store.Get(ctx, auth, scope(req.Scope))
	if err != nil {
		return pluginapi.CachePolicy{}, rpcError(err)
	}
	// Every durable active guard must have a current process-local owner. Restart
	// never silently adopts an old request or clears it to allow fresh dispatch.
	outstanding, err := a.store.Outstanding(ctx, auth, scope(req.Scope))
	if err != nil {
		return pluginapi.CachePolicy{}, rpcError(err)
	}
	for _, s := range outstanding {
		owner := a.requests[s.RequestID]
		if owner == nil || owner.policy.Scope != req.Scope || !sameCredentials(owner.auth, auth) {
			return pluginapi.CachePolicy{}, reject(409, "recovery_required", "Unsettled durable requests require explicit stopped-dispatch recovery.")
		}
		if s.AttemptID != "" {
			p := owner.attempts[s.AttemptID]
			if p == nil || !sameSnapshot(p.snapshot, s) {
				return pluginapi.CachePolicy{}, reject(409, "recovery_required", "An outstanding physical attempt has no current process owner.")
			}
		} else if selection(s, auth.CallerDigest) != owner.policy {
			return pluginapi.CachePolicy{}, reject(503, "unavailable", "Durable logical policy differs from its process-owned selection.")
		}
	}
	ttl := state.Active.TTL
	if owned != nil {
		ttl = owned.referenceTTL
	}
	s, err := a.store.BeginRequest(ctx, auth, cachepolicy.Request{Reference: cachepolicy.Reference{Scope: scope(req.Scope), ContextEpoch: req.ContextEpoch, Revision: req.Revision, TTL: ttl}, RequestID: req.RequestID, Kind: cachepolicy.Kind(req.Kind)})
	if err != nil {
		return pluginapi.CachePolicy{}, rpcError(err)
	}
	p := selection(s, req.Scope.CallerScope)
	if owned == nil {
		a.requests[req.RequestID] = &logical{auth: auth, resolve: clean, policy: p, referenceTTL: ttl, attempts: make(map[string]*physical)}
	}
	return p, nil
}

func (a *Adapter) CachePolicyAttempt(ctx context.Context, req pluginapi.CachePolicyAttemptRequest) (pluginapi.CachePolicyAttemptResponse, error) {
	auth, err := credentials(req.Policy.Scope, req.Capability)
	if err != nil {
		return pluginapi.CachePolicyAttemptResponse{}, err
	}
	if !id(req.RequestID) || !id(req.AttemptID) {
		return pluginapi.CachePolicyAttemptResponse{}, reject(400, "invalid", "Lifecycle and physical attempt IDs are required.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.requests[req.RequestID]
	if l == nil {
		return pluginapi.CachePolicyAttemptResponse{}, reject(409, "request_missing", "No process-owned lifecycle request exists; reconcile durable guards.")
	}
	if !sameCredentials(l.auth, auth) || l.policy.Scope != req.Policy.Scope {
		return pluginapi.CachePolicyAttemptResponse{}, reject(403, "scope_denied", "Attempt credentials do not match the lifecycle owner.")
	}
	if l.policy != req.Policy {
		return pluginapi.CachePolicyAttemptResponse{}, reject(409, "snapshot_conflict", "Attempt policy differs from the immutable resolved selection.")
	}
	attempt := cachepolicy.Attempt{Scope: scope(req.Policy.Scope), RequestID: req.RequestID, AttemptID: req.AttemptID}
	p := l.attempts[req.AttemptID]
	switch req.Operation {
	case "begin":
		if !req.StartedAt.IsZero() || req.Outcome != "" || req.Dispatched {
			return pluginapi.CachePolicyAttemptResponse{}, reject(400, "invalid", "Begin cannot carry dispatch, start or terminal evidence.")
		}
		if l.closing != "" || l.stopped {
			return pluginapi.CachePolicyAttemptResponse{}, reject(409, "request_closing", "The lifecycle request is closing; retries are paused.")
		}
		if p != nil {
			return pluginapi.CachePolicyAttemptResponse{}, reject(409, "attempt_reused", "A physical attempt ID cannot be dispatched twice.")
		}
		s, err := a.store.BeginAttempt(ctx, auth, attempt)
		if err != nil {
			return pluginapi.CachePolicyAttemptResponse{}, rpcError(err)
		}
		if selection(s, auth.CallerDigest) != l.policy {
			return pluginapi.CachePolicyAttemptResponse{}, reject(503, "unavailable", "Durable attempt policy does not match its lifecycle selection.")
		}
		l.attempts[req.AttemptID] = &physical{snapshot: s}
		return pluginapi.CachePolicyAttemptResponse{Policy: l.policy, StartedAt: s.StartedAt}, nil
	case "validate", "finish":
		if p == nil {
			return pluginapi.CachePolicyAttemptResponse{}, reject(409, "attempt_missing", "The physical attempt has not begun in this process.")
		}
		proposed := snapshot(req)
		if req.StartedAt.IsZero() || !sameSnapshot(proposed, p.snapshot) {
			return pluginapi.CachePolicyAttemptResponse{}, reject(409, "snapshot_conflict", "Attempt reference or start differs from its durable snapshot.")
		}
		if req.Operation == "validate" {
			if req.Outcome != "" || req.Dispatched {
				return pluginapi.CachePolicyAttemptResponse{}, reject(400, "invalid", "Validation must precede dispatch and settlement.")
			}
			if l.closing != "" || l.stopped {
				return pluginapi.CachePolicyAttemptResponse{}, reject(409, "request_closing", "Canceled or completed lifecycle work cannot begin upstream dispatch.")
			}
			if err := a.store.ValidateAttempt(ctx, auth, proposed); err != nil {
				return pluginapi.CachePolicyAttemptResponse{}, rpcError(err)
			}
			p.validated = true
			return pluginapi.CachePolicyAttemptResponse{}, nil
		}
		outcome := cachepolicy.Outcome(req.Outcome)
		if !terminal(outcome) || (outcome == cachepolicy.Succeeded && !req.Dispatched) {
			return pluginapi.CachePolicyAttemptResponse{}, reject(400, "invalid", "A precise terminal outcome is required; success requires actual dispatch.")
		}
		if req.Dispatched && !p.validated {
			return pluginapi.CachePolicyAttemptResponse{}, reject(409, "attempt_unvalidated", "A dispatched attempt must have passed final snapshot validation.")
		}
		if p.outcome != "" && (p.outcome != outcome || p.dispatched != req.Dispatched) {
			return pluginapi.CachePolicyAttemptResponse{}, reject(409, "outcome_conflict", "A different physical terminal outcome or dispatch fact is already recorded.")
		}
		settlement := context.WithoutCancel(ctx)
		durable, err := a.store.GetAttempt(settlement, auth, attempt)
		if err != nil {
			return pluginapi.CachePolicyAttemptResponse{}, rpcError(err)
		}
		if !sameSnapshot(proposed, durable) {
			return pluginapi.CachePolicyAttemptResponse{}, reject(409, "snapshot_conflict", "Physical settlement does not match its immutable durable snapshot.")
		}
		if outcome == cachepolicy.Canceled {
			l.stopped = true
		}
		if err = a.store.FinishAttempt(settlement, auth, attempt, outcome); err != nil {
			return pluginapi.CachePolicyAttemptResponse{}, rpcError(err)
		}
		p.outcome, p.dispatched = outcome, req.Dispatched
		return pluginapi.CachePolicyAttemptResponse{}, nil
	default:
		return pluginapi.CachePolicyAttemptResponse{}, reject(400, "invalid", "Attempt operation must be begin, validate or finish.")
	}
}

func terminal(o cachepolicy.Outcome) bool {
	return o == cachepolicy.Succeeded || o == cachepolicy.Failed || o == cachepolicy.Canceled
}

// CompleteRequest reports whether this process owns (or already settled) the
// logical request. Unmanaged/unknown completions are no-ops; they cannot clear an
// orphan guard after restart. Failed settlement retains credentials and blocks retries.
func (a *Adapter) CompleteRequest(ctx context.Context, req pluginapi.RequestCompletion) (bool, error) {
	if !id(req.RequestID) {
		return false, reject(400, "invalid", "A lifecycle completion ID is required.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.requests[req.RequestID]
	previous, completed := a.completed[req.RequestID]
	if l == nil && !completed {
		return false, nil
	}
	outcome := cachepolicy.Outcome(req.Outcome)
	if req.Outcome == pluginapi.RequestCompletionRejected {
		outcome = cachepolicy.Failed
	}
	if !terminal(outcome) {
		return true, reject(400, "invalid", "Unknown logical completion outcome.")
	}
	if completed {
		if previous != outcome {
			return true, reject(409, "outcome_conflict", "A different logical terminal outcome is already recorded.")
		}
		return true, nil
	}
	if l.closing != "" && l.closing != outcome {
		return true, reject(409, "outcome_conflict", "A different logical completion is already pending.")
	}
	l.closing = outcome
	if outcome == cachepolicy.Succeeded {
		succeeded := false
		for _, p := range l.attempts {
			succeeded = succeeded || p.outcome == cachepolicy.Succeeded && p.dispatched
		}
		if !succeeded {
			return true, reject(409, "inflight", "Successful completion requires a settled successful upstream attempt.")
		}
	}
	if err := a.store.FinishRequest(context.WithoutCancel(ctx), l.auth, scope(l.policy.Scope), req.RequestID, outcome); err != nil {
		return true, rpcError(err)
	}
	delete(a.requests, req.RequestID) // Drop private capability only after durable settlement.
	a.completed[req.RequestID] = outcome
	return true, nil
}

func (a *Adapter) HandleRequestComplete(ctx context.Context, req pluginapi.RequestCompletion) error {
	_, err := a.CompleteRequest(ctx, req)
	return err
}

// Outstanding is a host-local, capability-authorized recovery inventory, not an RPC.
func (a *Adapter) Outstanding(ctx context.Context, s pluginapi.CachePolicyScope, capability string) ([]cachepolicy.Snapshot, error) {
	auth, err := credentials(s, capability)
	if err != nil {
		return nil, err
	}
	items, err := a.store.Outstanding(ctx, auth, scope(s))
	return items, rpcError(err)
}

// FinishStoppedRequest is ONLY for a trusted host recovery controller that has
// positively established that every physical dispatch for this logical ID stopped.
// It is not exposed by Dispatch, and cannot infer success, expiry or compaction.
func (a *Adapter) FinishStoppedRequest(ctx context.Context, s pluginapi.CachePolicyScope, capability, requestID string, outcome cachepolicy.Outcome) error {
	auth, err := credentials(s, capability)
	if err != nil {
		return err
	}
	if !id(requestID) || (outcome != cachepolicy.Failed && outcome != cachepolicy.Canceled) {
		return reject(400, "invalid", "Stopped-dispatch recovery requires a logical ID and failed/canceled outcome.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if l := a.requests[requestID]; l != nil {
		if !sameCredentials(l.auth, auth) || l.policy.Scope != s {
			return reject(403, "scope_denied", "Recovery scope does not own the lifecycle ID.")
		}
		if l.closing != "" && l.closing != outcome {
			return reject(409, "outcome_conflict", "A different logical completion is already pending.")
		}
		l.closing = outcome
	}
	settlement := context.WithoutCancel(ctx)
	items, err := a.store.Outstanding(settlement, auth, scope(s))
	if err != nil {
		return rpcError(err)
	}
	for _, snap := range items {
		if snap.RequestID == requestID && snap.AttemptID != "" {
			if err = a.store.FinishAttempt(settlement, auth, cachepolicy.Attempt{Scope: scope(s), RequestID: requestID, AttemptID: snap.AttemptID}, outcome); err != nil {
				return rpcError(err)
			}
		}
	}
	if err = a.store.FinishRequest(settlement, auth, scope(s), requestID, outcome); err != nil {
		return rpcError(err)
	}
	delete(a.requests, requestID)
	a.completed[requestID] = outcome
	return nil
}
