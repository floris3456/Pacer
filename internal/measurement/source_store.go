package measurement

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

func sourceReserve(ctx context.Context, tx *sql.Tx, column string) error {
	limit := 8192
	switch column {
	case "consumers":
		limit = 1024
	case "memberships":
		limit = 4096
	case "bindings", "runs", "epochs", "gaps", "faults":
	default:
		return ErrInvalid
	}
	res, err := tx.ExecContext(ctx, "UPDATE measurement_source_limits SET "+column+"="+column+"+1 WHERE id=1 AND "+column+"<?", limit)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLimit
	}
	return nil
}

func validSourceRef(ref string) bool {
	return strings.HasPrefix(ref, "sha256:") && nativeSHA(strings.TrimPrefix(ref, "sha256:"))
}

// StartSourceConsumer is required for each new consumer object/process. IDs may
// not be reused, even after a clean shutdown; startup never acknowledges history.
func (s *Store) StartSourceConsumer(ctx context.Context, id, source string, at time.Time) error {
	if !nativeID(id) || !nativeID(source) || !validTime(at) || at.After(s.clock()) {
		return ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM measurement_source_consumers WHERE id=?`, id).Scan(&exists)
		if err == nil {
			return ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previous int64
		err = tx.QueryRowContext(ctx, `SELECT started_at FROM measurement_source_consumers WHERE source_id=? AND active=1`, source).Scan(&previous)
		// A coarse or regressed wall clock must not prevent durable ingestion on
		// reload. The new consumer is poisoned instead of borrowing prior proof.
		poisoned := err == nil && at.UnixNano() <= previous
		start := at.UnixNano()
		if poisoned {
			start = previous
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = sourceReserve(ctx, tx, "consumers"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE measurement_source_consumers SET active=0 WHERE source_id=? AND active=1`, source); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO measurement_source_consumers VALUES(?,?,?,1,?)`, id, source, start, poisoned)
		return err
	})
}

func (s *Store) PoisonSourceConsumer(ctx context.Context, id string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `UPDATE measurement_source_consumers SET poisoned=1 WHERE id=?`, id)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err == nil && n != 1 {
			return ErrInvalid
		}
		return err
	})
}

func canonicalMembership(m SourceMembership) (SourceMembership, []byte, error) {
	if !nativeID(m.ID) || !nativeID(m.SourceID) || m.Provider != "claude" || !nativeID(m.AccountID) || m.Revision < 1 || !validTime(m.From) || !validTime(m.Through) || m.Through.Before(m.From) || m.Complete && m.From.Equal(m.Through) || len(m.Members) > MaxSourceMembers {
		return m, nil, ErrInvalid
	}
	if m.Verified && !nativeID(m.MappingEvidenceRef) {
		return m, nil, ErrInvalid
	}
	m.From, m.Through = utc(m.From), utc(m.Through)
	m.Members = append([]SourceMember(nil), m.Members...)
	sort.Slice(m.Members, func(i, j int) bool { return m.Members[i].SourceRef < m.Members[j].SourceRef })
	for i, v := range m.Members {
		if !validSourceRef(v.SourceRef) || !nativeID(v.CredentialRef) || i > 0 && v.SourceRef == m.Members[i-1].SourceRef {
			return m, nil, ErrInvalid
		}
	}
	b, err := json.Marshal(m)
	return m, b, err
}

// PutSourceMembership persists caller-established membership. Changes cut the
// previous snapshot; intervals spanning snapshots are never silently joined.
func (s *Store) PutSourceMembership(ctx context.Context, m SourceMembership) error {
	m, raw, err := canonicalMembership(m)
	if err != nil {
		return err
	}
	if m.From.After(s.clock()) || m.Through.After(s.clock()) {
		return ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var old []byte
		err := tx.QueryRowContext(ctx, `SELECT body FROM measurement_source_memberships WHERE id=?`, m.ID).Scan(&old)
		if err == nil {
			if !bytes.Equal(old, raw) {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previous string
		err = tx.QueryRowContext(ctx, `SELECT id FROM measurement_source_membership_heads WHERE source_id=? AND provider=? AND account_id=?`, m.SourceID, m.Provider, m.AccountID).Scan(&previous)
		if err == nil {
			prior, _, err := sourceMembership(ctx, tx, previous)
			if err != nil {
				return err
			}
			if m.Revision <= prior.Revision || m.From.Before(prior.From) {
				return ErrConflict
			}
			if _, err = tx.ExecContext(ctx, `UPDATE measurement_source_memberships SET cut_at=min(cut_at,?) WHERE id=?`, m.From.UnixNano(), previous); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, member := range m.Members {
			if !m.Verified {
				continue
			}
			var source, provider, account, credential string
			err = tx.QueryRowContext(ctx, `SELECT source_id,provider,account_id,credential_ref FROM measurement_source_bindings WHERE source_ref=?`, member.SourceRef).Scan(&source, &provider, &account, &credential)
			if err == nil {
				if source != m.SourceID || provider != m.Provider || account != m.AccountID || credential != member.CredentialRef {
					return ErrConflict
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err = sourceReserve(ctx, tx, "bindings"); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO measurement_source_bindings VALUES(?,?,?,?,?)`, member.SourceRef, m.SourceID, m.Provider, m.AccountID, member.CredentialRef); err != nil {
				return err
			}
		}
		if err = sourceReserve(ctx, tx, "memberships"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO measurement_source_memberships VALUES(?,?,?,?,?,?,?)`, m.ID, m.SourceID, m.Provider, m.AccountID, m.Revision, m.Through.UnixNano(), raw); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO measurement_source_membership_heads VALUES(?,?,?,?) ON CONFLICT(source_id,provider,account_id) DO UPDATE SET id=excluded.id`, m.SourceID, m.Provider, m.AccountID, m.ID)
		return err
	})
}

