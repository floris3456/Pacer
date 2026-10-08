package capacity

import (
	"sort"
	"strconv"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

// SettleWindow is the labelled provider-settle assumption: the provider % at s2
// is trusted only if the last local dispatch completed at least this long before.
const SettleWindow = 120 * time.Second
const Basis = "sandwiched_cuts_provider_settle_assumed"

type Window struct {
	Scope   string `json:"scope"`
	Seconds int64  `json:"seconds"`
}

func (w Window) Key() string { return w.Scope + ":" + strconv.FormatInt(w.Seconds, 10) }

// CutRead is one host.usage.checkpoint read for one tuple, taken just before (A)
// or just after (B) a provider sample. Cut is nil unless the read returned a
// quiescent physical cut; Lower is that sink incarnation's first quiescent cut.
type CutRead struct {
	Tuple                string                           `json:"tuple"`
	Cut                  *measurement.PhysicalCutIdentity `json:"cut,omitempty"`
	Lower                *measurement.PhysicalCutIdentity `json:"lower,omitempty"`
	LastProducerClosedAt *time.Time                       `json:"last_producer_closed_at,omitempty"`
	Reason               string                           `json:"reason,omitempty"`
}

// Sample is one existing scheduled provider % sample with its bracketing reads.
type Sample struct {
	ID          int64     `json:"id"`
	Account     string    `json:"account"`
	Window      Window    `json:"window"`
	At          time.Time `json:"at"`
	UsedPercent float64   `json:"used_percent"`
	Quantum     float64   `json:"quantum"`
	Reset       time.Time `json:"reset"`
	AccountRef  string    `json:"account_ref,omitempty"`
	Before      int64     `json:"inventory_before"`
	After       int64     `json:"inventory_after"`
	A           []CutRead `json:"a"`
	B           []CutRead `json:"b"`
	Reasons     []string  `json:"reasons,omitempty"`
}

// Fact is one recorded local dispatch inside a durable physical selection.
type Fact struct {
	Sequence uint64
	EventID  string
	Revision int64
}

// Proof is the exact durable physical selection From..Through, re-read at
// judgment time so that a late invalidation revokes it.
type Proof struct {
	Known         bool
	Reason        string
	From, Through uint64
	Facts         []Fact
}

type RecordValue struct {
	Delivered, Final, CounterComplete, Priced bool
	LowNanoUSD                                int64
	EndedAt                                   time.Time
}

type CarryInput struct {
	From, To time.Time
	S1, S2   Sample
	Tuples   []string
	Proofs   map[string]Proof
	Record   func(Fact) RecordValue
}

type CarryIn struct {
	Holds   bool
	Reasons []string
	Records []RecordValue
}

func sameEpoch(x, y measurement.PhysicalCutIdentity) bool {
	return x.SourceID == y.SourceID && x.ConsumerID == y.ConsumerID && x.MembershipID == y.MembershipID && x.SourceRef == y.SourceRef && x.ManagerID == y.ManagerID && x.SinkID == y.SinkID
}

func sameCut(x, y measurement.PhysicalCutIdentity) bool {
	return sameEpoch(x, y) && x.Sequence == y.Sequence && x.At.Equal(y.At)
}

// Sandwiched reports whether no local dispatch happened between cut a and cut
// b of one tuple: both quiescent, one manager/sink/listener incarnation, and
// either equal sequences or a durable proof covering both that records no
// dispatch strictly between them.
func Sandwiched(a, b CutRead, p Proof) (bool, string) {
	switch {
	case a.Reason != "":
		return false, a.Reason
	case b.Reason != "":
		return false, b.Reason
	case a.Cut == nil || b.Cut == nil || a.Lower == nil || b.Lower == nil:
		return false, "cut_unavailable"
	case !sameEpoch(*a.Cut, *b.Cut) || !sameCut(*a.Lower, *b.Lower):
		return false, "incarnation_changed_between_cuts"
	case b.Cut.Sequence < a.Cut.Sequence:
		return false, "cut_sequence_regressed"
	case b.Cut.Sequence == a.Cut.Sequence:
		return true, ""
	case !p.Known || p.From > a.Cut.Sequence || p.Through < b.Cut.Sequence:
		return false, "dispatch_freedom_unproven"
	}
	for _, f := range p.Facts {
		if f.Sequence > a.Cut.Sequence && f.Sequence < b.Cut.Sequence {
			return false, "dispatch_between_cuts"
		}
	}
	return true, ""
}

func findRead(reads []CutRead, tuple string) (CutRead, bool) {
	for _, r := range reads {
		if r.Tuple == tuple {
			return r, true
		}
	}
	return CutRead{}, false
}

// JudgeCarryIn holds iff both samples are sandwiched for every tuple, in one
// incarnation, the counted records are exactly those with
// a1.sequence < dispatch_sequence < a2.sequence and each is delivered, final
// and counter-complete, and the last completed dispatch is at least
// SettleWindow before s2. Only the exact sampled interval is judged.
func JudgeCarryIn(in CarryInput) CarryIn {
	var k CarryIn
	add := func(r ...string) { k.Reasons = append(k.Reasons, r...) }
	s1, s2 := in.S1, in.S2
	if s1.Account != s2.Account {
		add("account_mismatch")
	}
	if !s1.At.Before(s2.At) {
		add("interval_order")
	}
	if !in.From.Equal(s1.At) || !in.To.Equal(s2.At) {
		add("sub_window_without_cut")
	}
	if len(s1.Reasons)+len(s2.Reasons) > 0 {
		add("sample_not_sandwiched")
		add(s1.Reasons...)
		add(s2.Reasons...)
	}
	if len(in.Tuples) == 0 {
		add("no_account_tuples")
	}
	counted := map[string]Fact{}
	var lastDone, latestA time.Time
	for _, tuple := range in.Tuples {
		a1, ok1 := findRead(s1.A, tuple)
		b1, ok2 := findRead(s1.B, tuple)
		a2, ok3 := findRead(s2.A, tuple)
		b2, ok4 := findRead(s2.B, tuple)
		if !ok1 || !ok2 || !ok3 || !ok4 {
			add("tuple_not_sandwiched")
			continue
		}
		p, ok := in.Proofs[tuple]
		if !ok || !p.Known {
			add("physical_proof_unavailable")
			if p.Reason != "" {
				add(p.Reason)
			}
		}
		good := true
		for _, pair := range [][2]CutRead{{a1, b1}, {a2, b2}} {
			if ok, reason := Sandwiched(pair[0], pair[1], p); !ok {
				add(reason)
				good = false
			}
		}
		if !good || !p.Known {
			continue
		}
		if !sameEpoch(*a1.Cut, *a2.Cut) || !sameCut(*a1.Lower, *a2.Lower) {
			add("incarnation_changed_between_samples")
			continue
		}
		if p.From != a2.Lower.Sequence || p.Through != b2.Cut.Sequence || b1.Cut.Sequence >= a2.Cut.Sequence {
			add("physical_proof_mismatch")
			continue
		}
		for _, f := range p.Facts {
			if f.Sequence <= p.From || f.Sequence >= p.Through {
				add("physical_proof_mismatch")
			} else if f.Sequence > a1.Cut.Sequence && f.Sequence < a2.Cut.Sequence {
				counted[f.EventID] = f
			}
		}
		if a2.LastProducerClosedAt != nil && a2.LastProducerClosedAt.After(lastDone) {
			lastDone = *a2.LastProducerClosedAt
		}
		if a2.Cut.At.After(latestA) {
			latestA = a2.Cut.At
		}
	}
	facts := make([]Fact, 0, len(counted))
	for _, f := range counted {
		facts = append(facts, f)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Sequence < facts[j].Sequence })
	for _, f := range facts {
		var v RecordValue
		if in.Record != nil {
			v = in.Record(f)
		}
		switch {
		case !v.Delivered:
			add("record_missing")
		case !v.Final || !v.CounterComplete:
			add("record_incomplete")
		}
		if v.EndedAt.After(lastDone) {
			lastDone = v.EndedAt
		}
		k.Records = append(k.Records, v)
	}
	if !lastDone.IsZero() && latestA.Sub(lastDone) < SettleWindow {
		add("provider_settle_unproven")
	}
	k.Reasons = unique(k.Reasons)
	k.Holds = len(k.Reasons) == 0
	return k
}
