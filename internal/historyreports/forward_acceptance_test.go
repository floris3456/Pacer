package historyreports

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

type forwardShortWriter struct{}

func TestForwardReadPlanUsesBoundedKeysets(t *testing.T) {
	s, _ := forwardLedger(t)
	ctx := context.Background()
	rows, err := s.reader.QueryContext(ctx, "EXPLAIN QUERY PLAN "+forwardBatchSQL, "origin", true, 100001, 100001, 1, "last-id", 1000000, true, 100001, "origin", true, true, 100001, 1, "last-id", 1000000)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Log(plan)
	if strings.Contains(plan, "USE TEMP B-TREE") || !strings.Contains(plan, "forward_native_page") || !strings.Contains(plan, "forward_base_page") {
		t.Fatal("batch query must seek both ordered streams without a whole-ledger sort")
	}
	if _, err = s.reader.ExecContext(ctx, `UPDATE forward_meta SET latest='untrusted'`); err == nil {
		t.Fatal("read pool is writable")
	}
}

func TestForwardCapturedCostIsNotRevaluation(t *testing.T) {
	raw, corrected := int64(11), int64(22)
	legacy := ForwardEvent{Origin: "baseline", Event: cpamp.Event{CostNano: &corrected, OriginalAccounting: &cpamp.OriginalAccounting{CostNano: &raw}}}
	cost, basis, conflict := forwardCapturedCost([]ForwardEvent{legacy})
	if cost == nil || *cost != raw || basis != "cpamp_original_captured_cost" || conflict {
		t.Fatal(cost, basis, conflict)
	}
	legacy.Event.OriginalAccounting.CostNano = nil
	if cost, _, _ := forwardCapturedCost([]ForwardEvent{legacy}); cost != nil {
		t.Fatal("missing raw cost replaced by derived correction")
	}
	legacy.Event.OriginalAccounting.CostNano = &raw
	if cost, _, conflict := forwardCapturedCost([]ForwardEvent{{Origin: "native", Event: cpamp.Event{CostNano: &corrected}}, legacy}); cost != nil || !conflict {
		t.Fatal("conflicting captured costs silently selected")
	}
}

