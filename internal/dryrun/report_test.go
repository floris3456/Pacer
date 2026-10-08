package dryrun

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
)

var (
	day    = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	secret = bytes.Repeat([]byte{7}, 32)
)

const opus = "claude-opus-5-5"

func at(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

var stays = map[string]bool{"protected": true, "current_best": true, "cold_margin": true}

// dec builds one admitted decision. An empty proposed account means Pacer did
// not handle the request and fell back to CPA.
func dec(when time.Time, chat, request, previous, actual, proposed, reason string) Decision {
	d := Decision{At: when, Chat: chat, RequestID: request, Provider: "claude", Model: opus, PreviousAccount: previous, ActualAuth: "auth-" + actual, ActualAccount: actual, Outcome: "admitted"}
	if proposed != "" {
		d.Proposed = routing.Decision{Handled: true, Auth: "auth-" + proposed, Account: proposed, Reason: reason, Forced: reason == "forced_move", Pace: 1.5}
	} else {
		d.Proposed = routing.Decision{Reason: reason}
	}
	return d
}

func sequence(ds []Decision) []Decision {
	for i := range ds {
		ds[i].Seq = int64(i + 1)
	}
	return ds
}

func covered(from, to time.Time) map[string][]Interval {
	return map[string][]Interval{"claude": {{From: from, To: to}}}
}

func build(t *testing.T, in Input) Report {
	t.Helper()
	if in.Day.IsZero() {
		in.Day = day
	}
	if in.Now.IsZero() {
		in.Now = day.Add(25 * time.Hour)
	}
	if in.Secret == nil {
		in.Secret = secret
	}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func has(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func near(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestEmptyDayIsExplicitlyIncomplete(t *testing.T) {
	r := build(t, Input{})
	m := r.Meta
	if m.Version != Version || m.Day != "2026-10-07" || !m.From.Equal(day) || !m.To.Equal(day.Add(24*time.Hour)) || !m.GeneratedAt.Equal(day.Add(25*time.Hour)) {
		t.Fatalf("meta: %+v", m)
	}
	if m.Complete || !has(m.Reasons, "dry_run_not_active") || m.Decisions != 0 || m.Chats != 0 {
		t.Fatalf("empty day must not claim completeness: %+v", m)
	}
	raw, _ := json.Marshal(r)
	for _, want := range []string{`"placements":[]`, `"items":[]`, `"resets":[]`, `"coverage":[]`, `"limit":200`, `"window_hours":24`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("empty report lists must be [] not null: %s", raw)
	}
}

func TestSingleCoveredDayPlacementsAndMoves(t *testing.T) {
	ds := sequence([]Decision{
		dec(at(1, 0), "chat-1", "r1", "", "A", "B", "new_chat"),
		dec(at(1, 5), "chat-1", "r2", "A", "A", "A", "protected"),
		dec(at(2, 0), "chat-2", "r3", "A", "A", "B", "cold_move"),
		dec(at(3, 0), "chat-3", "r4", "", "A", "A", "new_chat"),
		dec(at(4, 0), "chat-3", "r5", "A", "A", "", "no_known_budget"),
		dec(at(5, 0), "chat-4", "r6", "", "B", "B", "new_chat"),
	})
	ds[5].Model = "claude-sonnet-5"
	r := build(t, Input{Decisions: ds, Coverage: covered(day, day.Add(24*time.Hour))})
	if !r.Meta.Complete || len(r.Meta.Reasons) != 0 || r.Meta.Decisions != 6 || r.Meta.Chats != 4 {
		t.Fatalf("meta: %+v", r.Meta)
	}
	if len(r.Meta.Coverage) != 1 || r.Meta.Coverage[0].CoveredSeconds != 86400 || !r.Meta.Coverage[0].Complete {
		t.Fatalf("coverage: %+v", r.Meta.Coverage)
	}
	if len(r.Placements) != 2 {
		t.Fatalf("placements: %+v", r.Placements)
	}
	o := r.Placements[0]
	if o.Model != opus || o.Decisions != 5 || o.Chats != 3 || o.Agree != 2 || o.Differ != 2 || o.Unhandled != 1 {
		t.Fatalf("opus placement: %+v", o)
	}
	if o.Reasons["new_chat"] != 2 || o.Reasons["protected"] != 1 || o.Reasons["cold_move"] != 1 || o.Reasons["no_known_budget"] != 1 || o.Outcomes["admitted"] != 5 {
		t.Fatalf("opus reasons: %+v", o)
	}
	want := []AccountServed{{Account: "A", Actual: 5, Proposed: 2, Simulated: 2}, {Account: "B", Actual: 0, Proposed: 2, Simulated: 3}}
	if fmt.Sprint(o.Accounts) != fmt.Sprint(want) {
		t.Fatalf("accounts: %+v", o.Accounts)
	}
	s := r.Placements[1]
	if s.Model != "claude-sonnet-5" || s.Decisions != 1 || s.Agree != 1 || fmt.Sprint(s.Accounts) != fmt.Sprint([]AccountServed{{Account: "B", Actual: 1, Proposed: 1, Simulated: 1}}) {
		t.Fatalf("sonnet placement: %+v", s)
	}
	m := r.WouldBeMoves
	if m.Total != 1 || m.Truncated || len(m.Items) != 1 {
		t.Fatalf("moves: %+v", m)
	}
	if i := m.Items[0]; !i.At.Equal(at(2, 0)) || i.From != "A" || i.To != "B" || i.Actual != "A" || i.Reason != "cold_move" || i.ChatRef != ChatRef(secret, "chat-2") {
		t.Fatalf("move: %+v", i)
	}
}

func TestFlipFlopAcrossMidnight(t *testing.T) {
	prev := day.Add(-24 * time.Hour)
	ds := sequence([]Decision{
		dec(prev.Add(30*time.Minute), "slow", "g1", "", "A", "A", "new_chat"),
		dec(prev.Add(31*time.Minute), "slow", "g2", "A", "A", "B", "cold_move"),
		dec(prev.Add(22*time.Hour), "fast", "f1", "", "A", "A", "new_chat"),
		dec(prev.Add(23*time.Hour), "fast", "f2", "A", "A", "B", "cold_move"),
		// CPA itself moved both chats to B; Pacer would move them back to A.
		dec(at(1, 0), "fast", "f3", "B", "B", "A", "cold_move"),
		dec(at(1, 30), "slow", "g3", "B", "B", "A", "cold_move"),
	})
	in := Input{Decisions: ds, Coverage: covered(prev, day.Add(24*time.Hour))}
	r := build(t, in)
	f := r.FlipFlops
	if f.WindowHours != 24 || f.Total != 1 || len(f.Items) != 1 {
		t.Fatalf("flip-flops: %+v", f)
	}
	i := f.Items[0]
	if i.ChatRef != ChatRef(secret, "fast") || i.Account != "A" || i.Via != "B" || !i.LeftAt.Equal(prev.Add(23*time.Hour)) || !i.ReturnedAt.Equal(at(1, 0)) || i.Reason != "cold_move" {
		t.Fatalf("flip-flop: %+v", i)
	}
	// The previous day's report sees the departures only, never the return.
	in.Day, in.Now = prev, day.Add(time.Hour)
	earlier := build(t, in)
	if earlier.FlipFlops.Total != 0 || earlier.WouldBeMoves.Total != 2 {
		t.Fatalf("previous day: %+v %+v", earlier.FlipFlops, earlier.WouldBeMoves)
	}
	// A coverage gap breaks the trajectory: no flip-flop is claimed across it.
	in.Day, in.Now = day, day.Add(25*time.Hour)
	in.Coverage = map[string][]Interval{"claude": {{From: prev, To: day.Add(-time.Minute)}, {From: day, To: day.Add(24 * time.Hour)}}}
	if gap := build(t, in); gap.FlipFlops.Total != 0 {
		t.Fatalf("flip-flop claimed across a coverage gap: %+v", gap.FlipFlops)
	}
}

func TestWouldBeMovesAreBoundedWithExactTotal(t *testing.T) {
	ds := []Decision{}
	for i := 0; i < 250; i++ {
		ds = append(ds, dec(day.Add(time.Duration(i)*time.Minute), fmt.Sprintf("chat-%03d", i), fmt.Sprintf("r%d", i), "A", "A", "B", "cold_move"))
	}
	r := build(t, Input{Decisions: sequence(ds), Coverage: covered(day, day.Add(24*time.Hour))})
	m := r.WouldBeMoves
	if m.Total != 250 || m.Limit != MoveLimit || !m.Truncated || len(m.Items) != MoveLimit {
		t.Fatalf("moves: total=%d limit=%d truncated=%v items=%d", m.Total, m.Limit, m.Truncated, len(m.Items))
	}
	if !m.Items[0].At.Equal(day) || !m.Items[199].At.Equal(day.Add(199*time.Minute)) {
		t.Fatal("items are not the earliest moves in time order")
	}
}

func TestChatRefIsStableKeyedAndPrivate(t *testing.T) {
	const chat = "private-chat-key-0123456789abcdef"
	ds := sequence([]Decision{dec(at(9, 0), chat, "private-request", "A", "A", "B", "cold_move")})
	in := Input{Decisions: ds, Coverage: covered(day, day.Add(24*time.Hour))}
	first, second := build(t, in), build(t, in)
	ref := first.WouldBeMoves.Items[0].ChatRef
	if ref == "" || ref != second.WouldBeMoves.Items[0].ChatRef || ref != ChatRef(secret, chat) {
		t.Fatal("chat_ref is not stable", ref)
	}
	if len(ref) != 16 {
		t.Fatal("chat_ref must be 64 bits of hex", ref)
	}
	if _, err := hex.DecodeString(ref); err != nil {
		t.Fatal(err)
	}
	in.Secret = bytes.Repeat([]byte{8}, 32)
	if other := build(t, in).WouldBeMoves.Items[0].ChatRef; other == ref {
		t.Fatal("chat_ref does not depend on the installation secret")
	}
	if ChatRef(secret, "another-chat") == ref {
		t.Fatal("distinct chats share a chat_ref")
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "auth-") {
		t.Fatalf("private chat/request/credential identity escaped: %s", raw)
	}
	if _, err := Build(Input{Day: day, Now: day.Add(25 * time.Hour)}); err == nil {
		t.Fatal("a report without the installation secret must fail")
	}
}

func reading(when time.Time, used float64) *Reading { return &Reading{At: when, UsedPercent: used} }

func TestResetBaselinesFreshStaleNone(t *testing.T) {
	five := int64(5 * 3600)
	cycles := []Cycle{
		{Provider: "claude", Account: "fresh", Scope: "all", Seconds: five, CycleID: "c-fresh", Scheduled: at(10, 0), Closed: at(10, 1), Last: reading(at(9, 50), 70)},
		{Provider: "claude", Account: "stale", Scope: "all", Seconds: five, CycleID: "c-stale", Scheduled: at(12, 0), Last: reading(at(8, 0), 40)},
		{Provider: "claude", Account: "none", Scope: "all", Seconds: five, CycleID: "c-none", Scheduled: at(13, 0), Closed: at(13, 0)},
		{Provider: "claude", Account: "early", Scope: "all", Seconds: five, CycleID: "c-early", Scheduled: at(20, 0), Closed: at(15, 0), Last: reading(at(14, 30), 90)},
		{Provider: "claude", Account: "before", Scope: "all", Seconds: five, CycleID: "c-before", Scheduled: day.Add(-time.Hour), Closed: day.Add(time.Minute), Last: reading(day.Add(-90*time.Minute), 10)},
		{Provider: "claude", Account: "after", Scope: "all", Seconds: five, CycleID: "c-after", Scheduled: day.Add(26 * time.Hour), Last: reading(at(23, 0), 10)},
		{Provider: "claude", Account: "future", Scope: "all", Seconds: five, CycleID: "c-future", Scheduled: at(23, 0), Last: reading(at(18, 0), 10)},
	}
	r := build(t, Input{Cycles: cycles, Now: at(22, 0), Coverage: covered(day, day.Add(24*time.Hour))})
	if len(r.Resets) != 4 {
		t.Fatalf("resets: %+v", r.Resets)
	}
	byAccount := map[string]Reset{}
	for _, reset := range r.Resets {
		byAccount[reset.Account] = reset
	}
	f := byAccount["fresh"]
	if f.Baseline.Policy != "fill_first_observed" || f.Baseline.Status != "fresh" || !f.ResetAt.Equal(at(10, 0)) || f.ResetBasis != "scheduled" || *f.Baseline.GapSeconds != 600 {
		t.Fatalf("fresh: %+v", f)
	}
	near(t, f.Baseline.UnusedPercent, 30)
	if f.CycleStart == nil || !f.CycleStart.Equal(at(5, 0)) {
		t.Fatal("cycle start", f.CycleStart)
	}
	s := byAccount["stale"]
	if s.Baseline.Status != "stale" || s.ResetBasis != "scheduled_unconfirmed" || *s.Baseline.GapSeconds != 4*3600 || s.Projection.Known || !has(s.Projection.Reasons, "baseline_stale") {
		t.Fatalf("stale: %+v", s)
	}
	near(t, s.Baseline.UnusedPercent, 60)
	n := byAccount["none"]
	if n.Baseline.Status != "none" || n.Baseline.UnusedPercent != nil || n.Baseline.GapSeconds != nil || !has(n.Projection.Reasons, "baseline_none") {
		t.Fatalf("none: %+v", n)
	}
	e := byAccount["early"]
	if e.ResetBasis != "observed_early" || !e.ResetAt.Equal(at(15, 0)) || e.Baseline.Status != "fresh" {
		t.Fatalf("early: %+v", e)
	}
	if r.Resets[0].Account != "fresh" || r.Resets[3].Account != "early" {
		t.Fatal("resets are not ordered by reset time")
	}
}

// projectionInput: account A's 5-hour cycle [05:00,10:00) with 40% observed
// unused and $0.50 per percent (2 %/$). Pacer would move $3 in and $1.50 out.
func projectionInput() Input {
	ds := sequence([]Decision{
		dec(at(6, 0), "m1", "in", "", "B", "A", "new_chat"),
		dec(at(7, 0), "m2", "out-1", "A", "A", "B", "cold_move"),
		dec(at(7, 5), "m2", "out-2", "A", "A", "A", "protected"),
		dec(at(8, 0), "m3", "stay", "", "A", "A", "new_chat"),
	})
	attempt := func(request, account string, start time.Time, usd float64) Attempt {
		return Attempt{RequestID: request, Account: account, StartedAt: start, EndedAt: start.Add(10 * time.Second), NanoUSD: int64(usd * 1e9), Exact: true}
	}
	return Input{
		Decisions: ds,
		Coverage:  covered(day.Add(-24*time.Hour), day.Add(24*time.Hour)),
		Cycles:    []Cycle{{Provider: "claude", Account: "A", Scope: "all", Seconds: 5 * 3600, CycleID: "cycle-A", Scheduled: at(10, 0), Closed: at(10, 0).Add(30 * time.Second), Last: reading(at(9, 55), 60), Ratio: Ratio{Known: true, USDPerPercent: 0.5}}},
		Attempts:  []Attempt{attempt("in", "B", at(6, 0), 3), attempt("out-1", "A", at(7, 0), 1), attempt("out-2", "A", at(7, 5), 0.5), attempt("stay", "A", at(8, 0), 10)},
	}
}

func TestProjectionKnownUnderEqualQuality(t *testing.T) {
	r := build(t, projectionInput())
	if len(r.Resets) != 1 {
		t.Fatalf("resets: %+v", r.Resets)
	}
	p := r.Resets[0].Projection
	if !p.Known || len(p.Reasons) != 0 || p.Policy != "pace_dry_run_simulation" || !has(p.Assumptions, "equal_quality") {
		t.Fatalf("projection: %+v", p)
	}
	if p.RequestsIn != 1 || p.RequestsOut != 2 || p.ValueInUSD != 3 || p.ValueOutUSD != 1.5 {
		t.Fatalf("moved value: %+v", p)
	}
	near(t, p.PercentPerUSD, 2)
	near(t, p.RawUnusedPercent, 37)
	near(t, p.UnusedPercent, 37)
	near(t, r.Resets[0].Baseline.UnusedPercent, 40)
}

func TestProjectionClampsToMeterRange(t *testing.T) {
	in := projectionInput()
	in.Attempts[0].NanoUSD = 100e9
	p := build(t, in).Resets[0].Projection
	if !p.Known {
		t.Fatal(p.Reasons)
	}
	near(t, p.RawUnusedPercent, 40-2*(100-1.5))
	near(t, p.UnusedPercent, 0)
}

func TestProjectionUnknownReasons(t *testing.T) {
	cases := []struct {
		reason string
		change func(*Input)
	}{
		{"baseline_stale", func(in *Input) { in.Cycles[0].Last.At = at(8, 0) }},
		{"baseline_none", func(in *Input) { in.Cycles[0].Last = nil }},
		{"ratio_unknown", func(in *Input) { in.Cycles[0].Ratio = Ratio{Reasons: []string{"below_ten_point_delta"}} }},
		{"ratio:below_ten_point_delta", func(in *Input) { in.Cycles[0].Ratio = Ratio{Reasons: []string{"below_ten_point_delta"}} }},
		{"cycle_start_unknown", func(in *Input) { in.Cycles[0].Scheduled = time.Time{} }},
		{"dry_run_coverage_incomplete", func(in *Input) { in.Coverage = covered(at(6, 0), day.Add(24*time.Hour)) }},
		{"decision_write_errors", func(in *Input) { in.ErrorSegments = []Interval{{From: at(8, 0), To: at(8, 1)}} }},
		{"decision_unattributed", func(in *Input) { in.Decisions[3].ActualAccount = "" }},
		{"decision_without_request_id", func(in *Input) { in.Decisions[0].RequestID = "" }},
		{"moved_request_without_usage", func(in *Input) { in.Attempts = in.Attempts[1:] }},
		{"unresolved_value", func(in *Input) { in.Attempts[1].Exact = false }},
		{"moved_request_crosses_cycle_boundary", func(in *Input) { in.Attempts[0].StartedAt = at(4, 59) }},
		{"moved_request_crosses_cycle_boundary", func(in *Input) { in.Attempts[0].EndedAt = time.Time{} }},
		{"ambiguous_join", func(in *Input) {
			retry := dec(at(6, 1), "m1", "in", "", "B", "B", "new_chat")
			in.Decisions = sequence(append(in.Decisions[:1], append([]Decision{retry}, in.Decisions[1:]...)...))
		}},
		{"usage_records_over_limit", func(in *Input) { in.AttemptsUnavailable = "usage_records_over_limit" }},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			in := projectionInput()
			c.change(&in)
			r := build(t, in)
			if len(r.Resets) != 1 {
				t.Fatalf("resets: %+v", r.Resets)
			}
			p := r.Resets[0].Projection
			if p.Known || p.UnusedPercent != nil || p.RawUnusedPercent != nil || !has(p.Reasons, c.reason) {
				t.Fatalf("want unknown with %s: %+v", c.reason, p)
			}
		})
	}
}

func TestMovesOnOtherAccountsDoNotNeedUsage(t *testing.T) {
	in := projectionInput()
	in.Decisions = sequence(append(in.Decisions, dec(at(9, 0), "elsewhere", "", "C", "C", "D", "cold_move")))
	if p := build(t, in).Resets[0].Projection; !p.Known {
		t.Fatalf("a move between other accounts blocked A's projection: %+v", p.Reasons)
	}
}

func TestPartialRoutingModeDayIsIncomplete(t *testing.T) {
	half := Input{Coverage: covered(day, at(12, 0)), Decisions: sequence([]Decision{dec(at(1, 0), "c", "r", "", "A", "A", "new_chat")})}
	r := build(t, half)
	c := r.Meta.Coverage
	if r.Meta.Complete || !has(r.Meta.Reasons, "dry_run_coverage_partial") || len(c) != 1 || c[0].CoveredSeconds != 43200 || c[0].DaySeconds != 86400 || c[0].Complete {
		t.Fatalf("partial day: %+v", r.Meta)
	}
	if len(c[0].Intervals) != 1 || !c[0].Intervals[0].From.Equal(day) || !c[0].Intervals[0].To.Equal(at(12, 0)) {
		t.Fatalf("intervals: %+v", c[0].Intervals)
	}
	errs := Input{Coverage: covered(day.Add(-time.Hour), day.Add(25*time.Hour)), ErrorSegments: []Interval{{From: at(3, 0), To: at(3, 1)}}}
	if r = build(t, errs); r.Meta.Complete || !has(r.Meta.Reasons, "decision_write_errors") {
		t.Fatalf("decision write errors: %+v", r.Meta)
	}
	if r.Meta.Coverage[0].CoveredSeconds != 86400 {
		t.Fatal("coverage is not clipped to the day", r.Meta.Coverage)
	}
	progress := Input{Coverage: covered(day, at(18, 0)), Now: at(18, 0)}
	if r = build(t, progress); r.Meta.Complete || !has(r.Meta.Reasons, "day_in_progress") {
		t.Fatalf("day in progress: %+v", r.Meta)
	}
	unattributed := Input{Coverage: covered(day, day.Add(24*time.Hour)), Decisions: sequence([]Decision{dec(at(1, 0), "c", "r", "", "", "A", "new_chat")})}
	if r = build(t, unattributed); r.Meta.Complete || r.Meta.UnattributedDecisions != 1 || !has(r.Meta.Reasons, "decisions_unattributed") {
		t.Fatalf("unattributed: %+v", r.Meta)
	}
}

func TestBuildRejectsInvalidDay(t *testing.T) {
	for _, in := range []Input{
		{Day: day.Add(time.Hour), Now: day.Add(25 * time.Hour), Secret: secret},
		{Day: day, Now: day.Add(-time.Second), Secret: secret},
	} {
		if _, err := Build(in); err == nil {
			t.Fatalf("accepted %+v", in.Day)
		}
	}
}

func TestBuild100kDecisionsUnderTwoSeconds(t *testing.T) {
	const n = 100000
	accounts := []string{"A", "B", "C"}
	reasons := []string{"new_chat", "protected", "protected", "current_best", "cold_margin", "cold_move", "protected", "forced_move", "no_known_budget", "protected"}
	ds := make([]Decision, 0, n)
	attempts := make([]Attempt, 0, n)
	step := 24 * time.Hour / n
	for i := 0; i < n; i++ {
		when := day.Add(time.Duration(i) * step)
		chat := fmt.Sprintf("chat-%05d", i%20000)
		actual, proposed := accounts[i%3], accounts[(i/3)%3]
		reason := reasons[i%len(reasons)]
		if reason == "no_known_budget" {
			proposed = ""
		}
		request := fmt.Sprintf("request-%06d", i)
		ds = append(ds, dec(when, chat, request, actual, actual, proposed, reason))
		attempts = append(attempts, Attempt{RequestID: request, Account: actual, StartedAt: when, EndedAt: when.Add(time.Second), NanoUSD: 1e7, Exact: true})
	}
	cycles := []Cycle{}
	for h := 5; h <= 20; h += 5 {
		for _, a := range accounts {
			cycles = append(cycles, Cycle{Provider: "claude", Account: a, Scope: "all", Seconds: 5 * 3600, CycleID: fmt.Sprint(a, h), Scheduled: at(h, 0), Closed: at(h, 0), Last: reading(at(h, 0).Add(-time.Minute), 50), Ratio: Ratio{Known: true, USDPerPercent: 0.4}})
		}
	}
	in := Input{Day: day, Now: day.Add(25 * time.Hour), Secret: secret, Decisions: sequence(ds), Attempts: attempts, Cycles: cycles, Coverage: covered(day.Add(-24*time.Hour), day.Add(24*time.Hour))}
	start := time.Now()
	r, err := Build(in)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if r.Meta.Decisions != n || len(r.Resets) != len(cycles) || r.WouldBeMoves.Total == 0 || !r.WouldBeMoves.Truncated {
		t.Fatalf("unexpected report: %+v", r.Meta)
	}
	t.Logf("100k decisions in %v", elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("100k decisions took %v (limit 2s)", elapsed)
	}
}