func sourceMembership(ctx context.Context, tx *sql.Tx, id string) (SourceMembership, time.Time, error) {
	var m SourceMembership
	var raw []byte
	var cut int64
	err := tx.QueryRowContext(ctx, `SELECT body,cut_at FROM measurement_source_memberships WHERE id=?`, id).Scan(&raw, &cut)
	if err != nil {
		return m, time.Time{}, err
	}
	err = json.Unmarshal(raw, &m)
	return m, stamp(cut), err
}

func (s *Store) SourceMembership(ctx context.Context, id string) (SourceMembership, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM measurement_source_memberships WHERE id=?`, id).Scan(&raw)
	var m SourceMembership
	if err == nil {
		err = json.Unmarshal(raw, &m)
	}
	return m, err
}

// CurrentSourceMembership reads the indexed inventory head, not event history.
// A discovery adapter may reuse an unchanged incomplete point snapshot.
func (s *Store) CurrentSourceMembership(ctx context.Context, source, provider, account string) (SourceMembership, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT m.body FROM measurement_source_membership_heads h JOIN measurement_source_memberships m ON m.id=h.id WHERE h.source_id=? AND h.provider=? AND h.account_id=?`, source, provider, account).Scan(&raw)
	var m SourceMembership
	if err == nil {
		err = json.Unmarshal(raw, &m)
	}
	return m, err
}

func sourceConsumer(ctx context.Context, tx *sql.Tx, id string) (source string, start time.Time, usable bool, err error) {
	var at int64
	var active, poisoned bool
	err = tx.QueryRowContext(ctx, `SELECT source_id,started_at,active,poisoned FROM measurement_source_consumers WHERE id=?`, id).Scan(&source, &at, &active, &poisoned)
	return source, stamp(at), active && !poisoned, err
}

func invalidStamp(from *time.Time) int64 {
	if from == nil || !validTime(*from) {
		return 0
	}
	return from.UnixNano()
}

