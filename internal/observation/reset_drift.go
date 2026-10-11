package observation

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// Repair only retirements proven to be corrections by the original audit.
// Unexplained markers and real replenishments remain fenced. History is never
// rewritten or refreshed: the next live observation supplies the new balance.
func migrateCodexResetDrift(c *sql.Conn) error {
	ctx := context.Background()
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
