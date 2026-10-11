package observation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
)

// Repair only retirements proven to be corrections by the original audit.
// Unexplained markers and real replenishments remain fenced. History is never
// rewritten or refreshed: the next live observation supplies the new balance.
func migrateCodexResetDrift(c *sql.Conn) error {
	ctx := context.Background()
	if err := openResetDriftSchema(c); err != nil {
		return err
	}
	const id = "codex-reset-drift-v1"
	var done int
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM observation_migrations WHERE id=?", id).Scan(&done); err != nil || done != 0 {
		return err
	}
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
		var raw string
		if err := rows.Scan(&e.account, &e.key, &raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &e.window); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		var nearby int
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM observation_retired WHERE account=? AND key=? AND reset BETWEEN ? AND ?", e.account, e.key, e.window.Reset.Add(-CodexResetCorrection).UnixNano(), e.window.Reset.Add(CodexResetCorrection).UnixNano()).Scan(&nearby); err != nil {
			return err
		}
		if nearby == 0 {
			continue
		}
		history, err := c.QueryContext(ctx, "SELECT sample,disposition FROM observation_history WHERE account=? AND key=? ORDER BY id", e.account, e.key)
		if err != nil {
			return err
		}
		var original, corrected Window
		remove := map[time.Time]bool{}
		retired := map[time.Time]bool{}
		for history.Next() {
			var raw, disposition string
			if err = history.Scan(&raw, &disposition); err != nil {
				break
			}
			if !strings.HasPrefix(disposition, "accepted") && disposition != "conflict" {
				continue
			}
			var sample Window
			if err = json.Unmarshal([]byte(raw), &sample); err != nil {
				break
			}
			next, result := corrected, "stale-cycle"
			blocked := false
			for reset := range retired {
				blocked = blocked || sameReset(reset, sample.Reset)
			}
			if !blocked {
				next, result = merge(corrected, sample)
			}
			if disposition == "accepted-cycle" && result == "accepted-field" && codexResetCorrection(corrected, sample) {
				remove[original.Reset] = true
				remove[timestamp(original.Cycle)] = true
			}
			if result == "accepted-cycle" && !sameReset(corrected.Reset, next.Reset) {
				retired[corrected.Reset] = true
				retired[timestamp(corrected.Cycle)] = true
			}
			original, _ = mergeWithResetCorrections(original, sample, false)
			corrected = next
		}
		if err == nil {
			err = history.Err()
		}
		history.Close()
		if err != nil {
			return err
		}
		// A projection imported without its complete history is not repairable
		// by inference. In particular, never delete arbitrary nearby tombstones.
		if !reflect.DeepEqual(original, e.window) || len(remove) == 0 {
			continue
		}
		for reset := range remove {
			if reset.IsZero() || retired[reset] {
				continue
			}
			if _, err := c.ExecContext(ctx, "DELETE FROM observation_retired WHERE account=? AND key=? AND reset=?", e.account, e.key, reset.UnixNano()); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(corrected)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE observation_windows SET body=? WHERE account=? AND key=?", string(raw), e.account, e.key); err != nil {
			return err
		}
	}
	_, err = c.ExecContext(ctx, "INSERT INTO observation_migrations(id) VALUES(?)", id)
	return err
}

// The shared report feed guards the SQLite schema cookie. Admit only this
// additive DDL, after checking the incumbent generation, in Open's transaction.
func openResetDriftSchema(c *sql.Conn) error {
	ctx := context.Background()
	var exists, feed int
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='observation_migrations'").Scan(&exists); err != nil || exists != 0 {
		return err
	}
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='measurement_report_meta'").Scan(&feed); err != nil {
		return err
	}
	if feed != 0 {
		var version int
		var saved, current int64
		if err := c.QueryRowContext(ctx, "SELECT version,schema_cookie,(SELECT schema_version FROM pragma_schema_version) FROM measurement_report_meta WHERE id=1").Scan(&version, &saved, &current); err != nil {
			return err
		}
		if version != 2 || saved != current {
			return errors.New("observation: pre-existing report feed schema change")
		}
	}
	if _, err := c.ExecContext(ctx, "CREATE TABLE observation_migrations (id TEXT PRIMARY KEY)"); err != nil {
		return err
	}
	if feed != 0 {
		_, err := c.ExecContext(ctx, "UPDATE measurement_report_meta SET schema_cookie=(SELECT schema_version FROM pragma_schema_version) WHERE id=1 AND version=2")
		return err
	}
	return nil
}
