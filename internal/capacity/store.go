package capacity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("capacity: ledger unavailable")

// Retention bounds; variables only so tests can exercise them.
var maxSamples = 4096
var maxCheckpoints = 4096

// Inventory rows are run-length bindings: one row per unchanged
// (account_ref, tuple, binding, state) observed at consecutive known
// checkpoints of one host incarnation, from_seq..through_seq.
const schema = `
CREATE TABLE capacity_meta (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL);
INSERT INTO capacity_meta VALUES(1,1);
CREATE TABLE capacity_checkpoints (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 coverage TEXT NOT NULL,
 reason TEXT NOT NULL,
 incarnation TEXT NOT NULL,
 sequence INTEGER NOT NULL,
 at INTEGER NOT NULL,
 observed_at INTEGER NOT NULL,
 digest TEXT NOT NULL);
CREATE UNIQUE INDEX capacity_checkpoint_known ON capacity_checkpoints(incarnation,sequence) WHERE coverage='known';
CREATE TABLE capacity_inventory (
 account TEXT NOT NULL,
 tuple TEXT NOT NULL,
 binding_sequence INTEGER NOT NULL,
 state TEXT NOT NULL,
 from_seq INTEGER NOT NULL,
 through_seq INTEGER NOT NULL,
 from_at INTEGER NOT NULL,
 through_at INTEGER NOT NULL,
 incarnation TEXT NOT NULL,
 PRIMARY KEY(incarnation,tuple,from_seq));
CREATE INDEX capacity_inventory_through ON capacity_inventory(incarnation,through_seq);
CREATE TABLE capacity_samples (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 account TEXT NOT NULL,
 window_key TEXT NOT NULL,
 at INTEGER NOT NULL,
 body BLOB NOT NULL);
CREATE INDEX capacity_sample_account ON capacity_samples(account,at,id);
`

// Store borrows db; it never closes it or changes its connection settings.
type Store struct {
	db    *sql.DB
	clock func() time.Time
	mu    sync.Mutex
}

