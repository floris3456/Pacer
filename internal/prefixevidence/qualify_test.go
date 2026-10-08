package prefixevidence

import (
	"strings"
	"testing"
	"time"
)

var (
	t0     = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	caller = strings.Repeat("c", 64)
)

func ptr(v int64) *int64 { return &v }

func hex64(c byte) string { return strings.Repeat(string(c), 64) }

func outcomeOf(o Outcome) *Outcome { return &o }

// baseFact is a fully qualifying single-endpoint dispatch: every check passes
// and the counter for ttl is 1. Individual tests mutate one field at a time.
func baseFact(kind, ttl string) Fact {
	f := Fact{
		AttemptID: "att-1", Account: "acct-1", BindingRef: "bind-1",
		Caller: caller, Installation: "inst-1", Session: "sess-1", Route: "route-1", Epoch: "epoch-1",
		Revision: 7, SelectedTTL: ttl, Kind: kind, Dispatched: true, StartedAt: t0,
		Snapshot: &Snapshot{
			Version: 1, KeyID: "0123456789abcdef", SettingsTag: hex64('e'), Shape: "text_single_pass",
			Endpoints: []Endpoint{{Ordinal: 0, Section: "messages", TTL: ttl, Tag: hex64('d')}},
		},
	}
	if ttl == "1h" {
		f.Write1h = ptr(1)
	} else {
		f.Write5m = ptr(1)
	}
	return f
}

// Item 1: sole endpoint plus a positive write gives a named-entry estimate whose
// until is start+TTL, for both TTLs.
func TestQualifySoleEndpointWriteObservesPartial(t *testing.T) {
	for _, tc := range []struct {
		ttl   string
		until time.Duration
	}{{"5m", 5 * time.Minute}, {"1h", time.Hour}} {
		f := baseFact(KindPrimary, tc.ttl)
		got := Qualify(f, outcomeOf(OutcomeSucceeded), false, t0.Add(time.Second))
		if got.Status != "observed_partial" || got.Reason != "" {
			t.Fatalf("%s: status=%q reason=%q, want observed_partial with no reason", tc.ttl, got.Status, got.Reason)
		}
		if got.Relation != "primary_anchor" || got.TTL != tc.ttl {
			t.Fatalf("%s: relation=%q ttl=%q", tc.ttl, got.Relation, got.TTL)
		}
		if want := t0.Add(tc.until); !got.Until.Equal(want) {
			t.Fatalf("%s: until=%v, want start+TTL=%v", tc.ttl, got.Until, want)
		}
	}
}

// The estimate never extends to settlement time: qualifying long after t0+TTL
// still reports until=t0+TTL.
func TestQualifyUntilIsStartPlusTTLNotSettlement(t *testing.T) {
	f := baseFact(KindPrimary, "5m")
	got := Qualify(f, outcomeOf(OutcomeSucceeded), false, t0.Add(10*time.Hour))
	if got.Status != "observed_partial" {
		t.Fatalf("status=%q reason=%q, want observed_partial", got.Status, got.Reason)
	}
	if want := t0.Add(5 * time.Minute); !got.Until.Equal(want) {
		t.Fatalf("until=%v, want %v", got.Until, want)
	}
}

