package observation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Store shares its caller-owned SQLite database; Close belongs to the caller.
// Multiple Store instances/processes coordinate through SQLite write transactions.
type Store struct {
	db      *sql.DB
	clock   func() time.Time
	errMu   sync.Mutex
	lastErr error
}

const schema = `
CREATE TABLE IF NOT EXISTS observation_windows (
 account TEXT NOT NULL, key TEXT NOT NULL, provider TEXT NOT NULL, body TEXT NOT NULL,
 PRIMARY KEY(account,key));
CREATE TABLE IF NOT EXISTS observation_history (
 id INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, key TEXT NOT NULL,
 received_at INTEGER NOT NULL, sample TEXT NOT NULL, disposition TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS observation_history_account ON observation_history(account,id);
CREATE TABLE IF NOT EXISTS observation_retired (
 account TEXT NOT NULL, key TEXT NOT NULL, reset INTEGER NOT NULL,
 PRIMARY KEY(account,key,reset));
CREATE TABLE IF NOT EXISTS observation_jobs (
 account TEXT PRIMARY KEY, endpoint TEXT NOT NULL, reason TEXT NOT NULL,
 due INTEGER NOT NULL, state TEXT NOT NULL, token TEXT NOT NULL DEFAULT '',
 started INTEGER NOT NULL DEFAULT 0, joined INTEGER NOT NULL DEFAULT 0,
 cancelled INTEGER NOT NULL DEFAULT 0, activity INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS observation_endpoint_inflight
 ON observation_jobs(endpoint) WHERE state='inflight';
CREATE TABLE IF NOT EXISTS observation_budgets (
 kind TEXT NOT NULL, key TEXT NOT NULL, last_start INTEGER NOT NULL DEFAULT 0,
 not_before INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0,
 suspended INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(kind,key));
CREATE TABLE IF NOT EXISTS observation_attempts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, token TEXT NOT NULL UNIQUE,
 account TEXT NOT NULL, endpoint TEXT NOT NULL, reason TEXT NOT NULL,
 started INTEGER NOT NULL, finished INTEGER NOT NULL DEFAULT 0,
 status INTEGER NOT NULL DEFAULT 0, retry_after TEXT NOT NULL DEFAULT '');
`

func Open(db *sql.DB, clock func() time.Time) (*Store, error) {
	if db == nil {
		return nil, errors.New("observation: nil database")
	}
	if clock == nil {
		clock = time.Now
	}
	s := &Store{db: db, clock: clock}
	if err := s.write(func(c *sql.Conn) error {
		if _, err := c.ExecContext(context.Background(), schema); err != nil {
			return err
		}
		if err := addActivityColumn(c); err != nil {
			return err
		}
		if err := openPollingSchema(c); err != nil {
			return err
		}
		if err := migrateCodexPremium(c); err != nil {
			return err
		}
		return migrateCodexResetDrift(c)
	}); err != nil {
		return nil, fmt.Errorf("observation schema: %w", err)
	}
	return s, nil
}

// addActivityColumn upgrades a jobs table created before activity reads.
func addActivityColumn(c *sql.Conn) error {
	rows, err := c.QueryContext(context.Background(), "SELECT name FROM pragma_table_info('observation_jobs')")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		found = found || name == "activity"
	}
	err = rows.Err()
	rows.Close()
	if err != nil || found {
		return err
	}
	_, err = c.ExecContext(context.Background(), "ALTER TABLE observation_jobs ADD COLUMN activity INTEGER NOT NULL DEFAULT 0")
	return err
}

