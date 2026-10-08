package measurement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Store borrows a SQLite database. Its owner controls connection count,
// busy_timeout, WAL/durability, and Close. All queries inside a transaction use
// that transaction, including with db.SetMaxOpenConns(1).
type Store struct {
	db    *sql.DB
	clock func() time.Time
}

type Options struct{ Clock func() time.Time }

const schemaVersion = 1
const schema = `
CREATE TABLE measurement_meta (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL);
INSERT INTO measurement_meta VALUES(1,1);
CREATE TABLE measurement_books (id TEXT PRIMARY KEY, body BLOB NOT NULL);
CREATE TABLE measurement_records (
 event_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
 provider TEXT NOT NULL, account_id TEXT NOT NULL, started_at INTEGER NOT NULL, ended_at INTEGER NOT NULL,
 revision INTEGER NOT NULL, identity BLOB NOT NULL, received_at INTEGER NOT NULL,
 body BLOB NOT NULL, raw_usage BLOB NOT NULL);
CREATE UNIQUE INDEX measurement_attempt ON measurement_records(source_id,attempt_id) WHERE attempt_id<>'';
CREATE INDEX measurement_record_time ON measurement_records(provider,started_at,event_id);
CREATE TABLE measurement_record_revisions (
 event_id TEXT NOT NULL, revision INTEGER NOT NULL, received_at INTEGER NOT NULL,
 body BLOB NOT NULL, raw_usage BLOB NOT NULL, PRIMARY KEY(event_id,revision));
CREATE TABLE measurement_observations (
 id TEXT PRIMARY KEY, provider TEXT NOT NULL, account_id TEXT NOT NULL, scope TEXT NOT NULL,
 cycle_key TEXT NOT NULL, at INTEGER NOT NULL, body BLOB NOT NULL);
CREATE INDEX measurement_observation_time ON measurement_observations(provider,account_id,scope,at);
CREATE INDEX measurement_observation_cycle ON measurement_observations(cycle_key,at);
CREATE TABLE measurement_closures (cycle_key TEXT PRIMARY KEY, body BLOB NOT NULL);
CREATE TABLE measurement_delivery_runs (
 id TEXT PRIMARY KEY, started_at INTEGER NOT NULL, through_at INTEGER NOT NULL, closed INTEGER NOT NULL);
CREATE UNIQUE INDEX measurement_active_run ON measurement_delivery_runs(closed) WHERE closed=0;
CREATE TABLE measurement_gaps (
 id TEXT PRIMARY KEY, from_at INTEGER NOT NULL, to_at INTEGER NOT NULL, body BLOB NOT NULL);
CREATE INDEX measurement_gap_time ON measurement_gaps(from_at,to_at);
`

