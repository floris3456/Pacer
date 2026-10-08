package historyreports

import (
	"encoding/json"
	"math"
	"sort"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/usageidentity"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func forwardDigest(v any) string { b, _ := json.Marshal(v); return hash(string(b)) }
func forwardPhysical(source, attempt string) string {
	return "attempt:" + forwardDigest([]string{source, attempt})
}
func forwardSum(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}
func forwardUnique(in []string) []string {
	sort.Strings(in)
	out := []string{}
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func convertForward(entry measurement.ReportFeedEntry) ForwardEvent {
	r := entry.Record
	u := r.Usage
	f := ForwardEvent{ID: r.EventID, Origin: "native", SourceID: r.SourceID, AttemptID: r.AttemptID, LifecycleID: r.RequestID, Revision: r.Revision, Sequence: entry.Sequence, StartedAt: r.StartedAt, EndedAt: r.EndedAt, ReceivedAt: entry.ReceivedAt, StartBasis: r.StartTimeBasis, Final: r.Final, Accounting: u.AccountingVersion, Quality: u.Quality, Attribution: r.Attribution, Policy: r.Policy, BodyDigest: entry.BodyDigest, RawDigest: entry.RawDigest, Issues: []string{}}
	if r.AttemptID != "" {
		f.PhysicalKey = forwardPhysical(r.SourceID, r.AttemptID)
	} else {
		f.Issues = append(f.Issues, "physical_identity_missing")
	}
	if strings.HasPrefix(r.EventID, "cpa-native:") && r.EventID != "cpa-native:"+forwardDigest([]string{r.SourceID, r.AttemptID}) {
		f.Issues = append(f.Issues, "physical_identity_conflict")
	}
	if u.AccountingVersion != "cpa-token-accounting-v2" {
		f.Issues = append(f.Issues, "unsupported_accounting")
	}
	if r.Attribution.Provider != "claude" && r.Attribution.Provider != "codex" {
		f.Issues = append(f.Issues, "unsupported_capture_provider")
	}
	if u.Quality != "complete" || !r.Final {
		f.Issues = append(f.Issues, "incomplete_accounting")
	}
	input, valid := forwardSum(u.NonCacheInput, u.CacheRead)
	input, ok := forwardSum(input, u.CacheCreate)
	valid = valid && ok
	total, ok := forwardSum(input, u.Output)
	valid = valid && ok && u.Reasoning >= 0 && u.Reasoning <= u.Output
	w5, w1 := int64(0), int64(0)
	if u.CacheCreate5m != nil {
		w5 = *u.CacheCreate5m
	}
	if u.CacheCreate1h != nil {
		w1 = *u.CacheCreate1h
	}
	w, ok := forwardSum(w5, w1)
	valid = valid && ok && w <= u.CacheCreate && (u.TotalInput == nil || *u.TotalInput == input) && (u.Total == nil || *u.Total == total)
	if !valid {
		f.Issues = append(f.Issues, "invalid_counters")
	}
	if valid && r.Policy.AllWritesCovered && w < u.CacheCreate {
		f.Issues = append(f.Issues, "policy_inference_unsupported")
	}
	e := cpamp.Event{Hash: r.EventID, RequestID: r.AttemptID, Account: r.Attribution.AccountID, Provider: r.Attribution.Provider, Model: r.Attribution.Model, RequestedModel: r.Attribution.RequestedModel, ServiceTier: r.Attribution.ServiceTier, Started: r.StartedAt.UnixMilli(), Completed: r.EndedAt.UnixMilli(), Recorded: entry.ReceivedAt.UnixMilli(), Failed: !r.Success, CostNano: r.CapturedCostNanoUSD, AccountingVersion: 2, RawInput: u.NonCacheInput, Quality: u.Quality, Tokens: cpamp.Tokens{Input: input, Cached: u.CacheRead, CacheCreation: u.CacheCreate, Output: u.Output, Reasoning: u.Reasoning, Total: total}, SourceTokens: &cpamp.SourceTokenDetails{Output: &u.Output, Write5m: u.CacheCreate5m, Write1h: u.CacheCreate1h}}
	if r.Attribution.KeyRef != "" {
		key := strings.TrimPrefix(r.Attribution.KeyRef, "sha256:")
		if r.Attribution.KeyRef == "sha256:"+key && validHash(key) {
			e.APIKeyHash = key
		} else {
			f.Issues = append(f.Issues, "key_unattributed")
		}
	}
	if normalizedForwardTier(e.ServiceTier) == "auto" {
		f.Issues = append(f.Issues, "charged_tier_unknown")
	}
	if r.Native != nil && r.Native.RequestedServiceTier != "" && r.Native.ResponseServiceTier == "" && r.Native.RequestedServiceTier != "standard" && r.Native.RequestedServiceTier != "default" {
		f.Issues = append(f.Issues, "charged_tier_unknown")
	}
	if r.Native != nil {
		e.Streaming = r.Native.Stream
	}
	if r.CapturedCostNanoUSD != nil && *r.CapturedCostNanoUSD < 0 {
		f.Issues = append(f.Issues, "invalid_captured_cost")
		e.CostNano = nil
	}
	f.Event = e
	f.RawInputBasis = "derived_uncached_input"
	f.Usage = &u
	f.Native = r.Native
	return f
}

func valueForward(e ForwardEvent, book ForwardBook) (*ForwardValue, []string) {
	issues := append([]string{}, e.Issues...)
	qualified := false
	for _, q := range book.Qualifications {
		if q.SourceID == e.SourceID && q.Provider == e.Event.Provider && q.Model == usageidentity.AnalyticsModel(e.Event.Model) && q.Route == e.Attribution.ProviderRoute && q.AccountID == e.Event.Account && q.CredentialRef == e.Attribution.CredentialRef {
			qualified = true
			break
		}
	}
	if !qualified {
		issues = append(issues, "missing_price_qualification")
	}
	blocking := false
	for _, s := range issues {
		if s != "key_unattributed" && s != "invalid_captured_cost" && s != "policy_inference_unsupported" {
			blocking = true
		}
	}
	if blocking {
		return nil, forwardUnique(issues)
	}
	v := cpamp.ForwardValue(e.Event, book.Book)
	if v.Invalid > 0 {
		issues = append(issues, "invalid_counters")
	}
	if v.Missing > 0 {
		issues = append(issues, "missing_price")
	}
	if v.Invalid > 0 || v.Missing > 0 {
		return nil, forwardUnique(issues)
	}
	return &v, forwardUnique(issues)
}

// Guard the original unchecked addition without changing valuation or rounding.
func addForwardValue(a *ForwardValue, b ForwardValue) error {
	x, y := *a, b
	pairs := [][2]int64{{x.Low, y.Low}, {x.High, y.High}, {x.Missing, y.Missing}, {x.Invalid, y.Invalid}, {x.Tokens.Input, y.Tokens.Input}, {x.Tokens.Cached, y.Tokens.Cached}, {x.Tokens.Write5m, y.Tokens.Write5m}, {x.Tokens.Write1h, y.Tokens.Write1h}, {x.Tokens.WriteUnknown, y.Tokens.WriteUnknown}, {x.Tokens.WriteAssumed5m, y.Tokens.WriteAssumed5m}, {x.Tokens.Output, y.Tokens.Output}, {x.Tokens.Reasoning, y.Tokens.Reasoning}, {x.Costs.Input, y.Costs.Input}, {x.Costs.Cached, y.Costs.Cached}, {x.Costs.Output, y.Costs.Output}, {x.Costs.WriteLow, y.Costs.WriteLow}, {x.Costs.WriteHigh, y.Costs.WriteHigh}}
	for _, p := range pairs {
		if _, ok := forwardSum(p[0], p[1]); !ok {
			return ErrLimit
		}
	}
	cpamp.ForwardAdd(a, b)
	return nil
}
