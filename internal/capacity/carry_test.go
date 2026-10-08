package capacity

import (
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

var tupleA = TupleRef("a", "index-a")

func cut(seq uint64, at time.Time) *measurement.PhysicalCutIdentity {
	return &measurement.PhysicalCutIdentity{SourceID: "source", ConsumerID: "consumer", MembershipID: "membership", SourceRef: "sha256:ref", ManagerID: "manager", SinkID: "sink", Sequence: seq, At: at}
}

func read(seq uint64, at time.Time, closed *time.Time) CutRead {
	return CutRead{Tuple: tupleA, Cut: cut(seq, at), Lower: cut(10, epoch), LastProducerClosedAt: closed}
}

type carryFixture struct {
	in      CarryInput
	records map[string]RecordValue
}

// One tuple. Lower cut 10; s1 sandwiched by cuts 20/22, s2 by cuts 40/42; one
// dispatch (30) between the samples and one (15) before s1.
func newCarryFixture() *carryFixture {
	t1, t2 := epoch.Add(time.Minute), epoch.Add(61*time.Minute)
	closed := epoch.Add(30 * time.Minute)
	s1 := Sample{Account: "acct", Window: Window{"all", 604800}, At: t1, UsedPercent: 10, Quantum: 1, Reset: epoch.Add(96 * time.Hour), AccountRef: "ref-A", A: []CutRead{read(20, t1, nil)}, B: []CutRead{read(22, t1, nil)}}
	s2 := s1
	s2.At, s2.UsedPercent = t2, 13
	s2.A, s2.B = []CutRead{read(40, t2, &closed)}, []CutRead{read(42, t2, &closed)}
	f := &carryFixture{records: map[string]RecordValue{
		"e0": {Delivered: true, Final: true, CounterComplete: true, Priced: true, LowNanoUSD: 1e9, EndedAt: epoch.Add(30 * time.Second)},
		"e1": {Delivered: true, Final: true, CounterComplete: true, Priced: true, LowNanoUSD: 8e9, EndedAt: closed},
	}}
	f.in = CarryInput{From: t1, To: t2, S1: s1, S2: s2, Tuples: []string{tupleA}, Proofs: map[string]Proof{tupleA: {Known: true, From: 10, Through: 42, Facts: []Fact{{15, "e0", 1}, {30, "e1", 1}}}}}
	f.in.Record = func(x Fact) RecordValue { return f.records[x.EventID] }
	return f
}

func TestCarryInSandwichHoldsAdmissible(t *testing.T) {
	f := newCarryFixture()
	k := JudgeCarryIn(f.in)
	if !k.Holds || len(k.Reasons) != 0 || len(k.Records) != 1 || k.Records[0].LowNanoUSD != 8e9 {
		t.Fatal("admissible sandwich refused", k)
	}
	a, b := read(20, epoch, nil), read(20, epoch, nil)
	if ok, reason := Sandwiched(a, b, Proof{}); !ok {
		t.Fatal("equal-sequence sandwich refused", reason)
	}
	if ok, reason := Sandwiched(a, read(22, epoch, nil), Proof{Known: true, From: 10, Through: 22, Facts: []Fact{{15, "e0", 1}, {30, "e1", 1}}}); !ok {
		t.Fatal("dispatch-free sandwich refused", reason)
	}
}

func TestCarryInDispatchBetweenCutsNotSandwiched(t *testing.T) {
	if ok, reason := Sandwiched(read(20, epoch, nil), read(22, epoch, nil), Proof{Known: true, From: 10, Through: 22, Facts: []Fact{{21, "e2", 1}}}); ok || reason != "dispatch_between_cuts" {
		t.Fatal("dispatch between a and b accepted", reason)
	}
	if ok, _ := Sandwiched(read(20, epoch, nil), read(22, epoch, nil), Proof{}); ok {
		// No durable proof covers b, so absence of a dispatch is not shown.
		t.Fatal("unproven gap between cuts accepted")
	}
	for _, seq := range []uint64{21, 41} {
		f := newCarryFixture()
		p := f.in.Proofs[tupleA]
		p.Facts = append(p.Facts, Fact{seq, "e2", 1})
		f.in.Proofs[tupleA] = p
		f.records["e2"] = f.records["e1"]
		if k := JudgeCarryIn(f.in); k.Holds || !hasReason(k.Reasons, "dispatch_between_cuts") {
			t.Fatal("dispatch inside a sample sandwich accepted", seq, k)
		}
	}
}

func TestCarryInRestartBetweenCutsRefused(t *testing.T) {
	f := newCarryFixture()
	f.in.S1.B[0].Cut.ManagerID = "manager-restarted"
	if k := JudgeCarryIn(f.in); k.Holds || !hasReason(k.Reasons, "incarnation_changed_between_cuts") {
		t.Fatal("host restart between a and b accepted", k)
	}
	f = newCarryFixture()
	for _, r := range []*CutRead{&f.in.S2.A[0], &f.in.S2.B[0]} {
		r.Cut.ConsumerID = "consumer-after-pacer-restart"
		r.Lower.ConsumerID = "consumer-after-pacer-restart"
	}
	if k := JudgeCarryIn(f.in); k.Holds || !hasReason(k.Reasons, "incarnation_changed_between_samples") {
		t.Fatal("listener restart between samples accepted", k)
	}
	f = newCarryFixture()
	for _, r := range []*CutRead{&f.in.S2.A[0], &f.in.S2.B[0]} {
		r.Lower.Sequence = 33 // sink reload: a new first quiescent cut
	}
	if k := JudgeCarryIn(f.in); k.Holds {
		t.Fatal("sink reload between samples borrowed the old lower cut", k)
	}
}

func TestCarryInLateInvalidationRevokes(t *testing.T) {
	f := newCarryFixture()
	if !JudgeCarryIn(f.in).Holds {
		t.Fatal("fixture")
	}
	f.in.Proofs[tupleA] = Proof{Known: false, Reason: "physical_incarnation_gap"}
	k := JudgeCarryIn(f.in)
	if k.Holds || !hasReason(k.Reasons, "physical_proof_unavailable") || !hasReason(k.Reasons, "physical_incarnation_gap") {
		t.Fatal("late invalidation did not revoke", k)
	}
	delete(f.in.Proofs, tupleA)
	if k = JudgeCarryIn(f.in); k.Holds {
		t.Fatal("missing proof accepted", k)
	}
	// A proof for another selection (here one ending before b2) never stands in.
	f = newCarryFixture()
	f.in.Proofs[tupleA] = Proof{Known: true, From: 10, Through: 40, Facts: []Fact{{30, "e1", 1}}}
	if k = JudgeCarryIn(f.in); k.Holds {
		t.Fatal("proof of a different selection accepted", k)
	}
}

func TestCarryInSubWindowRefused(t *testing.T) {
	for _, shift := range []struct{ from, to time.Duration }{{time.Minute, 0}, {0, -time.Minute}, {-time.Second, 0}} {
		f := newCarryFixture()
		f.in.From = f.in.From.Add(shift.from)
		f.in.To = f.in.To.Add(shift.to)
		if k := JudgeCarryIn(f.in); k.Holds || !hasReason(k.Reasons, "sub_window_without_cut") {
			t.Fatal("sub-window borrowed a cut", shift, k)
		}
	}
	f := newCarryFixture()
	f.in.S1, f.in.S2 = f.in.S2, f.in.S1
	f.in.From, f.in.To = f.in.S1.At, f.in.S2.At
	if k := JudgeCarryIn(f.in); k.Holds {
		t.Fatal("reversed interval accepted", k)
	}
}

func TestCarryInSettleUnderTwoMinutesRefused(t *testing.T) {
	f := newCarryFixture()
	recent := f.in.S2.A[0].Cut.At.Add(-time.Minute)
	f.in.S2.A[0].LastProducerClosedAt = &recent
	if k := JudgeCarryIn(f.in); k.Holds || !hasReason(k.Reasons, "provider_settle_unproven") {
		t.Fatal("settle under 120 s accepted", k)
	}
	f = newCarryFixture()
	f.in.S2.A[0].LastProducerClosedAt = nil
	f.records["e1"] = RecordValue{Delivered: true, Final: true, CounterComplete: true, Priced: true, LowNanoUSD: 8e9, EndedAt: f.in.S2.A[0].Cut.At.Add(-119 * time.Second)}
	if k := JudgeCarryIn(f.in); k.Holds {
		t.Fatal("recent counted record ignored by settle", k)
	}
	f = newCarryFixture()
	edge := f.in.S2.A[0].Cut.At.Add(-SettleWindow)
	f.in.S2.A[0].LastProducerClosedAt = &edge
	if k := JudgeCarryIn(f.in); !k.Holds {
		t.Fatal("exactly 120 s refused", k)
	}
}

func TestCarryInCountsExactlyBetweenLowerCuts(t *testing.T) {
	f := newCarryFixture()
	p := f.in.Proofs[tupleA]
	p.Facts = append(p.Facts, Fact{35, "e3", 1})
	f.in.Proofs[tupleA] = p
	f.records["e3"] = RecordValue{Delivered: true, Final: true, CounterComplete: true, Priced: true, LowNanoUSD: 2e9, EndedAt: epoch.Add(31 * time.Minute)}
	k := JudgeCarryIn(f.in)
	if !k.Holds || len(k.Records) != 2 || k.Records[0].LowNanoUSD+k.Records[1].LowNanoUSD != 10e9 {
		t.Fatal("counted records are not exactly a1 < seq < a2", k)
	}
}

func TestCarryInRecordsMustBeFinalDeliveredCounterComplete(t *testing.T) {
	for name, v := range map[string]RecordValue{
		"missing":    {},
		"not-final":  {Delivered: true, CounterComplete: true, Priced: true},
		"incomplete": {Delivered: true, Final: true, Priced: true},
	} {
		f := newCarryFixture()
		f.records["e1"] = v
		if k := JudgeCarryIn(f.in); k.Holds {
			t.Fatal(name, "record accepted", k)
		}
	}
}

func TestCarryInEveryMembershipTupleNeedsSandwiches(t *testing.T) {
	f := newCarryFixture()
	f.in.Tuples = append(f.in.Tuples, TupleRef("alias", "index-alias"))
	if k := JudgeCarryIn(f.in); k.Holds || !hasReason(k.Reasons, "tuple_not_sandwiched") {
		t.Fatal("alias without cuts accepted", k)
	}
	f = newCarryFixture()
	f.in.S1.Reasons = []string{"inventory_unknown"}
	if k := JudgeCarryIn(f.in); k.Holds || !hasReason(k.Reasons, "inventory_unknown") {
		t.Fatal("sample refusal ignored", k)
	}
	f = newCarryFixture()
	f.in.S2.B[0] = CutRead{Tuple: tupleA, Reason: "cut_not_quiescent"}
	if k := JudgeCarryIn(f.in); k.Holds || !hasReason(k.Reasons, "cut_not_quiescent") {
		t.Fatal("non-quiescent b accepted", k)
	}
	f = newCarryFixture()
	f.in.Tuples = nil
	if k := JudgeCarryIn(f.in); k.Holds {
		t.Fatal("empty tuple set accepted", k)
	}
}
