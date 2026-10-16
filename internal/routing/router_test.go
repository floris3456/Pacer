package routing

import (
	"math"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC)

func short(left float64, in time.Duration) Budget {
	return Budget{Seconds: 18000, Remaining: left, Reset: now.Add(in), Known: true}
}

func weekly(left float64, in time.Duration) Budget {
	return Budget{Seconds: 604800, Remaining: left, Reset: now.Add(in), Known: true}
}

func dormant(seconds int64) Budget {
	return Budget{Seconds: seconds, Remaining: 100, Known: true, Dormant: true}
}

func claude(id string, budgets ...Budget) Account {
	return Account{Auth: id, Account: "acct-" + id, Budgets: budgets}
}

func pick(chat Chat, accounts ...Account) Decision { return Pick(Default(), now, chat, accounts) }

func want(t *testing.T, d Decision, auth, reason string, forced bool) {
	t.Helper()
	if !d.Handled || d.Auth != auth || d.Reason != reason || d.Forced != forced {
		t.Fatalf("want %s/%s forced=%v, got %+v", auth, reason, forced, d)
	}
}

func TestExtraCapacityKeepsOnlyAnOfferedCurrentAccount(t *testing.T) {
	a, b := claude("a", weekly(0, 24*time.Hour)), claude("b", weekly(80, 24*time.Hour))
	a.QuotaExhaustionUncertain = true
	for _, warm := range []bool{false, true} {
		want(t, pick(Chat{Account: a.Account, Warm: warm}, a, b), "a", "credit_status_unscored", false)
	}
	if d := pick(Chat{}, a); d.Handled {
		t.Fatal("invented a quota pace for extra credits", d)
	}
	next, force := Preview(Default(), now, a.Account, []Account{a, b}, 5)
	if next.Auth != "a" || next.Forced || force.Handled {
		t.Fatal("subscription floor advertised a credit-funded handoff", next, force)
	}
	a.Paused = true
	want(t, pick(Chat{Account: a.Account}, a, b), "b", ColdMove, false)
	want(t, pick(Chat{Account: a.Account, Warm: true}, a, b), "a", "credit_status_unscored", false)
	a.NotOffered = true
	d := pick(Chat{Account: a.Account, Warm: true}, a, b)
	if !d.Unoffered || d.Auth != "b" || !d.Forced {
		t.Fatal("unoffered extra capacity must wait or move, never be selected", d)
	}
	a.NotOffered, a.Paused, a.QuotaExhaustionUncertain = false, false, false
	want(t, pick(Chat{Account: a.Account}, a, b), "b", ForcedMove, true)
}

// The design's example: live meters on 2026-10-14 20:00 UTC.
func TestPaceSpendsTheExpiringMeterThenFillsTheBandOneWindowAtATime(t *testing.T) {
	ae0a := claude("ae0a", short(60, 2*time.Hour), weekly(13, 7*time.Hour))
	f0 := claude("70f0", dormant(18000), weekly(86, 159*time.Hour))
	a1655 := claude("1655", short(90, 4*time.Hour), weekly(48, 98*time.Hour))
	a41a := claude("a41a", short(70, 3*time.Hour), weekly(60, 123*time.Hour))
	d := pick(Chat{}, f0, a1655, a41a, ae0a)
	want(t, d, "ae0a", NewChat, false)
	if math.Abs(d.Pace-13.0/7) > 1e-9 || len(d.Candidates) != 4 {
		t.Fatalf("pace or trace: %+v", d)
	}
	// ae0a is used up: the rest are within the band. The running 5-hour window
	// that resets first is spent before the dormant one, which does not expire.
	ae0a.Budgets[0] = short(1, 2*time.Hour)
	want(t, pick(Chat{}, f0, a1655, a41a, ae0a), "a41a", NewChat, false)
	// A chat on 1655 stays within the band.
	want(t, pick(Chat{Account: "acct-1655"}, f0, a1655, a41a, ae0a), "1655", ColdStay, false)
	// Without running windows, the dormant account takes the chat.
	a1655.Budgets[0], a41a.Budgets[0] = dormant(18000), dormant(18000)
	want(t, pick(Chat{}, f0, a1655, a41a), "1655", NewChat, false)
}

func TestColdChatsMoveOutsideTheBandAndWarmChatsStay(t *testing.T) {
	low := claude("low", short(80, 3*time.Hour), weekly(30, 96*time.Hour))
	high := claude("high", short(80, 3*time.Hour), weekly(80, 96*time.Hour))
	want(t, pick(Chat{Account: "acct-low"}, low, high), "high", ColdMove, false)
	want(t, pick(Chat{Account: "acct-low", Warm: true}, low, high), "low", Warm, false)
	// Within 90 % of the best pace, staying wins.
	near := claude("near", short(80, 3*time.Hour), weekly(73, 96*time.Hour))
	want(t, pick(Chat{Account: "acct-near"}, near, high), "near", ColdStay, false)
	// A warm chat keeps an account whose meters are unknown: missing evidence never evicts.
	stale := low
	stale.Budgets = []Budget{short(80, 3*time.Hour), {Seconds: 604800, Remaining: 30, Reset: now.Add(time.Hour)}}
	want(t, pick(Chat{Account: "acct-low", Warm: true}, stale, high), "low", Warm, false)
	want(t, pick(Chat{Account: "acct-low"}, stale, high), "high", ColdMove, false)
}