// Open creates only the bounded, empty schema on first use and checks its
// version thereafter. It does not start delivery, claim coverage, or reprice
// history. RegisterPriceBook and StartDelivery are explicit startup writes.
func Open(ctx context.Context, db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: nil database", ErrInvalid)
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	s := &Store{db: db, clock: opts.Clock}
	// Acquire the SQLite writer before inspecting metadata (also cross-pool).
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err = c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	var exists int
	if err = c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='measurement_meta'`).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		if _, err = c.ExecContext(ctx, schema); err != nil {
			return nil, fmt.Errorf("measurement schema: %w", err)
		}
	}
	var version int
	if err = c.QueryRowContext(ctx, `SELECT version FROM measurement_meta WHERE id=1`).Scan(&version); err != nil {
		return nil, err
	}
	if version != schemaVersion {
		return nil, fmt.Errorf("measurement: unsupported schema version %d", version)
	}
	// Fail closed for partial/lost schema without scanning any evidence tables.
	for _, name := range []string{"measurement_books", "measurement_records", "measurement_record_revisions", "measurement_observations", "measurement_closures", "measurement_delivery_runs", "measurement_gaps"} {
		if err = c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&exists); err != nil {
			return nil, err
		}
		if exists != 1 {
			return nil, fmt.Errorf("measurement: missing table %s", name)
		}
	}
	if _, err = c.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// First operation is a write, avoiding deferred read-to-write lock upgrades.
	res, err := tx.ExecContext(ctx, `UPDATE measurement_meta SET version=version WHERE id=1 AND version=?`, schemaVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("measurement: schema unavailable")
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func validTime(t time.Time) bool {
	return !t.IsZero() && t.UnixNano() > 0 && time.Unix(0, t.UnixNano()).Equal(t)
}
func utc(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}
func stamp(n int64) time.Time  { return time.Unix(0, n).UTC() }
func digest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func key(v any) string         { raw, _ := json.Marshal(v); return digest(raw) }

func canonicalRecord(r Record) (Record, []byte, []byte, error) {
	if r.EventID == "" || r.SourceID == "" || r.Revision < 1 || r.Attribution.Provider == "" || r.Attribution.AccountID == "" || !validTime(r.StartedAt) {
		return r, nil, nil, ErrInvalid
	}
	if !r.EndedAt.IsZero() && (!validTime(r.EndedAt) || r.EndedAt.Before(r.StartedAt)) || r.Final && r.EndedAt.IsZero() {
		return r, nil, nil, ErrInvalid
	}
	if !r.Policy.RequestStartedAt.IsZero() && !r.Policy.RequestStartedAt.Equal(r.StartedAt) || r.Policy.Revision < 0 {
		return r, nil, nil, ErrInvalid
	}
	if r.Kind != "primary" && r.Kind != "warming" && r.Kind != "title" && r.Kind != "summary" && r.Kind != "other" {
		return r, nil, nil, ErrInvalid
	}
	if len(r.RawUsage) > 1<<20 || len(r.RawUsage) > 0 && !json.Valid(r.RawUsage) {
		return r, nil, nil, fmt.Errorf("%w: raw usage must be bounded JSON", ErrInvalid)
	}
	r.StartedAt, r.EndedAt, r.Policy.RequestStartedAt = utc(r.StartedAt), utc(r.EndedAt), utc(r.Policy.RequestStartedAt)
	body, err := json.Marshal(r)
	if err != nil {
		return r, nil, nil, err
	}
	if len(body) > 64<<10 {
		return r, nil, nil, ErrInvalid
	}
	identity := r
	identity.Revision = 0
	identity.EndedAt = time.Time{}
	identity.Final = false
	identity.Success = false
	identity.Usage = Usage{}
	identity.CapturedCostNanoUSD = nil
	identity.RawUsage = nil
	id, err := json.Marshal(identity)
	return r, body, id, err
}

// PutRecord commits synchronously: return success means SQLite has accepted the
// fact under the owner's durability settings. Adapters must record any dropped
// callback/queue write as a Gap. Repeated/late snapshots are idempotent; conflicting
// equal revisions or mutated attempt identities fail without changing history.
func (s *Store) PutRecord(ctx context.Context, r Record) (result PutResult, err error) {
	r, body, identity, err := canonicalRecord(r)
	if err != nil {
		return result, err
	}
	raw := []byte(r.RawUsage)
	if raw == nil {
		raw = []byte{}
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var previous, previousRaw []byte
		e := tx.QueryRowContext(ctx, `SELECT body,raw_usage FROM measurement_record_revisions WHERE event_id=? AND revision=?`, r.EventID, r.Revision).Scan(&previous, &previousRaw)
		if e == nil {
			if !bytes.Equal(previous, body) || !bytes.Equal(previousRaw, raw) {
				return ErrConflict
			}
			result.Duplicate = true
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var oldIdentity, oldBody []byte
		var oldRevision int64
		e = tx.QueryRowContext(ctx, `SELECT revision,identity,body FROM measurement_records WHERE event_id=?`, r.EventID).Scan(&oldRevision, &oldIdentity, &oldBody)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil {
			if !bytes.Equal(identity, oldIdentity) || r.Revision <= oldRevision {
				return ErrConflict
			}
			var old Record
			if err := json.Unmarshal(oldBody, &old); err != nil {
				return err
			}
			if old.Final && (!r.Final || old.Success != r.Success || !old.EndedAt.Equal(r.EndedAt)) {
				return ErrConflict
			}
			result.Revised = true
		} else {
			if r.AttemptID != "" {
				var id string
				e = tx.QueryRowContext(ctx, `SELECT event_id FROM measurement_records WHERE source_id=? AND attempt_id=?`, r.SourceID, r.AttemptID).Scan(&id)
				if e == nil {
					return fmt.Errorf("%w: attempt already has an event ID", ErrConflict)
				}
				if !errors.Is(e, sql.ErrNoRows) {
					return e
				}
			}
			result.Inserted = true
		}
		received := s.clock().UTC().UnixNano()
		if _, e = tx.ExecContext(ctx, `INSERT INTO measurement_record_revisions(event_id,revision,received_at,body,raw_usage) VALUES(?,?,?,?,?)`, r.EventID, r.Revision, received, body, raw); e != nil {
			return e
		}
		ended := int64(0)
		if !r.EndedAt.IsZero() {
			ended = r.EndedAt.UnixNano()
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO measurement_records(event_id,source_id,attempt_id,provider,account_id,started_at,ended_at,revision,identity,received_at,body,raw_usage) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET revision=excluded.revision,ended_at=excluded.ended_at,received_at=excluded.received_at,body=excluded.body,raw_usage=excluded.raw_usage`, r.EventID, r.SourceID, r.AttemptID, r.Attribution.Provider, r.Attribution.AccountID, r.StartedAt.UnixNano(), ended, r.Revision, identity, received, body, raw)
		return e
	})
	if err != nil {
		return PutResult{}, err
	}
	return result, nil
}

