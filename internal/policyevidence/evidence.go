// Package policyevidence retains optional, caller-scoped readback of the last
// successful native Claude dispatch's caller/CPA cache-marker TTL. It does not
// authorize requests, alter policy receipts, classify provider writes, or prove
// cache refresh, full-prefix coverage, cache residency, or warming readiness.
package policyevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

var (
	ErrInvalidRecord = errors.New("policyevidence: invalid record identity or scope")
	ErrInvalidState  = errors.New("policyevidence: invalid authorized state")
	ErrConflict      = errors.New("policyevidence: immutable event conflict")
	ErrUnavailable   = errors.New("policyevidence: optional evidence unavailable")
)

type Clock func() time.Time

// ObserveResult describes this ingestion only, never authorizes a request.
type ObserveResult struct {
	Disposition string // recorded | ignored | duplicate
	HeadChanged bool
	Reason      string // Fixed non-secret diagnostic, empty for known marker evidence.
}

type scope struct {
	Caller       string
	Installation string
	Session      string
	Route        string
	Epoch        string
}

func (s scope) args() []any { return []any{s.Caller, s.Installation, s.Session, s.Route, s.Epoch} }
func (s scope) valid() bool {
	return sha(s.Caller) && identifier(s.Installation) && identifier(s.Session) && identifier(s.Route) && identifier(s.Epoch)
}
func identifier(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}
func sha(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func reference(s string) bool {
	return strings.HasPrefix(s, "sha256:") && sha(strings.TrimPrefix(s, "sha256:"))
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func validTime(t time.Time) bool {
	return !t.IsZero() && t.UnixNano() > 0 && time.Unix(0, t.UnixNano()).Equal(t)
}
func ttl(t string) bool { return t == "5m" || t == "1h" }

// event is a minimal, sanitized index into the authoritative measurement store.
// Only the five scope components remain plaintext. Physical IDs and the immutable
// source proof are hashed; account/credential/model/raw usage are never copied.
type event struct {
	scope
	Key, AttemptKey, Fingerprint string
	Start                        int64
	PolicyRevision               int64
	Selected, Effective          string // Only supported TTL values or empty.
	Ordered, Known               bool
	Reason                       string
}

func project(r measurement.Record, now time.Time) (event, error) {
	a, p := r.Attribution, r.Policy
	e := event{scope: scope{a.CallerID, a.InstallationID, a.SessionID, a.ProviderRoute, p.ContextEpoch}}
	if !e.scope.valid() || !identifier(r.EventID) || !identifier(r.SourceID) || !identifier(r.AttemptID) {
		return e, ErrInvalidRecord
	}
	e.Key = hash(r.EventID)
	e.AttemptKey = hash([]string{r.SourceID, r.AttemptID})
	// Hash only immutable proof relevant to this marker readback. Usage enrichment
	// and accounting quality cannot change a dispatch's marker selection. Clock
	// eligibility is deliberately excluded so replay cannot rehabilitate a record
	// first seen with a future/untrusted timestamp merely by waiting.
	proof := struct {
		Event, Source, Attempt, Execution, Logical   string
		Scope                                        scope
		Provider, Client, Model, Account, Credential string
		Policy                                       measurement.PolicySnapshot
		Kind, Basis, EndBasis                        string
		SourceRequested, SourceFinalUsage            time.Time
		Requested, Started, Ended                    time.Time
		Final, Success, HasNative                    bool
		Executor, Binding, NodeKind, NativeSession   string
		Dispatched, Compaction                       bool
		Breakpoints                                  int
		Latency                                      time.Duration
	}{Event: r.EventID, Source: r.SourceID, Attempt: r.AttemptID, Execution: r.ExecutionID, Logical: r.RequestID,
		Scope: e.scope, Provider: a.Provider, Client: a.Client, Model: a.Model, Account: a.AccountID, Credential: a.CredentialRef,
		Policy: p, Kind: r.Kind, Basis: r.StartTimeBasis, EndBasis: r.EndTimeBasis, Requested: r.RequestedAt.UTC(), Started: r.StartedAt.UTC(), Ended: r.EndedAt.UTC(), Final: r.Final, Success: r.Success, HasNative: r.Native != nil}
	proof.Policy.RequestStartedAt = proof.Policy.RequestStartedAt.UTC()
	if n := r.Native; n != nil {
		proof.Executor, proof.Binding, proof.NodeKind = n.ExecutorType, n.CredentialBindingRef, n.NodeKind
		proof.NativeSession = n.SessionID
		proof.Dispatched, proof.Compaction, proof.Breakpoints, proof.Latency = n.Dispatched, n.IsCompaction, n.BreakpointCount, n.Latency
		if source := n.SourceEvidence; source != nil {
			proof.SourceRequested, proof.SourceFinalUsage = source.RequestedAt.UTC(), source.FinalUsageAt.UTC()
		}
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return e, ErrInvalidRecord
	}
	digest := sha256.Sum256(encoded)
	e.Fingerprint = hex.EncodeToString(digest[:])
	ignore := func(reason string) (event, error) { e.Reason = reason; return e, nil }
	if r.Native == nil {
		return ignore("missing_native_evidence")
	}
	n := r.Native
	if a.Provider != "claude" || a.Client != "cpa-native" || n.ExecutorType != "ClaudeExecutor" || !identifier(a.Model) || !strings.HasPrefix(a.Model, "claude-") || strings.ContainsAny(a.Model, "*/\\") {
		return ignore("unsupported_native_source")
	}
	if !identifier(a.AccountID) || !identifier(a.CredentialRef) || !reference(n.CredentialBindingRef) || r.ExecutionID != r.AttemptID || r.Policy.Revision < 1 {
		return ignore("unverified_attempt_identity")
	}
	// CPA UsageRecord.SessionID is client logging/affinity topology (possibly
	// prefixed or generated), not CachePolicy.Selected.Scope.SessionID. The
	// converter takes managed attribution from the authenticated policy scope.
	// Never use the logging ID either to reject that scope or to rebind it.
	if (r.Kind != "primary" && r.Kind != "warming") || p.Kind != r.Kind || n.IsCompaction || n.NodeKind == "auxiliary" || n.NodeKind == "title" || n.NodeKind == "summary" || n.NodeKind == "compaction" || (n.NodeKind == "primary" || n.NodeKind == "warming") && n.NodeKind != r.Kind {
		return ignore("incoherent_request_kind")
	}
	if !r.Final || !r.Success || !n.Dispatched {
		return ignore("no_successful_dispatch")
	}
	endMatches := r.EndTimeBasis == "" && r.RequestedAt.Add(n.Latency).Equal(r.EndedAt)
	if source := n.SourceEvidence; source != nil && r.EndTimeBasis == "source_final_usage_observation" {
		// Source final-usage collection can follow latency capture. It bounds
		// this observed dispatch; it is not a producer-close or billing clock.
		endMatches = source.RequestedAt.Equal(r.RequestedAt) && source.FinalUsageAt.Equal(r.EndedAt) && !r.RequestedAt.Add(n.Latency).After(r.EndedAt)
	}
	if !validTime(now) || r.StartTimeBasis != "upstream" || !validTime(r.StartedAt) || !validTime(r.RequestedAt) || !validTime(r.EndedAt) || !p.RequestStartedAt.Equal(r.StartedAt) || r.RequestedAt.After(r.StartedAt) || r.EndedAt.Before(r.StartedAt) || r.StartedAt.After(now) || r.EndedAt.After(now) || n.Latency < 0 || !endMatches {
		return ignore("unverified_dispatch_clock")
	}
	e.Ordered = true
	e.Start = r.StartedAt.UnixNano()
	e.PolicyRevision = p.Revision
	if ttl(string(p.SelectedTTL)) {
		e.Selected = string(p.SelectedTTL)
	}
	if ttl(string(p.EffectiveTTL)) {
		e.Effective = string(p.EffectiveTTL)
	}
	switch {
	case n.BreakpointCount <= 0:
		e.Reason = "no_caller_cpa_markers"
	case e.Selected == "" || e.Effective == "":
		e.Reason = "unknown_marker_ttl"
	case e.Selected != e.Effective:
		e.Reason = "selected_effective_mismatch"
	default:
		e.Known = true
	}
	return e, nil
}

type head struct {
	Start               int64
	PolicyRevision      int64
	Selected, Effective string
	Known               bool
	Reason              string
}

func (e event) head() head {
	return head{e.Start, e.PolicyRevision, e.Selected, e.Effective, e.Known, e.Reason}
}

func merge(old head, e event) (head, bool) {
	if !e.Ordered || e.Start < old.Start {
		return old, false
	}
	if e.Start > old.Start {
		return e.head(), true
	}
	// Same-clock agreement needs matching scope revision and TTL facts. Unknown
	// or conflicting witnesses at that clock permanently withhold a value at that
	// clock. Another delivery of one agreeing event cannot clear the conflict.
	if old.Known && e.Known && old.Selected == e.Selected && old.Effective == e.Effective && old.PolicyRevision == e.PolicyRevision {
		return old, false
	}
	if old.Reason == "equal_start_conflict" {
		return old, false
	}
	if !old.Known && !e.Known && old.Selected == e.Selected && old.Effective == e.Effective && old.PolicyRevision == e.PolicyRevision && old.Reason == e.Reason {
		return old, false
	}
	return head{Start: old.Start, Reason: "equal_start_conflict"}, true
}
