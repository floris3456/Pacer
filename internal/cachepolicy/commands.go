package cachepolicy

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
)

func validateCommand(c Command) error {
	if c.Protocol != Protocol {
		return failure(422, "unsupported", "Unsupported cache policy protocol.")
	}
	if !identifier(c.OperationID) || !identifier(c.ContextEpoch) || c.ExpectedRevision < 0 {
		return invalid("Operation ID, epoch and nonnegative expected revision are required.")
	}
	if c.TTL != TTL5m && c.TTL != TTL1h {
		return failure(422, "unsupported", "TTL must be 5m or 1h.")
	}
	if c.Enrollment != "" && c.Enrollment != "new_empty" {
		return failure(422, "enrollment_unsupported", "Existing-history enrollment and copied-context forks are not supported.")
	}
	switch c.Operation {
	case "ensure", "prepare":
		if c.TransitionID != "" || c.Checkpoint != "" || !c.CompactionStartedAt.IsZero() || !c.CheckpointCommittedAt.IsZero() {
			return invalid("Checkpoint proof is only accepted by commit.")
		}
	case "commit":
		if !identifier(c.TransitionID) || !identifier(c.Checkpoint) || c.CompactionStartedAt.IsZero() || c.CheckpointCommittedAt.IsZero() {
			return invalid("Commit requires a transition and a durably committed checkpoint acknowledgement.")
		}
	case "cancel":
		if !identifier(c.TransitionID) || c.Checkpoint != "" || !c.CompactionStartedAt.IsZero() || !c.CheckpointCommittedAt.IsZero() {
			return invalid("Cancel requires only a transition ID.")
		}
	default:
		return invalid("Operation must be ensure, prepare, commit or cancel.")
	}
	if c.Operation != "ensure" && c.Enrollment != "" {
		return invalid("Enrollment is only accepted by ensure.")
	}
	return nil
}