// Record returns local authoritative evidence, including RawUsage; never expose
// this directly through an untrusted/public resource. revision=0 reads current.
func (s *Store) Record(ctx context.Context, eventID string, revision int64) (StoredRecord, error) {
	var raw, body []byte
	var received int64
	var err error
	if revision == 0 {
		err = s.db.QueryRowContext(ctx, `SELECT body,raw_usage,received_at FROM measurement_records WHERE event_id=?`, eventID).Scan(&body, &raw, &received)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT body,raw_usage,received_at FROM measurement_record_revisions WHERE event_id=? AND revision=?`, eventID, revision).Scan(&body, &raw, &received)
	}
	var r StoredRecord
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(body, &r.Record); err != nil {
		return r, err
	}
	r.RawUsage = raw
	r.ReceivedAt = stamp(received)
	return r, nil
}

func (s *Store) RegisterPriceBook(ctx context.Context, b PriceBook) error {
	if err := validateBook(b); err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error { return immutable(ctx, tx, "measurement_books", "id", b.ID, raw) })
}

// immutable only receives package-constant table/column names.
func immutable(ctx context.Context, tx *sql.Tx, table, column, id string, raw []byte) error {
	var old []byte
	err := tx.QueryRowContext(ctx, "SELECT body FROM "+table+" WHERE "+column+"=?", id).Scan(&old)
	if err == nil {
		if !bytes.Equal(old, raw) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO "+table+"("+column+",body) VALUES(?,?)", id, raw)
	return err
}

func (o Observation) ref() CycleRef {
	return CycleRef{o.Provider, o.AccountID, o.Scope, o.CycleID, o.Duration}
}
func validRef(r CycleRef) bool {
	return r.Provider != "" && r.AccountID != "" && r.Scope != "" && r.CycleID != "" && r.Duration > 0
}

func (s *Store) PutObservation(ctx context.Context, o Observation) error {
	if o.ID == "" || !validRef(o.ref()) || !validTime(o.At) || !finite(o.UsedPercent) || o.UsedPercent < 0 || o.UsedPercent > 100 || o.Source == "" {
		return ErrInvalid
	}
	o.At = utc(o.At)
	o.Models = append([]string(nil), o.Models...)
	sort.Strings(o.Models)
	o.Models = unique(o.Models)
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var old []byte
		e := tx.QueryRowContext(ctx, `SELECT body FROM measurement_observations WHERE id=?`, o.ID).Scan(&old)
		if e == nil {
			if !bytes.Equal(old, raw) {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		// A cycle's model membership must not silently change between readings.
		e = tx.QueryRowContext(ctx, `SELECT body FROM measurement_observations WHERE cycle_key=? LIMIT 1`, key(o.ref())).Scan(&old)
		if e == nil {
			var prior Observation
			if err := json.Unmarshal(old, &prior); err != nil {
				return err
			}
			if key(prior.Models) != key(o.Models) {
				return ErrConflict
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO measurement_observations(id,provider,account_id,scope,cycle_key,at,body) VALUES(?,?,?,?,?,?,?)`, o.ID, o.Provider, o.AccountID, o.Scope, key(o.ref()), o.At.UnixNano(), raw)
		return e
	})
}

