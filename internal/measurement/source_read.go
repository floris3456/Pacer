package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) SourceSnapshot(ctx context.Context, consumer, membership, provider, account string, from, through time.Time) (SourceSnapshot, error) {
	if !validTime(from) || !validTime(through) || !from.Before(through) {
		return SourceSnapshot{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SourceSnapshot{}, err
	}
	defer tx.Rollback()
	snapshot, err := readSourceSnapshot(ctx, tx, consumer, membership, provider, account)
	if err != nil {
		return snapshot, err
	}
	if err = tx.Commit(); err != nil {
		return SourceSnapshot{}, err
	}
	return boundSourceSnapshot(snapshot, from, through), nil
}

func boundSourceSnapshot(s SourceSnapshot, from, through time.Time) SourceSnapshot {
	if from.Before(s.From) || through.After(s.Through) || !from.Before(through) {
		s.Reasons = append(s.Reasons, "outside_source_proof_window")
	}
	s.Reasons = unique(s.Reasons)
	s.Known = len(s.Reasons) == 0
	return s
}

// Each lookup is by a primary/unique key. Reads are bounded by the explicit
// membership (at most 128 tuples), never by historical usage or checkpoint count.
func readSourceSnapshot(ctx context.Context, tx *sql.Tx, consumer, membership, provider, account string) (SourceSnapshot, error) {
	out := SourceSnapshot{Provider: provider, AccountID: account, MembershipID: membership, Runs: []SourceRun{}}
	if consumer == "" || membership == "" {
		out.Reasons = []string{"source_membership_required"}
		return out, nil
	}
	m, cut, err := sourceMembership(ctx, tx, membership)
	if errors.Is(err, sql.ErrNoRows) {
		out.Reasons = []string{"source_membership_missing"}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if m.Provider != provider || m.AccountID != account {
		out.Reasons = []string{"source_account_mismatch"}
		return out, nil
	}
	source, start, usable, err := sourceConsumer(ctx, tx, consumer)
	if errors.Is(err, sql.ErrNoRows) {
		out.Reasons = []string{"source_consumer_missing"}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if source != m.SourceID || !usable {
		out.Reasons = append(out.Reasons, "source_consumer_inactive_or_failed")
	}
	if !m.Verified || m.MappingEvidenceRef == "" {
		out.Reasons = append(out.Reasons, "unverified_billing_account_mapping")
	}
	if !m.Complete || len(m.Members) == 0 {
		out.Reasons = append(out.Reasons, "incomplete_account_membership")
	}
	out.From = later(start, m.From)
	out.Through = cut
	for _, acct := range []string{"", account} {
		var at int64
		err = tx.QueryRowContext(ctx, `SELECT invalid_from FROM measurement_source_faults WHERE consumer_id=? AND account_id=?`, consumer, acct).Scan(&at)
		if err == nil {
			snapshotGap(&out, at, "consumer_record_gap")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	for _, member := range m.Members {
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT body FROM measurement_source_runs WHERE consumer_id=? AND membership_id=? AND source_ref=? AND active=1`, consumer, membership, member.SourceRef).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			out.Reasons = append(out.Reasons, "member_source_proof_missing")
			continue
		}
		if err != nil {
			return out, err
		}
		var run SourceRun
		if err = json.Unmarshal(raw, &run); err != nil {
			return out, err
		}
		out.Runs = append(out.Runs, run)
		var active bool
		var epochRaw []byte
		err = tx.QueryRowContext(ctx, `SELECT active,body FROM measurement_source_epochs WHERE source_ref=? AND manager_id=? AND sink_id=?`, member.SourceRef, run.ManagerID, run.SinkID).Scan(&active, &epochRaw)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !active {
			out.Reasons = append(out.Reasons, "retired_source_incarnation")
		} else if err != nil {
			return out, err
		} else {
			var epoch sourceEpoch
			if err = json.Unmarshal(epochRaw, &epoch); err != nil {
				return out, err
			}
			if epoch.Coverage != "known" {
				out.Reasons = append(out.Reasons, "source_incarnation_unknown")
			}
		}
		if run.Coverage != "known" || run.HostFrom.IsZero() || run.Through.IsZero() {
			out.Reasons = append(out.Reasons, "member_source_proof_unknown")
		}
		out.From = later(out.From, run.From)
		if run.Through.Before(out.Through) {
			out.Through = run.Through
		}
		for _, sink := range []string{"", run.SinkID} {
			var at int64
			err = tx.QueryRowContext(ctx, `SELECT invalid_from FROM measurement_source_gaps WHERE source_ref=? AND manager_id=? AND sink_id=?`, member.SourceRef, run.ManagerID, sink).Scan(&at)
			if err == nil {
				snapshotGap(&out, at, "source_incarnation_gap")
			} else if !errors.Is(err, sql.ErrNoRows) {
				return out, err
			}
		}
	}
	if !out.From.Before(out.Through) {
		out.Reasons = append(out.Reasons, "empty_source_proof_window")
	}
	out.Reasons = unique(out.Reasons)
	out.Known = len(out.Reasons) == 0
	return out, nil
}

func snapshotGap(s *SourceSnapshot, at int64, reason string) {
	unknown := s.HasGap && s.InvalidFrom == nil
	s.HasGap = true
	s.Reasons = append(s.Reasons, reason)
	if at == 0 || unknown {
		s.InvalidFrom = nil
		return
	}
	v := stamp(at)
	if s.InvalidFrom == nil || v.Before(*s.InvalidFrom) {
		s.InvalidFrom = &v
	}
}

func scopedCoverage(e evidence, account string, from, to time.Time) Coverage {
	c := Coverage{Delivery: "cpa-scoped-request-start"}
	s, ok := e.Sources[account]
	if !ok {
		c.Reasons = []string{"source_membership_required"}
		return c
	}
	s = boundSourceSnapshot(s, from, to)
	c.Reasons = append(c.Reasons, s.Reasons...)
	// CPA's horizon is on RequestedAt. An unobserved pre-registration producer
	// may physically overlap this interval after its already-acknowledged record.
	// Neither an empty queue nor observed records can exclude that carry-in.
	if !s.PhysicalCarryInProven {
		c.Reasons = append(c.Reasons, "physical_carry_in_unproven")
	}
	c.Reasons = unique(c.Reasons)
	c.Observed = len(c.Reasons) == 0
	return c
}
