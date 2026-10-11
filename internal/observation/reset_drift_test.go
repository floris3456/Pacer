package observation

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexResetCorrectionKeepsLiveBalance(t *testing.T) {
	s, c, path := testStore(t)
	reset := instant("2026-10-18T09:33:59Z")
	c.Set(instant("2026-10-11T13:00:00Z"))
	observe(t, s, "account", "codex", sample(c.Now(), reset, 71))
	initial := s.Windows("account")[0]
	for _, reading := range []struct {
		offset time.Duration
		left   float64
	}{{3 * time.Second, 53}, {12 * time.Second, 43}, {0, 3}} {
		c.Add(time.Hour)
		observe(t, s, "account", "codex", sample(c.Now(), reset.Add(reading.offset), reading.left))
		s, _ = openTest(t, path, c)
		got := s.Windows("account")[0]
		if *got.Remaining != reading.left || !got.Reset.Equal(reset.Add(reading.offset)) || got.Cycle != initial.Cycle || !got.CycleAt.Equal(initial.CycleAt) || !got.FreshComplete(c.Now(), time.Hour) {
			t.Fatalf("timer correction froze or replaced the quota cycle: %#v", got)
		}
	}
	// A delayed source sample must not restore the pre-correction balance.
	observe(t, s, "account", "codex", sample(initial.Usage.At, reset, 71))
	if got := s.Windows("account")[0]; *got.Remaining != 3 {
		t.Fatalf("delayed reading won: %#v", got)
	}
}

func TestCodexResetCorrectionDoesNotReviveReplenishedCycle(t *testing.T) {
	s, c, path := testStore(t)
	reset := c.Now().Add(7 * 24 * time.Hour)
	observe(t, s, "account", "codex", sample(c.Now(), reset, 3))
	c.Add(time.Minute)
	// Even a nearby deadline is a new cycle when quota replenished.
	observe(t, s, "account", "codex", sample(c.Now(), reset.Add(12*time.Second), 95))
	s, _ = openTest(t, path, c)
	c.Add(time.Minute)
	observe(t, s, "account", "codex", sample(c.Now(), reset, 2))
	got := s.Windows("account")[0]
	if *got.Remaining != 95 || !got.Reset.Equal(reset.Add(12*time.Second)) {
		t.Fatalf("retired cycle revived: %#v", got)
	}
	h, _ := s.History("account", 0, 100)
	if h[len(h)-1].Disposition != "stale-cycle" {
		t.Fatal("missing retired-cycle refusal", h)
	}
}