func (s *Store) CloseCycle(ctx context.Context, c Closure) error {
	if !validRef(c.CycleRef) || !validTime(c.At) || c.Source == "" {
		return ErrInvalid
	}
	c.At = utc(c.At)
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		return immutable(ctx, tx, "measurement_closures", "cycle_key", key(c.CycleRef), raw)
	})
}

func (s *Store) PutGap(ctx context.Context, g Gap) error {
	if g.ID == "" || g.Reason == "" || !validTime(g.From) || !validTime(g.To) || g.To.Before(g.From) {
		return ErrInvalid
	}
	g.From, g.To = utc(g.From), utc(g.To)
	return s.write(ctx, func(tx *sql.Tx) error { return insertGap(ctx, tx, g) })
}

func insertGap(ctx context.Context, tx *sql.Tx, g Gap) error {
	raw, err := json.Marshal(g)
	if err != nil {
		return err
	}
	var old []byte
	err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_gaps WHERE id=?`, g.ID).Scan(&old)
	if err == nil {
		if !bytes.Equal(old, raw) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO measurement_gaps(id,from_at,to_at,body) VALUES(?,?,?,?)`, g.ID, g.From.UnixNano(), g.To.UnixNano(), raw)
	return err
}

// StartDelivery must be called for each host observer incarnation with a new ID.
// Any earlier run is closed at its last acknowledged watermark; the intervening
// restart/reload gap is durable. First start does not invent pre-start history.
func (s *Store) StartDelivery(ctx context.Context, id string, at time.Time) error {
	if id == "" || !validTime(at) || at.After(s.clock()) {
		return ErrInvalid
	}
	at = utc(at)
	return s.write(ctx, func(tx *sql.Tx) error {
		var start int64
		err := tx.QueryRowContext(ctx, `SELECT started_at FROM measurement_delivery_runs WHERE id=?`, id).Scan(&start)
		if err == nil {
			if start != at.UnixNano() {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var prev string
		var through int64
		err = tx.QueryRowContext(ctx, `SELECT id,through_at FROM measurement_delivery_runs ORDER BY started_at DESC LIMIT 1`).Scan(&prev, &through)
		if err == nil {
			if at.UnixNano() < through {
				return ErrConflict
			}
			if err = insertGap(ctx, tx, Gap{ID: "delivery:" + id, From: stamp(through), To: at, Reason: "observer_restart_or_reload"}); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE measurement_delivery_runs SET closed=1 WHERE closed=0`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO measurement_delivery_runs(id,started_at,through_at,closed) VALUES(?,?,?,0)`, id, at.UnixNano(), at.UnixNano())
		return err
	})
}

// Checkpoint acknowledges a conservative delivery horizon after callbacks have
// drained. A timer alone is not evidence. It cannot erase a previously marked gap.
// close=true ends this incarnation; a later incarnation uses StartDelivery.
func (s *Store) Checkpoint(ctx context.Context, id string, through time.Time, close bool) error {
	if !validTime(through) || through.After(s.clock()) {
		return ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var old int64
		var closed bool
		if err := tx.QueryRowContext(ctx, `SELECT through_at,closed FROM measurement_delivery_runs WHERE id=?`, id).Scan(&old, &closed); err != nil {
			return err
		}
		if through.UnixNano() < old || closed && (through.UnixNano() != old || !close) {
			return ErrConflict
		}
		_, err := tx.ExecContext(ctx, `UPDATE measurement_delivery_runs SET through_at=?,closed=? WHERE id=?`, through.UnixNano(), close, id)
		return err
	})
}
