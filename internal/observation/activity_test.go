package observation

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func directWindow(at, reset time.Time, scope string, seconds int64, remaining float64) Window {
	w := Window{Provider: "antigravity", Scope: scope, Seconds: seconds, Remaining: &remaining, Usage: Field{At: at, Source: APISource, Quality: Measured}}
	if !reset.IsZero() {
		w.Reset, w.ResetEvidence = reset, Field{At: at, Source: APISource, Quality: Absolute}
	}
	return w
}

// read starts the due read of a direct-only account, records its answer and
// finishes it. A bucket without a running window has no reset.
func directRead(t *testing.T, s *Store, c *fakeClock, account string) {
	t.Helper()
	j := acquire(t, s, account, true)
	now := c.Now()
	observe(t, s, account, "antigravity",
		directWindow(now, now.Add(3*time.Hour), "gemini", 18000, 80),
		directWindow(now, time.Time{}, "gemini", 604800, 100))
	finish(t, s, j, 200, nil)
}

func TestDirectOnlyMeterReadEveryThirtyMinutesAndAfterBursts(t *testing.T) {
	s, c, _ := testStore(t)
	queue(t, s, "ag", "antigravity", ReasonSeed, time.Time{})
	c.Set(status(t, s, "ag").Next)
	directRead(t, s, c, "ag")
	read := c.Now()

	// A fresh read covers the account: no read at the 15-minute floor.
	if j := status(t, s, "ag"); j.State != "waiting" || j.Reason != ReasonIdle || !j.Next.Equal(read.Add(30*time.Minute)) {
		t.Fatalf("after a fresh read: %v (want an idle read 30 minutes later)", j)
	}
	c.Set(read.Add(16 * time.Minute))
	acquire(t, s, "ag", false)

	// Requests served after the read: one read 5 minutes after the last one.
	if err := s.Activity("ag", "antigravity", c.Now()); err != nil {
		t.Fatal(err)
	}
	c.Set(read.Add(18 * time.Minute))
	if err := s.Activity("ag", "antigravity", c.Now()); err != nil {
		t.Fatal(err)
	}
	if j := status(t, s, "ag"); j.Reason != ReasonActivity || !j.Next.Equal(read.Add(23*time.Minute)) {
		t.Fatalf("after a burst: %v (want a read 5 minutes after the last request)", j)
	}
	c.Set(read.Add(23*time.Minute - time.Nanosecond))
	acquire(t, s, "ag", false)
	c.Set(read.Add(23 * time.Minute))
	directRead(t, s, c, "ag")
	read = c.Now()
	if j := status(t, s, "ag"); j.Reason != ReasonIdle || !j.Next.Equal(read.Add(30*time.Minute)) {
		t.Fatalf("activity before the read still pending: %v", j)
	}

	// A request just after a read waits for the account floor.
	c.Set(read.Add(time.Minute))
	if err := s.Activity("ag", "antigravity", c.Now()); err != nil {
		t.Fatal(err)
	}
	if j := status(t, s, "ag"); !j.Next.Equal(read.Add(AccountMinGap)) || j.Blocked != "account-spacing" {
		t.Fatalf("activity bypassed the account floor: %v", j)
	}
	c.Set(read.Add(AccountMinGap))
	directRead(t, s, c, "ag")
	read = c.Now()

	// Steady use cannot postpone the read past 30 minutes.
	for minute := 1; minute <= 40; minute++ {
		c.Set(read.Add(time.Duration(minute) * time.Minute))
		if err := s.Activity("ag", "antigravity", c.Now()); err != nil {
			t.Fatal(err)
		}
		if minute < 30 {
			acquire(t, s, "ag", false)
		} else if minute == 30 {
			directRead(t, s, c, "ag")
			read = c.Now()
			break
		}
	}
	if !read.Equal(status(t, s, "ag").Started) {
		t.Fatal("steady use postponed the 30-minute read")
	}
}

func TestActivityIgnoresProvidersWithQuotaInResponses(t *testing.T) {
	s, _, _ := testStore(t)
	for _, provider := range []string{"claude", "codex"} {
		if err := s.Activity("a-"+provider, provider, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if _, exists, err := s.Status("a-" + provider); err != nil || exists {
			t.Fatalf("%s activity scheduled a read: %v %v", provider, exists, err)
		}
	}
	if err := s.Activity("og", "opencode-go", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if j := status(t, s, "og"); j.Endpoint != "opencode-go:usage" || j.Reason != ReasonActivity {
		t.Fatalf("first OpenCode Go request: %v", j)
	}
}

func TestOpenAddsActivityToOldJobsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE observation_jobs (
 account TEXT PRIMARY KEY, endpoint TEXT NOT NULL, reason TEXT NOT NULL,
 due INTEGER NOT NULL, state TEXT NOT NULL, token TEXT NOT NULL DEFAULT '',
 started INTEGER NOT NULL DEFAULT 0, joined INTEGER NOT NULL DEFAULT 0,
 cancelled INTEGER NOT NULL DEFAULT 0);
INSERT INTO observation_jobs(account,endpoint,reason,due,state) VALUES('ag','antigravity:quota','idle',1,'waiting');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	c := newClock()
	s, _ := openTest(t, path, c)
	if err := s.Activity("ag", "antigravity", c.Now()); err != nil {
		t.Fatal(err)
	}
	if j := status(t, s, "ag"); !j.Activity.Equal(c.Now()) {
		t.Fatalf("activity not stored after upgrade: %v", j)
	}
	// Opening again leaves the upgraded table alone.
	openTest(t, path, c)
}