// BEGIN IMMEDIATE makes read/check/write acquisition atomic even with separate
// database pools. busy_timeout is connection-local; no global pool settings change.
func (s *Store) write(fn func(*sql.Conn) error) (err error) {
	c, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err = c.ExecContext(context.Background(), "PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	if _, err = c.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = c.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = fn(c); err != nil {
		return err
	}
	_, err = c.ExecContext(context.Background(), "COMMIT")
	return err
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readWindows(q queryer, account string) ([]Window, error) {
	rows, err := q.QueryContext(context.Background(), "SELECT body FROM observation_windows WHERE account=? ORDER BY key", account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Window{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var w Window
		if err := json.Unmarshal([]byte(body), &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ReadWindows is the error-reporting form for routing and service boundaries.
func (s *Store) ReadWindows(account string) ([]Window, error) { return readWindows(s.db, account) }

// Windows is a convenient snapshot API. A database error returns nil and is
// retained in Err; integrations making availability decisions use ReadWindows.
func (s *Store) Windows(account string) []Window {
	w, err := s.ReadWindows(account)
	if err != nil {
		s.errMu.Lock()
		s.lastErr = err
		s.errMu.Unlock()
	}
	return w
}

func (s *Store) Err() error { s.errMu.Lock(); defer s.errMu.Unlock(); return s.lastErr }

func (s *Store) Observe(account, provider string, samples []Window) error {
	if account == "" {
		return errors.New("observation: account required")
	}
	provider = providerName(provider)
	if provider == "" {
		return errors.New("observation: provider required")
	}
	if len(samples) == 0 {
		return nil
	}
	return s.write(func(c *sql.Conn) error {
		for _, sample := range samples {
			if sample.Provider != "" && providerName(sample.Provider) != provider {
				return errors.New("observation: provider mismatch")
			}
			sample.Provider = provider
			if provider == "codex" && sample.Scope == "limit:premium" {
				sample.Scope = "all"
			}
			if !sample.Valid() {
				return errors.New("observation: invalid sample")
			}
			// Cycle/diagnostic flags are derived here, never trusted from adapters.
			sample.Cycle, sample.CycleAt, sample.Conflict, sample.Ambiguous = "", time.Time{}, false, false
			sample.ConflictAt, sample.ConflictingReset, sample.ConflictingResetEvidence = time.Time{}, time.Time{}, Field{}
			key := sample.Key()
			var old Window
			var body string
			err := c.QueryRowContext(context.Background(), "SELECT body FROM observation_windows WHERE account=? AND key=?", account, key).Scan(&body)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				if err := json.Unmarshal([]byte(body), &old); err != nil {
					return err
				}
			}
			retired := false
			if !sample.Reset.IsZero() {
				var count int
				if err := c.QueryRowContext(context.Background(), "SELECT count(*) FROM observation_retired WHERE account=? AND key=? AND reset BETWEEN ? AND ?", account, key, sample.Reset.Add(-CycleTolerance).UnixNano(), sample.Reset.Add(CycleTolerance).UnixNano()).Scan(&count); err != nil {
					return err
				}
				retired = count > 0
			}
			next, disposition := old, "stale-cycle"
			if !retired {
				next, disposition = merge(old, sample)
			}
			// A cycle ends on a new reset or when the window reads dormant again
			// (reset, not yet started); late readings of it are refused either way.
			if (disposition == "accepted-cycle" && !sameReset(old.Reset, next.Reset)) || (disposition == "accepted-status" && next.Dormant && !old.Dormant) {
				for _, reset := range []time.Time{old.Reset, timestamp(old.Cycle)} {
					if !reset.IsZero() {
						if _, err := c.ExecContext(context.Background(), "INSERT OR IGNORE INTO observation_retired(account,key,reset) VALUES(?,?,?)", account, key, reset.UnixNano()); err != nil {
							return err
						}
					}
				}
			}
			raw, err := json.Marshal(sample)
			if err != nil {
				return err
			}
			if _, err := c.ExecContext(context.Background(), "INSERT INTO observation_history(account,key,received_at,sample,disposition) VALUES(?,?,?,?,?)", account, key, s.clock().UnixNano(), string(raw), disposition); err != nil {
				return err
			}
			canonical, err := json.Marshal(next)
			if err != nil {
				return err
			}
			if _, err := c.ExecContext(context.Background(), "INSERT INTO observation_windows(account,key,provider,body) VALUES(?,?,?,?) ON CONFLICT(account,key) DO UPDATE SET body=excluded.body", account, key, provider, string(canonical)); err != nil {
				return err
			}
		}
		return s.reconcileJob(c, account, s.clock().UTC())
	})
}

type HistoryEntry struct {
	ID          int64     `json:"id"`
	ReceivedAt  time.Time `json:"received_at"`
	Sample      Window    `json:"sample"`
	Disposition string    `json:"disposition"`
}

// History pages append-only source evidence by sequence, including refused stale
// and conflicting observations. A consumer can keep the last ID across restarts.
func (s *Store) History(account string, afterID int64, limit int) ([]HistoryEntry, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.Query("SELECT id,received_at,sample,disposition FROM observation_history WHERE account=? AND id>? ORDER BY id LIMIT ?", account, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var h HistoryEntry
		var ns int64
		var raw string
		if err := rows.Scan(&h.ID, &ns, &raw, &h.Disposition); err != nil {
			return nil, err
		}
		h.ReceivedAt = fromNS(ns)
		if err := json.Unmarshal([]byte(raw), &h.Sample); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// AccountHistoryEntry is a history row with its account, for consumers that
// follow every account at once (the continuous usage record).
type AccountHistoryEntry struct {
	HistoryEntry
	Account string `json:"account"`
}

// HistorySince pages every account's append-only history by sequence.
func (s *Store) HistorySince(afterID int64, limit int) ([]AccountHistoryEntry, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.Query("SELECT id,account,received_at,sample,disposition FROM observation_history WHERE id>? ORDER BY id LIMIT ?", afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountHistoryEntry{}
	for rows.Next() {
		var h AccountHistoryEntry
		var at int64
		var raw string
		if err := rows.Scan(&h.ID, &h.Account, &at, &raw, &h.Disposition); err != nil {
			return nil, err
		}
		h.ReceivedAt = fromNS(at)
		if err := json.Unmarshal([]byte(raw), &h.Sample); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func fromNS(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
func ns(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
