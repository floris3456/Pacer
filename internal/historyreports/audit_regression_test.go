package historyreports

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func TestAuditEmptyMembershipRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, path := forwardLedger(t)
	feed, _ := forwardFeed(t)
	r := forwardNative(t, "membership-audit", forwardAt)
	forwardPut(t, feed, r)
	head := forwardAdvance(t, s, feed)
	book := forwardBook(t, s)
	account := ForwardAccountRef(r.SourceID, r.Attribution.Provider, r.Attribution.AccountID)
	config := ForwardProfileConfig{
		Profiles: []ForwardProfile{{ID: "legacy", Provider: "claude"}, {ID: "empty-seed", Provider: "claude", Memberships: []ForwardMembership{}}},
		Bindings: []ForwardBinding{{Profile: "legacy", Account: account}, {Profile: "empty-seed", Account: "other"}},
	}
	rev, err := s.SeedForwardProfiles(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	seed, _, err := s.ProfilesAt(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	if seed.Profiles[1].Memberships == nil {
		t.Error("F1 seed: expected explicit [], got nil (legacy fallback)")
	}
	old := forwardSeal(t, s, head, book, rev)
	var before bytes.Buffer
	if err := s.ExportForwardJSON(ctx, old, &before); err != nil {
		t.Fatal(err)
	}
	rev, err = s.EditProfiles(ctx, rev, ForwardProfileEdit{Profile: ForwardProfile{ID: "legacy", Provider: "claude", Memberships: []ForwardMembership{}}})
	if err != nil {
		t.Fatal(err)
	}
	pin := forwardSeal(t, s, head, book, rev)
	check := func(s *ForwardStore) {
		t.Helper()
		c, _, err := s.ProfilesAt(ctx, rev)
		if err != nil {
			t.Fatal(err)
		}
		if c.Profiles[0].Memberships == nil {
			t.Error("F1 edit: expected explicit [], got nil")
		}
		p, err := s.PinForward(ctx, pin.Generation, ForwardFilter{Profile: "legacy"})
		if err != nil {
			t.Fatal(err)
		}
		selected, err := s.ForwardReport(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if selected.Totals.Rows != 0 {
			t.Errorf("F1 cleared profile: expected 0 rows, got %d", selected.Totals.Rows)
		}
		global, err := s.ForwardReport(ctx, pin)
		if err != nil {
			t.Fatal(err)
		}
		if global.Totals.Rows != 1 || global.Profiles["unassigned"].Rows != 1 {
			t.Errorf("F1 global/unassigned: expected 1/1, got %d/%d", global.Totals.Rows, global.Profiles["unassigned"].Rows)
		}
		var replay bytes.Buffer
		if err = s.ExportForwardJSON(ctx, old, &replay); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before.Bytes(), replay.Bytes()) {
			t.Error("F1 old pin changed")
		}
	}
	check(s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenForward(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(s)
}

func TestAuditBaselineQualification(t *testing.T) {
	ctx := context.Background()
	type specimen struct {
		quality string
		tokens  cpamp.Tokens
		reason  string
	}
	cases := []specimen{
		{quality: "complete"},
		{quality: "partial", reason: "incomplete_accounting"},
		{quality: "", reason: "incomplete_accounting"},
		{quality: "unknown", reason: "incomplete_accounting"},
		{quality: "future-quality", reason: "incomplete_accounting"},
		{quality: "partial", tokens: cpamp.Tokens{Input: 100, Total: 100}, reason: "incomplete_accounting"},
		{quality: "complete", tokens: cpamp.Tokens{Cached: math.MaxInt64, CacheCreation: math.MaxInt64}, reason: "invalid_counters"},
		{quality: "complete", tokens: cpamp.Tokens{Input: 100, Total: 100}},
	}
	r := forwardFrozenFixture(t, func(db *sql.DB) {
		var raw []byte
		if err := db.QueryRow(`SELECT payload FROM analytics_events WHERE json_extract(payload,'$.provider')='claude' LIMIT 1`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var template cpamp.Event
		if err := json.Unmarshal(raw, &template); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM analytics_events`); err != nil {
			t.Fatal(err)
		}
		for i, sample := range cases {
			e := template
			e.ID = int64(i + 1)
			e.Hash = "audit-baseline-" + string(rune('a'+i))
			e.RequestID = e.Hash
			e.Model, e.ServiceTier = "claude-opus-5-5", ""
			e.AccountingVersion, e.RawInput = 2, sample.tokens.Input
			e.Quality, e.Tokens, e.SourceTokens, e.TokenEvidence = sample.quality, sample.tokens, nil, nil
			e.OriginalAccounting, e.CostNano = nil, nil
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO analytics_events(event_hash,event_id,account,started_ms,completed_ms,payload) VALUES(?,?,?,?,?,?)`, e.Hash, e.ID, e.Account, e.Started, e.Completed, string(raw)); err != nil {
				t.Fatal(err)
			}
		}
	})
	s, _ := forwardLedger(t)
	feed, _ := forwardFeed(t)
	head := forwardAdvance(t, s, feed)
	base, err := s.PrepareForwardBase(ctx, r, measurement.ReportBudget{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.ReadBaselineEvents(ctx, "", measurement.ReportBudget{})
	if err != nil || len(page.Events) != len(cases) {
		t.Fatal(len(page.Events), err)
	}
	e := page.Events[0]
	origin, err := s.RegisterForwardOrigins(ctx, ForwardOrigins{BaseDigest: base, Certificates: []ForwardOriginCertificate{{LegacyAccount: e.Event.Account, Provider: "claude", SourceID: "audit-producer", NativeAccount: "audit-account", NativeCredentialRef: "audit-credential", EvidenceRef: "synthetic-physical-authority", ProducerVersion: "test-v1", ParserVersion: "test-v1"}}})
	if err != nil {
		t.Fatal(err)
	}
	book, err := s.RegisterForwardBook(ctx, ForwardBook{ID: "audit-baseline", Book: ForwardComparisonBook{Policy: "profile-api-value-v3", Prices: map[string]ForwardModelPrice{"claude-opus-5-5": {Prompt: 4, Completion: 20, CacheRead: .2}}}, Qualifications: []ForwardPriceQualification{{SourceID: e.SourceID, Provider: "claude", Model: "claude-opus-5-5", AccountID: e.Event.Account}}})
	if err != nil {
		t.Fatal(err)
	}
	pin, err := s.SealForward(ctx, ForwardSeal{Head: head, BaseDigest: base, OriginDigest: origin, BookDigest: book, At: forwardAt.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for i, sample := range cases {
		proof, err := s.ForwardEvidence(ctx, pin, page.Events[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		if !proof.IdentityResolved {
			t.Fatal("fixture lacks legitimate identity certificate", proof)
		}
		if sample.reason != "" {
			if proof.Value != nil || !slices.Contains(proof.Issues, sample.reason) {
				t.Errorf("F2 %q/%d: expected nil value and %s; got value=%+v issues=%v", sample.quality, i, sample.reason, proof.Value, proof.Issues)
			}
		} else if proof.Value == nil || proof.Value.Low != sample.tokens.Input*4000 || proof.Value.High != proof.Value.Low {
			t.Errorf("F2 complete control %d: expected exact %d, got %+v", i, sample.tokens.Input*4000, proof.Value)
		}
	}
	report, err := s.ForwardReport(ctx, pin)
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Rows != int64(len(cases)) || report.Totals.UniqueAttempts == nil || *report.Totals.UniqueAttempts != int64(len(cases)) || report.Totals.ValuedCount != 2 || report.Totals.SelectionValue != nil {
		t.Errorf("F2 denominator: expected rows/attempts=%d valued=2 selected=nil, got %+v", len(cases), report.Totals)
	}
}