func TestMigrateCodexResetDrift(t *testing.T) {
	for _, mode := range []string{"correction", "genuine-cycle", "missing-history", "unexplained-marker"} {
		t.Run(mode, func(t *testing.T) {
			c := newClock()
			s, db := openTest(t, filepath.Join(t.TempDir(), "state.sqlite"), c)
			reset := c.Now().Add(7 * 24 * time.Hour)
			first := sample(c.Now(), reset, 53)
			c.Add(time.Hour)
			left := 43.0
			if mode == "genuine-cycle" {
				left = 95
			}
			second := sample(c.Now(), reset.Add(12*time.Second), left)
			// Seed the old release's actual projection and append-only audit. Do
			// not manufacture the regression by calling the corrected merge logic.
			for i, w := range []Window{first, second} {
				raw, _ := json.Marshal(w)
				disposition := []string{"accepted", "accepted-cycle"}[i]
				if _, err := db.Exec("INSERT INTO observation_history(account,key,received_at,sample,disposition) VALUES(?,?,?,?,?)", "account", w.Key(), w.ObservedAt().UnixNano(), string(raw), disposition); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(cycle(second))
			if _, err := db.Exec("INSERT INTO observation_windows(account,key,provider,body) VALUES(?,?,'codex',?)", "account", second.Key(), string(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("INSERT INTO observation_retired(account,key,reset) VALUES(?,?,?)", "account", first.Key(), reset.UnixNano()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("DELETE FROM observation_migrations WHERE id='codex-reset-drift-v1'"); err != nil {
				t.Fatal(err)
			}
			if mode == "missing-history" {
				if _, err := db.Exec("DELETE FROM observation_history"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unexplained-marker" {
				if _, err := db.Exec("INSERT INTO observation_retired(account,key,reset) VALUES(?,?,?)", "account", first.Key(), reset.Add(-time.Second).UnixNano()); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.History("account", 0, 100)
			for i := 0; i < 2; i++ {
				var err error
				s, err = Open(db, c.Now)
				if err != nil {
					t.Fatal(err)
				}
			}
			after, _ := s.History("account", 0, 100)
			oldJSON, _ := json.Marshal(before)
			newJSON, _ := json.Marshal(after)
			if string(oldJSON) != string(newJSON) {
				t.Fatal("migration rewrote audit evidence")
			}
			c.Add(time.Hour)
			observe(t, s, "account", "codex", sample(c.Now(), reset, 3))
			got := s.Windows("account")[0]
			want := 3.0
			if mode == "genuine-cycle" {
				want = 95
			}
			if mode == "missing-history" || mode == "unexplained-marker" {
				want = 43
			}
			if *got.Remaining != want {
				t.Fatalf("migration balance = %v, want %v", *got.Remaining, want)
			}
		})
	}
}

func TestCodexResetCorrectionEvidenceBounds(t *testing.T) {
	for _, mode := range []string{"minute", "over-minute", "older", "equal-time", "reset-only", "replenished", "expired", "estimated", "claude"} {
		t.Run(mode, func(t *testing.T) {
			s, c, _ := testStore(t)
			first := sample(c.Now(), c.Now().Add(7*24*time.Hour), 43)
			if mode == "claude" {
				first.Provider = "claude"
			}
			observe(t, s, "account", first.Provider, first)
			initial := s.Windows("account")[0]
			c.Add(time.Hour)
			next := sample(c.Now(), first.Reset.Add(time.Minute), 3)
			next.Provider = first.Provider
			switch mode {
			case "over-minute":
				next.Reset = next.Reset.Add(time.Nanosecond)
			case "older":
				next.Usage.At, next.ResetEvidence.At = first.Usage.At.Add(-time.Second), first.Usage.At.Add(-time.Second)
			case "equal-time":
				next.Usage.At, next.ResetEvidence.At = first.Usage.At, first.Usage.At
			case "reset-only":
				next.Remaining, next.Usage = nil, Field{}
			case "replenished":
				left := 95.0
				next.Remaining = &left
			case "expired":
				c.Set(first.Reset.Add(time.Second))
				next.Usage.At, next.ResetEvidence.At = c.Now(), c.Now()
			case "estimated":
				next.ResetEvidence.Quality = Estimated
			}
			observe(t, s, "account", next.Provider, next)
			got := s.Windows("account")[0]
			corrected := got.Cycle == initial.Cycle && got.Remaining != nil && *got.Remaining == 3 && got.Usage.At.Equal(next.Usage.At) && got.Reset.Equal(next.Reset) && !got.Conflict
			if corrected != (mode == "minute") {
				t.Fatalf("unexpected correction: %#v", got)
			}
		})
	}
}

func TestCodexResetCorrectionCannotWalkTheDeadline(t *testing.T) {
	s, c, _ := testStore(t)
	reset := c.Now().Add(7 * 24 * time.Hour)
	observe(t, s, "account", "codex", sample(c.Now(), reset, 50))
	initial := s.Windows("account")[0]
	for i := 1; i <= 3; i++ {
		c.Add(time.Minute)
		observe(t, s, "account", "codex", sample(c.Now(), reset.Add(time.Duration(i)*30*time.Second), 50-float64(i)))
		got := s.Windows("account")[0]
		if (got.Cycle == initial.Cycle) != (i <= 2) {
			t.Fatalf("correction budget walked with each deadline: %#v", got)
		}
	}
}
