// Package windowstart authorizes one minimal inference per confirmed dormant
// Claude quota cycle. It neither sends requests nor invents provider deadlines.
package windowstart

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
)

const (
	MaxEvidenceAge = 2 * time.Minute
	RetryDelay     = 15 * time.Minute
	RetryWindow    = 5 * time.Hour
	MaxAttempts    = 3
	MinStartGap    = 30 * time.Second
)

// Eligible requires one fresh API snapshot, including explicitly disabled paid
// overage. A zero balance, missing reset, expired reset or old header is not proof
// of dormancy. Leave a margin on every applicable active subscription limit.
func Eligible(windows []observation.Window, extra observation.Credits, model string, now time.Time) string {
	if extra.Source != observation.APISource || extra.ReadMS <= 0 || extra.ReadMS > now.UnixMilli() || now.UnixMilli()-extra.ReadMS >= MaxEvidenceAge.Milliseconds() {
		return "fresh_quota_required"
	}
	if extra.State != "empty" {
		return "paid_usage_not_disabled"
	}
	five, week := false, false
	for _, w := range windows {
		if w.Provider != "claude" || (!w.Applies(model) && !w.NotApplicable) {
			continue
		}
		if w.Scope != "all" && w.Scope != "oauth" && !w.Applies(model) {
			continue
		}
		at, source := w.Usage.At, w.Usage.Source
		if w.Dormant || w.NotApplicable {
			at, source = w.StatusEvidence.At, w.StatusEvidence.Source
		}
		if !w.Valid() || !w.FreshComplete(now, MaxEvidenceAge) || source != observation.APISource || at.UnixMilli() != extra.ReadMS {
			return "fresh_quota_required"
		}
		if w.Scope == "all" && w.Seconds == 18000 {
			if five || !w.Dormant || w.NotApplicable || !w.Reset.IsZero() || w.Remaining == nil || *w.Remaining != 100 {
				return "window_not_dormant"
			}
			five = true
		}
		if w.Scope == "all" && w.Seconds == 604800 {
			if week {
				return "fresh_quota_required"
			}
			week = true
		}
		if !w.NotApplicable && (w.Remaining == nil || *w.Remaining <= 1) {
			return "subscription_allowance_low"
		}
	}
	if !five || !week {
		return "fresh_quota_required"
	}
	return ""
}

// Store owns no connection lifecycle. Its caller provides a private sidecar DB:
// adding tables to Pacer's guarded measurement database would break its witness.
type Store struct{ db *sql.DB }

