package observation

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCodexPremiumHeadersAndAPIMerge(t *testing.T) {
	s, c, _ := testStore(t)
	at := c.Now()
	windows, err := API("codex", []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":67,"reset_after_seconds":604800}}}`), at)
	if err != nil {
		t.Fatal(err)
	}
	observe(t, s, "account", "codex", windows...)
	c.Add(3 * time.Minute)
	windows = Headers("codex", http.Header{
		"X-Codex-Active-Limit":                {"premium"},
		"X-Codex-Primary-Window-Minutes":      {"10080"},
		"X-Codex-Primary-Used-Percent":        {"69"},
		"X-Codex-Primary-Reset-After-Seconds": {"604623"},
	}, c.Now())
	observe(t, s, "account", "codex", windows...)
	got := s.Windows("account")
	if len(got) != 1 || got[0].Scope != "all" || *got[0].Remaining != 31 || got[0].Usage.Source != HeaderSource {
		t.Fatalf("duplicate or stale weekly allowance: %#v", got)
	}
	for _, name := range []string{"premium", "spark", "custom-gpu"} {
		data := []byte(`{"metered_limit_name":"` + name + `","rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":69}}}`)
		got, err := API("codex", data, at)
		want := map[string]string{"premium": "all", "spark": "spark", "custom-gpu": "limit:custom-gpu"}[name]
		if err != nil || len(got) != 1 || got[0].Scope != want {
			t.Fatalf("%s: %#v, %v", name, got, err)
		}
	}
}

func TestMigrateCodexPremium(t *testing.T) {
	for _, mode := range []string{"newer-header", "newer-api", "alias-only", "retired"} {
		t.Run(mode, func(t *testing.T) {
			c := newClock()
			s, db := openTest(t, filepath.Join(t.TempDir(), "state.sqlite"), c)
			at := c.Now()
			api := sample(at, at.Add(7*24*time.Hour), 33)
			alias := sample(at.Add(3*time.Minute), api.Reset.Add(3*time.Second), 31)
			alias.Scope = "limit:premium"
			want := 31.0
			if mode == "newer-api" {
				api.Usage.At = at.Add(4 * time.Minute)
				api.ResetEvidence.At = api.Usage.At
				want = 33
			}
			if mode != "alias-only" {
				observe(t, s, "account", "codex", api)
			}
			raw, _ := json.Marshal(alias)
			if _, err := db.Exec("INSERT INTO observation_windows(account,key,provider,body) VALUES(?,?,'codex',?)", "account", alias.Key(), string(raw)); err != nil {
				t.Fatal(err)
			}
			if mode == "retired" {
				want = 33
				if _, err := db.Exec("INSERT INTO observation_retired(account,key,reset) VALUES(?,?,?)", "account", api.Key(), alias.Reset.UnixNano()); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				migrated, err := Open(db, c.Now)
				if err != nil {
					t.Fatal(err)
				}
				got := migrated.Windows("account")
				if len(got) != 1 || got[0].Scope != "all" || *got[0].Remaining != want {
					t.Fatalf("migration %d: %#v", i, got)
				}
			}
		})
	}
}

func insertPremiumAlias(t *testing.T, db *sql.DB, alias Window) {
	t.Helper()
	raw, err := json.Marshal(alias)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO observation_windows(account,key,provider,body) VALUES(?,?,'codex',?)", "account", alias.Key(), string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateCodexPremiumCycleRetirement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		delta   time.Duration
		retired int
	}{
		{"same-reset", 0, 0},
		{"same-cycle-jitter", CycleTolerance, 0},
		{"earlier-cycle", -24 * time.Hour, 2},
		{"later-cycle", 24 * time.Hour, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClock()
			path := filepath.Join(t.TempDir(), "state.sqlite")
			s, db := openTest(t, path, c)
			control, _, _ := testStore(t)
			at := c.Now()
			first := sample(at, at.Add(7*24*time.Hour), 40)
			spark := sample(at, first.Reset, 9)
			spark.Scope = "spark"
			for _, store := range []*Store{s, control} {
				observe(t, store, "account", "codex", first, spark)
				// Successive corrections can move Reset beyond the tolerance of
				// its original Cycle. Both anchors must be retired on replacement.
				for i := 1; i <= 2; i++ {
					observe(t, store, "account", "codex", sample(at.Add(time.Duration(i)*time.Minute), first.Reset.Add(time.Duration(i)*CycleTolerance), 40-float64(i)))
				}
			}
			old := s.Windows("account")[0]
			alias := sample(at.Add(3*time.Minute), old.Reset.Add(tc.delta), 80)
			alias.Scope = "limit:premium"
			insertPremiumAlias(t, db, alias)
			observe(t, control, "account", "codex", alias)
			c.Set(alias.ObservedAt())
			for i := 0; i < 2; i++ {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				s, db = openTest(t, path, c)
				if got, want := s.Windows("account"), control.Windows("account"); !reflect.DeepEqual(got, want) {
					t.Fatalf("restart %d: migration = %#v, normal observation = %#v", i, got, want)
				}
				var retired int
				if err := db.QueryRow("SELECT count(*) FROM observation_retired WHERE account=?", "account").Scan(&retired); err != nil {
					t.Fatal(err)
				}
				if retired != tc.retired {
					t.Fatalf("restart %d: retired %d cycle anchors, want %d", i, retired, tc.retired)
				}
				history, err := s.History("account", 0, 100)
				if err != nil || len(history) != 4 {
					t.Fatalf("migration changed historical evidence: %d rows, %v", len(history), err)
				}
			}
			resets := []time.Time{alias.Reset}
			wantRemaining := 37.0
			if tc.retired > 0 {
				resets = []time.Time{old.Reset, first.Reset}
				wantRemaining = 80
			}
			for i, reset := range resets {
				late := sample(at.Add(time.Duration(4+i)*time.Minute), reset, 37)
				c.Set(late.ObservedAt())
				observe(t, s, "account", "codex", late)
				observe(t, control, "account", "codex", late)
				got := s.Windows("account")[0]
				if !got.Reset.Equal(alias.Reset) || *got.Remaining != wantRemaining {
					t.Fatalf("late cycle observation: %#v", got)
				}
				if tc.retired > 0 {
					history, err := s.History("account", 0, 100)
					if err != nil || history[len(history)-1].Disposition != "stale-cycle" {
						t.Fatalf("retired cycle was not refused: %#v, %v", history, err)
					}
				}
			}
			// The main allowance's retired reset remains valid in Spark's scope.
			spark = sample(at.Add(10*time.Minute), first.Reset, 7)
			spark.Scope = "spark"
			observe(t, s, "account", "codex", spark)
			observe(t, control, "account", "codex", spark)
			got, want := s.Windows("account"), control.Windows("account")
			if !reflect.DeepEqual(got, want) || len(got) != 2 || got[1].Scope != "spark" || *got[1].Remaining != 7 {
				t.Fatalf("migration replay = %#v, normal observation = %#v", got, want)
			}
		})
	}
}

