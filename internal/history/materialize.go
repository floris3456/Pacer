package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// MaterializeReports is a trusted offline library boundary, never an HTTP
// capability. It creates a NEW disposable report DB from a published archive.
// Only compiled CPAMP schema is executed, never archival SQL or source triggers.
// The caller prepares derived books then closes/reopens the result read-only.
func (s *Store) MaterializeReports(ctx context.Context, path string) (boundary Boundary, err error) {
	p, e := s.Status(ctx)
	if e != nil {
		return boundary, e
	}
	if !p.Complete {
		return boundary, ErrPending
	}
	boundary = p.Boundary
	abs, e := filepath.Abs(path)
	if e != nil {
		return boundary, ErrInput
	}
	f, e := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return boundary, ErrInput
	}
	f.Close()
	success := false
	defer func() {
		if !success {
			os.Remove(abs)
		}
	}()
	db, e := sql.Open("sqlite3", dsn(abs, "mode=rw&_journal_mode=DELETE&_synchronous=FULL"))
	if e != nil {
		return boundary, ErrStorage
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.ExecContext(ctx, cpampSchema); e != nil {
		return boundary, ErrStorage
	}
	// Only tables used by report calculations. Raw ledger identity fallback
	// excludes raw_json, failure bodies and arbitrary request metadata entirely.
	names := []string{"analytics_state", "analytics_accounts", "analytics_events", "analytics_event_keys", "analytics_observations", "analytics_prices", "analytics_poll_runs", "analytics_profile_revisions", "analytics_normalization_rules", "analytics_credential_inventory", "analytics_forecasts", "analytics_forecast_results", "analytics_forecast_work", "analytics_historical_valuations", "analytics_token_evidence", "model_prices", "model_price_context_tiers", "model_price_service_tiers", "api_key_aliases", "api_key_alias_history", "usage_events"}
	for _, name := range names {
		var raw []byte
		var spec tableSpec
		var expected int64
		var expectedChain string
		e = s.db.QueryRowContext(ctx, `SELECT spec,rows,chain FROM history_tables WHERE name=? AND complete=1`, name).Scan(&raw, &expected, &expectedChain)
		if errors.Is(e, sql.ErrNoRows) && boundary.Schema == "cpamp-usage-v2" && (name == "analytics_token_evidence" || name == "analytics_historical_valuations") {
			continue
		}
		if e != nil || json.Unmarshal(raw, &spec) != nil || spec.Virtual || len(spec.Keys) != 1 {
			return boundary, ErrSource
		}
		key := strings.ToLower(spec.Keys[0])
		if key != "_rowid_" && key != "rowid" && key != "oid" {
			return boundary, ErrSource
		}
		cols, e := columns(ctx, db, name)
		if e != nil {
			return boundary, e
		}
		indexes := []int{}
		targets := []string{"_rowid_"}
		marks := []string{"?"}
		for i, col := range spec.Columns {
			if cols[col].Type == "" {
				continue
			}
			if name == "usage_events" && !strings.Contains("|id|event_hash|timestamp_ms|timestamp|model|api_key_hash|created_at_ms|", "|"+col+"|") {
				continue
			}
			indexes = append(indexes, i)
			targets = append(targets, quote(col))
			marks = append(marks, "?")
		}
		insert := "INSERT INTO " + quote(name) + "(" + strings.Join(targets, ",") + ") VALUES(" + strings.Join(marks, ",") + ")"
		var after int64
		chain := ""
		for {
			rows, e := s.db.QueryContext(ctx, `SELECT ordinal,key,cells FROM history_rows WHERE tbl=? AND ordinal>? ORDER BY ordinal LIMIT 250`, name, after)
			if e != nil {
				return boundary, ErrStorage
			}
			type entry struct {
				ordinal int64
				r       archiveRow
			}
			batch := []entry{}
			for rows.Next() {
				var ordinal int64
				var k, c []byte
				var r archiveRow
				if rows.Scan(&ordinal, &k, &c) != nil || json.Unmarshal(k, &r.Key) != nil || json.Unmarshal(c, &r.Cells) != nil || len(r.Cells) != len(spec.Columns) || len(r.Key) != 1 {
					rows.Close()
					return boundary, ErrSource
				}
				batch = append(batch, entry{ordinal, r})
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return boundary, ErrStorage
			}
			if len(batch) == 0 {
				break
			}
			tx, e := db.BeginTx(ctx, nil)
			if e != nil {
				return boundary, ErrStorage
			}
			for _, item := range batch {
				if item.ordinal != after+1 {
					tx.Rollback()
					return boundary, ErrSource
				}
				rowid, e := item.r.Key[0].value()
				if e != nil || item.r.Key[0].Type != "integer" {
					tx.Rollback()
					return boundary, ErrSource
				}
				args := []any{rowid}
				for _, i := range indexes {
					v, e := item.r.Cells[i].value()
					if e != nil {
						tx.Rollback()
						return boundary, ErrSource
					}
					args = append(args, v)
				}
				if _, e = tx.ExecContext(ctx, insert, args...); e != nil {
					tx.Rollback()
					return boundary, ErrSource
				}
				after = item.ordinal
				chain = chainHash(chain, item.r)
			}
			if tx.Commit() != nil {
				return boundary, ErrStorage
			}
		}
		if after != expected || chain != expectedChain {
			return boundary, ErrDigest
		}
	}
	_, e = db.ExecContext(ctx, `CREATE INDEX report_events_account ON analytics_events(account,started_ms,event_id);CREATE INDEX report_observations_account ON analytics_observations(account,observed_ms);CREATE INDEX report_observations_window ON analytics_observations(window_id);CREATE INDEX report_tokens_event ON analytics_token_evidence(event_hash,revision);`)
	if e != nil {
		return boundary, ErrStorage
	}
	success = true
	return boundary, nil
}
