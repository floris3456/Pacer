package observation

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The state database is shared with the guarded measurement feed. Like the
// physical-proof schema migration, check its old generation before executing
// this specific additive DDL and advance only in the same write transaction.
// Never acknowledge a pre-existing schema change or a partial polling schema.
func openPollingSchema(c *sql.Conn) error {
	ctx := context.Background()
	var tables, feed int
	if err := c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('observation_poll_profiles','observation_poll_accounts')`).Scan(&tables); err != nil {
		return err
	}
	if tables == 2 {
		return nil
	}
	if tables != 0 {
		return errors.New("observation: partial polling schema")
	}
	if err := c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='measurement_report_meta'`).Scan(&feed); err != nil {
		return err
	}
	if feed != 0 {
		var version int
		var saved, current int64
		if err := c.QueryRowContext(ctx, `SELECT version,schema_cookie,(SELECT schema_version FROM pragma_schema_version) FROM measurement_report_meta WHERE id=1`).Scan(&version, &saved, &current); err != nil {
			return err
		}
		if version != 2 || saved != current {
			return errors.New("observation: pre-existing report feed schema change")
		}
	}
	if _, err := c.ExecContext(ctx, `CREATE TABLE observation_poll_profiles (profile TEXT PRIMARY KEY, seconds INTEGER NOT NULL, revision INTEGER NOT NULL);
CREATE TABLE observation_poll_accounts (account TEXT PRIMARY KEY, seconds INTEGER NOT NULL);`); err != nil {
		return err
	}
	if feed != 0 {
		_, err := c.ExecContext(ctx, `UPDATE measurement_report_meta SET schema_cookie=(SELECT schema_version FROM pragma_schema_version) WHERE id=1 AND version=2`)
		return err
	}
	return nil
}

type PollingSetting struct {
	Seconds  int   `json:"seconds"`
	Revision int64 `json:"revision"`
}

var ErrPollingConflict = errors.New("observation: polling setting changed")
var ErrPollingInput = errors.New("observation: polling interval must be 15–1440 minutes")

func (s *Store) PollingSettings() (map[string]PollingSetting, error) {
	rows, err := s.db.Query("SELECT profile,seconds,revision FROM observation_poll_profiles")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PollingSetting{}
	for rows.Next() {
		var profile string
		var v PollingSetting
		if err := rows.Scan(&profile, &v.Seconds, &v.Revision); err != nil {
			return nil, err
		}
		out[profile] = v
	}
	return out, rows.Err()
}

func (s *Store) SetPolling(profile string, seconds int, expected int64) error {
	if profile == "" || len(profile) > 80 || seconds < 900 || seconds > 86400 || seconds%60 != 0 || expected < 0 {
		return ErrPollingInput
	}
	return s.write(func(c *sql.Conn) error {
		var revision int64
		err := c.QueryRowContext(context.Background(), "SELECT revision FROM observation_poll_profiles WHERE profile=?", profile).Scan(&revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if revision != expected {
			return ErrPollingConflict
		}
		_, err = c.ExecContext(context.Background(), `INSERT INTO observation_poll_profiles VALUES(?,?,?) ON CONFLICT(profile) DO UPDATE SET seconds=excluded.seconds,revision=excluded.revision`, profile, seconds, revision+1)
		return err
	})
}

func pollingInterval(c *sql.Conn, account string) (time.Duration, error) {
	var seconds int
	err := c.QueryRowContext(context.Background(), "SELECT seconds FROM observation_poll_accounts WHERE account=?", account).Scan(&seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return IdleInterval, nil
	}
	return time.Duration(seconds) * time.Second, err
}

// ApplyPolling resolves profile memberships outside the scheduler; this replaces
// effective account intervals atomically. Provider backoff and pacing still apply.
func (s *Store) ApplyPolling(accounts map[string]int) error {
	return s.write(func(c *sql.Conn) error {
		for account, seconds := range accounts {
			if seconds < 900 || seconds > 86400 {
				return ErrPollingInput
			}
			previous, err := pollingInterval(c, account)
			if err != nil {
				return err
			}
			if previous == time.Duration(seconds)*time.Second {
				continue
			}
			if _, err = c.ExecContext(context.Background(), `INSERT INTO observation_poll_accounts VALUES(?,?) ON CONFLICT(account) DO UPDATE SET seconds=excluded.seconds`, account, seconds); err != nil {
				return err
			}
			if err = s.reconcileJob(c, account, s.clock()); err != nil {
				return err
			}
		}
		return nil
	})
}
