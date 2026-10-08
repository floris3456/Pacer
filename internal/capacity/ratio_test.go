package capacity

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func admissible() (Sample, Sample, Membership, CarryIn) {
	f := newCarryFixture()
	m := Membership{Holds: true, AccountRef: "ref-A", Tuples: []string{tupleA}, Active: []string{tupleA}}
	return f.in.S1, f.in.S2, m, JudgeCarryIn(f.in)
}

func TestRatioLowerBoundUsesDeltaPlusQuantum(t *testing.T) {
	s1, s2, m, k := admissible()
	i := Project(s1, s2, m, k)
	if i.Status != "lower_bound" || len(i.Reasons) != 0 || i.Value == nil {
		t.Fatal("admissible interval refused", i)
	}
	// $8 over Δ 3% with a 1% reporting quantum: 8 / (3 + 1).
	if i.Value.LowerBoundUSDPerPercent != 2 || i.Value.CPAValueUSD != 8 || i.Value.DeltaPercent != 3 || i.Value.QuantumPercent != 1 || i.Value.Records != 1 {
		t.Fatal("lower bound must use delta plus quantum", i.Value)
	}
	if i.Exclusivity != "unproven" || i.Basis != "sandwiched_cuts_provider_settle_assumed" || !i.From.Equal(s1.At) || !i.To.Equal(s2.At) || i.Window != "all:604800" {
		t.Fatal("interval labels", i)
	}
	s1.UsedPercent, s2.UsedPercent, s1.Quantum, s2.Quantum = 10.5, 12, 0.5, 0.1
	if i = Project(s1, s2, m, k); i.Value == nil || i.Value.QuantumPercent != 0.5 || i.Value.LowerBoundUSDPerPercent != 4 {
		t.Fatal("coarser quantum of the two samples must apply", i)
	}
}

func TestRatioUnpricedOrIncompleteIsUnknown(t *testing.T) {
	s1, s2, m, k := admissible()
	k.Records[0].Priced = false
	if i := Project(s1, s2, m, k); i.Status != "unknown" || i.Value != nil || !hasReason(i.Reasons, "record_unpriced") {
		t.Fatal("unpriced record produced a ratio", i)
	}
	s1, s2, m, k = admissible()
	k.Records[0].CounterComplete = false
	if i := Project(s1, s2, m, k); i.Status != "unknown" || i.Value != nil {
		t.Fatal("counter-incomplete record produced a ratio", i)
	}
	s1, s2, m, k = admissible()
	k.Holds, k.Reasons = false, []string{"provider_settle_unproven"}
	if i := Project(s1, s2, m, k); i.Status != "unknown" || !hasReason(i.Reasons, "carry_in_unproven") || !hasReason(i.Reasons, "provider_settle_unproven") {
		t.Fatal("unproven carry-in produced a ratio", i)
	}
	s1, s2, m, k = admissible()
	m.Holds, m.Reasons = false, []string{"binding_changed_inside"}
	if i := Project(s1, s2, m, k); i.Status != "unknown" || !hasReason(i.Reasons, "membership_unproven") || !hasReason(i.Reasons, "binding_changed_inside") {
		t.Fatal("unproven membership produced a ratio", i)
	}
}

func TestRatioResetBetweenSamplesIsUnknown(t *testing.T) {
	s1, s2, m, k := admissible()
	s1.Reset = s2.At.Add(-time.Second)
	if i := Project(s1, s2, m, k); i.Status != "unknown" || !hasReason(i.Reasons, "reset_between_samples") {
		t.Fatal("reset between samples accepted", i)
	}
	s1, s2, m, k = admissible()
	s2.Reset = s1.Reset.Add(time.Hour)
	if i := Project(s1, s2, m, k); i.Status != "unknown" || !hasReason(i.Reasons, "reset_between_samples") {
		t.Fatal("changed cycle accepted", i)
	}
	s1, s2, m, k = admissible()
	s1.Reset = time.Time{}
	if i := Project(s1, s2, m, k); i.Status != "unknown" {
		t.Fatal("unknown reset accepted", i)
	}
}

func TestRatioWindowsDifferOrNonpositiveIsUnknown(t *testing.T) {
	s1, s2, m, k := admissible()
	s2.Window = Window{"all", 18000}
	if i := Project(s1, s2, m, k); i.Status != "unknown" || !hasReason(i.Reasons, "windows_differ") {
		t.Fatal("different windows accepted", i)
	}
	for _, used := range []float64{9, 8} {
		s1, s2, m, k = admissible()
		s2.UsedPercent = used
		if i := Project(s1, s2, m, k); i.Status != "unknown" || !hasReason(i.Reasons, "nonpositive_change") {
			t.Fatal("delta plus quantum <= 0 accepted", used, i)
		}
	}
	s1, s2, m, k = admissible()
	s2.Quantum = 0
	if i := Project(s1, s2, m, k); i.Status != "unknown" || !hasReason(i.Reasons, "precision_unknown") {
		t.Fatal("unknown precision accepted", i)
	}
	s1, s2, m, k = admissible()
	s2.Account = "other"
	if i := Project(s1, s2, m, k); i.Status != "unknown" {
		t.Fatal("different accounts accepted", i)
	}
}