func insertSourceGap(ctx context.Context, tx *sql.Tx, ref, manager, sink string, from *time.Time) error {
	var old int64
	err := tx.QueryRowContext(ctx, `SELECT invalid_from FROM measurement_source_gaps WHERE source_ref=? AND manager_id=? AND sink_id=?`, ref, manager, sink).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		if err = sourceReserve(ctx, tx, "gaps"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO measurement_source_gaps VALUES(?,?,?,?) ON CONFLICT(source_ref,manager_id,sink_id) DO UPDATE SET invalid_from=min(invalid_from,excluded.invalid_from)`, ref, manager, sink, invalidStamp(from))
	return err
}

// InvalidateSource persists a sticky manager/sink invalidation. An empty sink
// applies to every sink in that source manager (usage evidence has no sink ID).
func (s *Store) InvalidateSource(ctx context.Context, ref, manager, sink string, from *time.Time) error {
	if !validSourceRef(ref) || !nativeID(manager) || sink != "" && !nativeID(sink) {
		return ErrInvalid
	}
	if from != nil && (!validTime(*from) || from.After(s.clock())) {
		from = nil
	}
	return s.write(ctx, func(tx *sql.Tx) error { return insertSourceGap(ctx, tx, ref, manager, sink, from) })
}

// FaultSourceAccount records uncorrelated/legacy activity. Empty account affects
// all accounts of this consumer. Reopening cannot reuse the consumer identity.
func (s *Store) FaultSourceAccount(ctx context.Context, consumer, account string, from *time.Time) error {
	if !nativeID(consumer) || account != "" && !nativeID(account) {
		return ErrInvalid
	}
	if from != nil && (!validTime(*from) || from.After(s.clock())) {
		from = nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, _, _, err := sourceConsumer(ctx, tx, consumer); err != nil {
			return err
		}
		var n int64
		err := tx.QueryRowContext(ctx, `SELECT invalid_from FROM measurement_source_faults WHERE consumer_id=? AND account_id=?`, consumer, account).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			if err = sourceReserve(ctx, tx, "faults"); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO measurement_source_faults VALUES(?,?,?) ON CONFLICT(consumer_id,account_id) DO UPDATE SET invalid_from=min(invalid_from,excluded.invalid_from)`, consumer, account, invalidStamp(from))
		return err
	})
}

