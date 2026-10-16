package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func (p *PhysicalProof) Apply(ctx context.Context, in PhysicalCheckpoint) error {
	var refusal error
	now := p.store.clock()
	prune := now.Sub(p.pruned) >= physicalPruneEvery
	err := p.write(ctx, func(tx *sql.Tx) error {
		if prune {
			if err := prunePhysical(ctx, tx, p.consumer); err != nil {
				return err
			}
		}
		i := in.Identity
		m, _, err := sourceMembership(ctx, tx, i.MembershipID)
		if err != nil {
			return err
		}
		if i.ConsumerID != p.consumer || i.SourceID != p.source || m.SourceID != p.source || !membershipHas(m, i.SourceRef) {
			return ErrInvalid
		}
		if nativeID(i.ManagerID) && validSourceRef(i.SourceRef) && i.SinkID == "" {
			return physicalGap(ctx, tx, i.SourceRef, i.ManagerID, "", "missing_sink_identity")
		}
		if !nativeID(i.ManagerID) || !nativeID(i.SinkID) || !validSourceRef(i.SourceRef) {
			_, err = tx.ExecContext(ctx, `UPDATE measurement_physical_consumers SET failed=1 WHERE id=?`, p.consumer)
			return err
		}
		gap := func(reason string) error { return physicalGap(ctx, tx, i.SourceRef, i.ManagerID, i.SinkID, reason) }
		// All negatives precede incarnation and sequence checks, including stale
		// callbacks for an old manager/sink after this consumer has moved on.
		bad := in.Invalid || !physicalSequence(i.Sequence) || !nativeSHA(in.Digest)
		if in.HasGap || bad || in.Missing {
			if err = gap("checkpoint_gap_or_unavailable"); err != nil {
				return err
			}
		}
		if bad {
			refusal = ErrInvalid
			return nil
		}
		if in.Missing {
			return nil
		}
		source, start, usable, err := sourceConsumer(ctx, tx, p.consumer)
		if err != nil {
			return err
		}
		if source != p.source || !usable {
			refusal = ErrStaleSource
			return nil
		}
		var id, owner string
		var active bool
		var last, at int64
		var lastDigest string
		var lower []byte
		err = tx.QueryRowContext(ctx, `SELECT id,consumer_id,active,last_seq,digest,lower_body,last_at FROM measurement_physical_epochs WHERE source_ref=? AND manager_id=? AND sink_id=?`, i.SourceRef, i.ManagerID, i.SinkID).Scan(&id, &owner, &active, &last, &lastDigest, &lower, &at)
		if err == nil {
			if owner != p.consumer || !active || i.Sequence < uint64(last) {
				refusal = ErrStaleSource
				return nil
			}
			if id != i.epoch() {
				// Same consumer and sink, another membership: the tuple's account
				// membership changed inside this sink incarnation (for example CPA
				// verified the account after a restart). No proof may span that
				// change. The epoch is re-keyed and its lower edge dropped, so
				// earlier proofs become unreadable (missing_epoch) and a later
				// quiescent cut observed here becomes the new lower edge.
				if i.Sequence == uint64(last) {
					refusal = ErrStaleSource
					return nil
				}
				if _, err = tx.ExecContext(ctx, `UPDATE measurement_physical_epochs SET id=?,lower_body=NULL,last_at=0 WHERE id=?`, i.epoch(), id); err != nil {
					return err
				}
				id, lower, at = i.epoch(), nil, 0
			} else if i.Sequence == uint64(last) {
				if in.Digest != lastDigest {
					refusal = ErrConflict
					return gap("equal_sequence_conflict")
				}
				return nil
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			id = i.epoch()
			if err = physicalReserve(ctx, tx, "epochs"); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE measurement_physical_epochs SET active=0 WHERE consumer_id=? AND source_ref=? AND active=1`, p.consumer, i.SourceRef); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO measurement_physical_epochs VALUES(?,?,?,?,?,1,0,'',NULL,0)`, id, p.consumer, i.SourceRef, i.ManagerID, i.SinkID); err != nil {
				return err
			}
		} else {
			return err
		}
		finish := func() error {
			_, err := tx.ExecContext(ctx, `UPDATE measurement_physical_epochs SET last_seq=?,digest=?,lower_body=?,last_at=? WHERE id=?`, int64(i.Sequence), in.Digest, lower, at, id)
			return err
		}
		if in.HasGap || in.Missing {
			return finish()
		}
		var stored *PhysicalCutIdentity
		if len(lower) > 0 {
			stored = new(PhysicalCutIdentity)
			if json.Unmarshal(lower, stored) != nil {
				return ErrInvalid
			}
		}
		if in.Coverage == "known" && in.From != nil && in.From.valid() && in.From.epoch() == id && !in.From.At.After(p.store.clock()) {
			switch {
			case stored == nil:
				// The host keeps one lower cut per sink and never repeats it. A
				// lower edge this consumer did not observe is never borrowed:
				// this checkpoint's own quiescent through cut becomes the lower
				// edge instead, and only later checkpoints can prove from it.
				// This covers a consumer that missed the sink's first cut and an
				// epoch re-keyed by a membership change.
				t := in.Through
				if t != nil && t.valid() && t.epoch() == id && t.Sequence == i.Sequence && in.From.Sequence < t.Sequence && !t.At.Before(start) && !t.At.After(p.store.clock()) {
					lower, _ = json.Marshal(t)
					at = t.At.UnixNano()
					if err = putPhysicalCut(ctx, tx, *t); err != nil {
						return err
					}
				}
				return finish()
			case in.From.Sequence < stored.Sequence && !in.From.At.After(stored.At):
				// The host's cut precedes the adopted lower edge, so the host's
				// known coverage includes [adopted edge, through]. Proofs start at
				// the adopted edge.
				adopted := *stored
				in.From = &adopted
			}
		}
		if in.From != nil && (in.From.epoch() != id || !in.From.valid() || in.From.At.Before(start) || in.From.At.After(p.store.clock())) {
			if err = gap("invalid_lower_cut"); err != nil {
				return err
			}
			return finish()
		}
		if in.Coverage == "unknown" {
			if in.Reason == "first_quiescent_cut" {
				if in.From == nil || in.From.Sequence != i.Sequence || len(lower) > 0 {
					if err = gap("conflicting_initial_cut"); err != nil {
						return err
					}
					return finish()
				}
				lower, _ = json.Marshal(in.From)
				at = in.From.At.UnixNano()
				if err = putPhysicalCut(ctx, tx, *in.From); err != nil {
					return err
				}
			}
			return finish()
		}
		if in.Coverage != "known" {
			return finish()
		}
		var from PhysicalCutIdentity
		if len(lower) == 0 {
			return finish()
		} // known alone cannot establish listener ownership of the lower cut
		if json.Unmarshal(lower, &from) != nil {
			return ErrInvalid
		}
		if in.From == nil || in.Through == nil || key(*in.From) != key(from) || !in.Through.valid() || in.Through.epoch() != id || in.Through.Sequence != i.Sequence || in.Through.Sequence <= from.Sequence || !in.Through.At.After(from.At) || in.Through.At.UnixNano() < at || in.Through.At.After(p.store.clock()) {
			if err = gap("inconsistent_causal_cut"); err != nil {
				return err
			}
			return finish()
		}
		selection := PhysicalSelection{From: from, Through: *in.Through}
		facts, reason, err := physicalJoin(ctx, tx, selection, m)
		if err != nil {
			return err
		}
		if reason != "" {
			if err = gap(reason); err != nil {
				return err
			}
			return finish()
		}
		if err = putPhysicalCut(ctx, tx, *in.Through); err != nil {
			return err
		}
		if err = physicalReserve(ctx, tx, "proofs"); err != nil {
			return err
		}
		proof := physicalManifest{Version: PhysicalVersion, Domain: PhysicalDomain, Selection: selection, ProofID: "physical-proof:" + key(selection), Count: len(facts), Digest: key(facts)}
		body, _ := json.Marshal(proof)
		if _, err = tx.ExecContext(ctx, `INSERT INTO measurement_physical_proofs VALUES(?,?,?,?)`, proof.ProofID, id, int64(i.Sequence), body); err != nil {
			return err
		}
		at = in.Through.At.UnixNano()
		return finish()
	})
	if err != nil {
		return err
	}
	if prune {
		p.pruned = now
	}
	return refusal
}