// Apply executes one CAS mutation. Successful and domain-rejected authenticated
// operations are recorded. A retry of the same operation ID returns its original
// receipt; a changed payload returns 409. A new attempt after a rejected operation
// needs a new ID (including an in-flight rejection that has since cleared).
func (s *Store) Apply(ctx context.Context, auth Credentials, cmd Command, evidence Evidence) (State, error) {
	key, capHash, err := credentials(auth, cmd.Scope)
	if err != nil {
		return State{}, err
	}
	if err = validateCommand(cmd); err != nil {
		return State{}, err
	}
	var result State
	var domainErr error
	err = s.write(ctx, func(tx *sql.Tx) error {
		var payload, oldKey, oldCap string
		var raw []byte
		var status sql.NullInt64
		var code, message sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT payload_hash,scope_key,capability_hash,result,error_status,error_code,error_message FROM cache_policy_operations WHERE caller_digest=? AND operation_id=?`, auth.CallerDigest, cmd.OperationID).Scan(&payload, &oldKey, &oldCap, &raw, &status, &code, &message)
		if err == nil {
			if subtle.ConstantTimeCompare([]byte(oldCap), []byte(capHash)) != 1 {
				return denied()
			}
			if oldKey != key || payload != digest(cmd) {
				return conflict("operation_reused", "Operation ID was already used with a different payload.")
			}
			if status.Valid {
				domainErr = &Error{Status: int(status.Int64), Code: code.String, Message: message.String}
				return nil
			}
			// A retained receipt is not a replacement for missing correctness state.
			if _, err = load(ctx, tx, key, capHash); err != nil {
				return err
			}
			if json.Unmarshal(raw, &result) != nil || !validState(result) {
				return unavailable()
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return unavailable()
		}
		state, err := load(ctx, tx, key, capHash)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || e.Code != "pacer_cache_state_missing" {
				return err
			}
			if cmd.Operation != "ensure" {
				domainErr = err
			} else {
				result, domainErr = s.enroll(ctx, tx, auth, cmd, evidence, key, capHash)
			}
		} else {
			result, domainErr = s.change(ctx, tx, key, state, cmd)
		}
		if domainErr != nil {
			var e *Error
			if !errors.As(domainErr, &e) || e.Status >= 500 {
				return domainErr
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO cache_policy_operations(caller_digest,operation_id,payload_hash,scope_key,capability_hash,error_status,error_code,error_message) VALUES(?,?,?,?,?,?,?,?)`, auth.CallerDigest, cmd.OperationID, digest(cmd), key, capHash, e.Status, e.Code, e.Message)
		} else {
			raw, err = json.Marshal(result)
			if err != nil {
				return unavailable()
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO cache_policy_operations(caller_digest,operation_id,payload_hash,scope_key,capability_hash,result) VALUES(?,?,?,?,?,?)`, auth.CallerDigest, cmd.OperationID, digest(cmd), key, capHash, raw)
		}
		if err != nil {
			return unavailable()
		}
		return nil
	})
	if err != nil {
		return State{}, err
	}
	if domainErr != nil {
		return State{}, domainErr
	}
	return result, nil
}

func (s *Store) enroll(ctx context.Context, tx *sql.Tx, auth Credentials, cmd Command, evidence Evidence, key, capHash string) (State, error) {
	if cmd.ExpectedRevision != 0 {
		return State{}, conflict("state_missing", "Previously enrolled state is missing; restore it before continuing.")
	}
	var prior int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM cache_policy_operations WHERE scope_key=? AND result IS NOT NULL`, key).Scan(&prior); err != nil {
		return State{}, unavailable()
	}
	if prior != 0 {
		return State{}, conflict("state_missing", "Previously enrolled state is missing; restore it before continuing.")
	}
	if cmd.Enrollment != "new_empty" || !evidence.NewEmpty {
		return State{}, failure(422, "enrollment_unsupported", "Only independently verified, genuinely empty sessions can enroll.")
	}
	if err := s.support(cmd.ProviderRoute, cmd.TTL); err != nil {
		return State{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM cache_policy_scopes WHERE caller_digest=? AND installation_id=? AND session_id=?`, auth.CallerDigest, cmd.InstallationID, cmd.SessionID).Scan(&count); err != nil {
		return State{}, unavailable()
	}
	if count != 0 {
		return State{}, failure(422, "enrollment_unsupported", "A route change cannot enroll existing context as empty.")
	}
	state := State{Protocol: Protocol, Scope: cmd.Scope, Revision: 1, Active: Policy{ContextEpoch: cmd.ContextEpoch, Checkpoint: cmd.ContextEpoch, TTL: cmd.TTL}}
	raw, err := json.Marshal(state)
	if err != nil {
		return State{}, unavailable()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO cache_policy_scopes(scope_key,caller_digest,installation_id,session_id,provider_route,capability_hash,state) VALUES(?,?,?,?,?,?,?)`, key, auth.CallerDigest, cmd.InstallationID, cmd.SessionID, cmd.ProviderRoute, capHash, raw)
	if err != nil {
		return State{}, unavailable()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO cache_policy_epochs(scope_key,context_epoch) VALUES(?,?)`, key, cmd.ContextEpoch); err != nil {
		return State{}, unavailable()
	}
	return state, nil
}

func (s *Store) change(ctx context.Context, tx *sql.Tx, key string, state State, cmd Command) (State, error) {
	if cmd.ExpectedRevision != state.Revision {
		return State{}, conflict("revision_conflict", "Expected revision does not match durable state.")
	}
	if cmd.Operation != "cancel" {
		if err := s.support(cmd.ProviderRoute, cmd.TTL); err != nil {
			return State{}, err
		}
	}
	if cmd.Operation != "commit" && cmd.ContextEpoch != state.Active.ContextEpoch {
		return State{}, conflict("epoch_conflict", "Context epoch does not match active policy.")
	}
	switch cmd.Operation {
	case "ensure":
		if state.Pending != nil {
			return State{}, conflict("transition_pending", "Reconcile or cancel the pending transition first.")
		}
		if cmd.TTL == state.Active.TTL {
			return state, nil
		}
		if state.Active.Used {
			return State{}, conflict("compaction_required", "Changing TTL for used context requires prepare, new compaction and commit.")
		}
		if state.ActivePrimary != 0 || state.ActiveWarming != 0 {
			return State{}, conflict("inflight", "Primary or warming requests are still active.")
		}
		state.Active.TTL = cmd.TTL
		state.Revision++
	case "prepare":
		if state.Pending != nil {
			return State{}, conflict("transition_pending", "A transition is already pending.")
		}
		id, err := randomID()
		if err != nil {
			return State{}, err
		}
		state.Revision++
		state.Pending = &Transition{ID: id, TTL: cmd.TTL, FromEpoch: state.Active.ContextEpoch, FromCheckpoint: state.Active.Checkpoint, PreparedAt: s.now(), PreparedRevision: state.Revision}
		state.WarmingPaused = true
	case "commit":
		p := state.Pending
		if p == nil || p.ID != cmd.TransitionID || p.TTL != cmd.TTL {
			return State{}, conflict("transition_conflict", "Commit does not match the pending transition.")
		}
		if cmd.ContextEpoch != cmd.Checkpoint || cmd.ContextEpoch == state.Active.ContextEpoch || cmd.Checkpoint == state.Active.Checkpoint {
			return State{}, conflict("checkpoint_required", "Commit requires a changed checkpoint as the new context epoch.")
		}
		if !cmd.CompactionStartedAt.After(p.PreparedAt) || cmd.CheckpointCommittedAt.Before(cmd.CompactionStartedAt) || cmd.CheckpointCommittedAt.After(s.now()) {
			return State{}, conflict("checkpoint_required", "Compaction must start after prepare and durably commit before acknowledgement.")
		}
		if state.ActivePrimary != 0 || state.ActiveWarming != 0 || state.ActiveAttempts != 0 {
			return State{}, conflict("inflight", "Old primary requests, warming or physical attempts are not settled.")
		}
		var seen int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM cache_policy_epochs WHERE scope_key=? AND context_epoch=?`, key, cmd.ContextEpoch).Scan(&seen); err != nil {
			return State{}, unavailable()
		}
		if seen != 0 {
			return State{}, conflict("checkpoint_required", "A previously used checkpoint cannot start a new epoch.")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cache_policy_epochs(scope_key,context_epoch) VALUES(?,?)`, key, cmd.ContextEpoch); err != nil {
			return State{}, unavailable()
		}
		state.Revision++
		// Compaction preserves existing history: even before its first dispatch the
		// new epoch cannot be reclassified as an empty chat for a direct TTL write.
		state.Active = Policy{ContextEpoch: cmd.ContextEpoch, Checkpoint: cmd.Checkpoint, TTL: cmd.TTL, Used: true}
		state.Pending = nil
		state.WarmingPaused = false
	case "cancel":
		if state.Pending == nil || state.Pending.ID != cmd.TransitionID || state.Pending.TTL != cmd.TTL {
			return State{}, conflict("transition_conflict", "Cancel does not match the pending transition.")
		}
		state.Revision++
		state.Pending = nil
		state.WarmingPaused = false
	}
	if err := save(ctx, tx, key, state); err != nil {
		return State{}, err
	}
	return state, nil
}
