package measurement

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const physicalSchema = `
CREATE TABLE measurement_physical_meta(id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL, poisoned INTEGER NOT NULL, records INTEGER NOT NULL, streams INTEGER NOT NULL, epochs INTEGER NOT NULL, cuts INTEGER NOT NULL, proofs INTEGER NOT NULL, gaps INTEGER NOT NULL, consumers INTEGER NOT NULL, faults INTEGER NOT NULL);
INSERT INTO measurement_physical_meta VALUES(1,1,0,0,0,0,0,0,0,0,0);
CREATE TABLE measurement_physical_consumers(id TEXT PRIMARY KEY, source_id TEXT NOT NULL, failed INTEGER NOT NULL);
CREATE TABLE measurement_physical_streams(id TEXT PRIMARY KEY, consumer_id TEXT NOT NULL, source_ref TEXT NOT NULL, manager_id TEXT NOT NULL, records INTEGER NOT NULL);
CREATE TABLE measurement_physical_records(event_id TEXT PRIMARY KEY, stream_id TEXT NOT NULL, dispatch_seq INTEGER NOT NULL, body BLOB NOT NULL);
CREATE UNIQUE INDEX measurement_physical_dispatch ON measurement_physical_records(stream_id,dispatch_seq);
CREATE TABLE measurement_physical_epochs(id TEXT PRIMARY KEY, consumer_id TEXT NOT NULL, source_ref TEXT NOT NULL, manager_id TEXT NOT NULL, sink_id TEXT NOT NULL, active INTEGER NOT NULL, last_seq INTEGER NOT NULL, digest TEXT NOT NULL, lower_body BLOB, last_at INTEGER NOT NULL);
CREATE UNIQUE INDEX measurement_physical_owner ON measurement_physical_epochs(source_ref,manager_id,sink_id);
CREATE UNIQUE INDEX measurement_physical_active ON measurement_physical_epochs(consumer_id,source_ref) WHERE active=1;
CREATE TABLE measurement_physical_cuts(id TEXT PRIMARY KEY, epoch_id TEXT NOT NULL, boundary_seq INTEGER NOT NULL, body BLOB NOT NULL);
CREATE INDEX measurement_physical_boundary ON measurement_physical_cuts(epoch_id,boundary_seq);
CREATE TABLE measurement_physical_proofs(id TEXT PRIMARY KEY, epoch_id TEXT NOT NULL, upper_seq INTEGER NOT NULL, body BLOB NOT NULL);
CREATE INDEX measurement_physical_upper ON measurement_physical_proofs(epoch_id,upper_seq);
CREATE TABLE measurement_physical_gaps(source_ref TEXT NOT NULL, manager_id TEXT NOT NULL, sink_id TEXT NOT NULL, reason TEXT NOT NULL, PRIMARY KEY(source_ref,manager_id,sink_id));
CREATE TABLE measurement_physical_faults(consumer_id TEXT NOT NULL, account_id TEXT NOT NULL, reason TEXT NOT NULL, PRIMARY KEY(consumer_id,account_id));
`

var physicalTables = []string{"meta", "consumers", "streams", "records", "epochs", "cuts", "proofs", "gaps", "faults"}

// PhysicalProof is an explicit, namespaced store handle. It borrows Store.write
// so the global measurement schema/version and future writer changes stay owned
// by Store. No migration or event scan is added to measurement.Open.
type PhysicalProof struct {
	store            *Store
	consumer, source string
	pruned           time.Time
}