func putPhysicalCut(ctx context.Context, tx *sql.Tx, c PhysicalCutIdentity) error {
	var old []byte
	err := tx.QueryRowContext(ctx, `SELECT body FROM measurement_physical_cuts WHERE id=?`, c.ID()).Scan(&old)
	b, _ := json.Marshal(c)
	if err == nil {
		if string(old) != string(b) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = physicalReserve(ctx, tx, "cuts"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO measurement_physical_cuts VALUES(?,?,?,?)`, c.ID(), c.epoch(), int64(c.Sequence), b)
	return err
}

// Join is bounded by the retained stream and selection limits. A metadata count
// guards against missing physical rows; exact revision digests guard against
// missing or replaced primary rows. A host proof has no expected record count.
func physicalJoin(ctx context.Context, q physicalQuerier, s PhysicalSelection, m SourceMembership) ([]PhysicalFact, string, error) {
	facts := []PhysicalFact{}
	i := s.From
	var failed int
	if err := q.QueryRowContext(ctx, `SELECT failed FROM measurement_physical_consumers WHERE id=? AND source_id=?`, i.ConsumerID, i.SourceID).Scan(&failed); err != nil {
		return nil, "", err
	}
	if failed != 0 {
		return facts, "consumer_persistence_fault", nil
	}
	var count int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM measurement_physical_faults WHERE consumer_id=? AND (account_id='' OR account_id=?)`, i.ConsumerID, m.AccountID).Scan(&count); err != nil {
		return nil, "", err
	}
	if count > 0 {
		return facts, "consumer_observation_fault", nil
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM measurement_physical_gaps WHERE source_ref=? AND manager_id=? AND (sink_id='' OR sink_id=?)`, i.SourceRef, i.ManagerID, i.SinkID).Scan(&count); err != nil {
		return nil, "", err
	}
	if count > 0 {
		return facts, "physical_incarnation_gap", nil
	}
	stream := physicalStream(i.ConsumerID, i.SourceRef, i.ManagerID)
	var expected int
	err := q.QueryRowContext(ctx, `SELECT records FROM measurement_physical_streams WHERE id=?`, stream).Scan(&expected)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM measurement_physical_records WHERE stream_id=?`, stream).Scan(&count); err != nil {
		return nil, "", err
	}
	if count != expected {
		return facts, "physical_record_missing", nil
	}
	rows, err := q.QueryContext(ctx, `SELECT p.body,r.body,c.body FROM measurement_physical_records p LEFT JOIN measurement_record_revisions r ON r.event_id=p.event_id AND r.revision=1 LEFT JOIN measurement_records c ON c.event_id=p.event_id WHERE p.stream_id=? AND p.dispatch_seq>? AND p.dispatch_seq<? ORDER BY p.dispatch_seq LIMIT ?`, stream, int64(i.Sequence), int64(s.Through.Sequence), MaxPhysicalSelection+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var pb, rb, cb []byte
		if err = rows.Scan(&pb, &rb, &cb); err != nil {
			return nil, "", err
		}
		var f PhysicalFact
		var r Record
		if json.Unmarshal(pb, &f) != nil || json.Unmarshal(rb, &r) != nil || len(cb) == 0 || digest(cb) != digest(rb) {
			return facts, "primary_record_missing", nil
		}
		if f.Version != PhysicalVersion || f.StartTimeBasis != "source_physical_dispatch_observation" {
			return facts, "unknown_physical_record_version", nil
		}
		member := false
		for _, v := range m.Members {
			member = member || v.SourceRef == f.SourceRef && v.CredentialRef == f.CredentialRef
		}
		if !member || f.SourceID != i.SourceID || f.ConsumerID != i.ConsumerID || f.SourceRef != i.SourceRef || f.ManagerID != i.ManagerID || f.MembershipID != i.MembershipID || f.AccountID != m.AccountID || f.AdmissionSequence <= i.Sequence || f.DispatchSequence <= f.AdmissionSequence || f.DispatchSequence >= s.Through.Sequence || f.RecordDigest != digest(rb) || f.Revision != r.Revision || f.EventID != r.EventID || f.StartedAt.Before(i.At) || f.StartedAt.After(s.Through.At) {
			return facts, "physical_join_mismatch", nil
		}
		if r.SourceID != f.SourceID || r.Attribution.CredentialRef != f.CredentialRef || r.Attribution.AccountID != f.AccountID || r.Native == nil || r.Native.SourceEvidence == nil || r.Native.SourceEvidence.SourceRef != f.SourceRef || r.Native.SourceEvidence.ManagerID != f.ManagerID || r.Native.CredentialBindingRef != f.CredentialBindingRef || r.Native.AccessTokenRef != f.AccessTokenRef || r.Native.SourceMembershipID != f.MembershipID || !r.Native.SourceEvidence.Tracked || r.Native.SourceEvidence.Gap || !r.Success || !r.Final {
			return facts, "physical_attribution_mismatch", nil
		}
		facts = append(facts, f)
		if len(facts) > MaxPhysicalSelection {
			// Too many dispatches since the lower edge: this epoch proves nothing
			// more, but the namespace stays usable.
			return []PhysicalFact{}, "physical_selection_limit", nil
		}
	}
	return facts, "", rows.Err()
}

// PhysicalLower returns the lower edge stored for the active epoch of cut c,
// or nil without one. Every proof that epoch can still write starts there,
// which may be later than the host's own lower cut.
func (s *Store) PhysicalLower(ctx context.Context, c PhysicalCutIdentity) (*PhysicalCutIdentity, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT lower_body FROM measurement_physical_epochs WHERE id=? AND consumer_id=? AND source_ref=? AND manager_id=? AND sink_id=? AND active=1`, c.epoch(), c.ConsumerID, c.SourceRef, c.ManagerID, c.SinkID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && len(raw) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lower PhysicalCutIdentity
	if json.Unmarshal(raw, &lower) != nil || lower.epoch() != c.epoch() {
		return nil, ErrInvalid
	}
	return &lower, nil
}

// SelectPhysical reads only an exact durable edge, including historical edges
// from retired consumers. All identity fields must be supplied explicitly. No
// current-inventory fallback, implicit current sink, time query or end inference.
func (s *Store) SelectPhysical(ctx context.Context, selection PhysicalSelection) (PhysicalResult, error) {
	out := PhysicalResult{Version: PhysicalVersion, Domain: PhysicalDomain, Selection: selection, Facts: []PhysicalFact{}, Reason: "missing_exact_proof"}
	if !selection.From.valid() || !selection.Through.valid() || selection.From.epoch() != selection.Through.epoch() || selection.From.Sequence >= selection.Through.Sequence {
		out.Reason = "invalid_cut_selection"
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = physicalMetadata(ctx, tx); err != nil {
		return out, err
	}
	var raw []byte
	id := "physical-proof:" + key(selection)
	err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_physical_proofs WHERE id=? AND epoch_id=?`, id, selection.From.epoch()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var stored physicalManifest
	if json.Unmarshal(raw, &stored) != nil || stored.ProofID != id || key(stored.Selection) != key(selection) || stored.Version != PhysicalVersion || stored.Domain != PhysicalDomain {
		out.Reason = "invalid_stored_proof"
		return out, nil
	}
	var owner, ref, manager, sink string
	var sequence int64
	if err = tx.QueryRowContext(ctx, `SELECT consumer_id,source_ref,manager_id,sink_id,last_seq FROM measurement_physical_epochs WHERE id=?`, selection.From.epoch()).Scan(&owner, &ref, &manager, &sink, &sequence); errors.Is(err, sql.ErrNoRows) {
		out.Reason = "missing_epoch"
		return out, nil
	} else if err != nil {
		return out, err
	}
	if owner != selection.From.ConsumerID || ref != selection.From.SourceRef || manager != selection.From.ManagerID || sink != selection.From.SinkID || sequence < 0 || uint64(sequence) < selection.Through.Sequence {
		out.Reason = "invalid_epoch_provenance"
		return out, nil
	}
	for _, cut := range []PhysicalCutIdentity{selection.From, selection.Through} {
		if err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_physical_cuts WHERE id=? AND epoch_id=?`, cut.ID(), cut.epoch()).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
			out.Reason = "missing_cut"
			return out, nil
		} else if err != nil {
			return out, err
		}
		var got PhysicalCutIdentity
		if json.Unmarshal(raw, &got) != nil || key(got) != key(cut) {
			out.Reason = "invalid_stored_cut"
			return out, nil
		}
	}
	m, _, err := sourceMembership(ctx, tx, selection.From.MembershipID)
	if err != nil {
		return out, err
	}
	facts, reason, err := physicalJoin(ctx, tx, selection, m)
	if err != nil {
		return out, err
	}
	if reason != "" {
		out.Reason = reason
		return out, nil
	}
	if len(facts) != stored.Count || key(facts) != stored.Digest {
		out.Reason = "changed_physical_manifest"
		return out, nil
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out.Known = true
	out.Reason = ""
	out.ProofID = id
	out.Facts = facts
	return out, nil
}
