// Package prefixevidence turns physical prefix-cache facts into immutable
// per-attempt observations. A fact is the Pacer view of one dispatch (its
// final-wire prefix snapshot, selected policy scope and cache-creation
// counters); a settlement is the terminal CPA physical outcome of the same
// attempt. Only a fact joined with a succeeded settlement can qualify as a
// named-entry estimate.
//
// The package is optional telemetry. It never authorizes, reroutes or alters a
// request, never reads prompt or response content, and never proves cache
// residency, full-prefix coverage or warming readiness. Its readback adds one
// object to an authorized policy state and leaves every other member byte-for-byte
// unchanged.
package prefixevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidRecord = errors.New("prefixevidence: invalid record identity or scope")
	ErrInvalidState  = errors.New("prefixevidence: invalid authorized state")
	ErrConflict      = errors.New("prefixevidence: immutable record conflict")
	ErrUnavailable   = errors.New("prefixevidence: optional evidence unavailable")
)

// Endpoint is one explicit cache marker in the final wire body.
type Endpoint struct {
	Ordinal int
	Section string // tools | system | messages
	TTL     string // 5m | 1h
	Tag     string // 64 hex; only a 32-hex prefix or a hashed ref ever leaves the store
}

// Snapshot is the Pacer copy of the CPA final-wire prefix snapshot.
type Snapshot struct {
	Version     int
	KeyID       string
	SettingsTag string
	Endpoints   []Endpoint
	Automatic   bool
	Shape       string // text_single_pass | unsupported
	Reasons     []string
}

// Fact is the Pacer side of one physical attempt. Its AttemptID is the CPA
// usage RequestID. Session is the selected policy scope value; it is a scope
// key, never compared with any logging session.
type Fact struct {
	AttemptID, Account, BindingRef              string
	Caller, Installation, Session, Route, Epoch string
	Revision                                    int64
	SelectedTTL, Kind                           string // Kind: primary | warming | other
	Dispatched                                  bool
	StartedAt                                   time.Time // validated upstream start (t0)
	Write5m, Write1h                            *int64    // nil = absent, 0 = zero
	Snapshot                                    *Snapshot
}

// Kind values accepted in Fact.Kind. Any other kind qualifies as unsupported_kind.
const (
	KindPrimary = "primary"
	KindWarming = "warming"
)

// Outcome is the terminal physical outcome of an attempt.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeCanceled  Outcome = "canceled"
)

// Observation is the pure qualification result for one attempt. Until is set
// only when Status is observed_partial and is always start+TTL.
type Observation struct {
	Status   string // observed_partial | unverified | uncertain
	Reason   string // one fixed code; empty when observed_partial
	Relation string // primary_anchor | matches_primary_anchor; empty unless observed
	TTL      string // 5m | 1h; empty unless observed
	Start    time.Time
	Until    time.Time
}

const (
	statusObserved   = "observed_partial"
	statusUnverified = "unverified"
	statusUncertain  = "uncertain"

	relationPrimary = "primary_anchor"
	relationMatched = "matches_primary_anchor"

	// shapeSingle is the only snapshot shape that can qualify (CPA S1).
	shapeSingle = "text_single_pass"
	// futureSkew bounds how far a fact's start may lead its collection clock.
	futureSkew = 60 * time.Second
)

// Fixed reason codes. These are the only reason strings that leave the package.
const (
	reasonNoSnapshot        = "no_snapshot"
	reasonNotDispatched     = "not_dispatched"
	reasonFutureEvidence    = "future_evidence"
	reasonUnsupportedShape  = "unsupported_shape"
	reasonAutomatic         = "automatic_marker"
	reasonNoEndpoint        = "no_endpoint"
	reasonMultiple          = "multiple_candidates"
	reasonTTLMismatch       = "ttl_mismatch"
	reasonSplitMissing      = "ttl_split_missing"
	reasonConflictingWrite  = "conflicting_write"
	reasonNoWrite           = "no_write"
	reasonPending           = "pending_settlement"
	reasonNotSucceeded      = "not_succeeded"
	reasonNoAnchor          = "no_primary_anchor"
	reasonUnsupportedKind   = "unsupported_kind"
	reasonConflicting       = "conflicting_evidence"
	reasonPendingOverflow   = "pending_overflow"
	reasonSettlementTimeout = "settlement_timeout"
	reasonEstimateElapsed   = "estimate_elapsed"
)