func Open(db *sql.DB, clock func() time.Time) (*Store, error) {
	if db == nil {
		return nil, ErrUnavailable
	}
	if clock == nil {
		clock = time.Now
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, ErrUnavailable
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	var count int
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='capacity_meta'`).Scan(&count); err != nil {
		return nil, ErrUnavailable
	}
	if count == 0 {
		if _, err = conn.ExecContext(ctx, schema); err != nil {
			return nil, ErrUnavailable
		}
	}
	var version int
	if err = conn.QueryRowContext(ctx, `SELECT version FROM capacity_meta WHERE id=1`).Scan(&version); err != nil || version != 1 {
		return nil, ErrUnavailable
	}
	for _, table := range []string{"capacity_checkpoints", "capacity_inventory", "capacity_samples"} {
		if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			return nil, ErrUnavailable
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, ErrUnavailable
	}
	return &Store{db: db, clock: clock}, nil
}

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func tuplesDigest(t []Tuple) string {
	raw, _ := json.Marshal(t)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// PutCheckpoint stores every inventory read, including unknown ones, and
// returns it with its ledger ID. A stale or conflicting known read is stored
// and returned as unknown; it never rewrites the ledger.
func (s *Store) PutCheckpoint(ctx context.Context, c Checkpoint) (Checkpoint, error) {
	c.At = c.At.Round(0).UTC()
	c.Tuples = append([]Tuple{}, c.Tuples...)
	sortTuples(c.Tuples)
	if c.Coverage == "known" && (c.Incarnation == "" || c.Sequence > math.MaxInt64 || !validTime(c.At)) {
		c.Coverage, c.Reason = "unknown", "inventory_invalid"
	}
	observed := s.clock().UnixNano()
	err := s.write(ctx, func(tx *sql.Tx) error {
		insert := func() error {
			if c.Coverage != "known" {
				c.Tuples = nil
				if c.Coverage == "" {
					c.Coverage = "unknown"
				}
				if c.Reason == "" {
					c.Reason = "inventory_unknown"
				}
			}
			at := int64(0)
			if !c.At.IsZero() {
				at = c.At.UnixNano()
			}
			sequence := int64(0)
			if c.Sequence <= math.MaxInt64 {
				sequence = int64(c.Sequence)
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO capacity_checkpoints(coverage,reason,incarnation,sequence,at,observed_at,digest) VALUES(?,?,?,?,?,?,?)`, c.Coverage, c.Reason, c.Incarnation, sequence, at, observed, tuplesDigest(c.Tuples))
			if err != nil {
				return err
			}
			c.ID, err = res.LastInsertId()
			return err
		}
		if c.Coverage != "known" {
			return insert()
		}
		digest := tuplesDigest(c.Tuples)
		var id int64
		var old string
		err := tx.QueryRowContext(ctx, `SELECT id,digest FROM capacity_checkpoints WHERE coverage='known' AND incarnation=? AND sequence=?`, c.Incarnation, int64(c.Sequence)).Scan(&id, &old)
		if err == nil {
			if old == digest {
				c.ID = id
				return nil
			}
			c.Coverage, c.Reason = "unknown", "conflicting_inventory_sequence"
			return insert()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previous sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT max(sequence) FROM capacity_checkpoints WHERE coverage='known' AND incarnation=?`, c.Incarnation).Scan(&previous); err != nil {
			return err
		}
		if previous.Valid && previous.Int64 > int64(c.Sequence) {
			c.Coverage, c.Reason = "unknown", "stale_inventory_sequence"
			return insert()
		}
		if err = insert(); err != nil {
			return err
		}
		for _, t := range c.Tuples {
			extended := int64(0)
			if previous.Valid {
				res, err := tx.ExecContext(ctx, `UPDATE capacity_inventory SET through_seq=?,through_at=? WHERE incarnation=? AND tuple=? AND account=? AND binding_sequence=? AND state=? AND through_seq=?`, int64(c.Sequence), c.At.UnixNano(), c.Incarnation, t.Ref, t.AccountRef, int64(t.BindingSequence), t.State, previous.Int64)
				if err != nil {
					return err
				}
				if extended, err = res.RowsAffected(); err != nil {
					return err
				}
			}
			if extended == 0 {
				if _, err = tx.ExecContext(ctx, `INSERT INTO capacity_inventory(account,tuple,binding_sequence,state,from_seq,through_seq,from_at,through_at,incarnation) VALUES(?,?,?,?,?,?,?,?,?)`, t.AccountRef, t.Ref, int64(t.BindingSequence), t.State, int64(c.Sequence), int64(c.Sequence), c.At.UnixNano(), c.At.UnixNano(), c.Incarnation); err != nil {
					return err
				}
			}
		}
		return s.retainCheckpoints(ctx, tx)
	})
	if err != nil {
		return Checkpoint{}, err
	}
	return c, nil
}

func (s *Store) retainCheckpoints(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM capacity_checkpoints WHERE id <= (SELECT id FROM capacity_checkpoints ORDER BY id DESC LIMIT 1 OFFSET ?)`, maxCheckpoints); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM capacity_inventory WHERE through_seq < COALESCE((SELECT min(sequence) FROM capacity_checkpoints c WHERE c.coverage='known' AND c.incarnation=capacity_inventory.incarnation), 9223372036854775807)`)
	return err
}

// Checkpoint reconstructs a stored checkpoint from the ledger rows. A missing
// checkpoint, or rows that no longer match its digest, is unknown.
func (s *Store) Checkpoint(ctx context.Context, id int64) (Checkpoint, error) {
	c := Checkpoint{ID: id}
	var sequence, at int64
	var digest string
	err := s.db.QueryRowContext(ctx, `SELECT coverage,reason,incarnation,sequence,at,digest FROM capacity_checkpoints WHERE id=?`, id).Scan(&c.Coverage, &c.Reason, &c.Incarnation, &sequence, &at, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{ID: id, Coverage: "unknown", Reason: "inventory_missing"}, nil
	}
	if err != nil {
		return Checkpoint{}, err
	}
	c.Sequence = uint64(sequence)
	if at != 0 {
		c.At = time.Unix(0, at).UTC()
	}
	if c.Coverage != "known" {
		return c, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT account,tuple,binding_sequence,state FROM capacity_inventory WHERE incarnation=? AND from_seq<=? AND through_seq>=?`, c.Incarnation, sequence, sequence)
	if err != nil {
		return Checkpoint{}, err
	}
	defer rows.Close()
	c.Tuples = []Tuple{}
	for rows.Next() {
		var t Tuple
		var binding int64
		if err = rows.Scan(&t.AccountRef, &t.Ref, &binding, &t.State); err != nil {
			return Checkpoint{}, err
		}
		t.BindingSequence = uint64(binding)
		c.Tuples = append(c.Tuples, t)
	}
	if err = rows.Err(); err != nil {
		return Checkpoint{}, err
	}
	sortTuples(c.Tuples)
	if tuplesDigest(c.Tuples) != digest {
		return Checkpoint{ID: id, Coverage: "unknown", Reason: "inventory_ledger_changed"}, nil
	}
	return c, nil
}

func (s *Store) PutSample(ctx context.Context, x Sample) (int64, error) {
	x.At = x.At.Round(0).UTC()
	x.Reset = x.Reset.Round(0).UTC()
	x.ID = 0
	body, err := json.Marshal(x)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO capacity_samples(account,window_key,at,body) VALUES(?,?,?,?)`, x.Account, x.Window.Key(), x.At.UnixNano(), body)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM capacity_samples WHERE id <= (SELECT id FROM capacity_samples ORDER BY id DESC LIMIT 1 OFFSET ?)`, maxSamples)
		return err
	})
	return id, err
}

// Samples returns an account's newest samples first, across windows.
func (s *Store) Samples(ctx context.Context, account string, limit int) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,body FROM capacity_samples WHERE account=? ORDER BY at DESC,id DESC LIMIT ?`, account, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var id int64
		var body []byte
		if err = rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		var x Sample
		if err = json.Unmarshal(body, &x); err != nil {
			return nil, err
		}
		x.ID = id
		out = append(out, x)
	}
	return out, rows.Err()
}
