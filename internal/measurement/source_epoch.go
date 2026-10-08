package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type sourceEpoch struct {
	Sequence       uint64
	Digest         string
	Coverage       string
	HostFrom       time.Time
	Floor, Through time.Time
}

// Epoch ordering is independent of membership and consumer snapshots: replaying
// a retired host/sink pair into a fresh membership cannot reactivate old proof.
func acceptSourceEpoch(ctx context.Context, tx *sql.Tx, p SourceCheckpoint) (time.Time, error) {
	var raw []byte
	var active bool
	var epoch sourceEpoch
	err := tx.QueryRowContext(ctx, `SELECT body,active FROM measurement_source_epochs WHERE source_ref=? AND manager_id=? AND sink_id=?`, p.SourceRef, p.ManagerID, p.SinkID).Scan(&raw, &active)
	if err == nil {
		if err = json.Unmarshal(raw, &epoch); err != nil {
			return time.Time{}, err
		}
		if !active || p.Sequence < epoch.Sequence {
			return time.Time{}, ErrStaleSource
		}
		if p.Sequence == epoch.Sequence {
			if p.PayloadDigest != epoch.Digest {
				if err = insertSourceGap(ctx, tx, p.SourceRef, p.ManagerID, p.SinkID, nil); err != nil {
					return time.Time{}, err
				}
				return time.Time{}, ErrConflict
			}
			return epoch.Floor, nil
		}
		if p.From != nil && !epoch.HostFrom.IsZero() && !p.From.Equal(epoch.HostFrom) || p.Coverage == "known" && p.Through != nil && p.Through.Before(epoch.Through) {
			if err = insertSourceGap(ctx, tx, p.SourceRef, p.ManagerID, p.SinkID, nil); err != nil {
				return time.Time{}, err
			}
			return time.Time{}, ErrConflict
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		var prior []byte
		err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_source_epochs WHERE source_ref=? AND active=1`, p.SourceRef).Scan(&prior)
		if err == nil {
			var old sourceEpoch
			if err = json.Unmarshal(prior, &old); err != nil {
				return time.Time{}, err
			}
			epoch.Floor = later(old.Floor, old.Through)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, err
		}
		if err = sourceReserve(ctx, tx, "epochs"); err != nil {
			return time.Time{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE measurement_source_epochs SET active=0 WHERE source_ref=? AND active=1`, p.SourceRef); err != nil {
			return time.Time{}, err
		}
	} else {
		return time.Time{}, err
	}
	epoch.Sequence = p.Sequence
	epoch.Digest = p.PayloadDigest
	epoch.Coverage = p.Coverage
	if p.From != nil {
		epoch.HostFrom = utc(*p.From)
	}
	if p.Through != nil {
		epoch.Through = later(epoch.Through, *p.Through)
	}
	raw, err = json.Marshal(epoch)
	if err != nil {
		return time.Time{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO measurement_source_epochs VALUES(?,?,?,1,?) ON CONFLICT(source_ref,manager_id,sink_id) DO UPDATE SET body=excluded.body`, p.SourceRef, p.ManagerID, p.SinkID, raw)
	return epoch.Floor, err
}
