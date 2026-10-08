package measurement

import (
	"fmt"
	"testing"
	"time"
)

func warmRecord(id, session string, at time.Time, ttl TTL, dollars int64) Record {
	r := record(id, at, 0)
	r.Attribution.SessionID = session
	r.Policy.SelectedTTL = ttl
	r.Policy.EffectiveTTL = ttl
	r.Activity.ProtectionTTL = ttl
	r.Activity.PrefixRef = "primary-prefix"
	// .2 USD read + .8 USD output = 1 USD per requested dollar.
	r.Usage.CacheRead = dollars * 1000000
	r.Usage.Output = dollars * 40000
	return r
}

func TestWarmthRequestStartNotStreamCompletionAndInFlight(t *testing.T) {
	for _, ttl := range []TTL{TTL5m, TTL1h} {
		t.Run(string(ttl), func(t *testing.T) {
			s, _ := fixture(t)
			r := warmRecord("stream", "chat", base, ttl, 1)
			r.EndedAt = base.Add(20 * time.Minute)
			put(t, s, r)
			streaming := measure(t, s, query(base.Add(6*time.Minute)))
			if len(streaming.Stretches) != 1 || !streaming.Stretches[0].InFlight || !streaming.Stretches[0].Warm(streaming.AsOf) {
				t.Fatal(streaming.Stretches)
			}
			done := measure(t, s, query(base.Add(21*time.Minute)))
			st := done.Stretches[0]
			if st.InFlight || !st.ProtectedUntil.Equal(base.Add(ttl.Duration())) || st.Warm(done.AsOf) != (ttl == TTL1h) {
				t.Fatal(st)
			}
			boundary := measure(t, s, query(base.Add(time.Hour)))
			if boundary.Stretches[0].Warm(boundary.AsOf) {
				t.Fatal("TTL boundary must be cold, not completion + TTL", boundary.Stretches[0])
			}
		})
	}
}

func TestOnlySuccessfulSamePrimaryPrefixActivityRefreshes(t *testing.T) {
	for _, kind := range []string{"title", "summary", "failed-primary", "unrelated-warming", "different-prefix-warming", "different-account-warming", "warming"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := fixture(t)
			put(t, s, warmRecord("primary", "chat", base, TTL5m, 1))
			r := warmRecord("later", "chat", base.Add(4*time.Minute), TTL5m, 1)
			switch kind {
			case "title", "summary":
				r.Kind = kind
			case "failed-primary":
				r.Success = false
			case "unrelated-warming":
				r.Kind = "warming"
				r.Activity.PrimaryPrefix = false
			case "different-prefix-warming":
				r.Kind = "warming"
				r.Activity.PrefixRef = "summary-prefix"
			case "different-account-warming":
				r.Kind = "warming"
				r.Attribution.AccountID = "b"
			case "warming":
				r.Kind = "warming"
			}
			put(t, s, r)
			report := measure(t, s, query(base.Add(6*time.Minute)))
			if len(report.Stretches) != 1 {
				t.Fatal(report.Stretches)
			}
			st := report.Stretches[0]
			want := base.Add(5 * time.Minute)
			if kind == "warming" {
				want = base.Add(9 * time.Minute)
			}
			if !st.ProtectedUntil.Equal(want) || st.Warm(report.AsOf) != (kind == "warming") {
				t.Fatal(st)
			}
			if kind == "failed-primary" {
				near(t, st.Value.LowUSD(), 2)
				if st.QualifyingRequests != 1 {
					t.Fatal(st)
				}
			}
		})
	}
}

