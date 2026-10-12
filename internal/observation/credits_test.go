package observation

import (
	"strings"
	"testing"
	"time"
)

func TestCodexCreditsAreSeparateFromSubscriptionLimits(t *testing.T) {
	at := instant("2026-10-12T12:00:00Z")
	for _, tc := range []struct{ body, want string }{
		{`{"credits":{"has_credits":true,"unlimited":false,"balance":"50157.8642475000"},"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1800000000}}}`, "available"},
		{`{"credits":{"has_credits":false,"unlimited":false}}`, "empty"},
		{`{"credits":{"has_credits":false,"unlimited":true}}`, "unlimited"},
		{`{"credits":{"has_credits":true,"unlimited":true}}`, "unlimited"},
		{`{}`, "unknown"}, {`{"credits":null}`, "unknown"}, {`{"credits":[]}`, "unknown"},
		{`{"credits":{"has_credits":true}}`, "unknown"},
		{`{"credits":{"has_credits":"true","unlimited":false}}`, "unknown"},
		{`{"credits":{"balance":"99999999999999999999999999999999999999"}}`, "unknown"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			c := CodexCredits([]byte(tc.body), at)
			if c.State != tc.want || c.ReadMS != at.UnixMilli() || c.ExpiresMS != at.Add(CreditMaxAge).UnixMilli() {
				t.Fatal(c)
			}
			if strings.Contains(tc.body, "rate_limit") {
				windows, err := API("codex", []byte(tc.body), at)
				if err != nil || len(windows) != 1 || *windows[0].Remaining != 0 {
					t.Fatal("credits changed the subscription balance", windows, err)
				}
			}
		})
	}
}

func TestCreditEvidenceSurvivesRestartButNotExpiryOrNewUnknown(t *testing.T) {
	s, clock, path := testStore(t)
	at := clock.Now()
	available := CodexCredits([]byte(`{"credits":{"has_credits":true,"unlimited":false}}`), at)
	if changed, err := s.ObserveCredits("account", available); !changed || err != nil {
		t.Fatal(changed, err)
	}
	if changed, err := s.ObserveCredits("account", available); changed || err != nil {
		t.Fatal("replay", changed, err)
	}
	reopened, _ := openTest(t, path, clock)
	got, err := reopened.ReadCredits("account")
	if err != nil || got != available {
		t.Fatal(got, err)
	}
	if unknown, err := s.ReadCredits("another-account"); err != nil || unknown.State != "unknown" || unknown.ReadMS != 0 {
		t.Fatal(unknown, err)
	}
	clock.Add(CreditMaxAge)
	observe(t, s, "account", "codex", sample(clock.Now(), clock.Now().Add(time.Hour), 0))
	got, err = s.ReadCredits("account")
	if err != nil || got != available || got.At(clock.Now()).State != "stale" || got.At(at.Add(-time.Millisecond)).State != "stale" {
		t.Fatal("headers refreshed credit evidence", got, err)
	}
	unknown := CodexCredits([]byte(`{}`), clock.Now())
	if changed, err := s.ObserveCredits("account", unknown); !changed || err != nil {
		t.Fatal(changed, err)
	}
	if changed, err := s.ObserveCredits("account", available); changed || err != nil {
		t.Fatal("old evidence revived", changed, err)
	}
	if got, _ := s.ReadCredits("account"); got.State != "unknown" || got.ReadMS != unknown.ReadMS {
		t.Fatal(got)
	}
	if _, err := s.ObserveCredits("account", CodexCredits([]byte(`{}`), clock.Now().Add(time.Second))); err == nil {
		t.Fatal("accepted future reading")
	}
}

func TestEqualTimeCreditConflictIsUnknownInBothOrders(t *testing.T) {
	for _, states := range [][]string{{"available", "empty"}, {"empty", "available"}} {
		s, clock, _ := testStore(t)
		for _, state := range states {
			c := Credits{State: state, ReadMS: clock.Now().UnixMilli(), Source: APISource}
			if _, err := s.ObserveCredits("account", c); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := s.ReadCredits("account"); got.State != "unknown" {
			t.Fatal(got)
		}
	}
}