func TestMigrateCodexPremiumOlderAliasCycleRetirement(t *testing.T) {
	for _, delta := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		t.Run(delta.String(), func(t *testing.T) {
			c := newClock()
			path := filepath.Join(t.TempDir(), "state.sqlite")
			s, db := openTest(t, path, c)
			control, _, _ := testStore(t)
			at := c.Now()
			first := sample(at, at.Add(7*24*time.Hour), 40)
			observe(t, control, "account", "codex", first)
			for i := 1; i <= 2; i++ {
				observe(t, control, "account", "codex", sample(at.Add(time.Duration(i)*time.Minute), first.Reset.Add(time.Duration(i)*CycleTolerance), 40-float64(i)))
			}
			alias := control.Windows("account")[0]
			canonical := sample(at.Add(3*time.Minute), alias.Reset.Add(delta), 80)
			observe(t, s, "account", "codex", canonical)
			observe(t, control, "account", "codex", canonical)
			alias.Scope = "limit:premium"
			insertPremiumAlias(t, db, alias)
			for i := 0; i < 2; i++ {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				s, db = openTest(t, path, c)
				if got, want := s.Windows("account"), control.Windows("account"); !reflect.DeepEqual(got, want) {
					t.Fatalf("restart %d changed the newer canonical cycle: %#v, want %#v", i, got, want)
				}
				history, err := s.History("account", 0, 100)
				if err != nil || len(history) != 1 {
					t.Fatalf("migration changed historical evidence: %#v, %v", history, err)
				}
			}
			// Delayed observations of either of the former alias's reset anchors
			// must stay refused after the alias row has been deleted and reopened.
			for i, reset := range []time.Time{alias.Reset, first.Reset} {
				late := sample(at.Add(time.Duration(4+i)*time.Minute), reset, 37)
				late.Scope = "limit:premium"
				observe(t, s, "account", "codex", late)
				observe(t, control, "account", "codex", late)
				if got, want := s.Windows("account"), control.Windows("account"); !reflect.DeepEqual(got, want) {
					t.Fatalf("delayed alias revived a retired cycle: %#v, want %#v", got, want)
				}
				history, err := s.History("account", 0, 100)
				if err != nil || history[len(history)-1].Disposition != "stale-cycle" {
					t.Fatalf("older alias was not refused: %#v, %v", history, err)
				}
			}
			var retired int
			if err := db.QueryRow("SELECT count(*) FROM observation_retired WHERE account=? AND key=?", "account", canonical.Key()).Scan(&retired); err != nil || retired != 2 {
				t.Fatalf("retired %d alias anchors, want 2: %v", retired, err)
			}
		})
	}
}

