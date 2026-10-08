package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type evidence struct {
	RequireSourceCoverage bool
	Sources               map[string]SourceSnapshot
	Book                  PriceBook
	Records               []Record
	Observations          []Observation
	Closures              map[string]Closure
	Runs                  []DeliveryRun
	Gaps                  []Gap
}

// Measure is read-only, with a consistent SQLite snapshot. AsOf is an event-time
// boundary, not a historical knowledge-time pin: late usage revisions change the
// EvidenceID. Preserve a report if exact prior report replay is needed.
func (s *Store) Measure(ctx context.Context, q Query) (Report, error) {
	var out Report
	if q.Provider == "" || q.AccountID == "" || q.Scope == "" || q.BookID == "" || !validTime(q.Since) || !validTime(q.AsOf) || !q.Since.Before(q.AsOf) {
		return out, ErrInvalid
	}
	if q.MaxRecords == 0 {
		q.MaxRecords = 100000
	}
	if q.MaxRecords < 1 || q.MaxRecords > 1000000 {
		return out, ErrInvalid
	}
	q.Since, q.AsOf = utc(q.Since), utc(q.AsOf)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	e := evidence{Closures: map[string]Closure{}, RequireSourceCoverage: q.RequireSourceCoverage, Sources: map[string]SourceSnapshot{}}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_books WHERE id=?`, q.BookID).Scan(&raw); err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &e.Book); err != nil {
		return out, err
	}
	// One maximum known TTL of lookback establishes whether the first stretch
	// is left-censored. Long streams/pending attempts are included for boundaries.
	lookback := q.Since.Add(-time.Hour)
	if err = readBodies(ctx, tx, `SELECT body FROM measurement_records WHERE provider=? AND started_at<=? AND (started_at>=? OR ended_at>=? OR ended_at=0) ORDER BY started_at,event_id LIMIT ?`, []any{q.Provider, q.AsOf.UnixNano(), lookback.UnixNano(), lookback.UnixNano(), q.MaxRecords + 1}, func(raw []byte) error {
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		e.Records = append(e.Records, r)
		// Defense in depth: native records can never borrow synthetic legacy runs
		// even when a caller forgets to use the scoped-query hook.
		if r.Native != nil {
			e.RequireSourceCoverage = true
		}
		if len(e.Records) > q.MaxRecords {
			return ErrLimit
		}
		return nil
	}); err != nil {
		return out, err
	}
	if err = readBodies(ctx, tx, `SELECT body FROM measurement_observations WHERE provider=? AND account_id=? AND scope=? AND at>=? AND at<=? ORDER BY at,id LIMIT ?`, []any{q.Provider, q.AccountID, q.Scope, q.Since.UnixNano(), q.AsOf.UnixNano(), q.MaxRecords + 1}, func(raw []byte) error {
		var o Observation
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		e.Observations = append(e.Observations, o)
		if len(e.Observations) > q.MaxRecords {
			return ErrLimit
		}
		return nil
	}); err != nil {
		return out, err
	}
	// Rows from the previous queries are fully closed before another SQL call.
	for _, o := range e.Observations {
		id := key(o.ref())
		if _, ok := e.Closures[id]; ok {
			continue
		}
		var c Closure
		err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_closures WHERE cycle_key=?`, id).Scan(&raw)
		if err != nil && err != sql.ErrNoRows {
			return out, err
		}
		if err == nil {
			if err = json.Unmarshal(raw, &c); err != nil {
				return out, err
			}
		}
		e.Closures[id] = c
	}
	if e.RequireSourceCoverage {
		if len(q.AccountScopes) > MaxSourceMembers {
			return out, ErrLimit
		}
		for account, id := range q.AccountScopes {
			v, err := readSourceSnapshot(ctx, tx, q.SourceConsumerID, id, q.Provider, account)
			if err != nil {
				return out, err
			}
			e.Sources[account] = v
		}
	} else {
		rows, err := tx.QueryContext(ctx, `SELECT id,started_at,through_at,closed FROM measurement_delivery_runs WHERE started_at<=? AND through_at>=? ORDER BY started_at,id LIMIT ?`, q.AsOf.UnixNano(), lookback.UnixNano(), q.MaxRecords+1)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var r DeliveryRun
			var start, through int64
			if err = rows.Scan(&r.ID, &start, &through, &r.Closed); err != nil {
				rows.Close()
				return out, err
			}
			r.StartedAt, r.Through = stamp(start), stamp(through)
			e.Runs = append(e.Runs, r)
			if len(e.Runs) > q.MaxRecords {
				rows.Close()
				return out, ErrLimit
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	if err = readBodies(ctx, tx, `SELECT body FROM measurement_gaps WHERE from_at<=? AND to_at>=? ORDER BY from_at,id LIMIT ?`, []any{q.AsOf.UnixNano(), lookback.UnixNano(), q.MaxRecords + 1}, func(raw []byte) error {
		var g Gap
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		e.Gaps = append(e.Gaps, g)
		if len(e.Gaps) > q.MaxRecords {
			return ErrLimit
		}
		return nil
	}); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	out = analyze(e, q)
	return out, nil
}

func readBodies(ctx context.Context, tx *sql.Tx, query string, args []any, visit func([]byte) error) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if err = visit(raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

func coverage(e evidence, account string, from, to time.Time) Coverage {
	if e.RequireSourceCoverage {
		return scopedCoverage(e, account, from, to)
	}
	c := Coverage{Delivery: "native-in-memory-best-effort"}
	if to.Before(from) {
		c.Reasons = []string{"invalid_interval"}
		return c
	}
	cursor := from
	for _, run := range e.Runs {
		if run.Through.Before(cursor) {
			continue
		}
		if run.StartedAt.After(cursor) {
			break
		}
		if run.Through.After(cursor) {
			cursor = run.Through
		}
		if !cursor.Before(to) {
			break
		}
	}
	if cursor.Before(to) || from.Equal(to) && !pointCovered(e.Runs, from) {
		c.Reasons = append(c.Reasons, "unacknowledged_delivery_interval")
	}
	for _, g := range e.Gaps {
		if g.AccountID != "" && g.AccountID != account {
			continue
		}
		// Gaps are [From,To); a zero-duration restart is a discontinuity point.
		overlaps := g.From.Before(to) && g.To.After(from)
		if g.From.Equal(g.To) {
			overlaps = !g.From.Before(from) && g.From.Before(to)
		}
		if overlaps {
			c.Reasons = append(c.Reasons, "delivery_gap:"+g.Reason)
		}
	}
	c.Reasons = unique(c.Reasons)
	c.Observed = len(c.Reasons) == 0
	return c
}

func pointCovered(runs []DeliveryRun, t time.Time) bool {
	for _, r := range runs {
		if !t.Before(r.StartedAt) && !t.After(r.Through) {
			return true
		}
	}
	return false
}

func evidenceID(e evidence, q Query) string {
	// Raw usage does not affect normalized valuation; revisions bind it to the
	// immutable local evidence. This is a fingerprint, not a replay checkpoint.
	payload := struct {
		Version  string
		Query    Query
		Evidence evidence
	}{ValuationVersion, q, e}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("validated measurement evidence cannot encode: %v", err))
	}
	return digest(raw)
}
