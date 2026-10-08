package prefixevidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	// pendingLimit bounds attempts with exactly one side (fact or settlement) present.
	pendingLimit = 1024
	// pendingAge bounds how long a one-sided attempt may wait for its counterpart.
	pendingAge = 15 * time.Minute
	// anchorRetention keeps anchors for 2h by primary start: max TTL 1h plus slack.
	anchorRetention = 2 * time.Hour
	// dependentWindow bounds which warming attempts one anchor change re-resolves:
	// those starting in [anchor start, anchor start + dependentWindow]. A warming
	// attempt starting later keeps the observation it was resolved with; this bounds
	// the work of a primary settle on a hot key.
	dependentWindow = 2 * time.Hour
	// attemptRetention keeps terminal attempts for 7 days by upstream start.
	attemptRetention = 7 * 24 * time.Hour
	// retentionCap bounds the rows one sweep() call deletes, across both kinds.
	retentionCap = 1000
)

// Store borrows db. It never closes it or changes its connection settings, and
// it is safe on a one-connection SQLite pool shared with the other stores.
type Store struct {
	db    *sql.DB
	clock func() time.Time
}

// schema keeps one row per physical attempt. The fact is stored as its
// canonical JSON (only the Fact fields, no content); the indexed copies exist
// for the joins and the readback. obs_* columns hold the derived observation,
// rewritten only by resolve.
const schema = `
CREATE TABLE prefix_evidence_meta (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL);
INSERT INTO prefix_evidence_meta VALUES(1,1);
CREATE TABLE prefix_attempts (
 attempt_id TEXT PRIMARY KEY,
 fact TEXT NOT NULL DEFAULT '',
 outcome TEXT NOT NULL DEFAULT '',
 arrived_ns INTEGER NOT NULL,
 observed_ns INTEGER NOT NULL DEFAULT 0,
 quarantine TEXT NOT NULL DEFAULT '',
 closed TEXT NOT NULL DEFAULT '',
 account TEXT NOT NULL DEFAULT '',
 binding TEXT NOT NULL DEFAULT '',
 key_id TEXT NOT NULL DEFAULT '',
 settings_tag TEXT NOT NULL DEFAULT '',
 caller TEXT NOT NULL DEFAULT '',
 installation TEXT NOT NULL DEFAULT '',
 session TEXT NOT NULL DEFAULT '',
 route TEXT NOT NULL DEFAULT '',
 epoch TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL DEFAULT '',
 tag TEXT NOT NULL DEFAULT '',
 started_ns INTEGER NOT NULL DEFAULT 0,
 revision INTEGER NOT NULL DEFAULT 0,
 obs_status TEXT NOT NULL DEFAULT '',
 obs_reason TEXT NOT NULL DEFAULT '',
 obs_relation TEXT NOT NULL DEFAULT '',
 obs_ttl TEXT NOT NULL DEFAULT '',
 obs_until_ns INTEGER NOT NULL DEFAULT 0,
 obs_entry_ref TEXT NOT NULL DEFAULT '',
 obs_binding_ref TEXT NOT NULL DEFAULT '',
 obs_snapshot_ref TEXT NOT NULL DEFAULT ''
);
CREATE INDEX prefix_attempts_scope ON prefix_attempts(caller, installation, session, route, epoch, started_ns);
CREATE INDEX prefix_attempts_key ON prefix_attempts(kind, account, binding, key_id, settings_tag, caller, installation, session, route, epoch, tag);
CREATE TABLE prefix_anchors (
 account TEXT NOT NULL, binding TEXT NOT NULL, key_id TEXT NOT NULL, settings_tag TEXT NOT NULL,
 caller TEXT NOT NULL, installation TEXT NOT NULL, session TEXT NOT NULL, route TEXT NOT NULL, epoch TEXT NOT NULL,
 tag TEXT NOT NULL, started_ns INTEGER NOT NULL, attempt_id TEXT NOT NULL,
 PRIMARY KEY(account, binding, key_id, settings_tag, caller, installation, session, route, epoch, tag, attempt_id)
);
CREATE INDEX prefix_anchors_attempt ON prefix_anchors(attempt_id);
`

