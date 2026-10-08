package policyevidence

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

// Store borrows db and never closes or changes its connection/durability settings.
// It is safe to share a one-connection SQLite pool with the policy/usage stores.
type Store struct {
	db    *sql.DB
	clock Clock
}

const schema = `
CREATE TABLE policy_evidence_meta (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL);
INSERT INTO policy_evidence_meta VALUES(1,1);
CREATE TABLE policy_evidence_events (
 event_key TEXT PRIMARY KEY, attempt_key TEXT NOT NULL UNIQUE, fingerprint TEXT NOT NULL,
 caller_digest TEXT NOT NULL, installation_id TEXT NOT NULL, session_id TEXT NOT NULL,
 provider_route TEXT NOT NULL, context_epoch TEXT NOT NULL,
 ordered INTEGER NOT NULL CHECK(ordered IN (0,1)), reason TEXT NOT NULL);
CREATE TABLE policy_evidence_heads (
 caller_digest TEXT NOT NULL, installation_id TEXT NOT NULL, session_id TEXT NOT NULL,
 provider_route TEXT NOT NULL, context_epoch TEXT NOT NULL,
 started_ns INTEGER NOT NULL CHECK(started_ns>0), policy_revision INTEGER NOT NULL,
 selected_ttl TEXT NOT NULL CHECK(selected_ttl IN ('','5m','1h')),
 effective_ttl TEXT NOT NULL CHECK(effective_ttl IN ('','5m','1h')),
 known INTEGER NOT NULL CHECK(known IN (0,1)), reason TEXT NOT NULL,
 PRIMARY KEY(caller_digest,installation_id,session_id,provider_route,context_epoch));
`

// Open creates only bounded empty schema on first use, or validates version 1 on
// reopen. It does not infer any telemetry from existing policy or usage tables.
func Open(db *sql.DB, clock Clock) (*Store, error) {
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
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='policy_evidence_meta'`).Scan(&count); err != nil {
		return nil, ErrUnavailable
	}
	if count == 0 {
		if _, err = conn.ExecContext(ctx, schema); err != nil {
			return nil, ErrUnavailable
		}
	}
	var version int
	if err = conn.QueryRowContext(ctx, `SELECT version FROM policy_evidence_meta WHERE id=1`).Scan(&version); err != nil || version != 1 {
		return nil, ErrUnavailable
	}
	for _, table := range []string{"policy_evidence_events", "policy_evidence_heads"} {
		if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			return nil, ErrUnavailable
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, ErrUnavailable
	}
	return &Store{db: db, clock: clock}, nil
}

// Observe is ONLY for a FromNative record AFTER successful authoritative
// measurement.PutRecord. It is not an alternate ingestion/authentication path.
// Facts with a usable scope but unverified clocks/source/dispatch are remembered
// as ignored, so duplicate replay after a clock advance cannot promote them.
func (s *Store) Observe(ctx context.Context, r measurement.Record) (ObserveResult, error) {
	e, err := project(r, s.clock().UTC())
	if err != nil {
		return ObserveResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObserveResult{}, dbError(ctx, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE policy_evidence_meta SET version=version WHERE id=1 AND version=1`)
	if err != nil {
		return ObserveResult{}, dbError(ctx, err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ObserveResult{}, ErrUnavailable
	}
	var fingerprint, reason string
	var ordered bool
	err = tx.QueryRowContext(ctx, `SELECT fingerprint,ordered,reason FROM policy_evidence_events WHERE event_key=?`, e.Key).Scan(&fingerprint, &ordered, &reason)
	if err == nil {
		if fingerprint != e.Fingerprint {
			return ObserveResult{}, ErrConflict
		}
		return ObserveResult{Disposition: "duplicate", Reason: reason}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ObserveResult{}, dbError(ctx, err)
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT event_key FROM policy_evidence_events WHERE attempt_key=?`, e.AttemptKey).Scan(&existing)
	if err == nil {
		return ObserveResult{}, ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ObserveResult{}, dbError(ctx, err)
	}
	args := append([]any{e.Key, e.AttemptKey, e.Fingerprint}, e.scope.args()...)
	args = append(args, e.Ordered, e.Reason)
	if _, err = tx.ExecContext(ctx, `INSERT INTO policy_evidence_events(event_key,attempt_key,fingerprint,caller_digest,installation_id,session_id,provider_route,context_epoch,ordered,reason) VALUES(?,?,?,?,?,?,?,?,?,?)`, args...); err != nil {
		return ObserveResult{}, dbError(ctx, err)
	}
	result := ObserveResult{Disposition: "ignored", Reason: e.Reason}
	if e.Ordered {
		old, err := readHead(ctx, tx, e.scope)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ObserveResult{}, dbError(ctx, err)
		}
		next, changed := merge(old, e)
		result.Disposition = "recorded"
		result.HeadChanged = changed
		if changed {
			args = e.scope.args()
			args = append(args, next.Start, next.PolicyRevision, next.Selected, next.Effective, next.Known, next.Reason)
			_, err = tx.ExecContext(ctx, `INSERT INTO policy_evidence_heads(caller_digest,installation_id,session_id,provider_route,context_epoch,started_ns,policy_revision,selected_ttl,effective_ttl,known,reason) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(caller_digest,installation_id,session_id,provider_route,context_epoch) DO UPDATE SET started_ns=excluded.started_ns,policy_revision=excluded.policy_revision,selected_ttl=excluded.selected_ttl,effective_ttl=excluded.effective_ttl,known=excluded.known,reason=excluded.reason`, args...)
			if err != nil {
				return ObserveResult{}, dbError(ctx, err)
			}
			result.Reason = next.Reason
		} else if e.Start < old.Start {
			result.Reason = "older_dispatch"
		}
	}
	if err = tx.Commit(); err != nil {
		return ObserveResult{}, dbError(ctx, err)
	}
	return result, nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readHead(ctx context.Context, q rowQuerier, key scope) (head, error) {
	var h head
	err := q.QueryRowContext(ctx, `SELECT started_ns,policy_revision,selected_ttl,effective_ttl,known,reason FROM policy_evidence_heads WHERE caller_digest=? AND installation_id=? AND session_id=? AND provider_route=? AND context_epoch=?`, key.args()...).Scan(&h.Start, &h.PolicyRevision, &h.Selected, &h.Effective, &h.Known, &h.Reason)
	return h, err
}

// Optional telemetry errors must not expose DB filenames, SQL arguments or raw
// provider/source facts through an authorized state response.
func dbError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrUnavailable
}