func membershipHas(m SourceMembership, ref string) bool {
	for _, v := range m.Members {
		if v.SourceRef == ref {
			return true
		}
	}
	return false
}
func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// ApplySourceCheckpoint commits gaps even on stale/conflicting/invalid proof.
// It never writes measurement_delivery_runs or calls the global Checkpoint API.
func (s *Store) ApplySourceCheckpoint(ctx context.Context, p SourceCheckpoint) error {
	var refusal error
	err := s.write(ctx, func(tx *sql.Tx) error {
		source, start, usable, err := sourceConsumer(ctx, tx, p.ConsumerID)
		if err != nil {
			return err
		}
		m, _, err := sourceMembership(ctx, tx, p.MembershipID)
		if err != nil {
			return err
		}
		if source != m.SourceID || !membershipHas(m, p.SourceRef) {
			return ErrConflict
		}
		if !nativeID(p.ManagerID) || !nativeID(p.SinkID) || p.Sequence == 0 || p.Sequence > math.MaxInt64 {
			_, err = tx.ExecContext(ctx, `UPDATE measurement_source_consumers SET poisoned=1 WHERE id=?`, p.ConsumerID)
			refusal = ErrInvalid
			return err
		}
		bad := p.Invalid || !nativeSHA(p.PayloadDigest) || (p.Coverage != "known" && p.Coverage != "unknown" && p.Coverage != "unsupported")
		if p.From != nil && (!validTime(*p.From) || p.From.After(s.clock())) {
			bad = true
		}
		if p.Coverage == "known" && (p.From == nil || p.Through == nil || !validTime(*p.Through) || p.Through.Before(*p.From) || p.Through.After(s.clock()) || p.HasGap) {
			bad = true
		}
		if p.Coverage != "known" && p.Through != nil {
			bad = true
		}
		if bad {
			p.HasGap = true
			p.InvalidFrom = nil
			refusal = ErrInvalid
		}
		if p.HasGap {
			if p.InvalidFrom != nil && (!validTime(*p.InvalidFrom) || p.InvalidFrom.After(s.clock())) {
				p.InvalidFrom = nil
			}
			if err = insertSourceGap(ctx, tx, p.SourceRef, p.ManagerID, p.SinkID, p.InvalidFrom); err != nil {
				return err
			}
		}
		if usable && !bad {
			floor, epochErr := acceptSourceEpoch(ctx, tx, p)
			if errors.Is(epochErr, ErrStaleSource) || errors.Is(epochErr, ErrConflict) {
				refusal = epochErr
				return nil
			}
			if epochErr != nil {
				return epochErr
			}
			start = later(start, floor)
		}
		var raw []byte
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT body,active FROM measurement_source_runs WHERE consumer_id=? AND membership_id=? AND source_ref=? AND manager_id=? AND sink_id=?`, p.ConsumerID, p.MembershipID, p.SourceRef, p.ManagerID, p.SinkID).Scan(&raw, &active)
		var run SourceRun
		if err == nil {
			if err = json.Unmarshal(raw, &run); err != nil {
				return err
			}
			if !active || p.Sequence < run.Sequence {
				if refusal == nil {
					refusal = ErrStaleSource
				}
				return nil
			}
			if p.Sequence == run.Sequence {
				if run.PayloadDigest != p.PayloadDigest {
					refusal = ErrConflict
					return insertSourceGap(ctx, tx, p.SourceRef, p.ManagerID, p.SinkID, nil)
				}
				return nil
			}
			if p.From != nil && !run.HostFrom.IsZero() && !p.From.Equal(run.HostFrom) || p.Coverage == "known" && p.Through != nil && p.Through.Before(run.Through) {
				refusal = ErrConflict
				return insertSourceGap(ctx, tx, p.SourceRef, p.ManagerID, p.SinkID, nil)
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if !usable {
				refusal = ErrStaleSource
				return nil
			}
			var priorRaw []byte
			err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_source_runs WHERE consumer_id=? AND membership_id=? AND source_ref=? AND active=1`, p.ConsumerID, p.MembershipID, p.SourceRef).Scan(&priorRaw)
			if err == nil {
				var prior SourceRun
				if err = json.Unmarshal(priorRaw, &prior); err != nil {
					return err
				}
				start = later(start, prior.Through)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err = sourceReserve(ctx, tx, "runs"); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE measurement_source_runs SET active=0 WHERE consumer_id=? AND membership_id=? AND source_ref=? AND active=1`, p.ConsumerID, p.MembershipID, p.SourceRef); err != nil {
				return err
			}
			run = SourceRun{ConsumerID: p.ConsumerID, MembershipID: p.MembershipID, SourceRef: p.SourceRef, ManagerID: p.ManagerID, SinkID: p.SinkID, From: later(start, m.From)}
		} else {
			return err
		}
		if !usable {
			refusal = ErrStaleSource
			return nil
		}
		run.Sequence = p.Sequence
		run.PayloadDigest = p.PayloadDigest
		run.Coverage = p.Coverage
		if bad {
			run.Coverage = "unknown"
		}
		if p.From != nil {
			run.HostFrom = utc(*p.From)
			run.From = later(run.From, run.HostFrom)
		}
		if p.Through != nil && !bad {
			run.Through = utc(*p.Through)
		}
		raw, err = json.Marshal(run)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO measurement_source_runs VALUES(?,?,?,?,?,1,?) ON CONFLICT(consumer_id,membership_id,source_ref,manager_id,sink_id) DO UPDATE SET body=excluded.body`, p.ConsumerID, p.MembershipID, p.SourceRef, p.ManagerID, p.SinkID, raw)
		return err
	})
	if err != nil {
		return err
	}
	return refusal
}