// indexes serve the pending sweep (open rows, oldest arrival first) and the
// terminal retention (oldest age first). SQLite appends rowid to every index
// implicitly, so the ORDER BY ..., rowid tie-break is served without naming it.
// They are IF NOT EXISTS and run on every Open, so a database created before
// they existed gains them without a version change.
const indexes = `
CREATE INDEX IF NOT EXISTS prefix_attempts_pending ON prefix_attempts(arrived_ns) WHERE closed='';
CREATE INDEX IF NOT EXISTS prefix_attempts_terminal ON prefix_attempts((CASE WHEN started_ns>0 THEN started_ns ELSE arrived_ns END));
`

// txn is the statement surface of a write transaction. *sql.Conn carries the
// BEGIN IMMEDIATE transaction in mutate; *sql.Tx satisfies it for direct sweeps.
type txn interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Open creates the empty schema on first use, or validates version 1 on reopen.
// It infers nothing from other stores' tables.
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
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='prefix_evidence_meta'`).Scan(&count); err != nil {
		return nil, ErrUnavailable
	}
	if count == 0 {
		if _, err = conn.ExecContext(ctx, schema); err != nil {
			return nil, ErrUnavailable
		}
	}
	var version int
	if err = conn.QueryRowContext(ctx, `SELECT version FROM prefix_evidence_meta WHERE id=1`).Scan(&version); err != nil || version != 1 {
		return nil, ErrUnavailable
	}
	for _, table := range []string{"prefix_attempts", "prefix_anchors"} {
		if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			return nil, ErrUnavailable
		}
	}
	if _, err = conn.ExecContext(ctx, indexes); err != nil {
		return nil, ErrUnavailable
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, ErrUnavailable
	}
	return &Store{db: db, clock: clock}, nil
}

// Observe records the Pacer fact of one attempt. An identical repeat is a no-op;
// a different fact for the same attempt quarantines it and returns ErrConflict.
func (s *Store) Observe(ctx context.Context, f Fact) error {
	if !identifier(f.AttemptID) || !identifier(f.Account) || !identifier(f.BindingRef) ||
		!(scope{f.Caller, f.Installation, f.Session, f.Route, f.Epoch}).valid() ||
		!(f.StartedAt.IsZero() || validTime(f.StartedAt)) {
		return ErrInvalidRecord
	}
	// Store the instant in UTC so the same fact in another zone is the same record.
	f.StartedAt = f.StartedAt.UTC()
	canonical, err := json.Marshal(f)
	if err != nil {
		return ErrUnavailable
	}
	return s.mutate(ctx, func(tx txn, now int64) error {
		row, err := loadAttempt(ctx, tx, f.AttemptID)
		if err != nil {
			return err
		}
		switch {
		case !row.found:
			if _, err = tx.ExecContext(ctx, `INSERT INTO prefix_attempts(attempt_id, arrived_ns) VALUES(?,?)`, f.AttemptID, now); err != nil {
				return dbError(ctx, err)
			}
		case row.fact == string(canonical):
			return nil
		case row.fact != "":
			return quarantine(ctx, tx, f.AttemptID)
		}
		if row.fact == "" {
			if err := writeFact(ctx, tx, f, string(canonical), now); err != nil {
				return err
			}
		}
		return resolve(ctx, tx, f.AttemptID)
	})
}

// Settle records the terminal physical outcome of an attempt. It is ignored for
// logical-request recovery; only the CPA physical finish may call it.
func (s *Store) Settle(ctx context.Context, attemptID string, o Outcome) error {
	if !identifier(attemptID) || !validOutcome(o) {
		return ErrInvalidRecord
	}
	return s.mutate(ctx, func(tx txn, now int64) error {
		row, err := loadAttempt(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		switch {
		case !row.found:
			if _, err = tx.ExecContext(ctx, `INSERT INTO prefix_attempts(attempt_id, outcome, arrived_ns) VALUES(?,?,?)`, attemptID, string(o), now); err != nil {
				return dbError(ctx, err)
			}
			return nil
		case row.outcome == string(o):
			return nil
		case row.outcome != "":
			return quarantine(ctx, tx, attemptID)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE prefix_attempts SET outcome=? WHERE attempt_id=?`, string(o), attemptID); err != nil {
			return dbError(ctx, err)
		}
		return resolve(ctx, tx, attemptID)
	})
}