// Qualify applies the ordered checks of the implementation spec (S3) to one
// fact, its settlement (nil = not yet settled) and whether a matching primary
// anchor exists. The first failing check wins. It is pure: it reads no clock
// and no store, and its until is always start+TTL.
func Qualify(f Fact, o *Outcome, anchor bool, now time.Time) Observation {
	out := Observation{Status: statusUnverified, Start: f.StartedAt}
	relation, reason := qualify(f, o, anchor, now)
	if reason != "" {
		out.Reason = reason
		return out
	}
	out.Status = statusObserved
	out.Relation = relation
	out.TTL = f.SelectedTTL
	out.Until = f.StartedAt.Add(ttlDuration(f.SelectedTTL))
	return out
}

// qualify returns the relation for a success, or the first failing reason.
func qualify(f Fact, o *Outcome, anchor bool, now time.Time) (relation, reason string) {
	s := f.Snapshot
	switch {
	case s == nil:
		return "", reasonNoSnapshot
	case !f.Dispatched || f.StartedAt.IsZero():
		return "", reasonNotDispatched
	case f.StartedAt.After(now.Add(futureSkew)):
		return "", reasonFutureEvidence
	case s.Shape != shapeSingle:
		return "", reasonUnsupportedShape
	case s.Automatic:
		return "", reasonAutomatic
	case len(s.Endpoints) == 0:
		return "", reasonNoEndpoint
	case len(s.Endpoints) > 1:
		return "", reasonMultiple
	}
	// Exactly one endpoint: it is the candidate, never a choice among several.
	ep := s.Endpoints[0]
	if !validTTL(f.SelectedTTL) || ep.TTL != f.SelectedTTL {
		return "", reasonTTLMismatch
	}
	own, other := f.Write5m, f.Write1h
	if f.SelectedTTL == "1h" {
		own, other = f.Write1h, f.Write5m
	}
	switch {
	case own == nil:
		return "", reasonSplitMissing
	case other != nil && *other > 0:
		return "", reasonConflictingWrite
	case *own <= 0:
		return "", reasonNoWrite
	}
	if o == nil {
		return "", reasonPending
	}
	if *o != OutcomeSucceeded {
		return "", reasonNotSucceeded
	}
	switch f.Kind {
	case KindPrimary:
		return relationPrimary, ""
	case KindWarming:
		if !anchor {
			return "", reasonNoAnchor
		}
		return relationMatched, ""
	default:
		return "", reasonUnsupportedKind
	}
}

func ttlDuration(ttl string) time.Duration {
	if ttl == "1h" {
		return time.Hour
	}
	return 5 * time.Minute
}

func validTTL(ttl string) bool { return ttl == "5m" || ttl == "1h" }

// scope is the caller-scoped key shared by facts, anchors and readback.
type scope struct {
	Caller, Installation, Session, Route, Epoch string
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

func validTime(t time.Time) bool {
	return t.UnixNano() > 0 && time.Unix(0, t.UnixNano()).Equal(t)
}

// Domain-separated hashed refs. Raw attempt ids, bindings and tags never leave
// the store; readback carries only these 32-hex refs or a 32-hex tag prefix.
const (
	attemptDomain = "prefix-evidence/attempt-v1\x00"
	bindingDomain = "prefix-evidence/binding-v1\x00"
	entryDomain   = "prefix-evidence/entry-v1\x00"
)

func hashRef(domain, v string) string {
	sum := sha256.Sum256([]byte(domain + v))
	return hex.EncodeToString(sum[:])[:32]
}

// entryRef is the first 32 hex of the endpoint tag. A tag that is not already
// lowercase hex of that length is hashed instead, so output is always 32 hex.
func entryRef(tag string) string {
	if len(tag) >= 32 && lowerHex(tag[:32]) {
		return tag[:32]
	}
	return hashRef(entryDomain, tag)
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
