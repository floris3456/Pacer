// Package windowstart authorizes one minimal inference per confirmed dormant
// Claude quota cycle. It neither sends requests nor invents provider deadlines.
package windowstart

import (
	"database/sql"
	"errors"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
)

const (
	MaxEvidenceAge = 2 * time.Minute
	RetryDelay     = 15 * time.Minute // Only after a proven non-dispatch.
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
	return &Store{db: db}, nil
}

// Claim commits before dispatch. A crash or ambiguous failure cannot permit a
// second request. Rearming requires a subsequently observed active window to
// have ended, followed by fresh dormant evidence. Only proven non-dispatches
// can retry without that intervening active window.
func (s *Store) Claim(account string, evidence, now time.Time) (bool, error) {
	if account == "" || evidence.IsZero() || evidence.After(now) || now.Sub(evidence) >= MaxEvidenceAge {
		return false, errors.New("window start: invalid claim")
	}
	result, err := s.db.Exec(`INSERT INTO quota_window_starts(account,started,state)
 SELECT ?,?,'unconfirmed' WHERE NOT EXISTS (
 SELECT 1 FROM quota_window_starts WHERE started > ?)
 ON CONFLICT(account) DO UPDATE SET started=excluded.started,finished=0,state='unconfirmed',reset=0,retry_at=0,status=0
 WHERE (quota_window_starts.reset>quota_window_starts.started AND quota_window_starts.reset<?)
 OR (quota_window_starts.state='not_dispatched' AND quota_window_starts.retry_at<=? AND quota_window_starts.started<?)`,
		account, now.UnixNano(), now.Add(-MinStartGap).UnixNano(), evidence.UnixNano(), now.UnixNano(), evidence.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// Finish never replaces a confirmation that raced with the callback result.
// Unknown transport errors must be passed as dispatched=true.
func (s *Store) Finish(account string, started, now time.Time, dispatched bool, status int) error {
	state, retry := "unconfirmed", int64(0)
	if !dispatched {
		state, retry = "not_dispatched", now.Add(RetryDelay).UnixNano()
	} else if status >= 200 && status < 300 {
		state = "awaiting_confirmation"
	}
	_, err := s.db.Exec(`UPDATE quota_window_starts SET finished=?,
 state=CASE WHEN state='confirmed' THEN state ELSE ? END,
 retry_at=CASE WHEN state='confirmed' THEN 0 ELSE ? END,status=?
 WHERE account=? AND started=?`, now.UnixNano(), state, retry, status, account, started.UnixNano())
	return err
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
	err := s.db.QueryRow(`SELECT started,finished,state,reset,retry_at,status FROM quota_window_starts WHERE account=?`, account).
		Scan(&started, &finished, &a.State, &reset, &retry, &a.HTTPStatus)
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
	return a, err
}
