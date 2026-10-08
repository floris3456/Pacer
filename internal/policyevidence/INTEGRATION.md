# Parent hook instructions (not applied here)

Only `internal/policyevidence` is implemented by this worktree. These independent
snippets describe insertion points in parent-owned code; they are not active hooks.

## 1. Open alongside the existing stores

```go
evidence, err := policyevidence.Open(db, p.now)
if err != nil {
    // Surface optional telemetry unavailability through the parent's diagnostics.
    // No policy/receipt/measurement state is reset or fabricated.
}
p.policyEvidence = evidence
```

Use the caller-owned existing SQLite database and its configured durable settings.
The evidence store does not close the DB or require another connection. A missing
optional evidence store must not change the policy service's authorization or TTL
enforcement behavior.

## 2. Observe only after authoritative native usage persistence succeeds

Insertion point: `internal/pacer/measurement.go:recordUsage`, immediately after the
successful `measurement.PutRecord` call. Use that exact immutable `record` value,
not the session's current policy or a fresh credential lookup.

```go
if _, err := p.measurement.PutRecord(ctx, record); err != nil {
    // Existing persistence failure path: do not call Observe.
    return err
}
if p.policyEvidence != nil {
    if _, err := p.policyEvidence.Observe(ctx, record); err != nil {
        p.metricError(err)
        // Optional projection failure is diagnostic. Keep the already-persisted
        // usage fact; never roll it back or rewrite it with a current policy.
    }
}
```

Calling Observe on an authoritative duplicate is safe and useful if a prior process
persisted the native usage then stopped before updating the optional projection.
Do not create a different event ID on a conflict. This call does not acknowledge
delivery coverage and must not bypass the existing partial-prefix warmth guard.

## 3. Augment successful, already-authorized GET state only

Insertion point: parent wrapper around the existing policy adapter's **control**
RPC response, after it has validated host caller, route and private capability.
Keep resolution/attempt dispatch responses and command POST receipts untouched.

```go
response, err := p.policy.HandleCachePolicy(ctx, request)
if err == nil && request.Method == http.MethodGet &&
    response.StatusCode == http.StatusOK && p.policyEvidence != nil {
    body, evidenceErr := p.policyEvidence.AugmentAuthorized(
        ctx,
        request.Scope.CallerScope, // already host-derived SHA; never hash again
        response.Body,            // canonical authorized State, not request JSON
    )
    if evidenceErr == nil {
        response.Body = body
    } else {
        p.metricError(evidenceErr)
        // Keep the canonical successful response, not a new error/HTTP status.
    }
}
return response, err
```

Adapt member names to the parent wrapper. This package intentionally imports no SDK
and does not own the plugin dispatcher. Do not mount AugmentAuthorized as a public
resource, inference route, or independent session lookup. An API key alone does
not substitute for the private chat capability checked by the existing policy RPC.

**GET-only is intentional:** an idempotent POST retry must return its original
command result/receipt. Adding live telemetry to that body would make the result
depend on callback timing. The client can reconcile with its normal authorized GET
after a command or successful inference. No request/policy hash, checkpoint receipt,
selected TTL, pending transition or warming state is changed by the projection.

## Parent acceptance after hooking

Native acceptance found that CPA's top-level `UsageRecord.SessionID` comes from
client logging/affinity metadata (`helps/usage_helpers.go` and
`pluginhost/adapters_usage_translation.go`), independently of the authenticated
`CachePolicy.Selected.Scope.SessionID`. Managed attribution in `FromNative` uses
the latter. Equality with the logging ID is not a policy authorization rule:
prefixed/generated logging IDs must neither suppress valid readback nor rebind it.
The parent regression uses different logging/policy IDs and still refuses reads
under a foreign policy session or caller. Previously ignored event receipts are
not rewritten; new valid physical evidence is needed for readback.

- Successful native 5m/1h inference then authorized GET shows `last_effective_ttl`.
  Absence of complete prefix proof still leaves cache refresh/warming unknown.
- No usage callback or selected choice alone shows a received/applied TTL.
- A new epoch shows unknown until evidence for that epoch arrives; delayed old
  callbacks and account switches cannot create a refresh timestamp.
- Missing/foreign capability, foreign caller, rejected control RPC and POST retry
  responses take their existing paths without augmentation.
- Callback and GET activity still produce no delivery checkpoint/coverage claims.

This package's tests do not substitute for this parent/native-client acceptance.
