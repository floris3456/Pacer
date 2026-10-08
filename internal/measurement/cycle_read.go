package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"
)

// Closures returns every recorded cycle closure in observed end order. It is
// read-only and fails with ErrLimit rather than truncating.
func (s *Store) Closures(ctx context.Context, limit int) ([]Closure, error) {
	if limit < 1 {
		return nil, ErrInvalid
	}
	var out []Closure
	err := s.read(ctx, func(tx *sql.Tx) error {
		return readBodies(ctx, tx, `SELECT body FROM measurement_closures LIMIT ?`, []any{limit + 1}, func(raw []byte) error {
			var c Closure
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
			out = append(out, c)
			if len(out) > limit {
				return ErrLimit
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return key(out[i].CycleRef) < key(out[j].CycleRef)
	})
	return out, nil
}

// LastReading returns the newest usable observation of one cycle at or before at.
func (s *Store) LastReading(ctx context.Context, ref CycleRef, at time.Time) (Observation, bool, error) {
	var out Observation
	if !validRef(ref) || !validTime(at) {
		return out, false, ErrInvalid
	}
	found := false
	err := s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT body FROM measurement_observations WHERE cycle_key=? AND at<=? ORDER BY at DESC,id DESC`, key(ref), utc(at).UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				return err
			}
			var o Observation
			if err = json.Unmarshal(raw, &o); err != nil {
				return err
			}
			if o.Usable {
				out, found = o, true
				return nil
			}
		}
		return rows.Err()
	})
	if err != nil {
		return Observation{}, false, err
	}
	return out, found, nil
}

// RecordsStarted returns one provider's records started in [from,to), in
// start order. It fails with ErrLimit rather than truncating.
func (s *Store) RecordsStarted(ctx context.Context, provider string, from, to time.Time, limit int) ([]Record, error) {
	if provider == "" || !validTime(from) || !validTime(to) || to.Before(from) || limit < 1 {
		return nil, ErrInvalid
	}
	var out []Record
	err := s.read(ctx, func(tx *sql.Tx) error {
		return readBodies(ctx, tx, `SELECT body FROM measurement_records WHERE provider=? AND started_at>=? AND started_at<? ORDER BY started_at,event_id LIMIT ?`, []any{provider, utc(from).UnixNano(), utc(to).UnixNano(), limit + 1}, func(raw []byte) error {
			var r Record
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			out = append(out, r)
			if len(out) > limit {
				return ErrLimit
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ObservedCycles returns the distinct cycles of one quota pool with a reading
// in [from,to], ordered by their first reading in that range. It fails with
// ErrLimit rather than truncating.
func (s *Store) ObservedCycles(ctx context.Context, provider, account, scope string, from, to time.Time, limit int) ([]CycleRef, error) {
	if provider == "" || account == "" || scope == "" || !validTime(from) || !validTime(to) || to.Before(from) || limit < 1 {
		return nil, ErrInvalid
	}
	var out []CycleRef
	err := s.read(ctx, func(tx *sql.Tx) error {
		// SQLite takes the bare body column from the row holding min(at).
		return readBodies(ctx, tx, `SELECT body FROM (SELECT body,min(at) AS first,cycle_key FROM measurement_observations WHERE provider=? AND account_id=? AND scope=? AND at>=? AND at<=? GROUP BY cycle_key) ORDER BY first,cycle_key LIMIT ?`, []any{provider, account, scope, utc(from).UnixNano(), utc(to).UnixNano(), limit + 1}, func(raw []byte) error {
			var o Observation
			if err := json.Unmarshal(raw, &o); err != nil {
				return err
			}
			out = append(out, o.ref())
			if len(out) > limit {
				return ErrLimit
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) read(ctx context.Context, visit func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = visit(tx); err != nil {
		return err
	}
	return tx.Commit()
}
