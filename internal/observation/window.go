// Package observation records provider quota evidence and budgets sparse analytics
// requests. It performs no provider I/O and never derives quota from token usage.
package observation

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	Measured       = "measured"
	Absolute       = "absolute"
	Estimated      = "estimated"
	HeaderSource   = "header"
	APISource      = "api"
	EventSource    = "event"
	CycleTolerance = 2 * time.Second
	// Codex can correct the same future deadline by several seconds while the
	// balance keeps falling. Bound corrections to the cycle's original anchor.
	CodexResetCorrection = time.Minute
)

// Field describes the source of one value, not the time its callback finished.
// Raw preserves the provider's scalar, including fractional timestamp precision.
type Field struct {
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
	Quality string    `json:"quality"`
	Raw     string    `json:"raw,omitempty"`
}

type Window struct {
	Provider                 string    `json:"provider"`
	Scope                    string    `json:"scope"`
	Seconds                  int64     `json:"seconds"`
	Remaining                *float64  `json:"remaining"`
	Reset                    time.Time `json:"reset"`
	Usage                    Field     `json:"usage"`
	ResetEvidence            Field     `json:"reset_evidence"`
	StatusEvidence           Field     `json:"status_evidence"`
	NotApplicable            bool      `json:"not_applicable,omitempty"`
	Dormant                  bool      `json:"dormant,omitempty"`
	Cycle                    string    `json:"cycle,omitempty"`
	CycleAt                  time.Time `json:"cycle_at"`
	Conflict                 bool      `json:"conflict,omitempty"`
	Ambiguous                bool      `json:"ambiguous,omitempty"`
	ConflictAt               time.Time `json:"conflict_at"`
	ConflictingReset         time.Time `json:"conflicting_reset"`
	ConflictingResetEvidence Field     `json:"conflicting_reset_evidence"`
}

func (w Window) Key() string {
	return w.Provider + ":" + w.Scope + ":" + strconv.FormatInt(w.Seconds, 10)
}

// Applies is deliberately conservative for unknown named limits. Spark and the
// provider's Bengalfox namespace describe the same verified model family.
// Non-model features (code review, overage) never gate ordinary inference.
func (w Window) Applies(model string) bool {
	if w.NotApplicable {
		return false
	}
	switch w.Scope {
	case "all", "oauth":
		return true
	case "opus", "sonnet", "haiku", "spark":
		return strings.Contains(strings.ToLower(model), w.Scope)
	default:
		return strings.HasPrefix(w.Scope, "model:") && strings.EqualFold(strings.TrimPrefix(w.Scope, "model:"), model)
	}
}

func (w Window) ObservedAt() time.Time {
	return latest(w.Usage.At, w.ResetEvidence.At, w.StatusEvidence.At)
}

func age(now, at time.Time) time.Duration {
	if at.IsZero() || at.After(now) {
		return 0
	}
	return now.Sub(at)
}

func (w Window) BalanceAge(now time.Time) time.Duration { return age(now, w.Usage.At) }
func (w Window) ResetAge(now time.Time) time.Duration   { return age(now, w.ResetEvidence.At) }

// State describes evidence, not service availability. Time alone never replenishes
// a balance or changes the source reset. Inspect field ages for routing freshness.
func (w Window) State(now time.Time) string {
	switch {
	case w.Conflict:
		return "conflicting"
	case w.Ambiguous:
		return "ambiguous"
	case w.NotApplicable:
		return "not-applicable"
	case w.Dormant:
		return "dormant"
	case !w.Reset.IsZero() && !w.Reset.After(now):
		return "awaiting-confirmation"
	case w.Remaining == nil && w.Reset.IsZero():
		return "unknown"
	case w.Remaining == nil || w.Reset.IsZero():
		return "partial"
	case w.ResetEvidence.Quality == Estimated:
		return "estimated"
	default:
		return "current"
	}
}

func (w Window) Complete(now time.Time) bool {
	switch w.State(now) {
	case "not-applicable", "dormant", "current", "estimated":
		return true
	default:
		return false
	}
}

// FreshComplete ages balances/status separately; an old but still-future absolute
// reset remains evidence. maxAge is policy, not a made-up provider expiry.
func (w Window) FreshComplete(now time.Time, maxAge time.Duration) bool {
	if !w.Complete(now) {
		return false
	}
	at := w.Usage.At
	if w.NotApplicable || w.Dormant {
		at = w.StatusEvidence.At
	}
	return !at.IsZero() && !at.After(now) && now.Sub(at) < maxAge
}

func (w Window) Valid() bool {
	if w.Scope == "" || w.Seconds <= 0 || w.ObservedAt().IsZero() {
		return false
	}
	if w.Remaining != nil && (math.IsNaN(*w.Remaining) || math.IsInf(*w.Remaining, 0) || *w.Remaining < 0 || *w.Remaining > 100 || w.Usage.At.IsZero()) {
		return false
	}
	if !w.Reset.IsZero() && w.ResetEvidence.At.IsZero() {
		return false
	}
	return w.Remaining != nil || !w.Reset.IsZero() || ((w.NotApplicable || w.Dormant) && !w.StatusEvidence.At.IsZero())
}

