package windowstart

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimitBudgetSurvivesRestartAndConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "windows.sqlite")
	s, _ := openStore(t, path)
	at := epoch
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		claim(t, s, "account", at, true)
		if err := s.Finish("account", at, at.Add(time.Second), Result{Dispatched: true, HTTPStatus: 429, Reason: "http_rate_limited"}); err != nil {
			t.Fatal(err)
		}
		s, _ = openStore(t, path)
		a, err := s.Read("account")
		if err != nil || a.Attempts != attempt || a.State != "retry_wait" || !a.CycleStarted.Equal(epoch) {
			t.Fatal(a, err)
		}
		want := at.Add(time.Second + RetryDelay*time.Duration(1<<(attempt-1)))
		if attempt == MaxAttempts {
			want = epoch.Add(RetryWindow)
		}
		if !a.RetryAt.Equal(want) {
			t.Fatal("unexpected retry time", a.RetryAt, want)
		}
		claim(t, s, "account", want.Add(-time.Second), false)
		at = a.RetryAt
	}
	var winners atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < 3; i++ {
		worker, _ := openStore(t, path)
		workers.Go(func() {
			claimed, err := worker.Claim("account", at, at)
			if err != nil {
				t.Error(err)
			}
			if claimed {
				winners.Add(1)
			}
		})
	}
	workers.Wait()
	a, err := s.Read("account")
	if err != nil || winners.Load() != 1 || a.Attempts != 1 || !a.CycleStarted.Equal(at) {
		t.Fatal("budget reset or concurrent claim failure", winners.Load(), a, err)
	}
	// A crash after a new claim is still ambiguous, even after another budget.
	claim(t, s, "account", at.Add(2*RetryWindow), false)
}

func TestRetryAfterPersistsAndOutlivesBudgetRollover(t *testing.T) {
	for _, after := range []string{"21600", epoch.Add(6 * time.Hour).Format(http.TimeFormat)} {
		path := filepath.Join(t.TempDir(), "windows.sqlite")
		s, _ := openStore(t, path)
		claim(t, s, "account", epoch, true)
		if err := s.Finish("account", epoch, epoch, Result{Dispatched: true, HTTPStatus: 429, RetryAfter: after}); err != nil {
			t.Fatal(err)
		}
		s, _ = openStore(t, path)
		claim(t, s, "account", epoch.Add(RetryWindow+time.Second), false)
		claim(t, s, "account", epoch.Add(6*time.Hour), true)
	}
	for _, invalid := range []string{"-1", "garbage", "", "NaN"} {
		s, _ := openStore(t, filepath.Join(t.TempDir(), "windows.sqlite"))
		claim(t, s, "account", epoch, true)
		if err := s.Finish("account", epoch, epoch, Result{Dispatched: true, HTTPStatus: 429, RetryAfter: invalid}); err != nil {
			t.Fatal(err)
		}
		a, _ := s.Read("account")
		if !a.RetryAt.Equal(epoch.Add(RetryDelay)) {
			t.Fatal("malformed header changed conservative delay", a)
		}
	}
	if at := retryAfter("9223372036854775807", epoch); !at.After(epoch.Add(100 * 365 * 24 * time.Hour)) {
		t.Fatal("overflow shortened the delay", at)
	}
}

func TestRetryNeverRearmsUnknownSuccessOrSpendLimit(t *testing.T) {
	for _, result := range []Result{
		{Dispatched: true}, {Dispatched: true, HTTPStatus: 503}, {Dispatched: true, HTTPStatus: 200},
		{Dispatched: true, HTTPStatus: 429, Reason: "spend_limit_reached"}, {Dispatched: true, HTTPStatus: 401},
	} {
		s, _ := openStore(t, filepath.Join(t.TempDir(), "windows.sqlite"))
		claim(t, s, "account", epoch, true)
		if err := s.Finish("account", epoch, epoch.Add(time.Second), result); err != nil {
			t.Fatal(err)
		}
		claim(t, s, "account", epoch.Add(24*time.Hour), false)
	}
}

func TestLegacy429UpgradePreservesAttemptsAndOtherOutcomes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "windows.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE quota_window_starts(account TEXT PRIMARY KEY,started INTEGER NOT NULL,finished INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL,reset INTEGER NOT NULL DEFAULT 0,retry_at INTEGER NOT NULL DEFAULT 0,status INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, state string
		status      int
		finished    int64
	}{
		{"refused", "unconfirmed", 429, epoch.Add(time.Second).UnixNano()},
		{"unfinished", "unconfirmed", 429, 0},
		{"success", "awaiting_confirmation", 200, epoch.Add(time.Second).UnixNano()},
		{"unknown", "unconfirmed", 503, epoch.Add(time.Second).UnixNano()},
		{"confirmed", "confirmed", 429, epoch.Add(time.Second).UnixNano()},
	} {
		if _, err = db.Exec(`INSERT INTO quota_window_starts(account,started,finished,state,status) VALUES(?,?,?,?,?)`, row.name, epoch.UnixNano(), row.finished, row.state, row.status); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"refused", "unfinished", "success", "unknown", "confirmed"} {
		a, err := s.Read(name)
		if err != nil || a.Attempts != 1 || !a.Started.Equal(epoch) || !a.CycleStarted.Equal(epoch) {
			t.Fatal(a, err)
		}
		if name == "refused" {
			if a.State != "retry_wait" || a.Reason != "http_rate_limited" || !a.RetryAt.Equal(epoch.Add(time.Second+RetryDelay)) {
				t.Fatal("legacy refusal not recovered", a)
			}
		} else if a.State == "retry_wait" || !a.RetryAt.IsZero() {
			t.Fatal("migration unfenced another outcome", name, a)
		}
	}
	claim(t, s, "refused", epoch.Add(time.Second+RetryDelay), true)
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Read("refused")
	if a.Attempts != 2 || a.State != "unconfirmed" {
		t.Fatal("reopening reset the budget or the in-flight fence", a)
	}
	claim(t, s, "refused", epoch.Add(24*time.Hour), false)
}
