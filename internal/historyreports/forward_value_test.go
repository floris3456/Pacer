package historyreports

import (
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func TestForwardConverterCategoriesTiersAndQualifications(t *testing.T) {
	ptr := func(n int64) *int64 { return &n }
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	r := measurement.Record{EventID: "typed-native", SourceID: "one", AttemptID: "physical", Revision: 1, StartedAt: at, EndedAt: at.Add(time.Second), Final: true, Success: true, Attribution: measurement.Attribution{Provider: "claude", AccountID: "account", CredentialRef: "credential", Model: "claude-opus-5-5"}, Usage: measurement.Usage{AccountingVersion: "cpa-token-accounting-v2", Quality: "complete", NonCacheInput: 100, CacheRead: 200, CacheCreate: 1000, CacheCreate5m: ptr(250), CacheCreate1h: ptr(750), Output: 300, Reasoning: 100, TotalInput: ptr(1300), Total: ptr(1600)}}
	book := ForwardBook{ID: "typed", Book: ForwardComparisonBook{Policy: "profile-api-value-v3", Prices: map[string]ForwardModelPrice{r.Attribution.Model: {Prompt: 4, CacheRead: .2, Completion: 20}}}, Qualifications: []ForwardPriceQualification{{SourceID: "one", Provider: "claude", AccountID: "account", CredentialRef: "credential", Model: r.Attribution.Model}}}
	convert := func() ForwardEvent {
		return convertForward(measurement.ReportFeedEntry{StoredRecord: measurement.StoredRecord{Record: r, ReceivedAt: at}})
	}
	f := convert()
	v, issues := valueForward(f, book)
	if v == nil || len(issues) != 0 || v.Low != 13690000 || v.High != 13690000 || f.Event.Tokens.Input != 1300 || f.Event.Tokens.Output != 300 || *f.Event.SourceTokens.Output != 300 {
		t.Fatal(f, v, issues)
	}
	// Removing legacy Gemini's heuristic must not bill reasoning a second time.
	f.Event.Provider = "gemini"
	f.Event.Model = "gemini-fixture"
	gemini := book.Book
	gemini.Prices = map[string]ForwardModelPrice{"gemini-fixture": {Prompt: 4, CacheRead: .2, Completion: 20, CacheCreation: 5}}
	gv := cpamp.ForwardValue(f.Event, gemini)
	if gv.Invalid != 0 || gv.Tokens.Output != 300 || gv.Tokens.Reasoning != 100 {
		t.Fatal(gv)
	}
	r.Usage = measurement.Usage{AccountingVersion: "cpa-token-accounting-v2", Quality: "complete", CacheCreate: 1000000, CacheCreate5m: ptr(250000), CacheCreate1h: ptr(750000)}
	p := book.Book.Prices[r.Attribution.Model]
	p.ContextTiers = []ForwardContextTier{{ThresholdTokens: 1000000, Prompt: 8, PromptConfigured: true}}
	p.ServiceTiers = []ForwardServiceTier{{Mode: "fast", ServiceTier: "priority", Prompt: 8, PromptConfigured: true}}
	book.Book.Prices[r.Attribution.Model] = p
	v, _ = valueForward(convert(), book)
	if v == nil || v.Low != 7250000000 {
		t.Fatal("exact context threshold", v)
	}
	r.Usage.NonCacheInput = 1
	v, _ = valueForward(convert(), book)
	if v == nil || v.Low != 14500008000 {
		t.Fatal("above context threshold", v)
	}
	r.Attribution.ServiceTier = "priority"
	v, _ = valueForward(convert(), book)
	if v == nil || v.Low != 29000016000 {
		t.Fatal("context plus explicit tier", v)
	}
	r.Attribution.CredentialRef = "compatible-gateway"
	v, issues = valueForward(convert(), book)
	if v != nil {
		t.Fatal("borrowed first-party price", issues)
	}
	r.Attribution.CredentialRef = "credential"
	r.Attribution.ServiceTier = ""
	r.Usage.NonCacheInput = 0
	r.Usage.CacheCreate1h = nil
	r.Policy.AllWritesCovered = true
	r.Policy.EffectiveTTL = measurement.TTL1h
	v, issues = valueForward(convert(), book)
	if v == nil || v.Low != 5e9 || v.High != 725e7 || v.Tokens.WriteUnknown != 750000 || len(issues) != 1 || issues[0] != "policy_inference_unsupported" {
		t.Fatal("unknown writes inferred", v, issues)
	}
	r.Usage.CacheCreate = 0
	r.Usage.CacheCreate5m = nil
	r.Policy.AllWritesCovered = false
	v, issues = valueForward(convert(), book)
	if v == nil || v.Low != 0 || v.High != 0 || v.Tokens.WriteUnknown != 0 || len(issues) != 0 {
		t.Fatal("zero writes uncertainty", v, issues)
	}
}