func latest(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func sameReset(a, b time.Time) bool {
	return !a.IsZero() && !b.IsZero() && a.Sub(b) <= CycleTolerance && b.Sub(a) <= CycleTolerance
}

func cycle(w Window) Window {
	w.CycleAt = w.ObservedAt()
	if !w.Reset.IsZero() {
		w.Cycle = w.Reset.UTC().Format(time.RFC3339Nano)
	}
	return w
}

func conflict(old, next Window) Window {
	old.Conflict = true
	old.ConflictAt = latest(old.ConflictAt, next.ObservedAt())
	if !next.Reset.IsZero() && !next.ResetEvidence.At.Before(old.ConflictingResetEvidence.At) {
		old.ConflictingReset, old.ConflictingResetEvidence = next.Reset, next.ResetEvidence
	}
	return old
}

// merge returns canonical evidence and an audit disposition. Retired cycles are
// checked by the store before this function. Equal-time contradictions are never
// settled by callback order; only strictly newer complete evidence clears them.
func merge(old, next Window) (Window, string) {
	return mergeWithResetCorrections(old, next, true)
}

// The migration also needs the original merge to verify the persisted audit
// reconstructs the old projection before repairing any retirement markers.
func mergeWithResetCorrections(old, next Window, corrections bool) (Window, string) {
	if !old.Valid() {
		return cycle(next), "accepted"
	}
	at := next.ObservedAt()
	if at.Before(old.CycleAt) {
		return old, "stale-cycle"
	}
	full := next.Remaining != nil && !next.Reset.IsZero() && next.Usage.At.Equal(next.ResetEvidence.At)
	if next.NotApplicable || next.Dormant {
		knownAt := latest(old.ObservedAt(), old.ConflictAt)
		if !next.StatusEvidence.At.After(knownAt) {
			if next.StatusEvidence.At.Equal(knownAt) && (next.NotApplicable != old.NotApplicable || next.Dormant != old.Dormant) {
				return conflict(old, next), "conflict"
			}
			return old, "stale"
		}
		return cycle(next), "accepted-status"
	}
	if old.NotApplicable || old.Dormant {
		if !at.After(latest(old.StatusEvidence.At, old.ConflictAt)) {
			return old, "stale"
		}
		return cycle(next), "accepted-cycle"
	}
	changedReset := !old.Reset.IsZero() && !next.Reset.IsZero() && !sameReset(old.Reset, next.Reset)
	if corrections && codexResetCorrection(old, next) {
		changedReset = false
	}
	if changedReset {
		if next.ResetEvidence.At.Before(latest(old.ObservedAt(), old.ConflictAt)) {
			return old, "stale"
		}
		// An estimate cannot casually revise an absolute deadline. It can
		// establish a labelled new cycle when full evidence shows replenishment
		// with a later reset, or when the preceding cycle has already passed.
		estimatedCorrection := old.ResetEvidence.Quality == Absolute && next.ResetEvidence.Quality == Estimated
		if !old.Reset.After(at) || (full && old.Remaining != nil && *next.Remaining > *old.Remaining && next.Reset.After(old.Reset)) {
			estimatedCorrection = false
		}
		if !full || !next.ObservedAt().After(latest(old.ObservedAt(), old.ConflictAt)) || estimatedCorrection || (!next.Reset.After(at) && old.Reset.After(at)) {
			return conflict(old, next), "conflict"
		}
		// Full, newer, source-backed early-reset evidence replaces the old cycle
		// immediately, without waiting for its previously advertised deadline.
		return cycle(next), "accepted-cycle"
	}
	result := old
	disposition := "stale"
	if full && at.After(latest(old.ObservedAt(), old.ConflictAt)) {
		result.Conflict, result.Ambiguous = false, false
		result.ConflictAt, result.ConflictingReset, result.ConflictingResetEvidence = time.Time{}, time.Time{}, Field{}
	}
	if next.Remaining != nil && !next.Usage.At.Before(old.Usage.At) {
		if old.Remaining != nil && next.Usage.At.Equal(old.Usage.At) && *next.Remaining != *old.Remaining {
			result = conflict(result, next)
			disposition = "conflict"
		} else if next.Usage.At.After(old.Usage.At) {
			if !full && old.Remaining != nil && *next.Remaining > *old.Remaining {
				result.Ambiguous = true
			}
			result.Remaining, result.Usage = next.Remaining, next.Usage
			disposition = "accepted-field"
		}
	}
	if !next.Reset.IsZero() && !next.ResetEvidence.At.Before(old.ResetEvidence.At) {
		if !old.Reset.IsZero() && next.ResetEvidence.At.Equal(old.ResetEvidence.At) && !next.Reset.Equal(old.Reset) {
			result = conflict(result, next)
			disposition = "conflict"
		} else if next.ResetEvidence.At.After(old.ResetEvidence.At) && !(old.ResetEvidence.Quality == Absolute && next.ResetEvidence.Quality == Estimated) {
			result.Reset, result.ResetEvidence = next.Reset, next.ResetEvidence
			if result.Cycle == "" {
				result.Cycle = next.Reset.UTC().Format(time.RFC3339Nano)
			}
			disposition = "accepted-field"
		}
	}
	return result, disposition
}

func codexResetCorrection(old, next Window) bool {
	if old.Provider != "codex" || next.Provider != "codex" || old.Key() != next.Key() ||
		old.Remaining == nil || next.Remaining == nil || *next.Remaining > *old.Remaining ||
		!next.Usage.At.Equal(next.ResetEvidence.At) || !next.Usage.At.After(latest(old.ObservedAt(), old.ConflictAt)) ||
		!old.Reset.After(next.Usage.At) || !next.Reset.After(next.Usage.At) ||
		(old.ResetEvidence.Quality == Absolute && next.ResetEvidence.Quality != Absolute) {
		return false
	}
	anchor := timestamp(old.Cycle)
	if anchor.IsZero() {
		anchor = old.Reset
	}
	return next.Reset.Sub(anchor) <= CodexResetCorrection && anchor.Sub(next.Reset) <= CodexResetCorrection
}
