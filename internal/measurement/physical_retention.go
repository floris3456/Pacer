package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// PhysicalRetention bounds the physical ledger by what is still read. Capacity
// judgment re-reads the proofs of each account's newest samples, about five
// hours at a 30-minute usage poll; an older proof is never asked for again.
// The cutoff counts back from the newest checkpoint stored, not the clock, so
// a restart after downtime keeps the history those newest samples name.
const PhysicalRetention = 12 * time.Hour

const physicalPruneEvery = 10 * time.Minute

// prunePhysical deletes the rows no reader can use any more and sets the meta
// counters to the rows left, so the limits bound what is stored instead of
// everything ever written:
//   - epochs of earlier consumers idle since the cutoff or that never got a
//     lower edge, with their proofs and cuts (a consumer never proves on another
//     consumer's epoch), and proofs or
//     cuts of epochs that no longer exist (re-keyed epochs);
//   - proofs whose through cut is older than the cutoff;
//   - cuts older than the cutoff that no kept proof or lower edge names;
//   - stream records at or below every remaining epoch's lower edge (proofs only
//     join dispatches above it; an epoch without one only ever adopts a lower
//     edge at or above its last sequence), and the records and streams of
//     earlier consumers with no epoch left. A stream's count shrinks with its
//     records, so a row that goes missing otherwise is still detected.
//
// Gaps, faults and consumers stay: they are negative evidence and grow only
// with incarnations.
func prunePhysical(ctx context.Context, tx *sql.Tx, consumer string) error {
	var newest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(last_at) FROM measurement_physical_epochs`).Scan(&newest); err != nil {
		return err
	}
	cutoff := time.Unix(0, newest.Int64).Add(-PhysicalRetention)
	if err := pruneEpochs(ctx, tx, consumer, cutoff.UnixNano()); err != nil {
		return err
	}
	keep, err := pruneProofs(ctx, tx, cutoff)
	if err != nil {
		return err
	}
	if err = pruneCuts(ctx, tx, keep, cutoff); err != nil {
		return err
	}
	if err = pruneRecords(ctx, tx, consumer); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE measurement_physical_meta SET records=(SELECT count(*) FROM measurement_physical_records),streams=(SELECT count(*) FROM measurement_physical_streams),epochs=(SELECT count(*) FROM measurement_physical_epochs),cuts=(SELECT count(*) FROM measurement_physical_cuts),proofs=(SELECT count(*) FROM measurement_physical_proofs),gaps=(SELECT count(*) FROM measurement_physical_gaps),consumers=(SELECT count(*) FROM measurement_physical_consumers),faults=(SELECT count(*) FROM measurement_physical_faults) WHERE id=1`)
	return err
}

// resetPhysical empties a poisoned namespace. The poison means some writes were
// refused, so no stored epoch may prove across that stretch: every epoch, cut
// and proof goes, and a consumer only proves from cuts it observes afterwards.
func resetPhysical(ctx context.Context, tx *sql.Tx) error {
	for _, name := range physicalTables[1:] {
		if _, err := tx.ExecContext(ctx, `DELETE FROM measurement_physical_`+name); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE measurement_physical_meta SET poisoned=0,records=0,streams=0,epochs=0,cuts=0,proofs=0,gaps=0,consumers=0,faults=0 WHERE id=1`)
	return err
}

func pruneEpochs(ctx context.Context, tx *sql.Tx, consumer string, cutoff int64) error {
	for _, q := range []string{
		`DELETE FROM measurement_physical_proofs WHERE epoch_id IN (SELECT id FROM measurement_physical_epochs WHERE consumer_id<>? AND (last_at<? OR last_at=0))`,
		`DELETE FROM measurement_physical_cuts WHERE epoch_id IN (SELECT id FROM measurement_physical_epochs WHERE consumer_id<>? AND (last_at<? OR last_at=0))`,
		`DELETE FROM measurement_physical_epochs WHERE consumer_id<>? AND (last_at<? OR last_at=0)`,
	} {
		if _, err := tx.ExecContext(ctx, q, consumer, cutoff); err != nil {
			return err
		}
	}
	for _, table := range []string{"proofs", "cuts"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM measurement_physical_`+table+` WHERE epoch_id NOT IN (SELECT id FROM measurement_physical_epochs)`); err != nil {
			return err
		}
	}
	return nil
}