func TestDistinctTTLPriorsLastVersusP75AndSevenDayCohort(t *testing.T) {
	s, _ := fixture(t)
	for i, cost := range []int64{1, 2, 4, 10} {
		for j := 0; j < 3; j++ {
			at := base.Add(time.Duration(i)*time.Hour + time.Duration(j)*time.Minute)
			put(t, s, warmRecord(fmt.Sprintf("five-%d-%d", i, j), fmt.Sprintf("five-%d", i), at, TTL5m, cost))
		}
	}
	for j := 0; j < 3; j++ {
		put(t, s, warmRecord(fmt.Sprintf("hour-%d", j), "hour", base.Add(time.Duration(j)*time.Minute), TTL1h, 100))
	}
	last := warmRecord("expensive-last", "expensive", base.Add(4*time.Hour), TTL5m, 15)
	put(t, s, last)
	q := query(base.Add(24 * time.Hour))
	report := measure(t, s, q)
	newChat := last.Chat()
	newChat.SessionID = "new"
	x := report.Expected(newChat, TTL5m)
	if !x.Known || x.PopulationSamples != 4 || x.HasLast {
		t.Fatal(x)
	}
	near(t, x.USD, 12)
	x = report.Expected(last.Chat(), TTL5m)
	near(t, x.USD, 15)
	if !x.HasLast {
		t.Fatal(x)
	}
	x = report.Expected(newChat, TTL1h)
	near(t, x.USD, 300)
	if x.PopulationSamples != 1 || x.Confidence != "sparse" {
		t.Fatal(x)
	}
	newEpoch := last.Chat()
	newEpoch.ContextEpoch = "epoch-2"
	x = report.Expected(newEpoch, TTL5m)
	if x.HasLast {
		t.Fatal("new epoch inherited previous context stretch", x)
	}
	near(t, x.USD, 12)
	newAccount := last.Chat()
	newAccount.AccountID = "b"
	x = report.Expected(newAccount, TTL5m)
	if x.HasLast {
		t.Fatal("new account reused transferred cache", x)
	}
	near(t, x.USD, 12)
	old := measure(t, s, query(base.Add(8*24*time.Hour)))
	if x = old.Expected(newChat, TTL5m); x.Known || x.PopulationSamples != 0 {
		t.Fatal("old stretches leaked into seven-day population", x)
	}
}

func TestAccountAndEpochSwitchesSplitStretchesAndLateUsageStaysOld(t *testing.T) {
	s, _ := fixture(t)
	first := warmRecord("first", "chat", base, TTL1h, 1)
	put(t, s, first)
	second := warmRecord("second", "chat", base.Add(time.Minute), TTL1h, 2)
	second.Attribution.AccountID = "b"
	put(t, s, second)
	third := warmRecord("third", "chat", base.Add(2*time.Minute), TTL1h, 3)
	third.Policy.ContextEpoch = "epoch-2"
	third.Policy.Revision = 8
	put(t, s, third)
	first.Revision = 2
	first.Usage.Output += 50000
	put(t, s, first)
	r := measure(t, s, query(base.Add(3*time.Minute)))
	if len(r.Stretches) != 3 || r.Stretches[0].Current || r.Stretches[1].Current || !r.Stretches[2].Current {
		t.Fatal(r.Stretches)
	}
	if r.Stretches[2].ContextEpoch != "epoch-2" || !r.Stretches[2].ProtectedUntil.Equal(third.StartedAt.Add(time.Hour)) {
		t.Fatal("late old usage refreshed current epoch", r.Stretches)
	}
	near(t, r.Stretches[0].Value.LowUSD(), 2)
	near(t, r.Stretches[2].Value.LowUSD(), 3)
	if r.WarmBurn.WarmChats != 1 {
		t.Fatal(r.WarmBurn)
	}
}

func TestUnknownAndPartialWarmthDoNotDefaultToFiveMinutes(t *testing.T) {
	for _, test := range []string{"unknown", "partial", "mixed-longer-prefix"} {
		t.Run(test, func(t *testing.T) {
			s, _ := fixture(t)
			r := warmRecord("unknown", "chat", base, TTL1h, 1)
			switch test {
			case "unknown":
				r.Policy.EffectiveTTL = ""
				r.Activity.ProtectionTTL = ""
			case "partial":
				r.Activity.Partial = true
			case "mixed-longer-prefix":
				r.Activity.ProtectionTTL = TTL5m
			}
			put(t, s, r)
			report := measure(t, s, query(base.Add(6*time.Minute)))
			st := report.Stretches[0]
			if !st.Warm(report.AsOf) {
				t.Fatal("early cold move permitted", st)
			}
			if test != "mixed-longer-prefix" && (!st.UnknownWarmth || report.WarmBurn.Bounded || !report.WarmBurn.Runway(100, report.AsOf, report.AsOf.Add(time.Hour)).BlocksNew) {
				t.Fatal(st, report.WarmBurn)
			}
			if test == "mixed-longer-prefix" && !st.ProtectedUntil.Equal(base.Add(time.Hour)) {
				t.Fatal(st)
			}
		})
	}
}