type Attempt struct {
	State          string    `json:"state"`
	Started        time.Time `json:"started"`
	Finished       time.Time `json:"finished"`
	ConfirmedReset time.Time `json:"confirmed_reset"`
	RetryAt        time.Time `json:"retry_at"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	Attempts       int       `json:"attempts"`
	CycleStarted   time.Time `json:"cycle_started"`
	Reason         string    `json:"reason,omitempty"`
}

type Result struct {
	Dispatched bool
	HTTPStatus int
	Reason     string
	RetryAfter string
}

func Open(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("window start: database required")
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS quota_window_starts (
 account TEXT PRIMARY KEY, started INTEGER NOT NULL, finished INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL, reset INTEGER NOT NULL DEFAULT 0, retry_at INTEGER NOT NULL DEFAULT 0,
 status INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		return nil, err
	}
	// Serialize additive upgrades across process starts; the original attempt
	// ledger remains authoritative and is never cleared to enable a retry.
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	rows, err := conn.QueryContext(context.Background(), "PRAGMA table_info(quota_window_starts)")
	if err != nil {
		return nil, err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var id, required, primary int
		var name, kind string
		var fallback any
		if err = rows.Scan(&id, &name, &kind, &required, &fallback, &primary); err != nil {
			rows.Close()
			return nil, err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, column := range []struct{ name, declaration string }{
		{"attempts", "INTEGER NOT NULL DEFAULT 1"},
		{"cycle_started", "INTEGER NOT NULL DEFAULT 0"},
		{"reason", "TEXT NOT NULL DEFAULT ''"},
	} {
		if !columns[column.name] {
			if _, err = conn.ExecContext(context.Background(), "ALTER TABLE quota_window_starts ADD COLUMN "+column.name+" "+column.declaration); err != nil {
				return nil, err
			}
		}
	}
	if _, err = conn.ExecContext(context.Background(), `UPDATE quota_window_starts SET cycle_started=started WHERE cycle_started=0`); err != nil {
		return nil, err
	}
	// A received HTTP 429 from the fixed first-party endpoint is a refusal even
	// when its body was lost. Do not invent a provider error type for old rows.
	if _, err = conn.ExecContext(context.Background(), `UPDATE quota_window_starts
 SET state='retry_wait',reason='http_rate_limited',retry_at=finished+?
 WHERE state='unconfirmed' AND status=429 AND finished>0 AND reason=''`, int64(RetryDelay)); err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Claim commits before dispatch. A crash or ambiguous failure cannot permit a
// second request. Rearming requires a subsequently observed active window to
// have ended, followed by fresh dormant evidence. Only an explicit rate-limit
// refusal or proven non-dispatch can retry without that intervening window.
func (s *Store) Claim(account string, evidence, now time.Time) (bool, error) {
	if account == "" || evidence.IsZero() || evidence.After(now) || now.Sub(evidence) >= MaxEvidenceAge {
		return false, errors.New("window start: invalid claim")
	}
	result, err := s.db.Exec(`INSERT INTO quota_window_starts(account,started,state,cycle_started,attempts)
 SELECT ?,?,'unconfirmed',?,1 WHERE NOT EXISTS (
 SELECT 1 FROM quota_window_starts WHERE started > ?)
 ON CONFLICT(account) DO UPDATE SET
 attempts=CASE WHEN (reset>started AND reset<?) OR cycle_started<=? THEN 1 ELSE attempts+1 END,
 cycle_started=CASE WHEN (reset>started AND reset<?) OR cycle_started<=? THEN excluded.started ELSE cycle_started END,
 started=excluded.started,finished=0,state='unconfirmed',reset=0,retry_at=0,status=0,reason=''
 WHERE (quota_window_starts.reset>quota_window_starts.started AND quota_window_starts.reset<?)
 OR (state IN ('not_dispatched','retry_wait') AND retry_at>0 AND retry_at<=? AND finished<?
 AND (attempts<? OR cycle_started<=?))`,
		account, now.UnixNano(), now.UnixNano(), now.Add(-MinStartGap).UnixNano(),
		evidence.UnixNano(), now.Add(-RetryWindow).UnixNano(), evidence.UnixNano(), now.Add(-RetryWindow).UnixNano(),
		evidence.UnixNano(), now.UnixNano(), evidence.UnixNano(), MaxAttempts, now.Add(-RetryWindow).UnixNano())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// Finish never replaces a confirmation that raced with the callback result.
// Unknown transport errors must be passed as dispatched=true.
func (s *Store) Finish(account string, started, now time.Time, result Result) error {
	a, err := s.Read(account)
	if err != nil || !a.Started.Equal(started) {
		return err
	}
	state, retry := "unconfirmed", int64(0)
	if !result.Dispatched || (result.HTTPStatus == 429 && result.Reason != "spend_limit_reached") {
		state = "retry_wait"
		next := now.Add(RetryDelay * time.Duration(1<<min(max(a.Attempts-1, 0), MaxAttempts-1)))
		if a.Attempts >= MaxAttempts && a.CycleStarted.Add(RetryWindow).After(next) {
			next = a.CycleStarted.Add(RetryWindow)
		}
		if after := retryAfter(result.RetryAfter, now); after.After(next) {
			next = after
		}
		retry = next.UnixNano()
	} else if result.HTTPStatus >= 200 && result.HTTPStatus < 300 {
		state = "awaiting_confirmation"
	} else if result.HTTPStatus >= 400 && result.HTTPStatus < 500 {
		state = "blocked"
	}
	_, err = s.db.Exec(`UPDATE quota_window_starts SET finished=?,
 state=CASE WHEN state='confirmed' THEN state ELSE ? END,
 retry_at=CASE WHEN state='confirmed' THEN 0 ELSE ? END,status=?,reason=?
 WHERE account=? AND started=?`, now.UnixNano(), state, retry, result.HTTPStatus, result.Reason, account, started.UnixNano())
	return err
}

func retryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		// Oversized delays must never wrap into an immediate retry.
		const limit = int64(1<<63 - 1)
		if seconds > (limit-now.UnixNano())/int64(time.Second) {
			return time.Unix(0, limit)
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	at, err := http.ParseTime(value)
	if err == nil && at.After(now) {
		if at.After(time.Unix(0, 1<<63-1)) {
			return time.Unix(0, 1<<63-1)
		}
		return at
	}
	return time.Time{}
}

// Confirm accepts only an actual, current five-hour reset observed after the
// attempt. A successful inference alone does not prove that a timer started.
func (s *Store) Confirm(account string, windows []observation.Window, now time.Time) error {
	for _, w := range windows {
		if w.Provider != "claude" || w.Scope != "all" || w.Seconds != 18000 || w.State(now) != "current" || !w.FreshComplete(now, MaxEvidenceAge) || w.ResetEvidence.Quality != observation.Absolute || w.ResetEvidence.At.After(now) || w.ResetEvidence.At.IsZero() {
			continue
		}
		_, err := s.db.Exec(`UPDATE quota_window_starts SET reset=?,state='confirmed',retry_at=0
 WHERE account=? AND started<? AND reset<?`, w.Reset.UnixNano(), account, w.ResetEvidence.At.UnixNano(), w.Reset.UnixNano())
		return err
	}
	return nil
}

func (s *Store) Read(account string) (Attempt, error) {
	var a Attempt
	var started, finished, reset, retry int64
	var cycle int64
	err := s.db.QueryRow(`SELECT started,finished,state,reset,retry_at,status,attempts,cycle_started,reason FROM quota_window_starts WHERE account=?`, account).
		Scan(&started, &finished, &a.State, &reset, &retry, &a.HTTPStatus, &a.Attempts, &cycle, &a.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{State: "idle"}, nil
	}
	fromNS := func(n int64) time.Time {
		if n == 0 {
			return time.Time{}
		}
		return time.Unix(0, n).UTC()
	}
	a.Started, a.Finished, a.ConfirmedReset, a.RetryAt = fromNS(started), fromNS(finished), fromNS(reset), fromNS(retry)
	a.CycleStarted = fromNS(cycle)
	return a, err
}
