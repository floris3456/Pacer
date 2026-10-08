package routing

import (
	"math"
	"testing"
	"time"
)

func TestFallbackNeverPrefersUnknownOrUnboundedToKnownRoom(t *testing.T) {
	for _, change := range []func(*Account){
		func(a *Account) { a.Budgets[0].Known = false },
		func(a *Account) { a.Budgets[1].Known = false },
		func(a *Account) { a.Budgets[1].Reset = instant },
		func(a *Account) { a.RunwayUnknown = true },
		func(a *Account) { a.ShortValue = .4; a.WarmBurn = 20 },
	} {
		a, b := account("a", 80, 1), account("b", 20, 4)
		change(&a)
		b.Budgets[0].Remaining = 10 // Does not fit the 15% default; still the safe fallback.
		for _, accounts := range [][]Account{{a, b}, {b, a}} {
			if d := Pick(Default(), instant, Chat{}, accounts); !d.Handled || d.Auth != "b" {
				t.Fatal(d)
			}
		}
		if d := Pick(Default(), instant, Chat{}, []Account{a}); d.Handled {
			t.Fatal("all-unknown/unbounded fallback placed a new chat", d)
		}
		if d := Pick(Default(), instant, Chat{Account: "a", ProtectedUntil: instant.Add(time.Hour)}, []Account{a, b}); !d.Handled || d.Auth != "a" || d.Forced {
			t.Fatal("uncertainty evicted a warm chat", d)
		}
	}
}

func TestInvalidRunwayMeasurementsCannotBecomeZeroLoad(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		for _, field := range []string{"short_value", "warm_burn"} {
			a, b := account("a", 80, 1), account("b", 20, 4)
			a.ShortValue = .4
			if field == "short_value" {
				a.ShortValue = bad
			} else {
				a.WarmBurn = bad
			}
			if d := Pick(Default(), instant, Chat{}, []Account{a, b}); !d.Handled || d.Auth != "b" {
				t.Fatalf("%s=%v: %+v", field, bad, d)
			}
			if d := Pick(Default(), instant, Chat{}, []Account{a}); d.Handled {
				t.Fatal("invalid measurement authorized fallback", d)
			}
			if d := Pick(Default(), instant, Chat{Account: "a", UnknownWarmth: true}, []Account{a, b}); !d.Handled || d.Auth != "a" || d.Forced {
				t.Fatal(d)
			}
		}
	}
	// Zero is still the documented unmeasured percentage/headroom fallback.
	a := account("a", 80, 1)
	if d := Pick(Default(), instant, Chat{}, []Account{a}); !d.Handled {
		t.Fatal(d)
	}
}

func TestForcedEmergencyAndColdFallbackHaveDifferentPlacementContracts(t *testing.T) {
	a, b := account("a", 30, 4), account("b", 80, 1)
	b.RunwayUnknown = true
	if d := Pick(Default(), instant, Chat{Account: "a"}, []Account{a, b}); !d.Handled || d.Auth != "a" || d.Forced {
		t.Fatal("cold optimization used an unbounded runway", d)
	}
	// An unavailable current account forces a host recovery path, not a new/cold
	// optimization. The adapter must keep compaction gating for this decision.
	if d := Pick(Default(), instant, Chat{Account: "a", UnknownWarmth: true}, []Account{b}); !d.Handled || d.Auth != "b" || !d.Forced {
		t.Fatal(d)
	}
	b.Budgets[1].Known = false
	if d := Pick(Default(), instant, Chat{Account: "a"}, []Account{b}); d.Handled || !d.Forced {
		t.Fatal("forced fallback manufactured quota", d)
	}
}