func TestQuantumIsReportedPrecision(t *testing.T) {
	for raw, want := range map[string]float64{"37": 1, "0": 1, "37.0": 1, "37.50": 0.1, "37.5": 0.1, "37.25": 0.01, "": 0, "abc": 0, "3e1": 0, "-1": 0, "101": 0, "NaN": 0, "1.2.3": 0} {
		if got := Quantum(raw); math.Abs(got-want) > 1e-12 {
			t.Fatal(raw, got, want)
		}
	}
}

func TestViewNewestBesideOlderNeverAveraged(t *testing.T) {
	s1, s2, m, k := admissible()
	older := Project(s1, s2, m, k)
	s3, s4 := s1, s2
	s3.At, s4.At = s2.At, s2.At.Add(time.Hour)
	s3.UsedPercent, s4.UsedPercent = 13, 14 // 8 / (1 + 1) = 4
	newer := Project(s3, s4, m, k)
	if older.Value.LowerBoundUSDPerPercent != 2 || newer.Value.LowerBoundUSDPerPercent != 4 {
		t.Fatal("fixture", older.Value, newer.Value)
	}
	now := s4.At.Add(90 * time.Second)
	v := NewView([]Interval{older, newer}, now)
	if v.Status != "lower_bound" || v.Value.LowerBoundUSDPerPercent != 4 || !v.To.Equal(s4.At) || *v.AgeSeconds != 90 {
		t.Fatal("newest interval not shown with its age", v)
	}
	if len(v.History) != 1 || v.History[0].Value.LowerBoundUSDPerPercent != 2 || !v.History[0].To.Equal(s2.At) {
		t.Fatal("older interval not reported beside the newer one", v.History)
	}
	var many []Interval
	for i := 0; i < 3*HistoryLimit; i++ {
		x := newer
		x.To = newer.To.Add(time.Duration(i) * time.Hour)
		many = append(many, x)
	}
	if v = NewView(many, now); len(v.History) != HistoryLimit {
		t.Fatal("history unbounded", len(v.History))
	}
	empty := NewView(nil, now, "inventory_unknown")
	if empty.Status != "unknown" || empty.Value != nil || empty.To != nil || empty.AgeSeconds != nil || !hasReason(empty.Reasons, "inventory_unknown") {
		t.Fatal("no interval must stay unknown, without a carried value", empty)
	}
}

func TestCapacityKnownCannotBeSetOrSeeded(t *testing.T) {
	s1, s2, m, k := admissible()
	views := []View{NewView(nil, epoch), NewView([]Interval{Project(s1, s2, m, k)}, s2.At)}
	for _, v := range views {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if known, ok := body["capacity_known"]; !ok || known != false || body["exclusivity"] != "unproven" {
			t.Fatal("capacity_known must be present and false", string(raw))
		}
	}
	// No exported field can carry a declared size or a trust flag.
	for _, typ := range []reflect.Type{reflect.TypeOf(View{}), reflect.TypeOf(Interval{}), reflect.TypeOf(Value{}), reflect.TypeOf(Sample{}), reflect.TypeOf(Membership{}), reflect.TypeOf(CarryIn{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
			for _, banned := range []string{"capacityknown", "capacity_known", "size", "declared", "claimed", "plan", "tier", "multiplier", "trust", "complete", "override", "seed"} {
				if strings.Contains(strings.ReplaceAll(name, " ", ""), banned) {
					t.Fatal("declared/trust field present", typ, f.Name)
				}
			}
		}
	}
	if typ := reflect.TypeOf(View{}); typ.NumMethod() != 1 {
		t.Fatal("view gained a setter", typ.NumMethod())
	}
}

func TestPairsAreConsecutivePerWindow(t *testing.T) {
	var samples []Sample
	for i := 0; i < 6; i++ {
		for _, seconds := range []int64{18000, 604800} {
			samples = append(samples, Sample{Account: "acct", Window: Window{"all", seconds}, At: epoch.Add(time.Duration(i) * time.Hour)})
		}
	}
	// Ledger order is newest first.
	for i, j := 0, len(samples)-1; i < j; i, j = i+1, j-1 {
		samples[i], samples[j] = samples[j], samples[i]
	}
	pairs := Pairs(samples, 3)
	if len(pairs) != 6 {
		t.Fatal("pairs per window", len(pairs))
	}
	for _, p := range pairs {
		if p[0].Window != p[1].Window || p[1].At.Sub(p[0].At) != time.Hour {
			t.Fatal("pairs must be consecutive samples of one window", p[0].At, p[1].At)
		}
	}
	if !pairs[0][1].At.Equal(epoch.Add(5 * time.Hour)) {
		t.Fatal("newest pair first", pairs[0][1].At)
	}
}