// Item 1: each failing check yields its fixed reason code (asserted, not just status).
func TestQualifyReasonCodes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Fact)
		o      *Outcome
		anchor bool
		now    time.Time
		status string
		reason string
	}{
		{"no snapshot", func(f *Fact) { f.Snapshot = nil }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "no_snapshot"},
		{"not dispatched", func(f *Fact) { f.Dispatched = false }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "not_dispatched"},
		{"zero start", func(f *Fact) { f.StartedAt = time.Time{} }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "not_dispatched"},
		{"future start beyond 60s", func(f *Fact) { f.StartedAt = t0.Add(61 * time.Second) }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "future_evidence"},
		{"unsupported shape", func(f *Fact) { f.Snapshot.Shape = "unsupported" }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "unsupported_shape"},
		{"automatic marker", func(f *Fact) { f.Snapshot.Automatic = true }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "automatic_marker"},
		{"no endpoint", func(f *Fact) { f.Snapshot.Endpoints = nil }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "no_endpoint"},
		{"ttl mismatch", func(f *Fact) { f.Snapshot.Endpoints[0].TTL = "1h" }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "ttl_mismatch"},
		{"ttl split missing", func(f *Fact) { f.Write5m = nil }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "ttl_split_missing"},
		{"other ttl write", func(f *Fact) { f.Write1h = ptr(1) }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "conflicting_write"},
		{"zero cache write", func(f *Fact) { f.Write5m = ptr(0) }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "no_write"},
		{"read-only hit (write 0)", func(f *Fact) { f.Write5m = ptr(0) }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "no_write"},
		{"not yet settled", func(f *Fact) {}, nil, false, t0, "unverified", "pending_settlement"},
		{"failed settlement", func(f *Fact) {}, outcomeOf(OutcomeFailed), false, t0, "unverified", "not_succeeded"},
		{"canceled settlement", func(f *Fact) {}, outcomeOf(OutcomeCanceled), false, t0, "unverified", "not_succeeded"},
		{"unsupported kind", func(f *Fact) { f.Kind = "other" }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "unsupported_kind"},
		{"warming without anchor", func(f *Fact) { f.Kind = KindWarming }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "no_primary_anchor"},
		{"multiple candidates", func(f *Fact) {
			f.Snapshot.Endpoints = append(f.Snapshot.Endpoints, Endpoint{Ordinal: 1, Section: "system", TTL: "5m", Tag: hex64('f')})
		}, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "multiple_candidates"},
		// First failing check wins: dispatch fails before the counter is examined.
		{"first failing check wins", func(f *Fact) { f.Dispatched = false; f.Write5m = nil }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "not_dispatched"},
		// Shape is checked before the automatic marker (S3 order).
		{"automatic with unsupported shape", func(f *Fact) { f.Snapshot.Automatic = true; f.Snapshot.Shape = "unsupported" }, outcomeOf(OutcomeSucceeded), false, t0, "unverified", "unsupported_shape"},
		// Relation: warming with a matching anchor observes; the anchor is required.
		{"warming with anchor", func(f *Fact) { f.Kind = KindWarming }, outcomeOf(OutcomeSucceeded), true, t0, "observed_partial", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := baseFact(KindPrimary, "5m")
			tc.mutate(&f)
			got := Qualify(f, tc.o, tc.anchor, tc.now)
			if got.Status != tc.status || got.Reason != tc.reason {
				t.Fatalf("status=%q reason=%q, want status=%q reason=%q", got.Status, got.Reason, tc.status, tc.reason)
			}
		})
	}
}

// Relation for a warming observation is the matching-anchor relation.
func TestQualifyWarmingRelation(t *testing.T) {
	f := baseFact(KindWarming, "5m")
	got := Qualify(f, outcomeOf(OutcomeSucceeded), true, t0)
	if got.Status != "observed_partial" || got.Relation != "matches_primary_anchor" {
		t.Fatalf("status=%q relation=%q, want observed_partial/matches_primary_anchor", got.Status, got.Relation)
	}
}

// Item 1 mutation guard: a variant that picks the last endpoint of a
// multi-endpoint snapshot would observe here (both endpoints match the TTL and
// the write). The correct behaviour is multiple_candidates, asserted by reason.
func TestMultipleEndpointsNeverPickOne(t *testing.T) {
	f := baseFact(KindPrimary, "5m")
	f.Snapshot.Endpoints = []Endpoint{
		{Ordinal: 0, Section: "tools", TTL: "5m", Tag: hex64('a')},
		{Ordinal: 1, Section: "messages", TTL: "5m", Tag: hex64('b')},
	}
	got := Qualify(f, outcomeOf(OutcomeSucceeded), false, t0)
	if got.Status != "unverified" || got.Reason != "multiple_candidates" {
		t.Fatalf("status=%q reason=%q, want unverified/multiple_candidates", got.Status, got.Reason)
	}
	if !got.Until.IsZero() || got.TTL != "" {
		t.Fatalf("non-observed result must carry no ttl/until: ttl=%q until=%v", got.TTL, got.Until)
	}
}

// Item 5: Session is a scope key only. Changing it never changes qualification.
func TestQualifySessionIsNotAnInput(t *testing.T) {
	a := baseFact(KindPrimary, "5m")
	b := baseFact(KindPrimary, "5m")
	b.Session = "some-other-session"
	ga := Qualify(a, outcomeOf(OutcomeSucceeded), false, t0)
	gb := Qualify(b, outcomeOf(OutcomeSucceeded), false, t0)
	if ga != gb {
		t.Fatalf("session changed qualification: %+v vs %+v", ga, gb)
	}
}
