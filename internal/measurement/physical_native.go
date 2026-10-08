package measurement

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

type physicalDispatch struct {
	Version   int       `json:"version"`
	Admission uint64    `json:"admission_sequence"`
	Dispatch  uint64    `json:"dispatch_sequence"`
	Started   time.Time `json:"started_at"`
}

// The optional extension is decoded separately from primary native accounting.
// Malformed/unsupported physical data is a durable negative, not a lost record.
// Unknown keys and duplicate keys are refused; no arbitrary input text is stored.
func decodePhysical(raw []byte) (physicalDispatch, bool) {
	var d physicalDispatch
	if len(raw) == 0 || len(raw) > 2048 {
		return d, false
	}
	object, ok := physicalObject(raw)
	if !ok {
		return d, false
	}
	for k := range object {
		switch k {
		case "version", "admission_sequence", "dispatch_sequence", "started_at":
		default:
			return d, false
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&d) != nil {
		return d, false
	}
	return d, d.Version == PhysicalVersion && physicalSequence(d.Admission) && physicalSequence(d.Dispatch) && d.Admission < d.Dispatch && validTime(d.Started)
}

func physicalObject(raw []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	seen := map[string]bool{}
	object := map[string]json.RawMessage{}
	for dec.More() {
		if len(seen) >= 128 {
			return nil, false
		}
		k, e := dec.Token()
		if e != nil {
			return nil, false
		}
		key, ok := k.(string)
		if !ok || seen[strings.ToLower(key)] {
			return nil, false
		}
		seen[strings.ToLower(key)] = true
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return nil, false
		}
		object[key] = v
	}
	if _, err = dec.Token(); err != nil {
		return nil, false
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, false
	}
	return object, true
}

func physicalStream(consumer, ref, manager string) string {
	return key([]string{consumer, ref, manager})
}

// ObserveRecord is called synchronously after primary PutRecord, before ack.
// The primary body is rejoined inside the same physical transaction. A crash
// between these commits cannot acknowledge delivery or manufacture a host cut.
func (p *PhysicalProof) ObserveRecord(ctx context.Context, raw []byte, r Record) error {
	if r.Native == nil || r.Native.SourceEvidence == nil || r.Native.SourceEvidence.SourceRef == "" {
		return p.Fault(ctx, r.Attribution.AccountID)
	}
	se := r.Native.SourceEvidence
	envelope, uniqueEnvelope := physicalObject(raw)
	evidence, uniqueEvidence := physicalObject(envelope["SourceEvidence"])
	d, valid := decodePhysical(evidence["physical"])
	valid = valid && uniqueEnvelope && uniqueEvidence
	valid = valid && se.Tracked && !se.Gap && r.Final && r.Success && r.Revision == 1 && r.SourceID == p.source && se.UpstreamStartedAt != nil
	if valid {
		valid = !d.Started.Before(*se.UpstreamStartedAt) && !d.Started.Before(se.RequestedAt) && !d.Started.After(se.FinalUsageAt)
	}
	if !valid {
		return p.Invalidate(ctx, se.SourceRef, se.ManagerID, "")
	}
	f := PhysicalFact{EventID: r.EventID, ConsumerID: p.consumer, SourceID: p.source, SourceRef: se.SourceRef, ManagerID: se.ManagerID, MembershipID: r.Native.SourceMembershipID, AccountID: r.Attribution.AccountID, CredentialRef: r.Attribution.CredentialRef, CredentialBindingRef: r.Native.CredentialBindingRef, AccessTokenRef: r.Native.AccessTokenRef, Revision: r.Revision, RecordDigest: physicalRecordDigest(r), AdmissionSequence: d.Admission, DispatchSequence: d.Dispatch, StartedAt: d.Started.Round(0).UTC()}
	f.Version = PhysicalVersion
	f.StartTimeBasis = "source_physical_dispatch_observation"
	return p.write(ctx, func(tx *sql.Tx) error {
		gap := func(reason string) error { return physicalGap(ctx, tx, f.SourceRef, f.ManagerID, "", reason) }
		m, _, err := sourceMembership(ctx, tx, f.MembershipID)
		if errors.Is(err, sql.ErrNoRows) || f.MembershipID == "" {
			return gap("missing_captured_membership")
		}
		if err != nil {
			return err
		}
		member := false
		for _, v := range m.Members {
			member = member || v.SourceRef == f.SourceRef && v.CredentialRef == f.CredentialRef
		}
		if !member || m.SourceID != p.source || m.Provider != r.Attribution.Provider || m.AccountID != f.AccountID {
			return gap("captured_membership_mismatch")
		}
		var body []byte
		if err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_record_revisions WHERE event_id=? AND revision=?`, r.EventID, r.Revision).Scan(&body); err != nil {
			return err
		}
		var stored Record
		if json.Unmarshal(body, &stored) != nil || physicalRecordDigest(stored) != f.RecordDigest {
			return gap("primary_record_mismatch")
		}
		f.RecordDigest = digest(body) // exact stored bytes survive additive Record type changes
		var prior []byte
		err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_physical_records WHERE event_id=?`, f.EventID).Scan(&prior)
		if err == nil {
			var old PhysicalFact
			if json.Unmarshal(prior, &old) != nil {
				return gap("invalid_physical_record")
			}
			// Re-delivery through a later consumer cannot reassign the original
			// callback. Exact replay is harmless; changed evidence revokes origin.
			original := f
			original.ConsumerID = old.ConsumerID
			if key(original) != key(old) {
				if err = physicalGap(ctx, tx, old.SourceRef, old.ManagerID, "", "conflicting_physical_record"); err != nil {
					return err
				}
				return gap("conflicting_physical_record")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		stream := physicalStream(p.consumer, f.SourceRef, f.ManagerID)
		var n int
		err = tx.QueryRowContext(ctx, `SELECT records FROM measurement_physical_streams WHERE id=?`, stream).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			if err = physicalReserve(ctx, tx, "streams"); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO measurement_physical_streams VALUES(?,?,?,?,0)`, stream, p.consumer, f.SourceRef, f.ManagerID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		var other string
		err = tx.QueryRowContext(ctx, `SELECT event_id FROM measurement_physical_records WHERE stream_id=? AND dispatch_seq=?`, stream, int64(f.DispatchSequence)).Scan(&other)
		if err == nil {
			return gap("dispatch_sequence_collision")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Any quiescent boundary requires acknowledged delivery, including the
		// initial lower cut. A later listener cannot hide an unseen old callback
		// in its own stream. Usage has no sink identity, so revoke the origin
		// manager across sinks, not the unrelated current manager.
		err = tx.QueryRowContext(ctx, `SELECT c.id FROM measurement_physical_cuts c JOIN measurement_physical_epochs e ON e.id=c.epoch_id WHERE e.source_ref=? AND e.manager_id=? AND c.boundary_seq>=? LIMIT 1`, f.SourceRef, f.ManagerID, int64(f.DispatchSequence)).Scan(&other)
		if err == nil {
			if err = gap("late_record_after_cut"); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = physicalReserve(ctx, tx, "records"); err != nil {
			return err
		}
		body, err = json.Marshal(f)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO measurement_physical_records VALUES(?,?,?,?)`, f.EventID, stream, int64(f.DispatchSequence), body); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE measurement_physical_streams SET records=records+1 WHERE id=?`, stream)
		return err
	})
}