// NewPhysicalProof is called only after StartSourceConsumer has durably assigned
// a new listener ID, before its first checkpoint. Startup checks metadata only.
func (s *Store) NewPhysicalProof(ctx context.Context, consumer string) (*PhysicalProof, error) {
	p := &PhysicalProof{store: s, consumer: consumer}
	var refusal error
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		p.source, _, _, err = sourceConsumer(ctx, tx, consumer)
		if err != nil {
			return err
		}
		exists := 0
		for _, name := range physicalTables {
			var n int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, "measurement_physical_"+name).Scan(&n); err != nil {
				return err
			}
			exists += n
		}
		if exists == 0 {
			if _, err = tx.ExecContext(ctx, physicalSchema); err != nil {
				return err
			}
			// Store.write checked the previous feed cookie before this compiled,
			// additive DDL. No other writer can interleave. Advance only here;
			// never bless a pre-existing schema change or recreate a lost feed.
			if _, err = tx.ExecContext(ctx, `UPDATE measurement_report_meta SET schema_cookie=(SELECT schema_version FROM pragma_schema_version) WHERE id=1 AND version=2`); err != nil {
				return err
			}
		} else if exists != len(physicalTables) {
			return errors.New("measurement: partial physical schema")
		}
		for _, name := range []string{"dispatch", "owner", "active", "upper", "boundary"} {
			var n int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, "measurement_physical_"+name).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				return errors.New("measurement: partial physical indexes")
			}
		}
		var old string
		err = tx.QueryRowContext(ctx, `SELECT source_id FROM measurement_physical_consumers WHERE id=?`, consumer).Scan(&old)
		if err == nil {
			// Reopening this consumer keeps the namespace as it is, poison included.
			if old != p.source {
				return ErrConflict
			}
			return physicalMetadata(ctx, tx)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// A new consumer (a restart) starts a poisoned namespace over.
		if err = physicalMetadata(ctx, tx); errors.Is(err, ErrLimit) {
			err = resetPhysical(ctx, tx)
		}
		if err != nil {
			return err
		}
		if err = physicalReserve(ctx, tx, "consumers"); err != nil {
			if errors.Is(err, ErrLimit) {
				refusal = err
				_, err = tx.ExecContext(ctx, `UPDATE measurement_physical_meta SET poisoned=1 WHERE id=1`)
			}
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO measurement_physical_consumers VALUES(?,?,0)`, consumer, p.source); err != nil {
			return err
		}
		return prunePhysical(ctx, tx, consumer)
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	p.pruned = s.clock()
	return p, nil
}

type physicalQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func physicalMetadata(ctx context.Context, q physicalQuerier) error {
	var version, poisoned int
	if err := q.QueryRowContext(ctx, `SELECT version,poisoned FROM measurement_physical_meta WHERE id=1`).Scan(&version, &poisoned); err != nil {
		return err
	}
	if version != 1 {
		return errors.New("measurement: unsupported physical schema")
	}
	if poisoned != 0 {
		return ErrLimit
	}
	return nil
}

func physicalReserve(ctx context.Context, tx *sql.Tx, column string) error {
	limit := 8192
	switch column {
	case "consumers":
		limit = 1024
	case "streams", "epochs":
		limit = 4096
	case "records", "cuts":
		limit = 16384
	case "proofs", "gaps", "faults":
	default:
		return ErrInvalid
	}
	r, err := tx.ExecContext(ctx, `UPDATE measurement_physical_meta SET `+column+`=`+column+`+1 WHERE id=1 AND `+column+`<?`, limit)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLimit
	}
	return nil
}

// Exhaustion commits a durable namespace poison instead of rolling back into a
// reusable apparent success. Other SQL errors propagate to the acknowledged hook.
func (p *PhysicalProof) write(ctx context.Context, fn func(*sql.Tx) error) error {
	var refusal error
	err := p.store.write(ctx, func(tx *sql.Tx) error {
		e := physicalMetadata(ctx, tx)
		if e == nil {
			e = fn(tx)
		}
		if errors.Is(e, ErrLimit) {
			refusal = e
			_, e = tx.ExecContext(ctx, `UPDATE measurement_physical_meta SET poisoned=1 WHERE id=1`)
		}
		return e
	})
	if err != nil {
		return err
	}
	return refusal
}

func physicalGap(ctx context.Context, tx *sql.Tx, ref, manager, sink, reason string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM measurement_physical_gaps WHERE source_ref=? AND manager_id=? AND sink_id=?`, ref, manager, sink).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = physicalReserve(ctx, tx, "gaps"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO measurement_physical_gaps VALUES(?,?,?,?)`, ref, manager, sink, reason)
	return err
}

func (p *PhysicalProof) Invalidate(ctx context.Context, ref, manager, sink string) error {
	if !validSourceRef(ref) || !nativeID(manager) || sink != "" && !nativeID(sink) {
		return p.Fault(ctx, "")
	}
	return p.write(ctx, func(tx *sql.Tx) error { return physicalGap(ctx, tx, ref, manager, sink, "source_invalidation") })
}
func (p *PhysicalProof) Fault(ctx context.Context, account string) error {
	if account != "" && !nativeID(account) {
		account = ""
	}
	return p.write(ctx, func(tx *sql.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM measurement_physical_faults WHERE consumer_id=? AND account_id=?`, p.consumer, account).Scan(&exists)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = physicalReserve(ctx, tx, "faults"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO measurement_physical_faults VALUES(?,?,'unavailable_observation')`, p.consumer, account)
		return err
	})
}
func (p *PhysicalProof) Poison(ctx context.Context) error {
	return p.store.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `UPDATE measurement_physical_consumers SET failed=1 WHERE id=?`, p.consumer)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err == nil && n != 1 {
			return fmt.Errorf("measurement: physical consumer missing")
		}
		return err
	})
}
