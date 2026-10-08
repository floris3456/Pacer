package historyreports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

// An explicitly modified COPY of the original synthetic SQL fixture supplies
// physical IDs for identity tests. Original golden files/oracles are untouched.
func forwardFrozenFixture(t *testing.T, edits ...func(*sql.DB)) *Reader {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(historicalGolden(t, "historical-fixture.sql"))); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE analytics_events SET payload=json_set(payload,'$.request_id','synthetic-physical-'||event_id)`); err != nil {
		t.Fatal(err)
	}
	for _, edit := range edits {
		edit(db)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := history.Open(filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b := history.Boundary{Schema: "cpamp-usage-v3", SnapshotSHA256: hash(string(raw)), LineageSHA256: strings.Repeat("b", 64), CheckpointSHA256: strings.Repeat("c", 64), HMS: 1790812800000, EventsThrough: 240, ObservationsThrough: 65}
	if _, err = a.Import(ctx, history.ImportOptions{SourcePath: path, Boundary: b, BatchSize: 13}); err != nil {
		t.Fatal(err)
	}
	r, err := Prepare(ctx, a, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestForwardBaselineUnionCutoverAndFrozenPreservation(t *testing.T) {
	ctx := context.Background()
	r := forwardFrozenFixture(t)
	frozenBefore, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := forwardLedger(t)
	feed, _ := forwardFeed(t)
	head := forwardAdvance(t, s, feed)
	base, err := s.PrepareForwardBase(ctx, r, measurement.ReportBudget{Rows: 7})
	if err != nil {
		t.Fatal(err)
	}
	book, config, err := r.ForwardMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.SeedForwardProfiles(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	all := []ForwardEvent{}
	after := ""
	for {
		page, e := r.ReadBaselineEvents(ctx, after, measurement.ReportBudget{Rows: 9})
		if e != nil {
			t.Fatal(e)
		}
		all = append(all, page.Events...)
		after = page.Next
		if page.Done {
			break
		}
	}
	if len(all) != 80 {
		t.Fatal("admission boundary", len(all))
	}
	origins := ForwardOrigins{BaseDigest: base}
	accounts := map[string]bool{}
	qualifications := map[ForwardPriceQualification]bool{}
	maxAt := forwardAt.Add(time.Hour)
	var matched ForwardEvent
	for _, e := range all {
		if e.StartedAt.After(maxAt) {
			maxAt = e.StartedAt.Add(time.Hour)
		}
		if !accounts[e.Event.Account] {
			origins.Certificates = append(origins.Certificates, ForwardOriginCertificate{LegacyAccount: e.Event.Account, Provider: e.Event.Provider, SourceID: "producer", NativeAccount: "native-" + e.Event.Account, NativeCredentialRef: "credential", EvidenceRef: "synthetic-origin-extension-v1", ParserVersion: "synthetic-cpamp-physical-id-v1", ProducerVersion: "synthetic-cpa-v1"})
			accounts[e.Event.Account] = true
		}
		qualifications[ForwardPriceQualification{SourceID: e.SourceID, Provider: e.Event.Provider, Model: e.Event.Model, AccountID: e.Event.Account}] = true
		if matched.ID == "" && e.Event.Provider == "claude" && e.Event.Tokens.Total == e.Event.Tokens.Input+e.Event.Tokens.Output && e.Event.Tokens.Input >= e.Event.Tokens.Cached+e.Event.Tokens.CacheCreation {
			matched = e
		}
	}
	if matched.ID == "" {
		t.Fatal("fixture requires matched Claude event")
	}
	fb := ForwardBook{ID: "historical-and-forward", Book: book}
	for q := range qualifications {
		fb.Qualifications = append(fb.Qualifications, q)
	}
	fb.Qualifications = append(fb.Qualifications, ForwardPriceQualification{SourceID: "producer", Provider: "claude", Model: "claude-opus-5-5", AccountID: "native-" + matched.Event.Account, CredentialRef: "credential"})
	if matched.Event.Model != "claude-opus-5-5" {
		fb.Qualifications = append(fb.Qualifications, ForwardPriceQualification{SourceID: "producer", Provider: "claude", Model: matched.Event.Model, AccountID: "native-" + matched.Event.Account, CredentialRef: "credential"})
	}
	bookID, err := s.RegisterForwardBook(ctx, fb)
	if err != nil {
		t.Fatal(err)
	}
	seal := ForwardSeal{Head: head, BaseDigest: base, BookDigest: bookID, ProfileRevision: rev, At: maxAt}
	unknown, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	unknownReport, err := s.ForwardReport(ctx, unknown)
	if err != nil || unknownReport.Totals.SourceRecords != 80 || unknownReport.Totals.UniqueAttempts != nil || unknownReport.Totals.SelectionValue != nil {
		t.Fatal(unknownReport, err)
	}
	originID, err := s.RegisterForwardOrigins(ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	seal.OriginDigest = originID
	baselinePin, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.ForwardReport(ctx, baselinePin)
	if err != nil || before.Totals.UniqueAttempts == nil || *before.Totals.UniqueAttempts != 80 {
		t.Fatal(before.Totals, err)
	}
	profile := ForwardProfile{ID: "new-forward-profile", Provider: "claude", Name: "Future", Memberships: []ForwardMembership{{Account: matched.Event.Account, Start: r.meta.Boundary.HMS}}}
	rev, err = s.EditProfiles(ctx, rev, ForwardProfileEdit{Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	native := forwardNative(t, "new-after-H", forwardAt)
	native.Attribution.AccountID = "native-" + matched.Event.Account
	forwardPut(t, feed, native)
	head = forwardAdvance(t, s, feed)
	seal.Head = head
	seal.ProfileRevision = rev
	newPin, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	afterReport, err := s.ForwardReport(ctx, newPin)
	if err != nil {
		t.Fatal(err)
	}
	if afterReport.Totals.UniqueAttempts == nil || *afterReport.Totals.UniqueAttempts != 81 || afterReport.Totals.ValuedSubset.Low-before.Totals.ValuedSubset.Low != 725e7 || afterReport.Totals.ValuedSubset.High-before.Totals.ValuedSubset.High != 725e7 || afterReport.Profiles[profile.ID].Rows < 1 {
		t.Fatalf("new usage absent/inflated %+v before=%+v", afterReport.Totals, before.Totals)
	}
	// A physical overlap substitutes once, preserving both immutable source facts.
	overlap := forwardNative(t, matched.AttemptID, matched.StartedAt)
	overlap.Attribution.AccountID = "native-" + matched.Event.Account
	overlap.Attribution.Model = matched.Event.Model
	overlap.Attribution.ServiceTier = matched.Event.ServiceTier
	overlap.EndedAt = matched.EndedAt
	overlap.Success = !matched.Event.Failed
	tokens := matched.Event.Tokens
	overlap.Usage = measurement.Usage{AccountingVersion: "cpa-token-accounting-v2", Quality: "complete", NonCacheInput: tokens.Input - tokens.Cached - tokens.CacheCreation, CacheRead: tokens.Cached, CacheCreate: tokens.CacheCreation, Output: tokens.Output, Reasoning: tokens.Reasoning, TotalInput: &tokens.Input, Total: &tokens.Total}
	forwardPut(t, feed, overlap)
	head = forwardAdvance(t, s, feed)
	seal.Head = head
	overlapPin, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	overlapReport, err := s.ForwardReport(ctx, overlapPin)
	if err != nil || overlapReport.Totals.UniqueAttempts == nil || *overlapReport.Totals.UniqueAttempts != 81 || overlapReport.Totals.SourceRecords != 82 {
		t.Fatal(overlapReport.Totals, err)
	}
	evidence, err := s.ForwardEvidence(ctx, overlapPin, "native-event:"+overlap.EventID)
	if err != nil || len(evidence.Origins) != 2 {
		t.Fatal(evidence, err)
	}
	withoutProof := seal
	withoutProof.OriginDigest = ""
	unresolved, err := s.SealForward(ctx, withoutProof)
	if err != nil {
		t.Fatal(err)
	}
	unresolvedReport, err := s.ForwardReport(ctx, unresolved)
	if err != nil || unresolvedReport.Totals.SourceRecords != 82 || unresolvedReport.Totals.UniqueAttempts != nil || unresolvedReport.Totals.SelectionValue != nil {
		t.Fatal("unproven overlap added or dropped", unresolvedReport.Totals, err)
	}
	// An unmatched late old native row is admitted by the exhaustive identity index,
	// never by receipt time and never into the old historical assumption cohort.
	late := forwardNative(t, "late-old-not-in-baseline", time.UnixMilli(r.meta.Boundary.HMS).Add(-time.Hour))
	late.Attribution.AccountID = native.Attribution.AccountID
	late.Usage.CacheCreate5m = nil
	late.Usage.CacheCreate1h = nil
	forwardPut(t, feed, late)
	head = forwardAdvance(t, s, feed)
	seal.Head = head
	latePin, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	lateEvidence, err := s.ForwardEvidence(ctx, latePin, "native-event:"+late.EventID)
	if err != nil || lateEvidence.Value == nil || lateEvidence.Value.Low != 5e9 || lateEvidence.Value.High != 8e9 || lateEvidence.Value.Tokens.WriteAssumed5m != 0 {
		t.Fatal(lateEvidence, err)
	}
	// Retained-but-unadmitted evidence must not suppress a separately admitted
	// native attempt with that physical ID.
	var archivedRaw []byte
	if err = r.db.QueryRowContext(ctx, `SELECT payload FROM analytics_events WHERE event_id>? LIMIT 1`, r.meta.Pin.Through).Scan(&archivedRaw); err != nil {
		t.Fatal(err)
	}
	var archived struct {
		RequestID string `json:"request_id"`
	}
	if err = json.Unmarshal(archivedRaw, &archived); err != nil || archived.RequestID == "" {
		t.Fatal(err)
	}
	unadmitted := forwardNative(t, archived.RequestID, forwardAt)
	unadmitted.Attribution.AccountID = native.Attribution.AccountID
	forwardPut(t, feed, unadmitted)
	head = forwardAdvance(t, s, feed)
	seal.Head = head
	boundaryPin, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	boundaryReport, err := s.ForwardReport(ctx, boundaryPin)
	if err != nil || boundaryReport.Totals.UniqueAttempts == nil || *boundaryReport.Totals.UniqueAttempts != 83 {
		t.Fatal("unadmitted row suppressed native usage", boundaryReport.Totals, err)
	}
	// Same physical text in another producer namespace is distinct, and cannot
	// borrow this producer's trusted account/price qualification.
	other := native
	other.SourceID = "another-producer"
	other.EventID = "cpa-native:" + forwardDigest([]string{other.SourceID, other.AttemptID})
	forwardPut(t, feed, other)
	head = forwardAdvance(t, s, feed)
	seal.Head = head
	otherPin, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	otherReport, err := s.ForwardReport(ctx, otherPin)
	if err != nil || otherReport.Totals.UniqueAttempts == nil || *otherReport.Totals.UniqueAttempts != 84 || otherReport.Totals.Reasons["missing_price_qualification"] == 0 {
		t.Fatal(otherReport.Totals, err)
	}
	// Conflicting aggregate enrichment retains the proven physical count and both
	// origins, while refusing to choose a cheaper/more recent valuation.
	overlap.Revision = 2
	overlap.Usage.NonCacheInput++
	overlap.Usage.TotalInput = forwardInt(tokens.Input + 1)
	overlap.Usage.Total = forwardInt(tokens.Total + 1)
	forwardPut(t, feed, overlap)
	head = forwardAdvance(t, s, feed)
	seal.Head = head
	conflictPin, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	conflictEvidence, err := s.ForwardEvidence(ctx, conflictPin, "native-event:"+overlap.EventID)
	if err != nil || !conflictEvidence.IdentityResolved || conflictEvidence.Value != nil || len(conflictEvidence.Origins) != 2 {
		t.Fatal(conflictEvidence, err)
	}
	// A matching physical string cannot override a contradictory captured account
	// or credential. Both origins remain separately selectable and unresolved.
	var candidate ForwardEvent
	for _, e := range all {
		if e.ID != matched.ID {
			candidate = e
			break
		}
	}
	wrong := forwardNative(t, candidate.AttemptID, candidate.StartedAt)
	wrong.Attribution.Provider = candidate.Event.Provider
	wrong.Attribution.AccountID = "wrong-captured-account"
	wrong.Attribution.CredentialRef = "wrong-credential"
	forwardPut(t, feed, wrong)
	head = forwardAdvance(t, s, feed)
	seal.Head = head
	wrongPin, err := s.SealForward(ctx, seal)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{candidate.ID, "native-event:" + wrong.EventID} {
		proof, e := s.ForwardEvidence(ctx, wrongPin, id)
		if e != nil || len(proof.Origins) != 1 || proof.IdentityResolved || proof.Value != nil {
			t.Fatal("conflicting qualifier merged or dropped", proof, e)
		}
	}
	wrongReport, err := s.ForwardReport(ctx, wrongPin)
	if err != nil || wrongReport.Totals.UniqueAttempts != nil || wrongReport.Totals.SelectionValue != nil || wrongReport.Totals.Reasons["origin_qualifier_conflict"] != 2 {
		t.Fatal(wrongReport.Totals, err)
	}
	duplicate := origins
	duplicate.Certificates = append(append([]ForwardOriginCertificate{}, origins.Certificates...), origins.Certificates[0])
	if _, err = s.RegisterForwardOrigins(ctx, duplicate); !errors.Is(err, ErrForwardConflict) {
		t.Fatal("ambiguous producer certificate accepted", err)
	}
	oldAgain, err := s.ForwardReport(ctx, baselinePin)
	if err != nil {
		t.Fatal(err)
	}
	oldBytes, _ := json.Marshal(before)
	againBytes, _ := json.Marshal(oldAgain)
	if string(oldBytes) != string(againBytes) {
		t.Fatal("baseline saved pin changed")
	}
	frozenAfter, err := os.ReadFile(r.path)
	if err != nil || hash(string(frozenBefore)) != hash(string(frozenAfter)) {
		t.Fatal("frozen artifact/pins/forecast ledger mutated", err)
	}
	if book.Historical == nil || book.Historical.At != r.meta.Boundary.HMS {
		t.Fatal("historical H changed", book.Historical)
	}
	rebound := fb
	rebound.ID = "invalid-moved-H"
	hcopy := *fb.Book.Historical
	hcopy.At++
	rebound.Book.Historical = &hcopy
	badBook, e := s.RegisterForwardBook(ctx, rebound)
	if e != nil {
		t.Fatal(e)
	}
	badSeal := seal
	badSeal.BookDigest = badBook
	if _, e = s.SealForward(ctx, badSeal); !errors.Is(e, ErrForwardConflict) {
		t.Fatal("book moved baseline H", e)
	}
}

func TestForwardOriginalProfileOwnershipAndDates(t *testing.T) {
	c := ForwardProfileConfig{Profiles: []ForwardProfile{{ID: "first", Provider: "claude"}, {ID: "second", Provider: "claude"}, {ID: "selection", Provider: "claude", Memberships: []ForwardMembership{{Account: "a", Start: 100, End: 200}}}}, Bindings: []ForwardBinding{{Profile: "first", Account: "a", Start: 0}, {Profile: "second", Account: "a", Start: 0}}}
	for _, tt := range []struct {
		at   int64
		want string
	}{{99, "first"}, {100, "first,selection"}, {199, "first,selection"}, {200, "first"}} {
		if got := strings.Join(forwardMemberships(c, "a", "claude", tt.at), ","); got != tt.want {
			t.Fatal(tt, got)
		}
	}
}