func TestForcedMovesLeaveAccountsAtTheirFloorOrNotOffered(t *testing.T) {
	high := claude("high", short(80, 3*time.Hour), weekly(80, 96*time.Hour))
	for name, current := range map[string]Account{
		"5-hour floor": claude("cur", short(2, time.Hour), weekly(80, 96*time.Hour)),
		"7-day floor":  claude("cur", short(80, time.Hour), weekly(1, 96*time.Hour)),
	} {
		t.Run(name, func(t *testing.T) {
			want(t, pick(Chat{Account: "acct-cur", Warm: true}, current, high), "high", ForcedMove, true)
		})
	}
	want(t, pick(Chat{Account: "acct-gone", Warm: true}, high), "high", ForcedMove, true)
	// No other account can take a new placement: CPA decides, still forced.
	low := claude("low", short(10, 3*time.Hour), weekly(80, 96*time.Hour))
	d := pick(Chat{Account: "acct-gone"}, low)
	if d.Handled || d.Reason != NoEligible || !d.Forced {
		t.Fatalf("%+v", d)
	}
}

func TestUnofferedWarmAccountWithUsableMetersIsReported(t *testing.T) {
	high := claude("high", short(80, 3*time.Hour), weekly(80, 96*time.Hour))
	jan := claude("jan", short(96, 3*time.Hour), weekly(77, 150*time.Hour))
	jan.NotOffered = true
	d := pick(Chat{Account: "acct-jan", Warm: true}, jan, high)
	want(t, d, "high", ForcedMove, true)
	if !d.Unoffered || len(d.Candidates) != 2 || d.Candidates[0].Eligible || d.Candidates[0].Offered == nil || *d.Candidates[0].Offered {
		t.Fatalf("unoffered usable account: %+v", d)
	}
	// A cold chat, or an account at its floor, is simply moved.
	if d := pick(Chat{Account: "acct-jan"}, jan, high); d.Unoffered || d.Reason != ForcedMove {
		t.Fatalf("cold: %+v", d)
	}
	floor := jan
	floor.Budgets = []Budget{short(2, time.Hour), weekly(77, 150*time.Hour)}
	if d := pick(Chat{Account: "acct-jan", Warm: true}, floor, high); d.Unoffered {
		t.Fatalf("floor: %+v", d)
	}
	// Never chosen, even as the only account.
	if d := pick(Chat{Account: "acct-jan", Warm: true}, jan); d.Handled || !d.Unoffered || d.Reason != NoUsableData {
		t.Fatalf("alone: %+v", d)
	}
	// The offered entry wins over a not-offered alias of the same account.
	offered := claude("jan2", short(96, 3*time.Hour), weekly(77, 150*time.Hour))
	offered.Account = "acct-jan"
	want(t, pick(Chat{Account: "acct-jan", Warm: true}, jan, offered, high), "jan2", Warm, false)
}

func TestPausedAccountsKeepWarmChatsOnly(t *testing.T) {
	best := claude("best", short(80, 3*time.Hour), weekly(13, 7*time.Hour))
	other := claude("other", short(80, 3*time.Hour), weekly(60, 96*time.Hour))
	best.Paused = true
	want(t, pick(Chat{}, best, other), "other", NewChat, false)
	want(t, pick(Chat{Account: "acct-best", Warm: true}, best, other), "best", Warm, false)
	want(t, pick(Chat{Account: "acct-best"}, best, other), "other", ColdMove, false)
	if d := pick(Chat{}, best); d.Handled || d.Reason != NoEligible || !d.Candidates[0].Paused {
		t.Fatalf("all paused: %+v", d)
	}
}

func TestNewPlacementsNeedFifteenPercentOfTheShortWindow(t *testing.T) {
	expiring := claude("exp", short(14, 3*time.Hour), weekly(50, 10*time.Hour))
	other := claude("other", short(15, 3*time.Hour), weekly(50, 96*time.Hour))
	want(t, pick(Chat{}, expiring, other), "other", NewChat, false)
	// The chat already there keeps using it down to the floor.
	want(t, pick(Chat{Account: "acct-exp"}, expiring, other), "exp", ColdStay, false)
	expiring.Budgets[0] = short(3, time.Hour)
	want(t, pick(Chat{Account: "acct-exp"}, expiring, other), "exp", ColdStay, false)
}

