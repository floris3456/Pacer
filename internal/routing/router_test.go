package routing

import (
	"testing"
	"time"
)

var instant = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func account(id string, left, days float64) Account {
	return Account{Auth: id, Account: id, Budgets: []Budget{{18000, 80, instant.Add(3 * time.Hour), true}, {604800, left, instant.Add(time.Duration(days*24) * time.Hour), true}}}
}

func TestPacePreservesWeeklyCapacityAndSpendsExpiringBudget(t *testing.T) {
	for _, tc := range []struct {
		days float64
		want string
	}{{4, "b"}, {1, "a"}} {
		d := Pick(Default(), instant, Chat{}, []Account{account("a", 30, tc.days), account("b", 80, 4)})
		if !d.Handled || d.Auth != tc.want {
			t.Fatalf("days=%v: %+v", tc.days, d)
		}
	}
}

func TestWarmAndUnknownContextsStayColdMovesNeedMargin(t *testing.T) {
	a, b := account("a", 30, 4), account("b", 80, 4)
	for _, chat := range []Chat{{Account: "a", ProtectedUntil: instant.Add(time.Hour)}, {Account: "a", UnknownWarmth: true}, {Account: "a", InFlight: true}} {
		if d := Pick(Default(), instant, chat, []Account{a, b}); d.Auth != "a" || d.Forced {
			t.Fatal(d)
		}
	}
	if d := Pick(Default(), instant, Chat{Account: "a", ProtectedUntil: instant}, []Account{a, b}); d.Auth != "b" || d.Reason != "cold_move" || d.Forced {
		t.Fatal(d)
	}
	b.Budgets[1].Remaining = 37
	if d := Pick(Default(), instant, Chat{Account: "a"}, []Account{a, b}); d.Auth != "a" {
		t.Fatal(d)
	}
}

func TestForcedMovesOnlyForUnavailableOrConfirmedFloor(t *testing.T) {
	a, b := account("a", 1, 4), account("b", 80, 4)
	chat := Chat{Account: "a", ProtectedUntil: instant.Add(time.Hour)}
	for _, candidates := range [][]Account{{a, b}, {b}} {
		if d := Pick(Default(), instant, chat, candidates); d.Auth != "b" || !d.Forced {
			t.Fatal(d)
		}
	}
	a.Budgets[1].Known = false
	if d := Pick(Default(), instant, chat, []Account{a, b}); d.Auth != "a" || d.Forced {
		t.Fatal(d)
	}
	a.Budgets[1].Known = true
	a.Budgets[1].Reset = instant
	if d := Pick(Default(), instant, Chat{}, []Account{a}); d.Handled {
		t.Fatal("expiry manufactured quota", d)
	}
}

func TestPlacementFitRunwayAndFallbackDoNotEvictWarmChats(t *testing.T) {
	a, b := account("a", 80, 1), account("b", 60, 4)
	a.ShortValue = .4
	a.WarmBurn = 20
	b.ShortValue = .4
	if d := Pick(Default(), instant, Chat{ExpectedValue: 4}, []Account{a, b}); d.Auth != "b" {
		t.Fatal(d)
	}
	if d := Pick(Default(), instant, Chat{Account: "a", ProtectedUntil: instant.Add(time.Hour)}, []Account{a, b}); d.Auth != "a" {
		t.Fatal(d)
	}
	b.Budgets[0].Remaining = 10
	if d := Pick(Default(), instant, Chat{ExpectedValue: 50}, []Account{a, b}); d.Auth != "a" {
		t.Fatal(d)
	}
	if d := Pick(Default(), instant, Chat{Account: "b", ExpectedValue: 50}, []Account{a, b}); d.Auth != "b" {
		t.Fatal("fallback caused discretionary move", d)
	}
}

func TestModelBudgetUnknownsDailyDurationsAndKeyTies(t *testing.T) {
	a, b := account("a", 80, 4), account("b", 78, 4)
	b.KeyWarm = true
	if d := Pick(Default(), instant, Chat{}, []Account{a, b}); d.Auth != "b" {
		t.Fatal(d)
	}
	a.Budgets = append(a.Budgets, Budget{604800, 1, instant.Add(24 * time.Hour), true})
	if d := Pick(Default(), instant, Chat{}, []Account{a, b}); d.Auth != "b" {
		t.Fatal(d)
	}
	a = account("a", 80, 4)
	a.Budgets[0].Seconds = 86400
	a.Budgets[0].Remaining = 2
	if d := Pick(Default(), instant, Chat{}, []Account{a}); d.Handled {
		t.Fatal("daily floor ignored", d)
	}
	a = account("a", 80, 4)
	a.Budgets[1].Known = false
	if d := Pick(Default(), instant, Chat{}, []Account{a, b}); d.Auth != "b" {
		t.Fatal(d)
	}
}