// pruneProofs returns the cut IDs the kept proofs name.
func pruneProofs(ctx context.Context, tx *sql.Tx, cutoff time.Time) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,body FROM measurement_physical_proofs`)
	if err != nil {
		return nil, err
	}
	keep, old := map[string]bool{}, []string{}
	for rows.Next() {
		var id string
		var body []byte
		if err = rows.Scan(&id, &body); err != nil {
			rows.Close()
			return nil, err
		}
		var m physicalManifest
		if json.Unmarshal(body, &m) != nil || m.Selection.Through.At.Before(cutoff) {
			old = append(old, id)
			continue
		}
		keep[m.Selection.From.ID()], keep[m.Selection.Through.ID()] = true, true
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range old {
		if _, err = tx.ExecContext(ctx, `DELETE FROM measurement_physical_proofs WHERE id=?`, id); err != nil {
			return nil, err
		}
	}
	return keep, nil
}

func pruneCuts(ctx context.Context, tx *sql.Tx, keep map[string]bool, cutoff time.Time) error {
	lowers, err := tx.QueryContext(ctx, `SELECT lower_body FROM measurement_physical_epochs WHERE lower_body IS NOT NULL`)
	if err != nil {
		return err
	}
	for lowers.Next() {
		var raw []byte
		if err = lowers.Scan(&raw); err != nil {
			lowers.Close()
			return err
		}
		var c PhysicalCutIdentity
		if json.Unmarshal(raw, &c) == nil {
			keep[c.ID()] = true
		}
	}
	if err = lowers.Close(); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,body FROM measurement_physical_cuts`)
	if err != nil {
		return err
	}
	old := []string{}
	for rows.Next() {
		var id string
		var body []byte
		if err = rows.Scan(&id, &body); err != nil {
			rows.Close()
			return err
		}
		var c PhysicalCutIdentity
		if !keep[id] && (json.Unmarshal(body, &c) != nil || c.At.Before(cutoff)) {
			old = append(old, id)
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, id := range old {
		if _, err = tx.ExecContext(ctx, `DELETE FROM measurement_physical_cuts WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

func pruneRecords(ctx context.Context, tx *sql.Tx, consumer string) error {
	type stream struct{ id, consumer, ref, manager string }
	rows, err := tx.QueryContext(ctx, `SELECT id,consumer_id,source_ref,manager_id FROM measurement_physical_streams`)
	if err != nil {
		return err
	}
	streams := []stream{}
	for rows.Next() {
		var s stream
		if err = rows.Scan(&s.id, &s.consumer, &s.ref, &s.manager); err != nil {
			rows.Close()
			return err
		}
		streams = append(streams, s)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, s := range streams {
		floor, epochs, err := recordFloor(ctx, tx, s.consumer, s.ref, s.manager)
		if err != nil {
			return err
		}
		if epochs == 0 {
			if s.consumer == consumer {
				continue // records can precede this consumer's first checkpoint
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM measurement_physical_records WHERE stream_id=?`, s.id); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM measurement_physical_streams WHERE id=?`, s.id); err != nil {
				return err
			}
			continue
		}
		r, err := tx.ExecContext(ctx, `DELETE FROM measurement_physical_records WHERE stream_id=? AND dispatch_seq<=?`, s.id, floor)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE measurement_physical_streams SET records=records-? WHERE id=?`, n, s.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordFloor is the lowest sequence any epoch of the stream can still prove
// from: its lower edge, or its last sequence while it has none.
func recordFloor(ctx context.Context, tx *sql.Tx, consumer, ref, manager string) (int64, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT last_seq,lower_body FROM measurement_physical_epochs WHERE consumer_id=? AND source_ref=? AND manager_id=?`, consumer, ref, manager)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var floor int64
	epochs := 0
	for rows.Next() {
		var last int64
		var raw []byte
		if err = rows.Scan(&last, &raw); err != nil {
			return 0, 0, err
		}
		at := last
		if len(raw) > 0 {
			var lower PhysicalCutIdentity
			at = 0 // an unreadable lower edge keeps every record of the stream
			if json.Unmarshal(raw, &lower) == nil && physicalSequence(lower.Sequence) {
				at = int64(lower.Sequence)
			}
		}
		if epochs == 0 || at < floor {
			floor = at
		}
		epochs++
	}
	return floor, epochs, rows.Err()
}