func TestMigrateCodexPremiumDoesNotRetireUnprovenAliasCycle(t *testing.T) {
	for _, mode := range []string{"same-reset", "same-cycle-jitter", "conflicting-main", "ambiguous-main", "partial-main"} {
		t.Run(mode, func(t *testing.T) {
			c := newClock()
			s, db := openTest(t, filepath.Join(t.TempDir(), "state.sqlite"), c)
			at := c.Now()
			alias := sample(at, at.Add(7*24*time.Hour), 40)
			alias.Scope = "limit:premium"
			canonical := sample(at.Add(time.Minute), alias.Reset, 80)
			if mode == "same-cycle-jitter" {
				canonical.Reset = canonical.Reset.Add(CycleTolerance)
			} else if mode != "same-reset" {
				canonical.Reset = canonical.Reset.Add(-24 * time.Hour)
			}
			if mode == "partial-main" {
				canonical.Remaining, canonical.Usage = nil, Field{}
			}
			observe(t, s, "account", "codex", canonical)
			if mode == "conflicting-main" {
				observe(t, s, "account", "codex", sample(canonical.ObservedAt(), canonical.Reset, 79))
			} else if mode == "ambiguous-main" {
				partial := sample(at.Add(2*time.Minute), time.Time{}, 81)
				partial.ResetEvidence = Field{}
				observe(t, s, "account", "codex", partial)
			}
			insertPremiumAlias(t, db, alias)
			migrated, err := Open(db, c.Now)
			if err != nil {
				t.Fatal(err)
			}
			var retired int
			if err := db.QueryRow("SELECT count(*) FROM observation_retired WHERE account=?", "account").Scan(&retired); err != nil || retired != 0 {
				t.Fatalf("unproven cycle replacement retired %d anchors: %v", retired, err)
			}
			late := sample(at.Add(3*time.Minute), alias.Reset, 37)
			late.Scope = "limit:premium"
			observe(t, migrated, "account", "codex", late)
			got := migrated.Windows("account")
			if len(got) != 1 || got[0].Conflict || got[0].Ambiguous || got[0].Remaining == nil || *got[0].Remaining != 37 {
				t.Fatalf("coherent confirmation was incorrectly refused: %#v", got)
			}
		})
	}
}

func TestMigrateCodexPremiumOlderAliasFailureRollsBack(t *testing.T) {
	for _, point := range []string{"retirement", "alias-delete"} {
		t.Run(point, func(t *testing.T) {
			c := newClock()
			s, db := openTest(t, filepath.Join(t.TempDir(), "state.sqlite"), c)
			at := c.Now()
			canonical := sample(at.Add(time.Minute), at.Add(6*24*time.Hour), 80)
			observe(t, s, "account", "codex", canonical)
			alias := sample(at, at.Add(7*24*time.Hour), 40)
			alias.Scope = "limit:premium"
			insertPremiumAlias(t, db, alias)
			before, err := s.ReadWindows("account")
			if err != nil {
				t.Fatal(err)
			}
			trigger := "BEFORE INSERT ON observation_retired"
			if point == "alias-delete" {
				trigger = "BEFORE DELETE ON observation_windows WHEN OLD.key = 'codex:limit:premium:604800'"
			}
			if _, err := db.Exec("CREATE TRIGGER fail_migration " + trigger + " BEGIN SELECT RAISE(ABORT, 'injected migration failure'); END"); err != nil {
				t.Fatal(err)
			}
			if migrated, err := Open(db, c.Now); migrated != nil || err == nil || !strings.Contains(err.Error(), "injected migration failure") {
				t.Fatalf("failed migration returned store %v, error %v", migrated, err)
			}
			after, err := s.ReadWindows("account")
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("failed migration changed windows: %#v, %v", after, err)
			}
			var retired int
			if err := db.QueryRow("SELECT count(*) FROM observation_retired WHERE account=?", "account").Scan(&retired); err != nil || retired != 0 {
				t.Fatalf("failed migration retained %d retirements: %v", retired, err)
			}
			if _, err := db.Exec("DROP TRIGGER fail_migration"); err != nil {
				t.Fatal(err)
			}
			migrated, err := Open(db, c.Now)
			if err != nil {
				t.Fatal(err)
			}
			got := migrated.Windows("account")
			if len(got) != 1 || got[0].Scope != "all" || !got[0].Reset.Equal(canonical.Reset) || *got[0].Remaining != 80 {
				t.Fatalf("retry changed canonical window: %#v", got)
			}
			if err := db.QueryRow("SELECT count(*) FROM observation_retired WHERE account=? AND key=? AND reset=?", "account", canonical.Key(), alias.Reset.UnixNano()).Scan(&retired); err != nil || retired != 1 {
				t.Fatalf("retry retained %d alias retirements, want 1: %v", retired, err)
			}
		})
	}
}