func TestUnusableMetersFallBackToCPA(t *testing.T) {
	for name, a := range map[string]Account{
		"stale":          claude("a", short(80, time.Hour), Budget{Seconds: 604800, Remaining: 50, Reset: now.Add(time.Hour)}),
		"reset passed":   claude("a", short(80, -time.Second), weekly(50, time.Hour)),
		"no paced meter": claude("a", short(80, time.Hour)),
		"out of range":   claude("a", short(80, time.Hour), weekly(101, time.Hour)),
		"none":           claude("a"),
	} {
		d := pick(Chat{}, a)
		if d.Handled || d.Reason != NoUsableData || d.Candidates[0].Known {
			t.Fatal(name, d)
		}
		if pace, ok := Pace(Default(), now, a); ok || pace != 0 {
			t.Fatal(name, "pace from unusable meters", pace)
		}
	}
	// A known account at its floor makes the rest "no eligible", not "unusable".
	d := pick(Chat{}, claude("a", short(1, time.Hour), weekly(50, time.Hour)))
	if d.Handled || d.Reason != NoEligible {
		t.Fatal(d)
	}
}

func TestPaceUsesTheSlowestPacedMeterAndAnHourMinimum(t *testing.T) {
	codex := Account{Auth: "c", Account: "acct-c", Budgets: []Budget{weekly(50, 10*time.Minute)}}
	if pace, ok := Pace(Default(), now, codex); !ok || pace != 50 {
		t.Fatal("one-hour minimum horizon", pace, ok)
	}
	// Codex has no 5-hour window: the short conditions are skipped.
	want(t, pick(Chat{}, codex), "c", NewChat, false)
	// A dormant 7-day window has its whole length ahead.
	idle := Account{Auth: "i", Account: "acct-i", Budgets: []Budget{dormant(604800)}}
	if pace, ok := Pace(Default(), now, idle); !ok || math.Abs(pace-100.0/168) > 1e-9 {
		t.Fatal(pace, ok)
	}
	model := claude("m", short(80, time.Hour), weekly(80, 100*time.Hour), weekly(10, 100*time.Hour))
	if pace, _ := Pace(Default(), now, model); math.Abs(pace-0.1) > 1e-9 {
		t.Fatal("model weekly limit ignored", pace)
	}
}

func TestTiesBreakOnLowestAuth(t *testing.T) {
	a := claude("a", short(80, 3*time.Hour), weekly(50, 96*time.Hour))
	b := claude("b", short(80, 3*time.Hour), weekly(50, 96*time.Hour))
	want(t, pick(Chat{}, b, a), "a", NewChat, false)
	// Aliases of the current account count as staying.
	alias := a
	alias.Auth = "0-alias"
	want(t, pick(Chat{Account: "acct-a"}, b, a, alias), "0-alias", ColdStay, false)
}

func TestPreviewNamesTheColdMoveAndAComingForcedMove(t *testing.T) {
	low := claude("low", short(80, 3*time.Hour), weekly(30, 96*time.Hour))
	high := claude("high", short(80, 3*time.Hour), weekly(80, 96*time.Hour))
	next, force := Preview(Default(), now, "acct-low", []Account{low, high}, 5)
	want(t, next, "high", ColdMove, false)
	if force.Handled {
		t.Fatalf("no floor is near: %+v", force)
	}
	// Within the band the chat stays: no NEXT USE.
	if next, _ := Preview(Default(), now, "acct-high", []Account{low, high}, 5); next.Reason != ColdStay || next.Auth != "high" {
		t.Fatalf("%+v", next)
	}
	// 7-day 5 % left with a 1 % floor: within 5 points, the forced move is near.
	high.Budgets[1] = weekly(5, 2*time.Hour)
	next, force = Preview(Default(), now, "acct-high", []Account{low, high}, 5)
	want(t, next, "high", ColdStay, false)
	want(t, force, "low", ForcedMove, true)
	// At the 5-hour floor it is due: both name the forced move.
	high.Budgets[0] = short(2, time.Hour)
	next, force = Preview(Default(), now, "acct-high", []Account{low, high}, 0)
	want(t, next, "low", ForcedMove, true)
	want(t, force, "low", ForcedMove, true)
	// Left out by CPA but able to serve: a warm chat waits for it, nothing forced.
	high.Budgets[0], high.NotOffered = short(80, time.Hour), true
	high.Budgets[1] = weekly(80, 96*time.Hour)
	next, force = Preview(Default(), now, "acct-high", []Account{low, high}, 5)
	want(t, next, "high", ColdStay, false)
	if force.Handled {
		t.Fatalf("transient absence warned: %+v", force)
	}
	// No other account can take the chat: nothing to show.
	low.Budgets[0] = short(1, time.Hour)
	high.NotOffered = false
	high.Budgets[1] = weekly(3, time.Hour)
	if _, force = Preview(Default(), now, "acct-high", []Account{low, high}, 5); force.Handled {
		t.Fatalf("%+v", force)
	}
}
