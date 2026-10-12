package observation

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCreditBalancePreservesBoundedProviderDecimals(t *testing.T) {
	at := instant("2026-10-12T12:00:00Z")
	for _, tc := range []struct{ raw, want string }{
		{`"50157.8642475000"`, "50157.8642475"}, {`0`, "0"}, {`"001.200"`, "1.2"},
		{`"0.0001"`, "0.0001"}, {`"999999999999999999"`, "999999999999999999"},
		{`null`, ""}, {`true`, ""}, {`{}`, ""}, {`-1`, ""}, {`"NaN"`, ""},
		{`"1e9"`, ""}, {`"9999999999999999999"`, ""}, {`"1\u001b"`, ""},
	} {
		c := CodexCredits([]byte(fmt.Sprintf(`{"credits":{"has_credits":true,"unlimited":false,"balance":%s}}`, tc.raw)), at)
		if c.State != "available" || c.Balance != tc.want {
			t.Fatalf("%s: %+v", tc.raw, c)
		}
	}
}

func TestCreditBalanceMigrationPersistenceAndConflicts(t *testing.T) {
	_, clock, path := testStore(t)
	db, err := sql.Open("sqlite3", path+".credits.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE observation_credits (account TEXT PRIMARY KEY, state TEXT NOT NULL, observed INTEGER NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO observation_credits VALUES ('account','available',?)`, clock.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenCredits(db, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadCredits("account"); err != nil || got.State != "available" || got.Balance != "" {
		t.Fatal(got, err)
	}
	clock.Add(time.Second)
	sample := Credits{State: "available", ReadMS: clock.Now().UnixMilli(), Source: APISource, Balance: "12.34"}
	if changed, err := s.ObserveCredits("account", sample); !changed || err != nil {
		t.Fatal(changed, err)
	}
	reopened, err := OpenCredits(db, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.ReadCredits("account"); err != nil || got.Balance != "12.34" {
		t.Fatal(got, err)
	}
	sample.Balance = "56.78"
	if changed, err := s.ObserveCredits("account", sample); !changed || err != nil {
		t.Fatal(changed, err)
	}
	if got, _ := s.ReadCredits("account"); got.State != "unknown" || got.Balance != "" {
		t.Fatal("conflict", got)
	}
	clock.Add(time.Second)
	sample.ReadMS, sample.Balance = clock.Now().UnixMilli(), ""
	if _, err := s.ObserveCredits("account", sample); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadCredits("account"); got.State != "available" || got.Balance != "" {
		t.Fatal("missing balance revived old amount", got)
	}
}

func openTestCredits(t *testing.T, path string, clock *fakeClock) *CreditStore {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := OpenCredits(db, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

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
	windows, clock, path := testStore(t)
	path += ".credits.sqlite"
	s := openTestCredits(t, path, clock)
	at := clock.Now()
	available := CodexCredits([]byte(`{"credits":{"has_credits":true,"unlimited":false}}`), at)
	if changed, err := s.ObserveCredits("account", available); !changed || err != nil {
		t.Fatal(changed, err)
	}
	if changed, err := s.ObserveCredits("account", available); changed || err != nil {
		t.Fatal("replay", changed, err)
	}
	reopened := openTestCredits(t, path, clock)
	got, err := reopened.ReadCredits("account")
	if err != nil || got != available {
		t.Fatal(got, err)
	}
	if unknown, err := s.ReadCredits("another-account"); err != nil || unknown.State != "unknown" || unknown.ReadMS != 0 {
		t.Fatal(unknown, err)
	}
	clock.Add(CreditMaxAge)
	observe(t, windows, "account", "codex", sample(clock.Now(), clock.Now().Add(time.Hour), 0))
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
		_, clock, path := testStore(t)
		s := openTestCredits(t, path+".credits.sqlite", clock)
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
