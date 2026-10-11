package observation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// migrateCodexPremium reconciles the former header-only alias with the main
// allowance. Run inside Open's transaction, before serving any snapshots.
// Historical samples remain unchanged; current state uses normal field/cycle
// freshness rules so a stale alias cannot overwrite a newer API observation.
func migrateCodexPremium(c *sql.Conn) error {
	ctx := context.Background()
	rows, err := c.QueryContext(ctx, "SELECT account,key,body FROM observation_windows WHERE provider='codex'")
	if err != nil {
		return err
	}
	type entry struct {
		account, key string
		window       Window
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var body string
		if err := rows.Scan(&e.account, &e.key, &body); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(body), &e.window); err != nil {
			rows.Close()
			return err
		}
		if e.window.Scope == "limit:premium" {
			entries = append(entries, e)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		e.window.Scope = "all"
		key := e.window.Key()
		var body string
		var old Window
		err := c.QueryRowContext(ctx, "SELECT body FROM observation_windows WHERE account=? AND key=?", e.account, key).Scan(&body)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal([]byte(body), &old); err != nil {
				return err
			}
		}
		next, _ := merge(old, e.window)
		if old.Valid() && !e.window.Reset.IsZero() {
			var retired int
			if err := c.QueryRowContext(ctx, "SELECT count(*) FROM observation_retired WHERE account=? AND key IN (?,?) AND reset BETWEEN ? AND ?", e.account, key, e.key, e.window.Reset.Add(-CycleTolerance).UnixNano(), e.window.Reset.Add(CycleTolerance).UnixNano()).Scan(&retired); err != nil {
				return err
			}
			if retired > 0 {
				next = old
			}
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO observation_windows(account,key,provider,body) VALUES(?,?,'codex',?) ON CONFLICT(account,key) DO UPDATE SET body=excluded.body", e.account, key, string(raw)); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO observation_retired(account,key,reset) SELECT account,?,reset FROM observation_retired WHERE account=? AND key=?", key, e.account, e.key); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM observation_retired WHERE account=? AND key=?", e.account, e.key); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM observation_windows WHERE account=? AND key=?", e.account, e.key); err != nil {
			return err
		}
	}
	return nil
}