func TestMigrateCodexPremiumFailureRollsBack(t *testing.T) {
	for _, point := range []string{"retirement", "canonical-write", "alias-delete"} {
		t.Run(point, func(t *testing.T) {
			c := newClock()
			s, db := openTest(t, filepath.Join(t.TempDir(), "state.sqlite"), c)
			at := c.Now()
			old := sample(at, at.Add(7*24*time.Hour), 40)
			observe(t, s, "account", "codex", old)
			alias := sample(at.Add(time.Minute), at.Add(6*24*time.Hour), 80)
			alias.Scope = "limit:premium"
			insertPremiumAlias(t, db, alias)
			if _, err := db.Exec("INSERT INTO observation_retired(account,key,reset) VALUES(?,?,?)", "account", alias.Key(), at.Add(-24*time.Hour).UnixNano()); err != nil {
				t.Fatal(err)
			}
			before, err := s.ReadWindows("account")
			if err != nil {
				t.Fatal(err)
			}
			trigger := map[string]string{
				"retirement":      fmt.Sprintf("BEFORE INSERT ON observation_retired WHEN NEW.reset = %d", old.Reset.UnixNano()),
				"canonical-write": "BEFORE UPDATE ON observation_windows WHEN OLD.key = 'codex:all:604800'",
				"alias-delete":    "BEFORE DELETE ON observation_windows WHEN OLD.key = 'codex:limit:premium:604800'",
			}[point]
			if _, err := db.Exec("CREATE TRIGGER fail_migration " + trigger + " BEGIN SELECT RAISE(ABORT, 'injected migration failure'); END"); err != nil {
				t.Fatal(err)
			}
			if migrated, err := Open(db, c.Now); migrated != nil || err == nil || !strings.Contains(err.Error(), "injected migration failure") {
				t.Fatalf("failed migration returned store %v, error %v", migrated, err)
			}
			after, err := s.ReadWindows("account")
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("failed migration changed windows: %#v, %v", after, err)
			}
			for key, want := range map[string]int{old.Key(): 0, alias.Key(): 1} {
				var count int
				if err := db.QueryRow("SELECT count(*) FROM observation_retired WHERE account=? AND key=?", "account", key).Scan(&count); err != nil || count != want {
					t.Fatalf("retirements after rollback for %s: %d, want %d, %v", key, count, want, err)
				}
			}
			if _, err := db.Exec("DROP TRIGGER fail_migration"); err != nil {
				t.Fatal(err)
			}
			migrated, err := Open(db, c.Now)
			if err != nil {
				t.Fatal(err)
			}
			got := migrated.Windows("account")
			if len(got) != 1 || got[0].Scope != "all" || !got[0].Reset.Equal(alias.Reset) || *got[0].Remaining != 80 {
				t.Fatalf("retry failed: %#v", got)
			}
			for key, want := range map[string]int{old.Key(): 2, alias.Key(): 0} {
				var count int
				if err := db.QueryRow("SELECT count(*) FROM observation_retired WHERE account=? AND key=?", "account", key).Scan(&count); err != nil || count != want {
					t.Fatalf("retirements after retry for %s: %d, want %d, %v", key, count, want, err)
				}
			}
		})
	}
}
