package historyreports

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

// Contract §5 (forward-continuity-contract.md): ownership, cutover, runtime gate
// and reopen persistence. The frozen fixture has H=2026-10-01T00:00Z,
// events_through=240, observations_through=65 and 80 admitted events, 45 of
// them on 2026-09-30.
const (
	continuityH  = int64(1790812800000)
	continuityN0 = int64(1790726400000) // 2026-09-30T00:00Z: overlap [N0,H) is one day
)

type continuityPlan struct {
	n0        int64
	plainSeed bool   // revision 1 without frozen provenance
	uncovered string // legacy account left without a certificate
	edits     []func(*sql.DB)
}

type continuityFixture struct {
	r              *Reader
	s              *ForwardStore
	path           string
	feed           *measurement.Store
	base, origin   string
	book           string
	rev            int64
	seed           ForwardProfileSeed
	frozen         ForwardProfileConfig
	all            []ForwardEvent
	firstOverlap   ForwardEvent
	firstPreCutoff ForwardEvent
}

// continuityStore prepares a base, a frozen profile seed, partitioned origin
// certificates [0,n0) and [n0,∞) for every legacy account and a qualified book.
// The cutover record itself is left to each test.
func continuityStore(t *testing.T, plan continuityPlan) continuityFixture {
	t.Helper()
	ctx := context.Background()
	f := continuityFixture{r: forwardFrozenFixture(t, plan.edits...)}
	f.s, f.path = forwardLedger(t)
	f.feed, _ = forwardFeed(t)
	var err error
	if f.base, err = f.s.PrepareForwardBase(ctx, f.r, measurement.ReportBudget{Rows: 11}); err != nil {
		t.Fatal(err)
	}
	comparison, frozen, err := f.r.ForwardMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.frozen = frozen
	if plan.plainSeed {
		f.rev, err = f.s.SeedForwardProfiles(ctx, frozen)
	} else {
		f.rev, f.seed, err = f.s.SeedFrozenForwardProfiles(ctx, f.r, f.base)
	}
	if err != nil {
		t.Fatal(err)
	}
	for after := ""; ; {
		page, e := f.r.ReadBaselineEvents(ctx, after, measurement.ReportBudget{})
		if e != nil {
			t.Fatal(e)
		}
		f.all = append(f.all, page.Events...)
		after = page.Next
		if page.Done {
			break
		}
	}
	origins := ForwardOrigins{BaseDigest: f.base}
	book := ForwardBook{ID: "continuity", Book: comparison}
	seen := map[string]bool{}
	qualified := map[ForwardPriceQualification]bool{}
	qualify := func(q ForwardPriceQualification) {
		if !qualified[q] {
			qualified[q] = true
			book.Qualifications = append(book.Qualifications, q)
		}
	}
	for _, e := range f.all {
		if !seen[e.Event.Account] && e.Event.Account != plan.uncovered {
			for _, window := range [][2]int64{{0, plan.n0}, {plan.n0, 0}} {
				origins.Certificates = append(origins.Certificates, ForwardOriginCertificate{LegacyAccount: e.Event.Account, Provider: e.Event.Provider, SourceID: "producer", NativeAccount: "native-" + e.Event.Account, NativeCredentialRef: "credential", StartMS: window[0], EndMS: window[1], EvidenceRef: "synthetic-continuity-v1", ProducerVersion: "synthetic-cpa-v1", ParserVersion: "synthetic-cpamp-physical-id-v1"})
			}
		}
		seen[e.Event.Account] = true
		qualify(ForwardPriceQualification{SourceID: e.SourceID, Provider: e.Event.Provider, Model: e.Event.Model, AccountID: e.Event.Account})
		for _, model := range []string{e.Event.Model, "claude-opus-5-5"} {
			qualify(ForwardPriceQualification{SourceID: "producer", Provider: e.Event.Provider, Model: model, AccountID: "native-" + e.Event.Account, CredentialRef: "credential"})
		}
		started := e.Event.Started
		if f.firstOverlap.ID == "" && started >= plan.n0 && started < continuityH && e.Event.Provider == "claude" && e.Event.Tokens.Total == e.Event.Tokens.Input+e.Event.Tokens.Output && e.Event.Tokens.Input >= e.Event.Tokens.Cached+e.Event.Tokens.CacheCreation {
			f.firstOverlap = e
		}
		if f.firstPreCutoff.ID == "" && started < plan.n0 {
			f.firstPreCutoff = e
		}
	}
	if f.origin, err = f.s.RegisterForwardOrigins(ctx, origins); err != nil {
		t.Fatal(err)
	}
	if f.book, err = f.s.RegisterForwardBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f continuityFixture) native(t *testing.T, id string, at time.Time, legacy string) measurement.Record {
	t.Helper()
	r := forwardNative(t, id, at)
	r.Attribution.AccountID = "native-" + legacy
	return r
}