// Sweep applies the pending age and overflow bounds. Observe and Settle run it
// too, before and after their own write.
func (s *Store) Sweep(ctx context.Context) error {
	return s.mutate(ctx, func(txn, int64) error { return nil })
}

// mutate runs one write transaction on a single connection. BEGIN IMMEDIATE takes
// the write lock before the clock is read, as Open does, so the sweep and the
// write are decided under one lock. ErrConflict is returned after the quarantine
// it wrote is committed; every other error rolls back.
func (s *Store) mutate(ctx context.Context, fn func(tx txn, now int64) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return dbError(ctx, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return dbError(ctx, err)
	}
	committed := false
	defer func() {
		if !committed {
			// Background, not ctx: the rollback must still run after a cancellation.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	now := s.clock().UnixNano()
	if err := sweep(ctx, conn, now); err != nil {
		return err
	}
	ferr := fn(conn, now)
	if ferr != nil && !errors.Is(ferr, ErrConflict) {
		return ferr
	}
	if err := sweep(ctx, conn, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return dbError(ctx, err)
	}
	committed = true
	return ferr
}

func validOutcome(o Outcome) bool {
	return o == OutcomeSucceeded || o == OutcomeFailed || o == OutcomeCanceled
}

// attemptRow is the stored state of one attempt that the joins need.
type attemptRow struct {
	found      bool
	fact       string
	outcome    string
	quarantine string
	closed     string
	observedNS int64
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadAttempt(ctx context.Context, q rowQuerier, id string) (attemptRow, error) {
	var r attemptRow
	err := q.QueryRowContext(ctx, `SELECT fact, outcome, quarantine, closed, observed_ns FROM prefix_attempts WHERE attempt_id=?`, id).
		Scan(&r.fact, &r.outcome, &r.quarantine, &r.closed, &r.observedNS)
	if errors.Is(err, sql.ErrNoRows) {
		return attemptRow{}, nil
	}
	if err != nil {
		return attemptRow{}, dbError(ctx, err)
	}
	r.found = true
	return r, nil
}

// writeFact stores the canonical fact and its indexed copies. observed_ns is
// the collection clock of this Observe and is the reference clock for qualification.
func writeFact(ctx context.Context, tx txn, f Fact, canonical string, observedNS int64) error {
	keyID, settings, tag := "", "", ""
	if f.Snapshot != nil {
		keyID, settings = f.Snapshot.KeyID, f.Snapshot.SettingsTag
		if len(f.Snapshot.Endpoints) == 1 {
			tag = f.Snapshot.Endpoints[0].Tag
		}
	}
	var started int64
	if !f.StartedAt.IsZero() {
		started = f.StartedAt.UnixNano()
	}
	_, err := tx.ExecContext(ctx, `UPDATE prefix_attempts SET fact=?, observed_ns=?, account=?, binding=?, key_id=?, settings_tag=?, caller=?, installation=?, session=?, route=?, epoch=?, kind=?, tag=?, started_ns=?, revision=? WHERE attempt_id=?`,
		canonical, observedNS, f.Account, f.BindingRef, keyID, settings, f.Caller, f.Installation, f.Session, f.Route, f.Epoch, f.Kind, tag, started, f.Revision, f.AttemptID)
	if err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// quarantine marks an attempt whose immutable inputs conflict. Its observation
// becomes uncertain/conflicting_evidence, and nothing can clear it.
func quarantine(ctx context.Context, tx txn, id string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE prefix_attempts SET quarantine=? WHERE attempt_id=?`, reasonConflicting, id); err != nil {
		return dbError(ctx, err)
	}
	if err := resolve(ctx, tx, id); err != nil {
		return err
	}
	return ErrConflict
}

// resolve recomputes one attempt's derived observation from its stored inputs.
// It depends only on those inputs, the collection clock stored at Observe and
// the current anchors, so the join order cannot change the result. When an
// attempt's anchors change, the warming attempts that depend on them are resolved again.
func resolve(ctx context.Context, tx txn, id string) error {
	row, err := loadAttempt(ctx, tx, id)
	if err != nil {
		return err
	}
	if row.fact == "" {
		// Settlement without a fact: nothing to derive yet.
		return nil
	}
	var f Fact
	if err := json.Unmarshal([]byte(row.fact), &f); err != nil {
		return ErrUnavailable
	}
	var d derivedRow
	switch {
	case row.quarantine != "":
		d = derivedRow{status: statusUncertain, reason: reasonConflicting}
	case row.closed != "":
		d = derivedRow{status: statusUnverified, reason: row.closed}
	default:
		var o *Outcome
		if row.outcome != "" {
			oo := Outcome(row.outcome)
			o = &oo
		}
		anchorID, anchored, err := lookupAnchor(ctx, tx, f)
		if err != nil {
			return err
		}
		d = deriveRow(id, f, Qualify(f, o, anchored, time.Unix(0, row.observedNS)), anchorID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE prefix_attempts SET obs_status=?, obs_reason=?, obs_relation=?, obs_ttl=?, obs_until_ns=?, obs_entry_ref=?, obs_binding_ref=?, obs_snapshot_ref=? WHERE attempt_id=?`,
		d.status, d.reason, d.relation, d.ttl, d.untilNS, d.entry, d.binding, d.snapshot, id); err != nil {
		return dbError(ctx, err)
	}
	if f.Kind == KindPrimary {
		return syncAnchors(ctx, tx, id, anchorsFor(f, row))
	}
	return nil
}

// derivedRow holds the readback-ready columns of one observation.
type derivedRow struct {
	status, reason, relation, ttl string
	untilNS                       int64
	entry, binding, snapshot      string
}

func deriveRow(id string, f Fact, obs Observation, anchorID string) derivedRow {
	d := derivedRow{status: obs.Status, reason: obs.Reason}
	if obs.Status != statusObserved {
		return d
	}
	// An observed result always has exactly one endpoint (see qualify).
	d.relation, d.ttl, d.untilNS = obs.Relation, obs.TTL, obs.Until.UnixNano()
	d.entry = entryRef(f.Snapshot.Endpoints[0].Tag)
	d.binding = hashRef(bindingDomain, f.BindingRef)
	ref := id
	if obs.Relation == relationMatched {
		ref = anchorID
	}
	d.snapshot = hashRef(attemptDomain, ref)
	return d
}

// anchorKey is the identity a warming attempt must match exactly.
type anchorKey struct {
	account, binding, keyID, settings, caller, installation, session, route, epoch, tag string
	startedNS                                                                           int64
	attemptID                                                                           string
}

// anchorsFor lists the anchors a primary attempt registers: every endpoint of a
// dispatched primary with a succeeded settlement and a valid snapshot, of any shape.
// Quarantined or timed-out attempts register none.
func anchorsFor(f Fact, row attemptRow) []anchorKey {
	if f.Kind != KindPrimary || row.outcome != string(OutcomeSucceeded) || row.quarantine != "" || row.closed != "" ||
		!f.Dispatched || f.StartedAt.IsZero() || f.Snapshot == nil || f.Snapshot.Version != 1 {
		return nil
	}
	out := make([]anchorKey, 0, len(f.Snapshot.Endpoints))
	for _, e := range f.Snapshot.Endpoints {
		out = append(out, anchorKey{
			account: f.Account, binding: f.BindingRef, keyID: f.Snapshot.KeyID, settings: f.Snapshot.SettingsTag,
			caller: f.Caller, installation: f.Installation, session: f.Session, route: f.Route, epoch: f.Epoch,
			tag: e.Tag, startedNS: f.StartedAt.UnixNano(), attemptID: f.AttemptID,
		})
	}
	return out
}

// lookupAnchor finds the latest primary anchor whose start is not later than
// the warming start, for the exact key. Only a warming fact with one endpoint can match.
func lookupAnchor(ctx context.Context, tx txn, f Fact) (string, bool, error) {
	if f.Kind != KindWarming || f.Snapshot == nil || len(f.Snapshot.Endpoints) != 1 || f.StartedAt.IsZero() {
		return "", false, nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM prefix_anchors WHERE account=? AND binding=? AND key_id=? AND settings_tag=? AND caller=? AND installation=? AND session=? AND route=? AND epoch=? AND tag=? AND started_ns<=? ORDER BY started_ns DESC, attempt_id DESC LIMIT 1`,
		f.Account, f.BindingRef, f.Snapshot.KeyID, f.Snapshot.SettingsTag, f.Caller, f.Installation, f.Session, f.Route, f.Epoch,
		f.Snapshot.Endpoints[0].Tag, f.StartedAt.UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, dbError(ctx, err)
	}
	return id, true, nil
}

// syncAnchors replaces one primary attempt's anchors, then resolves every
// warming attempt whose key matched the old or the new anchors.
func syncAnchors(ctx context.Context, tx txn, id string, wanted []anchorKey) error {
	old, err := anchorsOf(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM prefix_anchors WHERE attempt_id=?`, id); err != nil {
		return dbError(ctx, err)
	}
	for _, a := range wanted {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO prefix_anchors(account, binding, key_id, settings_tag, caller, installation, session, route, epoch, tag, started_ns, attempt_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			a.account, a.binding, a.keyID, a.settings, a.caller, a.installation, a.session, a.route, a.epoch, a.tag, a.startedNS, a.attemptID); err != nil {
			return dbError(ctx, err)
		}
	}
	for _, k := range append(old, wanted...) {
		deps, err := dependents(ctx, tx, k)
		if err != nil {
			return err
		}
		for _, d := range deps {
			if d != id {
				if err := resolve(ctx, tx, d); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func anchorsOf(ctx context.Context, tx txn, id string) ([]anchorKey, error) {
	// started_ns is loaded too: dependents bounds its window by the anchor start.
	rows, err := tx.QueryContext(ctx, `SELECT account, binding, key_id, settings_tag, caller, installation, session, route, epoch, tag, started_ns FROM prefix_anchors WHERE attempt_id=?`, id)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer rows.Close()
	var out []anchorKey
	for rows.Next() {
		var k anchorKey
		if err := rows.Scan(&k.account, &k.binding, &k.keyID, &k.settings, &k.caller, &k.installation, &k.session, &k.route, &k.epoch, &k.tag, &k.startedNS); err != nil {
			return nil, dbError(ctx, err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(ctx, err)
	}
	return out, nil
}

// dependents returns the settled warming attempts that match an anchor key and
// start within dependentWindow of the anchor start (the same lower bound as
// lookupAnchor). The ids are collected before any write, because the single
// transaction connection cannot run a write while a result set is open.
func dependents(ctx context.Context, tx txn, k anchorKey) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT attempt_id FROM prefix_attempts WHERE kind=? AND outcome<>'' AND fact<>'' AND account=? AND binding=? AND key_id=? AND settings_tag=? AND caller=? AND installation=? AND session=? AND route=? AND epoch=? AND tag=? AND started_ns>=? AND started_ns<=?`,
		KindWarming, k.account, k.binding, k.keyID, k.settings, k.caller, k.installation, k.session, k.route, k.epoch, k.tag,
		k.startedNS, k.startedNS+int64(dependentWindow))
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, dbError(ctx, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(ctx, err)
	}
	return ids, nil
}

// sweep closes one-sided attempts older than pendingAge, then the oldest one-sided
// attempts beyond pendingLimit. Closed attempts are terminal: a late counterpart
// is recorded but never promotes them. It then deletes retired rows, at most
// retentionCap in total: anchors first, then terminal attempts. Pending rows are
// never retired.
func sweep(ctx context.Context, tx txn, now int64) error {
	aged, err := idsOf(ctx, tx, `SELECT attempt_id FROM prefix_attempts WHERE closed='' AND ((fact<>'') <> (outcome<>'')) AND arrived_ns<=? ORDER BY arrived_ns, rowid`, now-int64(pendingAge))
	if err != nil {
		return err
	}
	for _, id := range aged {
		if err := closeAttempt(ctx, tx, id, reasonSettlementTimeout); err != nil {
			return err
		}
	}
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM prefix_attempts WHERE closed='' AND ((fact<>'') <> (outcome<>''))`).Scan(&open); err != nil {
		return dbError(ctx, err)
	}
	if excess := open - pendingLimit; excess > 0 {
		overflow, err := idsOf(ctx, tx, `SELECT attempt_id FROM prefix_attempts WHERE closed='' AND ((fact<>'') <> (outcome<>'')) ORDER BY arrived_ns, rowid LIMIT ?`, int64(excess))
		if err != nil {
			return err
		}
		for _, id := range overflow {
			if err := closeAttempt(ctx, tx, id, reasonPendingOverflow); err != nil {
				return err
			}
		}
	}
	budget := retentionCap
	n, err := deleteAnchors(ctx, tx, now-int64(anchorRetention), budget)
	if err != nil {
		return err
	}
	budget -= n
	if budget > 0 {
		if _, err := deleteTerminal(ctx, tx, now-int64(attemptRetention), budget); err != nil {
			return err
		}
	}
	return nil
}

// deleteAnchors removes up to limit anchors whose primary start is before cutoff,
// oldest first. A warming attempt that arrives later finds no anchor and gets
// no_primary_anchor, which is the conservative outcome.
func deleteAnchors(ctx context.Context, tx txn, cutoff int64, limit int) (int, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM prefix_anchors WHERE rowid IN (SELECT rowid FROM prefix_anchors WHERE started_ns<? ORDER BY started_ns, rowid LIMIT ?)`, cutoff, limit)
	if err != nil {
		return 0, dbError(ctx, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, dbError(ctx, err)
	}
	return int(n), nil
}

// deleteTerminal removes up to limit terminal attempts (both sides present, or
// closed) whose age is before cutoff, oldest first. Age is the upstream start, or
// the arrival time for a settlement without a fact. The newest attempt of each
// (scope, epoch) is always kept so readback still has its last observation.
func deleteTerminal(ctx context.Context, tx txn, cutoff int64, limit int) (int, error) {
	const age = `(CASE WHEN started_ns>0 THEN started_ns ELSE arrived_ns END)`
	ids, err := idsOf(ctx, tx, `SELECT attempt_id FROM prefix_attempts
 WHERE (closed<>'' OR (fact<>'' AND outcome<>'')) AND `+age+`<?
   AND attempt_id NOT IN (
     SELECT attempt_id FROM (
       SELECT attempt_id, ROW_NUMBER() OVER (PARTITION BY caller, installation, session, route, epoch ORDER BY started_ns DESC, attempt_id DESC) AS rn
       FROM prefix_attempts WHERE fact<>'')
     WHERE rn=1)
 ORDER BY `+age+`, rowid LIMIT ?`, cutoff, int64(limit))
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM prefix_attempts WHERE attempt_id=?`, id); err != nil {
			return 0, dbError(ctx, err)
		}
	}
	return len(ids), nil
}

// idsOf collects the ids of a query before any write, because the single
// transaction connection cannot run a write while a result set is open.
func idsOf(ctx context.Context, tx txn, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, dbError(ctx, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError(ctx, err)
	}
	return ids, nil
}

func closeAttempt(ctx context.Context, tx txn, id, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE prefix_attempts SET closed=? WHERE attempt_id=?`, reason, id); err != nil {
		return dbError(ctx, err)
	}
	return resolve(ctx, tx, id)
}

// Optional telemetry errors must not expose DB filenames, SQL arguments or raw
// fact content through an authorized state response.
func dbError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrUnavailable
}