func (forwardShortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestForwardProofBookRevisionAndUnknownScope(t *testing.T) {
	ctx := context.Background()
	s, _ := forwardLedger(t)
	feed, _ := forwardFeed(t)
	book := forwardBook(t, s)
	r := forwardNative(t, "captured-membership", forwardAt)
	r.Native.SourceMembershipID = "captured-membership"
	r.Native.SourceEvidence = &measurement.NativeSource{RequestedAt: r.RequestedAt, Gap: true}
	forwardPut(t, feed, r)
	head := forwardAdvance(t, s, feed)
	pin := forwardSeal(t, s, head, book, 0)
	var saved bytes.Buffer
	if err := s.ExportForwardJSON(ctx, pin, &saved); err != nil {
		t.Fatal(err)
	}
	snapshot := measurement.SourceSnapshot{Provider: "claude", AccountID: r.Attribution.AccountID, MembershipID: "captured-membership", From: forwardAt.Add(-time.Second), Through: forwardAt.Add(time.Hour), HasGap: true, Known: false, Reasons: []string{"incomplete_account_membership"}}
	q := ForwardSeal{Head: head, BookDigest: book, At: forwardAt.Add(time.Hour), Provenance: ForwardProvenance{Snapshots: []measurement.SourceSnapshot{snapshot}}}
	proofPin, err := s.SealForward(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.ForwardReport(ctx, proofPin)
	if err != nil {
		t.Fatal(err)
	}
	if proofPin.Generation == pin.Generation || report.Capacity != "unavailable" || report.Totals.SelectionValue == nil || report.Totals.SelectionValue.Low != 725e7 || report.Totals.CoverageReasons["incomplete_account_membership"] != 1 || report.Totals.CoverageReasons["source_gap"] != 1 || report.Totals.CoverageReasons["physical_carry_in_unknown"] != 1 {
		t.Fatal(report.Totals)
	}
	// An irrelevant passing scope must never erase the captured scope's omission.
	q.Provenance.Snapshots[0].Known = true
	q.Provenance.Snapshots[0].MembershipID = "other-membership"
	wrongScope, err := s.SealForward(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := s.ForwardReport(ctx, wrongScope)
	if err != nil || wrong.Totals.CoverageReasons["source_membership_snapshot_missing"] != 1 || wrong.Capacity != "unavailable" {
		t.Fatal(wrong, err)
	}
	var b ForwardBook
	if err = s.object(ctx, "book", book, &b); err != nil {
		t.Fatal(err)
	}
	p := b.Book.Prices[r.Attribution.Model]
	p.Prompt = 8
	b.Book.Prices[r.Attribution.Model] = p
	if _, err = s.RegisterForwardBook(ctx, b); !errors.Is(err, ErrForwardConflict) {
		t.Fatal("book ID rewritten", err)
	}
	b.ID = "new-trusted-book"
	newBook, err := s.RegisterForwardBook(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	q.BookDigest = newBook
	repriced, err := s.SealForward(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.ForwardReport(ctx, repriced)
	if err != nil || changed.Totals.SelectionValue.Low != 145e8 {
		t.Fatal(changed, err)
	}
	var replay bytes.Buffer
	if err = s.ExportForwardJSON(ctx, pin, &replay); err != nil || !bytes.Equal(saved.Bytes(), replay.Bytes()) {
		t.Fatal("old knowledge/price replay changed", err)
	}
	// Re-sealing an old identical vector is idempotent without rewinding latest.
	oldAgain := forwardSeal(t, s, head, book, 0)
	status, err := s.Status(ctx)
	if err != nil || oldAgain != pin || status.Latest != repriced.Generation {
		t.Fatal(status, oldAgain, err)
	}
	if err = s.ExportForwardJSON(ctx, pin, forwardShortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if _, err = s.ExportForwardCSV(ctx, pin, 25, "", forwardShortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

func TestForwardProfileSeedCASInvalidDatesAndArchive(t *testing.T) {
	ctx := context.Background()
	s, _ := forwardLedger(t)
	feed, _ := forwardFeed(t)
	book := forwardBook(t, s)
	r := forwardNative(t, "profile-account", forwardAt)
	forwardPut(t, feed, r)
	head := forwardAdvance(t, s, feed)
	// Identical empty content can have a distinct branch revision without rewriting
	// the initial revision or colliding in the content-addressed object index.
	rev, err := s.SeedForwardProfiles(ctx, ForwardProfileConfig{Profiles: []ForwardProfile{}, Bindings: []ForwardBinding{}})
	if err != nil {
		t.Fatal(err)
	}
	ref := ForwardAccountRef(r.SourceID, r.Attribution.Provider, r.Attribution.AccountID)
	p := ForwardProfile{ID: "dated", Provider: "claude", Memberships: []ForwardMembership{{Account: ref, Start: forwardAt.UnixMilli(), End: forwardAt.Add(time.Second).UnixMilli()}}}
	bad := p
	bad.Memberships = append(append([]ForwardMembership{}, p.Memberships...), p.Memberships[0])
	if _, err = s.EditProfiles(ctx, rev, ForwardProfileEdit{Profile: bad}); !errors.Is(err, ErrInput) {
		t.Fatal("overlapping membership", err)
	}
	bad = p
	bad.Memberships = []ForwardMembership{{Account: "unknown", Start: 1}}
	if _, err = s.EditProfiles(ctx, rev, ForwardProfileEdit{Profile: bad}); !errors.Is(err, ErrInput) {
		t.Fatal("unknown account", err)
	}
	rev, err = s.EditProfiles(ctx, rev, ForwardProfileEdit{Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	pin := forwardSeal(t, s, head, book, rev)
	rev, err = s.EditProfiles(ctx, rev, ForwardProfileEdit{ArchiveID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	archived := forwardSeal(t, s, head, book, rev)
	a, err := s.ForwardReport(ctx, archived)
	if err != nil || a.Profiles[p.ID].Rows != 1 {
		t.Fatal("archive erased membership", a, err)
	}
	b, err := s.ForwardReport(ctx, pin)
	if err != nil || b.Profiles[p.ID].Rows != 1 {
		t.Fatal(b, err)
	}
}
