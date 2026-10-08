package pacer

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/prefixevidence"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Prefix evidence is optional. Every helper here only copies or projects
// already-persisted Pacer state; none of them changes routing, policy receipts,
// the usage record or the cache-activity tracker.

// prefixFact maps one persisted native record to the prefixevidence fact. The
// caller must have checked that r.Native is non-nil.
func prefixFact(r measurement.Record) prefixevidence.Fact {
	a := r.Attribution
	f := prefixevidence.Fact{
		AttemptID:    r.AttemptID,
		Account:      a.AccountID,
		Caller:       a.CallerID,
		Installation: a.InstallationID,
		Session:      a.SessionID,
		Route:        a.ProviderRoute,
		Epoch:        r.Policy.ContextEpoch,
		Revision:     r.Policy.Revision,
		SelectedTTL:  string(r.Policy.SelectedTTL),
		Kind:         prefixKind(r.Kind),
		Write5m:      copyPrefixCounter(r.Usage.CacheCreate5m),
		Write1h:      copyPrefixCounter(r.Usage.CacheCreate1h),
		Snapshot:     copyPrefixSnapshot(r.Native.PrefixSnapshot),
	}
	f.BindingRef = r.Native.CredentialBindingRef
	f.Dispatched = r.Native.Dispatched
	// A fallback start (requested_at_unverified) is not a measured t0.
	if r.StartTimeBasis == "" || r.StartTimeBasis == "upstream" {
		f.StartedAt = r.StartedAt
	}
	return f
}

// prefixKind maps record kinds onto the two kinds prefixevidence qualifies.
// Every other kind (title, summary, other) is unsupported and never observed.
func prefixKind(kind string) string {
	switch kind {
	case "primary":
		return prefixevidence.KindPrimary
	case "warming":
		return prefixevidence.KindWarming
	default:
		return "other"
	}
}

// copyPrefixCounter keeps absent counters absent; it never turns nil into zero.
func copyPrefixCounter(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// copyPrefixSnapshot deep-copies the snapshot so later changes to the persisted
// record cannot alter an observation. Nil slices stay nil.
func copyPrefixSnapshot(s *measurement.PrefixSnapshot) *prefixevidence.Snapshot {
	if s == nil {
		return nil
	}
	out := &prefixevidence.Snapshot{
		Version:     s.Version,
		KeyID:       s.KeyID,
		SettingsTag: s.SettingsTag,
		Automatic:   s.Automatic,
		Shape:       s.Shape,
	}
	if s.Endpoints != nil {
		out.Endpoints = make([]prefixevidence.Endpoint, len(s.Endpoints))
		for i, e := range s.Endpoints {
			out.Endpoints[i] = prefixevidence.Endpoint{Ordinal: e.Ordinal, Section: e.Section, TTL: e.TTL, Tag: e.Tag}
		}
	}
	if s.Reasons != nil {
		out.Reasons = append([]string(nil), s.Reasons...)
	}
	return out
}

// settlePrefixAttempt forwards the CPA physical finish outcome. Logical
// CompleteRequest settlements are deliberately not forwarded: they are not
// physical outcomes.
func (p *Plugin) settlePrefixAttempt(raw []byte) {
	var request pluginapi.CachePolicyAttemptRequest
	if json.Unmarshal(raw, &request) != nil || request.Operation != "finish" {
		return
	}
	switch outcome := prefixevidence.Outcome(request.Outcome); outcome {
	case prefixevidence.OutcomeSucceeded, prefixevidence.OutcomeFailed, prefixevidence.OutcomeCanceled:
		p.metricError(p.prefixEvidence.Settle(context.Background(), request.AttemptID, outcome))
	}
}

// augmentPrefixReadback adds prefix_cache_evidence to a fresh, authorized GET
// 200 policy state. On any failure the caller's response is returned unchanged.
func (p *Plugin) augmentPrefixReadback(raw []byte, result any) any {
	var request pluginapi.CachePolicyRequest
	if json.Unmarshal(raw, &request) != nil || request.Method != http.MethodGet {
		return result
	}
	response, ok := result.(pluginapi.CachePolicyResponse)
	if !ok || response.StatusCode != http.StatusOK {
		return result
	}
	body, err := p.prefixEvidence.AugmentAuthorized(context.Background(), request.Scope.CallerScope, response.Body)
	if err != nil {
		p.metricError(err)
		return result
	}
	response.Body = body
	return response
}
