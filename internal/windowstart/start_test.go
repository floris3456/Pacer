package windowstart

import (
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	_ "github.com/mattn/go-sqlite3"
)

var epoch = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func snapshot(t *testing.T, at time.Time, body string) ([]observation.Window, observation.Credits) {
	t.Helper()
	w, err := observation.API("claude", []byte(body), at)
	if err != nil {
		t.Fatal(err)
	}
	return w, observation.ClaudeExtraUsage([]byte(body), at)
}

const dormant = `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":20,"resets_at":"2026-10-20T12:00:00Z"},"extra_usage":{"is_enabled":false}}`

func TestEligibilityRequiresFreshExplicitDormancyAndNoPaidUsage(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"dormant", dormant, ""},
		{"first use", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":0,"resets_at":null},"extra_usage":{"is_enabled":false}}`, ""},
		{"no weekly cap", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":null,"extra_usage":{"is_enabled":false}}`, ""},
		{"active unused", `{"five_hour":{"utilization":0,"resets_at":"2026-10-13T17:00:00Z"},"seven_day":null,"extra_usage":{"is_enabled":false}}`, "window_not_dormant"},
		{"missing reset", `{"five_hour":{"utilization":0},"seven_day":null,"extra_usage":{"is_enabled":false}}`, "fresh_quota_required"},
		{"no five hour cap", `{"five_hour":null,"seven_day":null,"extra_usage":{"is_enabled":false}}`, "window_not_dormant"},
		{"missing week", `{"five_hour":{"utilization":0,"resets_at":null},"extra_usage":{"is_enabled":false}}`, "fresh_quota_required"},
		{"exhausted week", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":100,"resets_at":"2026-10-20T12:00:00Z"},"extra_usage":{"is_enabled":false}}`, "subscription_allowance_low"},
		{"weekly margin", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":99,"resets_at":"2026-10-20T12:00:00Z"},"extra_usage":{"is_enabled":false}}`, "subscription_allowance_low"},
		{"oauth exhausted", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":null,"seven_day_oauth_apps":{"utilization":100,"resets_at":"2026-10-20T12:00:00Z"},"extra_usage":{"is_enabled":false}}`, "subscription_allowance_low"},
		{"unrelated opus exhausted", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":null,"seven_day_opus":{"utilization":100,"resets_at":"2026-10-20T12:00:00Z"},"extra_usage":{"is_enabled":false}}`, ""},
		{"paid enabled", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":null,"extra_usage":{"is_enabled":true}}`, "paid_usage_not_disabled"},
		{"paid unknown", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":null}`, "paid_usage_not_disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, extra := snapshot(t, epoch, tc.body)
			if got := Eligible(w, extra, "claude-haiku-5-5", epoch); got != tc.want {
				t.Fatalf("reason=%q want=%q", got, tc.want)
			}
		})
	}
	for _, mutate := range []func([]observation.Window, *observation.Credits){
		func(w []observation.Window, c *observation.Credits) { c.ReadMS-- },
		func(w []observation.Window, c *observation.Credits) { c.Source = observation.HeaderSource },
		func(w []observation.Window, c *observation.Credits) { w[0].Conflict = true },
		func(w []observation.Window, c *observation.Credits) { w[0].Ambiguous = true },
		func(w []observation.Window, c *observation.Credits) {
			w[0].StatusEvidence.Source = observation.HeaderSource
			w[0].Usage.Source = observation.HeaderSource
		},
	} {
		w, c := snapshot(t, epoch, dormant)
		mutate(w, &c)
		if got := Eligible(w, c, "claude-haiku-5-5", epoch); got == "" {
			t.Fatal("ambiguous or mixed snapshot was eligible")
		}
	}
	w, c := snapshot(t, epoch, dormant)
	for _, now := range []time.Time{epoch.Add(MaxEvidenceAge), epoch.Add(-time.Millisecond)} {
		if Eligible(w, c, "claude-haiku-5-5", now) == "" {
			t.Fatal("stale/future evidence admitted")
		}
	}
}

func openStore(t *testing.T, path string) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func claim(t *testing.T, s *Store, account string, at time.Time, want bool) {
	t.Helper()
	if got, err := s.Claim(account, at, at); err != nil || got != want {
		t.Fatalf("claim=%v want=%v error=%v", got, want, err)
	}
}

func TestThreeCallersAndAliasesShareOneDurableAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "windows.sqlite")
	var workers sync.WaitGroup
	var starts atomic.Int32
	for i := 0; i < 3; i++ {
		s, _ := openStore(t, path)
		workers.Go(func() {
			ok, err := s.Claim("verified-account", epoch, epoch)
			if err != nil {
				t.Error(err)
			}
			if ok {
				starts.Add(1)
			}
		})
	}
	workers.Wait()
	if starts.Load() != 1 {
		t.Fatal("duplicate dispatch", starts.Load())
	}
	s, _ := openStore(t, path)
	// Even a crash before recording the HTTP result cannot replay inference.
	claim(t, s, "verified-account", epoch.Add(24*time.Hour), false)
	claim(t, s, "different-account", epoch.Add(24*time.Hour), true)
	claim(t, s, "another-account", epoch.Add(24*time.Hour+29*time.Second), false)
	claim(t, s, "another-account", epoch.Add(24*time.Hour+MinStartGap), true)
}

func TestOnlyConfirmedExpiredWindowRearmsAnUncertainDispatch(t *testing.T) {
	s, _ := openStore(t, filepath.Join(t.TempDir(), "windows.sqlite"))
	claim(t, s, "account", epoch, true)
	if err := s.Finish("account", epoch, epoch.Add(time.Second), Result{Dispatched: true, HTTPStatus: 503}); err != nil {
		t.Fatal(err)
	}
	claim(t, s, "account", epoch.Add(6*time.Hour), false)
	active, _ := snapshot(t, epoch.Add(time.Minute), `{"five_hour":{"utilization":0,"resets_at":"2026-10-13T17:00:00Z"}}`)
	if err := s.Confirm("account", active, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Late failure cannot erase independently observed provider confirmation.
	if err := s.Finish("account", epoch, epoch.Add(2*time.Minute), Result{Dispatched: true, HTTPStatus: 500}); err != nil {
		t.Fatal(err)
	}
	a, err := s.Read("account")
	if err != nil || a.State != "confirmed" || a.ConfirmedReset != epoch.Add(5*time.Hour) || a.Finished.IsZero() || a.HTTPStatus != 500 {
		t.Fatal(a, err)
	}
	claim(t, s, "account", epoch.Add(4*time.Hour), false)
	claim(t, s, "account", epoch.Add(5*time.Hour), false) // Evidence must be after reset.
	claim(t, s, "account", epoch.Add(5*time.Hour+time.Second), true)
	claim(t, s, "account", epoch.Add(10*time.Hour), false)
}

func TestNonDispatchRetryIsDelayedAndRequiresNewEvidence(t *testing.T) {
	s, _ := openStore(t, filepath.Join(t.TempDir(), "windows.sqlite"))
	claim(t, s, "account", epoch, true)
	if err := s.Finish("account", epoch, epoch, Result{Dispatched: false, HTTPStatus: 0}); err != nil {
		t.Fatal(err)
	}
	claim(t, s, "account", epoch.Add(time.Minute), false)
	claim(t, s, "account", epoch.Add(RetryDelay), true)
	// Successful response still needs provider reset evidence.
	if err := s.Finish("account", epoch.Add(RetryDelay), epoch.Add(RetryDelay+time.Second), Result{Dispatched: true, HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	claim(t, s, "account", epoch.Add(24*time.Hour), false)
}

func TestConfirmationRejectsDormantStaleConflictingAndEstimatedEvidence(t *testing.T) {
	for _, mutate := range []func([]observation.Window){
		func(w []observation.Window) { w[0].Conflict = true },
		func(w []observation.Window) { w[0].ResetEvidence.Quality = observation.Estimated },
		func(w []observation.Window) { w[0].ResetEvidence.At = epoch },
		func(w []observation.Window) { w[0].ResetEvidence.At = epoch.Add(time.Hour) },
		func(w []observation.Window) { w[0].Dormant = true },
	} {
		s, _ := openStore(t, filepath.Join(t.TempDir(), "windows.sqlite"))
		claim(t, s, "account", epoch, true)
		w, _ := snapshot(t, epoch.Add(time.Minute), `{"five_hour":{"utilization":0,"resets_at":"2026-10-13T17:00:00Z"}}`)
		mutate(w)
		if err := s.Confirm("account", w, epoch.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		a, err := s.Read("account")
		if err != nil || a.State != "unconfirmed" {
			t.Fatal(a, err)
		}
	}
}
