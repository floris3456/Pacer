package measurement_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	_ "github.com/mattn/go-sqlite3"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// SDK wire is the native bridge's output, not the raw provider response. CPA must
// leave ResponseServiceTier empty for absent, invalid or conflicting observations.
func TestClaudeResponseTierNativeForwardValueAndImmutableUnknown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	at := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	ptr := func(n int64) *int64 { return &n }
	wire := pluginapi.UsageRecord{RequestID: "unknown-attempt", Provider: "claude", AuthID: "dummy-auth", AuthIndex: "dummy-index", Model: "claude-opus-5-5", ServiceTier: "auto", RequestedAt: at, Latency: time.Second, AccountingVersion: 2,
		CachePolicy:    &pluginapi.CachePolicyEvidence{Dispatched: true, UpstreamStartedAt: at},
		TokenBreakdown: pluginapi.UsageTokenBreakdown{SchemaVersion: 2, Quality: "complete", TotalTokens: 614, Input: pluginapi.UsageTokenInput{TotalTokens: 610, UncachedTokens: 10, CacheReadTokens: 500, CacheWriteTokens: 100}, Output: pluginapi.UsageTokenOutput{TotalTokens: 4, NonReasoningTokens: 4}},
		Detail:         pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 4, CachedTokens: 500, CacheReadTokens: 500, CacheCreationTokens: 100, TotalTokens: 614, CacheCreation: &pluginapi.CacheCreationUsage{TotalTokens: ptr(100), Ephemeral5mInputTokens: ptr(25), Ephemeral1hInputTokens: ptr(75), UnknownTokens: ptr(0), Quality: "complete"}},
	}
	credential := measurement.NativeCredential{Provider: "claude", AccountID: "dummy-account", CredentialRef: "dummy-credential", AuthID: wire.AuthID, AuthIndex: wire.AuthIndex}
	convert := func(w pluginapi.UsageRecord) measurement.Record {
		t.Helper()
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		r, err := measurement.FromNative(raw, credential, "dummy-producer")
		if err != nil {
			t.Fatal(err)
		}
		if r.Native.RequestedServiceTier != "auto" || r.Native.ResponseServiceTier != w.ResponseServiceTier || *r.Usage.CacheCreate5m != 25 || *r.Usage.CacheCreate1h != 75 || r.Activity.PrimaryPrefix || !r.Activity.Partial {
			t.Fatal("tier conversion changed authority or counters", r)
		}
		return r
	}
	unknown := convert(wire)
	if unknown.Attribution.ServiceTier != "auto" {
		t.Fatal("unknown charged tier was defaulted", unknown.Attribution)
	}
	wire.RequestID = "standard-attempt"
	wire.ResponseServiceTier = "standard"
	standard := convert(wire)
	if standard.Attribution.ServiceTier != "standard" {
		t.Fatal("assigned standard tier not retained", standard.Attribution)
	}
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "native.sqlite")+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	store, err := measurement.Open(ctx, db, measurement.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []measurement.Record{unknown, standard} {
		if _, err = store.PutRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	var oldBody []byte
	if err = db.QueryRow(`SELECT body FROM measurement_records WHERE event_id=?`, unknown.EventID).Scan(&oldBody); err != nil {
		t.Fatal(err)
	}
	advance := func(step func() (bool, error)) {
		t.Helper()
		for range 10 {
			done, e := step()
			if e != nil {
				t.Fatal(e)
			}
			if done {
				return
			}
		}
		t.Fatal("bounded fixture feed did not finish")
	}
	advance(func() (bool, error) { return store.BootstrapReportFeed(ctx, measurement.ReportBudget{Rows: 16}) })
	head, err := store.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := historyreports.OpenForward(ctx, filepath.Join(t.TempDir(), "forward.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	advance(func() (bool, error) {
		return ledger.AdvanceForward(ctx, store, head, measurement.ReportBudget{Rows: 16})
	})
	book, err := ledger.RegisterForwardBook(ctx, historyreports.ForwardBook{ID: "dummy-standard-book", Book: historyreports.ForwardComparisonBook{ID: "dummy-standard-book", Policy: "profile-api-value-v3", Prices: map[string]historyreports.ForwardModelPrice{"claude-opus-5-5": {Prompt: 4, Completion: 20, CacheRead: .2}}}, Qualifications: []historyreports.ForwardPriceQualification{{SourceID: "dummy-producer", Provider: "claude", AccountID: "dummy-account", CredentialRef: "dummy-credential", Model: "claude-opus-5-5"}}})
	if err != nil {
		t.Fatal(err)
	}
	pin, err := ledger.SealForward(ctx, historyreports.ForwardSeal{Head: head, BookDigest: book, At: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []measurement.Record{unknown, standard} {
		proof, e := ledger.ForwardEvidence(ctx, pin, "native-event:"+r.EventID)
		if e != nil {
			t.Fatal(e)
		}
		if r.EventID == unknown.EventID {
			found := false
			for _, issue := range proof.Issues {
				found = found || issue == "charged_tier_unknown"
			}
			if proof.Value != nil || !found {
				t.Fatal("unobserved tier became priced", proof)
			}
		} else if proof.Value == nil || proof.Value.Low != 945000 || proof.Value.High != 945000 {
			t.Fatal("assigned standard independent split valuation", proof)
		}
		if proof.Capacity != "unavailable" {
			t.Fatal("assigned tier opened completeness/capacity", proof)
		}
		t.Logf("assigned=%q issues=%v value=%+v capacity=%s", r.Native.ResponseServiceTier, proof.Issues, proof.Value, proof.Capacity)
	}
	// Preserve the actual old unvalued identity rather than backfilling it with a
	// newly observed tier. The observation belongs to a new physical attempt.
	wire.RequestID = unknown.AttemptID
	if _, err = store.PutRecord(ctx, convert(wire)); !errors.Is(err, measurement.ErrConflict) {
		t.Fatal("old unknown tier rewritten", err)
	}
	var after []byte
	if err = db.QueryRow(`SELECT body FROM measurement_records WHERE event_id=?`, unknown.EventID).Scan(&after); err != nil || !bytes.Equal(oldBody, after) {
		t.Fatal("old native body changed", err)
	}
	afterHead, err := store.ReportFeedHead(ctx)
	if err != nil || afterHead != head {
		t.Fatal("conflicting tier replay altered feed", afterHead, head, err)
	}
}