// linked replays the same physical attempt as a baseline event (RequestID).
func (f continuityFixture) linked(t *testing.T, m ForwardEvent) measurement.Record {
	t.Helper()
	r := f.native(t, m.AttemptID, m.StartedAt, m.Event.Account)
	r.Attribution.Model = m.Event.Model
	r.Attribution.ServiceTier = m.Event.ServiceTier
	r.EndedAt = m.EndedAt
	r.Success = !m.Event.Failed
	tokens := m.Event.Tokens
	r.Usage = measurement.Usage{AccountingVersion: "cpa-token-accounting-v2", Quality: "complete", NonCacheInput: tokens.Input - tokens.Cached - tokens.CacheCreation, CacheRead: tokens.Cached, CacheCreate: tokens.CacheCreation, Output: tokens.Output, Reasoning: tokens.Reasoning, TotalInput: &tokens.Input, Total: &tokens.Total}
	return r
}

func (f continuityFixture) seal(t *testing.T, at time.Time, natives ...measurement.Record) ForwardPin {
	t.Helper()
	for _, r := range natives {
		forwardPut(t, f.feed, r)
	}
	head := forwardAdvance(t, f.s, f.feed)
	pin, err := f.s.SealForward(context.Background(), ForwardSeal{Head: head, BaseDigest: f.base, OriginDigest: f.origin, BookDigest: f.book, ProfileRevision: f.rev, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

func continuityReport(t *testing.T, s *ForwardStore, generation string, filter ForwardFilter) ForwardReport {
	t.Helper()
	ctx := context.Background()
	pin, err := s.PinForward(ctx, generation, filter)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ForwardReport(ctx, pin)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func continuityExports(t *testing.T, s *ForwardStore, pin ForwardPin) []byte {
	t.Helper()
	ctx := context.Background()
	var out bytes.Buffer
	if err := s.ExportForwardJSON(ctx, pin, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportForwardCSV(ctx, pin, 100, "", &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func continuityEvidence(t *testing.T, s *ForwardStore, pin ForwardPin, id string) (ForwardEvidence, bool) {
	t.Helper()
	e, err := s.ForwardEvidence(context.Background(), pin, id)
	if errors.Is(err, ErrInput) {
		return e, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return e, true
}

// Moves admitted event 8 (account d, 2026-09-30) to H+2h inside events_through.
func continuityPostH(db *sql.DB) {
	at := continuityH + 2*3600000
	if _, err := db.Exec(`UPDATE analytics_events SET started_ms=?,completed_ms=?,payload=json_set(payload,'$.started_at_ms',?,'$.completed_at_ms',?,'$.recorded_at_ms',?) WHERE event_id=8`, at, at+100, at, at+100, at+200); err != nil {
		panic(err)
	}
}

func TestForwardContinuityOwnershipFrozenProfiles(t *testing.T) {
	ctx := context.Background()
	f := continuityStore(t, continuityPlan{n0: continuityN0})
	base, err := f.s.loadForwardBase(ctx, f.base)
	if err != nil {
		t.Fatal(err)
	}
	// Revision 1 is the frozen configuration at H, with recorded provenance.
	seeded, seededDigest, err := f.s.ProfilesAt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := ForwardProfileSeed{BaseDigest: f.base, ArtifactDigest: base.ArtifactDigest, FrozenRevision: f.r.DefaultPin().Revision, ConfigDigest: forwardDigest(f.frozen)}
	if f.rev != 1 || forwardDigest(seeded) != forwardDigest(f.frozen) || f.seed != want {
		t.Errorf("seed revision=%d config=%v provenance=%+v want %+v", f.rev, forwardDigest(seeded) == forwardDigest(f.frozen), f.seed, want)
	}
	if len(f.frozen.Profiles) == 0 {
		t.Fatal("fixture needs frozen profiles")
	}
	frozenFile := func() []byte {
		raw, e := os.ReadFile(f.r.ArtifactPath())
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	frozenExports := func() []byte {
		var out bytes.Buffer
		if e := f.r.ExportJSON(ctx, Query{}, ExportOptions{}, &out); e != nil {
			t.Fatal(e)
		}
		if e := f.r.ExportLegacyJSON(ctx, LegacyQuery{}, LegacyExportOptions{View: "profiles"}, &out); e != nil {
			t.Fatal(e)
		}
		pin, _ := json.Marshal(f.r.DefaultPin())
		return append(out.Bytes(), pin...)
	}
	frozenExports() // the first CPAMP export fills its in-process cache, then reports cache state
	fileBefore, exportsBefore := frozenFile(), frozenExports()
	if _, err = f.s.PrepareForwardCutover(ctx, f.origin, false); err != nil {
		t.Error(err)
	}
	old := f.seal(t, forwardAt.Add(time.Hour), f.native(t, "ownership-live", forwardAt, "a"))
	oldBytes := continuityExports(t, f.s, old)

	// Editing a frozen profile refuses through every library route.
	for _, p := range f.frozen.Profiles {
		edit := ForwardProfile{ID: p.ID, Name: "rewritten", Provider: "claude", Memberships: []ForwardMembership{{Account: "a", Start: continuityN0}}}
		if _, e := f.s.EditProfiles(ctx, 1, ForwardProfileEdit{Profile: edit}); !errors.Is(e, ErrForwardFrozenProfile) {
			t.Errorf("frozen profile %q edited: %v", p.ID, e)
		}
		if _, e := f.s.EditProfiles(ctx, 1, ForwardProfileEdit{ArchiveID: p.ID}); !errors.Is(e, ErrForwardFrozenProfile) {
			t.Errorf("frozen profile %q archived: %v", p.ID, e)
		}
	}
	if _, e := f.s.SeedForwardProfiles(ctx, ForwardProfileConfig{Profiles: []ForwardProfile{}, Bindings: []ForwardBinding{}}); !errors.Is(e, ErrForwardConflict) {
		t.Error("frozen seed replaced", e)
	}
	if _, _, e := f.s.SeedFrozenForwardProfiles(ctx, f.r, f.base); !errors.Is(e, ErrForwardConflict) {
		t.Error("frozen seed repeated", e)
	}
	// New memberships are a forward revision and a new generation.
	rev, err := f.s.EditProfiles(ctx, 1, ForwardProfileEdit{Profile: ForwardProfile{ID: "forward-only", Name: "Forward", Provider: "claude", Memberships: []ForwardMembership{{Account: "a", Start: continuityN0}}}})
	if err != nil || rev != 2 {
		t.Fatal(rev, err)
	}
	f.rev = rev
	edited := f.seal(t, forwardAt.Add(2*time.Hour))
	if r := continuityReport(t, f.s, edited.Generation, ForwardFilter{}); r.Profiles["forward-only"].Rows < 1 || r.Manifest.ProfileRevision != 2 {
		t.Error("forward profile revision not published", r.Profiles["forward-only"], r.Manifest.ProfileRevision)
	}
	if after, afterDigest, e := f.s.ProfilesAt(ctx, 1); e != nil || afterDigest != seededDigest || !reflect.DeepEqual(after, seeded) {
		t.Error("frozen revision 1 changed", e)
	}
	if !bytes.Equal(fileBefore, frozenFile()) || !bytes.Equal(exportsBefore, frozenExports()) {
		t.Error("frozen artifact, pins or legacy exports changed after a forward profile edit")
	}
	if !bytes.Equal(oldBytes, continuityExports(t, f.s, old)) {
		t.Error("old forward pin changed after a forward profile edit")
	}
}

func TestForwardContinuityCutoverCombinedDomain(t *testing.T) {
	ctx := context.Background()
	f := continuityStore(t, continuityPlan{n0: continuityN0, edits: []func(*sql.DB){continuityPostH}})
	if _, err := f.s.PrepareForwardCutover(ctx, f.origin, true); !errors.Is(err, ErrInput) {
		t.Error("coverage gap declared without a gap", err)
	}
	cut, err := f.s.PrepareForwardCutover(ctx, f.origin, false)
	wantCut := ForwardCutover{HMS: continuityH, EventsThrough: 240, ObservationsThrough: 65, N0MS: continuityN0}
	if err != nil || !reflect.DeepEqual(cut, wantCut) {
		t.Error("cutover", cut, err)
	}
	var postH ForwardEvent
	overlapBaseline := 0
	for _, e := range f.all {
		switch {
		case e.Event.Started >= continuityH:
			postH = e
		case e.Event.Started >= continuityN0:
			overlapBaseline++
		}
	}
	if postH.ID == "" || overlapBaseline != 44 || f.firstOverlap.ID == "" {
		t.Fatal("fixture", postH.ID, overlapBaseline, f.firstOverlap.ID)
	}
	m := f.firstOverlap
	linked := f.linked(t, m)
	unlinked := f.native(t, "unlinked-overlap-native", time.UnixMilli(continuityH).Add(-30*time.Minute), "a")
	pre := f.native(t, "pre-cutover-native", time.UnixMilli(continuityN0).Add(-time.Hour), "a")
	live := f.native(t, "after-h-native", forwardAt, "a")
	pin := f.seal(t, forwardAt.Add(time.Hour), linked, unlinked, pre, live)
	check := func(s *ForwardStore) {
		t.Helper()
		r := continuityReport(t, s, pin.Generation, ForwardFilter{})
		if r.Manifest.Cutover == nil || !reflect.DeepEqual(*r.Manifest.Cutover, wantCut) {
			t.Error("manifest cutover", r.Manifest.Cutover)
		}
		if r.Cutover == nil || r.Cutover.ForwardCutover != wantCut || r.Cutover.PreCutoverNative != 1 || r.Cutover.PostHBaseline != 1 {
			t.Errorf("cutover status %+v", r.Cutover)
		}
		// 79 baseline (<H) - 1 replaced + linked + unlinked + after-H natives.
		if r.Totals.Rows != 81 || r.Totals.SourceRecords != 82 || r.Totals.UniqueAttempts == nil || *r.Totals.UniqueAttempts != 81 {
			t.Errorf("combined counts rows=%d records=%d unique=%v", r.Totals.Rows, r.Totals.SourceRecords, r.Totals.UniqueAttempts)
		}
		wantDedup := &ForwardDedup{UnlinkedBaseline: 43, UnlinkedNative: 1}
		if !reflect.DeepEqual(r.Totals.DedupUnverified, wantDedup) || !reflect.DeepEqual(r.Daily["2026-09-30"].DedupUnverified, wantDedup) || r.Daily["2026-09-30"].Rows != 45 {
			t.Errorf("overlap day dedup totals=%+v day=%+v", r.Totals.DedupUnverified, r.Daily["2026-09-30"])
		}
		for day, rows := range map[string]int64{"2026-09-28": 31, "2026-09-29": 4, "2026-10-02": 1} {
			if v := r.Daily[day]; v.Rows != rows || v.DedupUnverified != nil {
				t.Errorf("day %s rows=%d dedup=%+v", day, v.Rows, v.DedupUnverified)
			}
		}
		if _, ok := r.Daily["2026-10-01"]; ok || len(r.Daily) != 4 {
			t.Error("baseline after H reported", r.Daily["2026-10-01"])
		}
		for _, w := range []struct {
			filter ForwardFilter
			rows   int64
			dedup  *ForwardDedup
		}{
			{ForwardFilter{To: time.UnixMilli(continuityN0)}, 35, nil},
			{ForwardFilter{From: time.UnixMilli(continuityN0), To: time.UnixMilli(continuityH)}, 45, wantDedup},
			{ForwardFilter{From: time.UnixMilli(continuityH)}, 1, nil},
		} {
			got := continuityReport(t, s, pin.Generation, w.filter)
			if got.Totals.Rows != w.rows || !reflect.DeepEqual(got.Totals.DedupUnverified, w.dedup) {
				t.Errorf("window %+v rows=%d dedup=%+v", w.filter, got.Totals.Rows, got.Totals.DedupUnverified)
			}
		}
		if e, ok := continuityEvidence(t, s, pin, "native-event:"+linked.EventID); !ok || len(e.Origins) != 2 || e.DedupUnverified != "" || !e.IdentityResolved {
			t.Errorf("linked overlap %+v", e)
		}
		if _, ok := continuityEvidence(t, s, pin, m.ID); ok {
			t.Error("linked baseline counted twice")
		}
		if e, ok := continuityEvidence(t, s, pin, "native-event:"+unlinked.EventID); !ok || e.DedupUnverified != "unlinked_native" || len(e.Origins) != 1 {
			t.Errorf("unlinked native %+v", e)
		}
		for _, b := range f.all {
			if b.ID != m.ID && b.Event.Started >= continuityN0 && b.Event.Started < continuityH {
				if e, ok := continuityEvidence(t, s, pin, b.ID); !ok || e.DedupUnverified != "unlinked_baseline" {
					t.Errorf("unlinked baseline %+v", e)
				}
				break
			}
		}
		if e, ok := continuityEvidence(t, s, pin, f.firstPreCutoff.ID); !ok || e.DedupUnverified != "" {
			t.Errorf("pre-N0 baseline %+v", e)
		}
		if _, ok := continuityEvidence(t, s, pin, "native-event:"+pre.EventID); ok {
			t.Error("native before N0 entered the combined domain")
		}
		if _, ok := continuityEvidence(t, s, pin, postH.ID); ok {
			t.Error("baseline after H entered the combined domain")
		}
		native := continuityReport(t, s, pin.Generation, ForwardFilter{Domain: "native"})
		if native.Totals.Rows != 4 || native.Totals.DedupUnverified != nil {
			t.Errorf("native domain rows=%d dedup=%+v", native.Totals.Rows, native.Totals.DedupUnverified)
		}
		nativePin, err := s.PinForward(ctx, pin.Generation, ForwardFilter{Domain: "native"})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := continuityEvidence(t, s, nativePin, "native-event:"+pre.EventID); !ok {
			t.Error("pre-cutover native hidden from the native domain")
		}
		baseline := continuityReport(t, s, pin.Generation, ForwardFilter{Domain: "baseline"})
		if baseline.Totals.Rows != 79 || baseline.Totals.DedupUnverified != nil {
			t.Errorf("baseline domain rows=%d dedup=%+v", baseline.Totals.Rows, baseline.Totals.DedupUnverified)
		}
	}
	check(f.s)
	saved := continuityExports(t, f.s, pin)
	window, err := f.s.PinForward(ctx, pin.Generation, ForwardFilter{From: time.UnixMilli(continuityN0), To: time.UnixMilli(continuityH)})
	if err != nil {
		t.Fatal(err)
	}
	savedWindow := continuityExports(t, f.s, window)
	// Reopen (offline and prepared-runtime openers) preserves all of it.
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenForward(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	check(s)
	if !bytes.Equal(saved, continuityExports(t, s, pin)) || !bytes.Equal(savedWindow, continuityExports(t, s, window)) {
		t.Error("cutover exports changed on reopen")
	}
	if got, e := s.PreparedCutover(ctx); e != nil || got == nil || !reflect.DeepEqual(*got, wantCut) {
		t.Error("store cutover not persisted", got, e)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	prepared, err := OpenPreparedForward(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	check(prepared)
	if q, e := prepared.ForwardSelection(ctx, f.book); e != nil || q.BaseDigest != f.base || q.OriginDigest != f.origin {
		t.Error("prepared runtime refused the reopened cutover store", q, e)
	}
	if got, e := prepared.GenerationCutover(ctx, pin.Generation); e != nil || got == nil || got.PreCutoverNative != 1 || got.ForwardCutover != wantCut {
		t.Error("generation cutover status", got, e)
	}
}

func TestForwardContinuityCoverageGapDeclaration(t *testing.T) {
	ctx := context.Background()
	n0 := continuityH + 24*3600000
	f := continuityStore(t, continuityPlan{n0: n0})
	if _, err := f.s.PrepareForwardCutover(ctx, f.origin, false); !errors.Is(err, ErrForwardCoverageGap) {
		t.Error("N0 > H prepared without a declaration", err)
	}
	if got, err := f.s.PreparedCutover(ctx); err != nil || got != nil {
		t.Error("refused cutover recorded", got, err)
	}
	if q, err := f.s.ForwardSelection(ctx, f.book); err == nil {
		t.Error("runtime accepted an undeclared gap", q)
	}
	want := ForwardCutover{HMS: continuityH, EventsThrough: 240, ObservationsThrough: 65, N0MS: n0, CoverageGap: &ForwardCoverageGap{FromMS: continuityH, ToMS: n0, Accounts: []string{"a", "b", "c", "d", "forecast", "gap"}}}
	cut, err := f.s.PrepareForwardCutover(ctx, f.origin, true)
	if err != nil || !reflect.DeepEqual(cut, want) {
		t.Errorf("declared gap %+v %v", cut, err)
	}
	if q, err := f.s.ForwardSelection(ctx, f.book); err != nil || q.BaseDigest != f.base {
		t.Error("runtime refused a declared gap", q, err)
	}
	gapNative := f.native(t, "inside-gap", time.UnixMilli(continuityH).Add(time.Hour), "a")
	live := f.native(t, "after-n0", time.UnixMilli(n0).Add(time.Hour), "a")
	pin := f.seal(t, time.UnixMilli(n0).Add(2*time.Hour), gapNative, live)
	r := continuityReport(t, f.s, pin.Generation, ForwardFilter{})
	if r.Manifest.Cutover == nil || !reflect.DeepEqual(*r.Manifest.Cutover, want) || r.Cutover == nil || !reflect.DeepEqual(r.Cutover.CoverageGap, want.CoverageGap) || r.Cutover.PreCutoverNative != 1 {
		t.Errorf("coverage gap not reported manifest=%+v status=%+v", r.Manifest.Cutover, r.Cutover)
	}
	if _, filled := r.Daily["2026-10-01"]; filled || r.Totals.Rows != 81 || r.Totals.DedupUnverified != nil {
		t.Errorf("gap filled or overlap labelled rows=%d dedup=%+v", r.Totals.Rows, r.Totals.DedupUnverified)
	}
	if _, ok := continuityEvidence(t, f.s, pin, "native-event:"+gapNative.EventID); ok {
		t.Error("native inside the gap entered the combined domain")
	}
}

func TestForwardContinuityReadPlanUsesBoundedKeysets(t *testing.T) {
	s, _ := forwardLedger(t)
	args := []any{sql.Named("origin", "origin"), sql.Named("through", 100001), sql.Named("h", 1000000), sql.Named("n0", 10), sql.Named("native_after", 10), sql.Named("native_after_id", ""), sql.Named("after", 1), sql.Named("after_id", "last-id"), sql.Named("to", 1000000), sql.Named("base_to", 1000000)}
	rows, err := s.reader.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+forwardCutoverBatchSQL, args...)
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
	if bytes.Contains([]byte(plan), []byte("USE TEMP B-TREE")) || !bytes.Contains([]byte(plan), []byte("forward_native_page")) || !bytes.Contains([]byte(plan), []byte("forward_base_page")) {
		t.Fatal("cutover batch query must seek both ordered streams without a whole-ledger sort")
	}
}

func TestForwardContinuityRuntimeGate(t *testing.T) {
	ctx := context.Background()
	native, _ := forwardLedger(t)
	book := forwardBook(t, native)
	want, err := native.NativeForwardSelection(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	if got, e := native.ForwardSelection(ctx, book); e != nil || !reflect.DeepEqual(got, want) {
		t.Error("native-only selection changed", got, e)
	}
	ready := continuityStore(t, continuityPlan{n0: continuityN0})
	if q, e := ready.s.ForwardSelection(ctx, ready.book); e == nil {
		t.Error("baseline accepted without a cutover record", q)
	}
	if _, e := ready.s.PrepareForwardCutover(ctx, ready.origin, false); e != nil {
		t.Error(e)
	}
	if q, e := ready.s.ForwardSelection(ctx, ready.book); e != nil || q.BaseDigest != ready.base || q.OriginDigest != ready.origin || q.ProfileRevision != 1 || q.BookDigest != ready.book {
		t.Error("prepared cutover refused", q, e)
	}
	if _, e := ready.s.NativeForwardSelection(ctx, ready.book); e == nil {
		t.Error("native-only selection accepted a baseline")
	}
	for name, plan := range map[string]continuityPlan{
		"profiles-not-from-frozen-digest": {n0: continuityN0, plainSeed: true},
		"origins-missing-events":          {n0: continuityN0, uncovered: "c"},
	} {
		t.Run(name, func(t *testing.T) {
			f := continuityStore(t, plan)
			if _, e := f.s.PrepareForwardCutover(ctx, f.origin, false); e != nil {
				t.Error(e)
			}
			if q, e := f.s.ForwardSelection(ctx, f.book); e == nil {
				t.Error("runtime gate accepted", q)
			}
		})
	}
}
