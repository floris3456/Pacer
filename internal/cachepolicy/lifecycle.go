package cachepolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// BeginRequest durably pins a logical request before dispatch. The same active
// request ID/payload returns its existing snapshot without incrementing counts.
// Replaying a lifecycle receipt is not permission to redispatch an attempt ID.
func (s *Store) BeginRequest(ctx context.Context, auth Credentials, request Request) (Snapshot, error) {
	key, capHash, err := credentials(auth, request.Scope)
	if err != nil {
		return Snapshot{}, err
	}
	if !identifier(request.RequestID) || !identifier(request.ContextEpoch) || request.Revision < 1 {
		return Snapshot{}, invalid("A request ID and complete policy reference are required.")
	}
	if request.Kind != Primary && request.Kind != Warming && request.Kind != Auxiliary {
		return Snapshot{}, invalid("Unknown host-classified request kind.")
	}
	var result Snapshot
	err = s.write(ctx, func(tx *sql.Tx) error {
		state, err := load(ctx, tx, key, capHash)
		if err != nil {
			return err
		}
		if state.EnrollmentRequired && request.Kind != Auxiliary {
			return conflict("enrollment_required", "Initial existing-history compaction must commit before primary or warming requests.")
		}
		var raw []byte
		var payload, outcome string
		err = tx.QueryRowContext(ctx, `SELECT payload_hash,snapshot,outcome FROM cache_policy_requests WHERE scope_key=? AND request_id=?`, key, request.RequestID).Scan(&payload, &raw, &outcome)
		if err == nil {
			if payload != digest(request) {
				return conflict("request_reused", "Request ID was already used with a different payload.")
			}
			if outcome != "" {
				return conflict("request_settled", "The logical request has already settled.")
			}
			if json.Unmarshal(raw, &result) != nil || !validSnapshot(result) || result.Scope != request.Scope || result.RequestID != request.RequestID || result.AttemptID != "" {
				return unavailable()
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return unavailable()
		}
		if request.Revision != state.Revision {
			return conflict("revision_conflict", "Request policy revision is stale.")
		}
		if request.ContextEpoch != state.Active.ContextEpoch || request.TTL != state.Active.TTL {
			return conflict("policy_conflict", "Request epoch and TTL must match the active policy.")
		}
		if request.Kind != Auxiliary {
			if err = s.support(request.ProviderRoute, request.TTL); err != nil {
				return err
			}
			if state.Pending != nil {
				return conflict("transition_pending", "New primary and warming requests pause until the transition settles.")
			}
			state.Active.Used = true
		}
		result = Snapshot{Protocol: Protocol, Scope: request.Scope, ContextEpoch: request.ContextEpoch, Revision: request.Revision, TTL: request.TTL, RequestID: request.RequestID, Kind: request.Kind, Managed: request.Kind != Auxiliary, StartedAt: s.now()}
		switch request.Kind {
		case Primary:
			state.ActivePrimary++
		case Warming:
			state.ActiveWarming++
		case Auxiliary:
			state.ActiveAuxiliary++
			result.TTL = ""
		}
		if !validSnapshot(result) {
			return unavailable()
		}
		raw, err = json.Marshal(result)
		if err != nil {
			return unavailable()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO cache_policy_requests(scope_key,request_id,payload_hash,snapshot,kind) VALUES(?,?,?,?,?)`, key, request.RequestID, digest(request), raw, request.Kind); err != nil {
			return unavailable()
		}
		return save(ctx, tx, key, state)
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

func validSnapshot(s Snapshot) bool {
	if s.Protocol != Protocol || !validScope(s.Scope) || !identifier(s.ContextEpoch) || !identifier(s.RequestID) || s.Revision < 1 || s.StartedAt.IsZero() {
		return false
	}
	if s.AttemptID != "" && !identifier(s.AttemptID) {
		return false
	}
	if s.Kind == Auxiliary {
		return !s.Managed && s.TTL == ""
	}
	return (s.Kind == Primary || s.Kind == Warming) && s.Managed && (s.TTL == TTL5m || s.TTL == TTL1h)
}

func requestSnapshot(ctx context.Context, q queryer, key, requestID string) (Snapshot, string, error) {
	var snap Snapshot
	var raw []byte
	var outcome string
	err := q.QueryRowContext(ctx, `SELECT snapshot,outcome FROM cache_policy_requests WHERE scope_key=? AND request_id=?`, key, requestID).Scan(&raw, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, "", conflict("request_missing", "The lifecycle request does not exist.")
	}
	if err != nil || json.Unmarshal(raw, &snap) != nil || !validSnapshot(snap) || snap.RequestID != requestID || snap.AttemptID != "" {
		return Snapshot{}, "", unavailable()
	}
	return snap, outcome, nil
}

func attemptSnapshot(ctx context.Context, q queryer, key string, attempt Attempt) (Snapshot, string, error) {
	var snap Snapshot
	var raw []byte
	var outcome string
	err := q.QueryRowContext(ctx, `SELECT snapshot,outcome FROM cache_policy_attempts WHERE scope_key=? AND attempt_id=?`, key, attempt.AttemptID).Scan(&raw, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, "", conflict("attempt_missing", "The physical attempt does not exist.")
	}
	if err != nil || json.Unmarshal(raw, &snap) != nil || !validSnapshot(snap) || snap.AttemptID != attempt.AttemptID || snap.Scope != attempt.Scope {
		return Snapshot{}, "", unavailable()
	}
	if snap.RequestID != attempt.RequestID {
		return Snapshot{}, "", conflict("attempt_reused", "Attempt ID belongs to a different request.")
	}
	return snap, outcome, nil
}

func validAttempt(a Attempt) bool { return identifier(a.RequestID) && identifier(a.AttemptID) }

// BeginAttempt must precede each physical upstream dispatch, including retries.
// Attempt IDs are unique within the scope. Pending transitions block new managed
// attempts, but already-created immutable attempts can settle normally.
func (s *Store) BeginAttempt(ctx context.Context, auth Credentials, attempt Attempt) (Snapshot, error) {
	key, capHash, err := credentials(auth, attempt.Scope)
	if err != nil {
		return Snapshot{}, err
	}
	if !validAttempt(attempt) {
		return Snapshot{}, invalid("Request ID and physical attempt ID are required.")
	}
	var result Snapshot
	err = s.write(ctx, func(tx *sql.Tx) error {
		state, err := load(ctx, tx, key, capHash)
		if err != nil {
			return err
		}
		snap, outcome, err := attemptSnapshot(ctx, tx, key, attempt)
		if err == nil {
			if outcome != "" {
				return conflict("attempt_settled", "The physical attempt has already settled.")
			}
			result = snap
			return nil
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != "pacer_cache_attempt_missing" {
			return err
		}
		snap, outcome, err = requestSnapshot(ctx, tx, key, attempt.RequestID)
		if err != nil {
			return err
		}
		if snap.Scope != attempt.Scope {
			return unavailable()
		}
		if outcome != "" {
			return conflict("request_settled", "The logical request has already settled.")
		}
		if snap.Managed {
			if state.EnrollmentRequired {
				return conflict("enrollment_required", "Initial existing-history compaction has not committed.")
			}
			if state.Pending != nil {
				return conflict("transition_pending", "New physical primary and warming attempts pause during a transition.")
			}
			if snap.ContextEpoch != state.Active.ContextEpoch || snap.Revision != state.Revision || snap.TTL != state.Active.TTL {
				return conflict("policy_conflict", "The logical request policy is no longer current.")
			}
			if err = s.support(snap.ProviderRoute, snap.TTL); err != nil {
				return err
			}
			state.ActiveAttempts++
		}
		snap.AttemptID = attempt.AttemptID
		snap.StartedAt = s.now()
		if !validSnapshot(snap) {
			return unavailable()
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return unavailable()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO cache_policy_attempts(scope_key,attempt_id,request_id,snapshot) VALUES(?,?,?,?)`, key, attempt.AttemptID, attempt.RequestID, raw); err != nil {
			return unavailable()
		}
		result = snap
		return save(ctx, tx, key, state)
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

// GetAttempt returns the historical snapshot even after settlement or a later
// epoch. Usage must correlate by physical attempt, never by current chat policy.
func (s *Store) GetAttempt(ctx context.Context, auth Credentials, attempt Attempt) (Snapshot, error) {
	key, capHash, err := credentials(auth, attempt.Scope)
	if err != nil {
		return Snapshot{}, err
	}
	if !validAttempt(attempt) {
		return Snapshot{}, invalid("Request ID and physical attempt ID are required.")
	}
	// A read transaction makes scope authorization and receipt lookup consistent.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, unavailable()
	}
	defer tx.Rollback()
	if _, err = load(ctx, tx, key, capHash); err != nil {
		return Snapshot{}, err
	}
	snap, _, err := attemptSnapshot(ctx, tx, key, attempt)
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// ValidateAttempt compares the complete proposed snapshot with its durable
// immutable receipt and verifies it is active. It does not rewrite old attempts
// when a prepare occurs. The executor must separately validate final wire policy.
func (s *Store) ValidateAttempt(ctx context.Context, auth Credentials, proposed Snapshot) error {
	key, capHash, err := credentials(auth, proposed.Scope)
	if err != nil {
		return err
	}
	if !validSnapshot(proposed) || proposed.AttemptID == "" {
		return invalid("A complete physical-attempt snapshot is required.")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return unavailable()
	}
	defer tx.Rollback()
	if _, err = load(ctx, tx, key, capHash); err != nil {
		return err
	}
	snap, outcome, err := attemptSnapshot(ctx, tx, key, Attempt{Scope: proposed.Scope, RequestID: proposed.RequestID, AttemptID: proposed.AttemptID})
	if err != nil {
		return err
	}
	// Compare canonical wire values rather than time.Time's process-local location.
	if digest(snap) != digest(proposed) {
		return conflict("snapshot_conflict", "The proposed snapshot differs from its immutable durable receipt.")
	}
	if outcome != "" {
		return conflict("attempt_settled", "The physical attempt has already settled.")
	}
	if snap.Managed {
		return s.support(snap.ProviderRoute, snap.TTL)
	}
	return nil
}

func validOutcome(o Outcome) bool { return o == Succeeded || o == Failed || o == Canceled }

// FinishAttempt is idempotent for the same terminal outcome. Only the host may
// call settlement, after dispatch really stopped (including on restart recovery).
func (s *Store) FinishAttempt(ctx context.Context, auth Credentials, attempt Attempt, outcome Outcome) error {
	key, capHash, err := credentials(auth, attempt.Scope)
	if err != nil {
		return err
	}
	if !validAttempt(attempt) || !validOutcome(outcome) {
		return invalid("A physical attempt and terminal outcome are required.")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		state, err := load(ctx, tx, key, capHash)
		if err != nil {
			return err
		}
		snap, previous, err := attemptSnapshot(ctx, tx, key, attempt)
		if err != nil {
			return err
		}
		if previous != "" {
			if previous == string(outcome) {
				return nil
			}
			return conflict("outcome_conflict", "A different terminal outcome is already recorded.")
		}
		if snap.Managed {
			state.ActiveAttempts--
		}
		if _, err = tx.ExecContext(ctx, `UPDATE cache_policy_attempts SET outcome=? WHERE scope_key=? AND attempt_id=?`, outcome, key, attempt.AttemptID); err != nil {
			return unavailable()
		}
		return save(ctx, tx, key, state)
	})
}

// FinishRequest requires every physical attempt to be settled first. It never
// releases an in-flight guard on a timer, client cancellation claim, or restart.
func (s *Store) FinishRequest(ctx context.Context, auth Credentials, scope Scope, requestID string, outcome Outcome) error {
	key, capHash, err := credentials(auth, scope)
	if err != nil {
		return err
	}
	if !identifier(requestID) || !validOutcome(outcome) {
		return invalid("A lifecycle request ID and terminal outcome are required.")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		state, err := load(ctx, tx, key, capHash)
		if err != nil {
			return err
		}
		snap, previous, err := requestSnapshot(ctx, tx, key, requestID)
		if err != nil {
			return err
		}
		if snap.Scope != scope {
			return unavailable()
		}
		if previous != "" {
			if previous == string(outcome) {
				return nil
			}
			return conflict("outcome_conflict", "A different terminal outcome is already recorded.")
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM cache_policy_attempts WHERE scope_key=? AND request_id=? AND outcome=''`, key, requestID).Scan(&count); err != nil {
			return unavailable()
		}
		if count != 0 {
			return conflict("inflight", "Physical attempts must settle before their logical request.")
		}
		switch snap.Kind {
		case Primary:
			state.ActivePrimary--
		case Warming:
			state.ActiveWarming--
		case Auxiliary:
			state.ActiveAuxiliary--
		}
		if _, err = tx.ExecContext(ctx, `UPDATE cache_policy_requests SET outcome=? WHERE scope_key=? AND request_id=?`, outcome, key, requestID); err != nil {
			return unavailable()
		}
		return save(ctx, tx, key, state)
	})
}

// Outstanding is a read-only recovery inventory of active logical requests and
// physical attempts in this authorized scope. The host must verify each stopped
// before using FinishAttempt/FinishRequest; no automatic crash expiry exists.
func (s *Store) Outstanding(ctx context.Context, auth Credentials, scope Scope) ([]Snapshot, error) {
	key, capHash, err := credentials(auth, scope)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, unavailable()
	}
	defer tx.Rollback()
	if _, err = load(ctx, tx, key, capHash); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT snapshot FROM cache_policy_requests WHERE scope_key=? AND outcome='' UNION ALL SELECT snapshot FROM cache_policy_attempts WHERE scope_key=? AND outcome=''`, key, key)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	result := make([]Snapshot, 0)
	for rows.Next() {
		var raw []byte
		var snap Snapshot
		if rows.Scan(&raw) != nil || json.Unmarshal(raw, &snap) != nil || !validSnapshot(snap) || snap.Scope != scope {
			return nil, unavailable()
		}
		result = append(result, snap)
	}
	if rows.Err() != nil {
		return nil, unavailable()
	}
	return result, nil
}