func TestBurnRunwayBoundedUnknownWritesAndDeliveryGaps(t *testing.T) {
	s, _ := fixture(t)
	r := warmRecord("burn", "chat", base, TTL1h, 1)
	put(t, s, r)
	report := measure(t, s, query(base.Add(30*time.Minute)))
	if !report.WarmBurn.Known || !report.WarmBurn.Bounded {
		t.Fatal(report.WarmBurn)
	}
	near(t, report.WarmBurn.HighUSDPerHour, 2)
	runway := report.WarmBurn.Runway(1, report.AsOf, report.AsOf.Add(time.Hour))
	near(t, runway.Hours, .5)
	if !runway.Known || !runway.BlocksNew {
		t.Fatal(runway)
	}
	r.Revision = 2
	r.Usage.CacheCreate = 1000000
	put(t, s, r)
	report = measure(t, s, query(base.Add(30*time.Minute)))
	if report.WarmBurn.Known || !report.WarmBurn.Bounded {
		t.Fatal(report.WarmBurn)
	}
	near(t, report.WarmBurn.LowUSDPerHour, 12)
	near(t, report.WarmBurn.HighUSDPerHour, 18)
	if err := s.PutGap(ctx, Gap{ID: "dropped", From: base.Add(10 * time.Minute), To: base.Add(11 * time.Minute), Reason: "callback_overflow"}); err != nil {
		t.Fatal(err)
	}
	report = measure(t, s, query(base.Add(30*time.Minute)))
	if report.WarmBurn.Bounded || !report.WarmBurn.Runway(100, report.AsOf, report.AsOf.Add(time.Hour)).BlocksNew {
		t.Fatal(report.WarmBurn)
	}
}

func TestMissingDeliveryCannotClaimZeroWarmBurnAndLeftCensoredPrior(t *testing.T) {
	s, db := fixture(t)
	if _, err := db.Exec(`DELETE FROM measurement_delivery_runs`); err != nil {
		t.Fatal(err)
	}
	report := measure(t, s, query(base.Add(time.Hour)))
	if report.WarmBurn.Bounded || report.WarmBurn.Known {
		t.Fatal("no delivery evidence became zero load", report.WarmBurn)
	}
	s, _ = fixture(t)
	for i := 0; i < 3; i++ {
		put(t, s, warmRecord(fmt.Sprint(i), "censored", base.Add(-time.Minute+time.Duration(i)*time.Minute), TTL1h, 1))
	}
	report = measure(t, s, query(base.Add(2*time.Hour)))
	if len(report.Stretches) != 1 || !report.Stretches[0].LeftCensored || report.Stretches[0].PriorEligible {
		t.Fatal(report.Stretches)
	}
}

func TestUnknownLatestStretchCannotUseOlderCheapLast(t *testing.T) {
	s, _ := fixture(t)
	first := warmRecord("known-old", "same-chat", base, TTL5m, 1)
	put(t, s, first)
	latest := warmRecord("unknown-new", "same-chat", base.Add(time.Hour), TTL5m, 1)
	latest.Usage.CacheCreate = 1000000
	put(t, s, latest)
	for i := 0; i < 3; i++ {
		put(t, s, warmRecord(fmt.Sprintf("population-%d", i), "population", base.Add(time.Duration(i)*time.Minute), TTL5m, 1))
	}
	report := measure(t, s, query(base.Add(2*time.Hour)))
	x := report.Expected(latest.Chat(), TTL5m)
	if x.Known || !x.HasLast || x.LastKnown || x.Confidence != "unknown_last_stretch" || x.PopulationSamples != 1 {
		t.Fatal(x)
	}
}
