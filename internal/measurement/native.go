package measurement

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

var ErrUnrecordable = errors.New("measurement: unrecordable native usage")

// NativeError contains only a fixed diagnostic code, never source JSON/secrets.
type NativeError struct{ Code string }

func (e *NativeError) Error() string { return ErrUnrecordable.Error() + ": " + e.Code }
func (e *NativeError) Unwrap() error { return ErrUnrecordable }
func nativeError(code string) error  { return &NativeError{Code: code} }

// NativeCredential is an immutable value captured from verified host credential
// metadata for this event. Both wire AuthID/AuthIndex, when supplied, must match.
// Never remap replayed evidence using current mutable inventory. CredentialRef is
// an opaque non-secret reference, not a filename/email/token. The optional token
// fingerprint can additionally bind a per-attempt mapping, not a later rotation.
type NativeCredential struct {
	Provider          string
	AccountID         string
	CredentialRef     string
	AuthID            string
	AuthIndex         string
	AccessTokenSHA256 string
}

// NativeMetadata is a sanitized extension of immutable attempt attribution. It
// retains topology and provenance, not addresses, URLs, user agents, raw AuthID,
// native Source (which may itself contain a key), headers, or failure bodies.
type NativeMetadata struct {
	SourceMembershipID     string        `json:"source_membership_id,omitempty"`
	BillingMappingVerified bool          `json:"billing_mapping_verified,omitempty"`
	SourceEvidence         *NativeSource `json:"source_evidence,omitempty"`
	AccountingVersion      int           `json:"accounting_version"`
	BreakdownVersion       int           `json:"breakdown_version"`
	CacheCreationQuality   string        `json:"cache_creation_quality,omitempty"`
	CredentialBindingRef   string        `json:"credential_binding_ref"`
	AccessTokenRef         string        `json:"access_token_ref,omitempty"`
	AuthIndex              string        `json:"auth_index,omitempty"`
	AuthType               string        `json:"auth_type,omitempty"`
	ExecutorType           string        `json:"executor_type,omitempty"`
	ParentSessionID        string        `json:"parent_session_id,omitempty"`
	SessionID              string        `json:"session_id,omitempty"`
	NodeKind               string        `json:"node_kind,omitempty"`
	IsFork                 bool          `json:"is_fork"`
	IsCompaction           bool          `json:"is_compaction"`
	Generate               bool          `json:"generate"`
	Stream                 bool          `json:"stream"`
	Latency                time.Duration `json:"latency"`
	TTFT                   time.Duration `json:"ttft"`
	RequestedServiceTier   string        `json:"requested_service_tier,omitempty"`
	ResponseServiceTier    string        `json:"response_service_tier,omitempty"`
	ExecutionModel         string        `json:"execution_model,omitempty"`
	ResponseModel          string        `json:"response_model,omitempty"`
	Dispatched             bool          `json:"dispatched"`
	BreakpointCount        int           `json:"breakpoint_count"`
	Diagnostics            []string      `json:"diagnostics,omitempty"`
	// PrefixSnapshot is optional, validated CPA prefix evidence. It is nil when
	// absent or invalid (then Diagnostics has prefix_snapshot_invalid). Absent
	// means omitted, so snapshot-less records keep their existing encoding.
	PrefixSnapshot *PrefixSnapshot `json:"prefix_snapshot,omitempty"`
}

// PrefixSnapshot is CPA's keyed fingerprint of the final-wire cacheable prefix
// for one dispatch. Tags are only comparable within one KeyID; a CPA restart
// rotates the key. It never contains content, tool names, IDs, or URLs.
type PrefixSnapshot struct {
	Version     int              `json:"version"`
	KeyID       string           `json:"key_id"`
	SettingsTag string           `json:"settings_tag"`
	Endpoints   []PrefixEndpoint `json:"endpoints"`
	Automatic   bool             `json:"automatic"`
	Shape       string           `json:"shape"`
	Reasons     []string         `json:"reasons,omitempty"`
}

// PrefixEndpoint is one explicit cache marker, in tools→system→messages order.
type PrefixEndpoint struct {
	Ordinal int    `json:"ordinal"`
	Section string `json:"section"`
	TTL     string `json:"ttl"`
	Tag     string `json:"tag"`
}

// nativePrefixSnapshot and nativePrefixEndpoint are the decoder's view of the
// wire member. They are copied into the exported types only after validation,
// so decoder memory is never aliased by a retained record.
type nativePrefixSnapshot struct {
	Version     int                    `json:"version"`
	KeyID       string                 `json:"key_id"`
	SettingsTag string                 `json:"settings_tag"`
	Endpoints   []nativePrefixEndpoint `json:"endpoints"`
	Automatic   bool                   `json:"automatic"`
	Shape       string                 `json:"shape"`
	Reasons     []string               `json:"reasons"`
}
type nativePrefixEndpoint struct {
	Ordinal int    `json:"ordinal"`
	Section string `json:"section"`
	TTL     string `json:"ttl"`
	Tag     string `json:"tag"`
}

// Fixed vocabularies from CPA's cache_prefix snapshot. Unknown values are
// invalid, so a future CPA shape is never mistaken for a known one.
const maxPrefixEndpoints = 4

var prefixSectionRank = map[string]int{"tools": 0, "system": 1, "messages": 2}
var prefixReasons = map[string]bool{"automatic_marker": true, "typed_tool": true, "hidden_processing": true,
	"thinking": true, "non_text_content": true, "server_tool": true, "no_endpoint": true, "invalid_marker": true}

// decodeNativePrefix decodes and validates one present prefix member. Any JSON
// decode error (wrong type at any depth) is treated as an invalid snapshot, so
// the caller drops the snapshot and keeps the record.
func decodeNativePrefix(raw json.RawMessage) *PrefixSnapshot {
	var w nativePrefixSnapshot
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil
	}
	return validNativePrefix(w)
}

// validNativePrefix returns a fresh, fully validated copy of w, or nil if any
// rule fails. Unknown wire members were already ignored by the decoder.
func validNativePrefix(w nativePrefixSnapshot) *PrefixSnapshot {
	if w.Version != 1 || !lowerHex(w.KeyID, 16) || !lowerHex(w.SettingsTag, 64) {
		return nil
	}
	// Non-nil is required: "endpoints" must be present, though it may be empty.
	if w.Endpoints == nil || len(w.Endpoints) > maxPrefixEndpoints {
		return nil
	}
	if w.Shape != "text_single_pass" && w.Shape != "unsupported" || (w.Shape == "text_single_pass") != (len(w.Reasons) == 0) {
		return nil
	}
	automatic := false
	for i, reason := range w.Reasons {
		// Strictly ascending means sorted and unique.
		if !prefixReasons[reason] || (i > 0 && reason <= w.Reasons[i-1]) {
			return nil
		}
		automatic = automatic || reason == "automatic_marker"
	}
	if automatic != w.Automatic {
		return nil
	}
	if hasNoEndpoint := containsString(w.Reasons, "no_endpoint"); hasNoEndpoint != (len(w.Endpoints) == 0) {
		return nil
	}
	endpoints := make([]PrefixEndpoint, len(w.Endpoints))
	lastRank := 0
	for i, e := range w.Endpoints {
		rank, known := prefixSectionRank[e.Section]
		if e.Ordinal != i || !known || rank < lastRank || (e.TTL != "5m" && e.TTL != "1h") || !lowerHex(e.Tag, 64) {
			return nil
		}
		lastRank = rank
		endpoints[i] = PrefixEndpoint{Ordinal: e.Ordinal, Section: e.Section, TTL: e.TTL, Tag: e.Tag}
	}
	// append onto nil keeps Reasons nil when empty, matching omitempty.
	return &PrefixSnapshot{Version: w.Version, KeyID: w.KeyID, SettingsTag: w.SettingsTag, Endpoints: endpoints,
		Automatic: w.Automatic, Shape: w.Shape, Reasons: append([]string(nil), w.Reasons...)}
}

// lowerHex reports whether s is exactly n lowercase hexadecimal characters.
func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// nativeEnvelope intentionally spells out CPA pluginapi.UsageRecord's PascalCase
// JSON members. It is independent of the SDK ABI/import path (v7/v8). Unsafe fields
// are not decoded. Nested accounting/cache-policy contracts use snake_case tags.
// Pointer counters distinguish malformed/missing data from legitimate zeros.
type nativeEnvelope struct {
	SourceEvidence      *nativeSourceEvidence `json:"SourceEvidence"`
	LifecycleRequestID  string                `json:"LifecycleRequestID"`
	RequestID           string                `json:"RequestID"`
	TraceID             string                `json:"TraceID"`
	Provider            string                `json:"Provider"`
	Model               string                `json:"Model"`
	Alias               string                `json:"Alias"`
	ResponseModel       string                `json:"ResponseModel"`
	APIKey              string                `json:"APIKey"`
	AuthID              string                `json:"AuthID"`
	AuthIndex           string                `json:"AuthIndex"`
	AuthType            string                `json:"AuthType"`
	AccessTokenSHA256   string                `json:"AccessTokenSHA256"`
	ExecutorType        string                `json:"ExecutorType"`
	SessionID           string                `json:"SessionID"`
	ParentSessionID     string                `json:"ParentSessionID"`
	NodeKind            string                `json:"NodeKind"`
	IsFork              bool                  `json:"IsFork"`
	IsCompaction        bool                  `json:"IsCompaction"`
	Generate            bool                  `json:"Generate"`
	Stream              bool                  `json:"Stream"`
	RequestedAt         time.Time             `json:"RequestedAt"`
	Latency             *time.Duration        `json:"Latency"`
	TTFT                time.Duration         `json:"TTFT"`
	Failed              *bool                 `json:"Failed"`
	ServiceTier         string                `json:"ServiceTier"`
	ResponseServiceTier string                `json:"ResponseServiceTier"`
	AccountingVersion   int                   `json:"AccountingVersion"`
	TokenBreakdown      *nativeBreakdown      `json:"TokenBreakdown"`
	Detail              *nativeDetail         `json:"Detail"`
	CachePolicy         *nativePolicyEvidence `json:"CachePolicy"`
}

type nativePolicyEvidence struct {
	// AttemptStartedAt is before policy admission/dispatch. Intentionally unused.
	Selected          *nativeSelectedPolicy `json:"selected"`
	EffectiveTTL      string                `json:"effective_ttl"`
	BreakpointCount   int                   `json:"breakpoint_count"`
	Dispatched        bool                  `json:"dispatched"`
	UpstreamStartedAt time.Time             `json:"upstream_started_at"`
	// Prefix is optional CPA evidence. It is kept raw so a malformed member
	// (wrong JSON type anywhere inside) cannot fail the whole envelope decode.
	// It is decoded separately by decodeNativePrefix.
	Prefix json.RawMessage `json:"prefix"`
}
type nativeSelectedPolicy struct {
	Protocol int `json:"protocol"`
	Scope    struct {
		CallerScope    string `json:"caller_scope"`
		InstallationID string `json:"installation_id"`
		SessionID      string `json:"session_id"`
		ProviderRoute  string `json:"provider_route"`
	} `json:"scope"`
	ContextEpoch string `json:"context_epoch"`
	Revision     int64  `json:"revision"`
	TTL          string `json:"ttl"`
	Kind         string `json:"kind"`
}
type nativeDetail struct {
	CacheCreation       *nativeCacheCreation `json:"CacheCreation"`
	InputTokens         *int64               `json:"InputTokens,omitempty"`
	OutputTokens        *int64               `json:"OutputTokens,omitempty"`
	ReasoningTokens     *int64               `json:"ReasoningTokens,omitempty"`
	CachedTokens        *int64               `json:"CachedTokens,omitempty"`
	CacheReadTokens     *int64               `json:"CacheReadTokens,omitempty"`
	CacheCreationTokens *int64               `json:"CacheCreationTokens,omitempty"`
	TotalTokens         *int64               `json:"TotalTokens,omitempty"`
}
type nativeCacheCreation struct {
	TotalTokens            *int64 `json:"total_tokens,omitempty"`
	Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens,omitempty"`
	UnknownTokens          *int64 `json:"unknown_tokens,omitempty"`
	Quality                string `json:"quality"`
}
type nativeBreakdown struct {
	SchemaVersion int    `json:"schema_version"`
	Quality       string `json:"quality"`
	TotalTokens   *int64 `json:"total_tokens,omitempty"`
	Input         struct {
		TotalTokens      *int64 `json:"total_tokens,omitempty"`
		UncachedTokens   *int64 `json:"uncached_tokens,omitempty"`
		CacheReadTokens  *int64 `json:"cache_read_tokens,omitempty"`
		CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	} `json:"input"`
	Output struct {
		TotalTokens        *int64 `json:"total_tokens,omitempty"`
		NonReasoningTokens *int64 `json:"non_reasoning_tokens,omitempty"`
		ReasoningTokens    *int64 `json:"reasoning_tokens,omitempty"`
	} `json:"output"`
	UnclassifiedTokens *int64 `json:"unclassified_tokens,omitempty"`
}

// FromNative converts one final CPA native callback without I/O, wall-clock reads,
// provider/observer calls, or coverage acknowledgment. sourceID is a stable host
// installation namespace, NOT a process incarnation. Physical RequestID becomes
// AttemptID/ExecutionID; LifecycleRequestID becomes the logical Record.RequestID.
func FromNative(rawJSON []byte, credential NativeCredential, sourceID string) (Record, error) {
	var n nativeEnvelope
	if len(rawJSON) == 0 || len(rawJSON) > 8<<20 {
		return Record{}, nativeError("envelope_size")
	}
	if err := json.Unmarshal(rawJSON, &n); err != nil {
		return Record{}, nativeError("invalid_json_shape")
	}
	if !nativeID(sourceID) || !nativeID(n.RequestID) {
		return Record{}, nativeError("missing_source_or_physical_request_id")
	}
	if n.Failed == nil {
		return Record{}, nativeError("missing_final_outcome")
	}
	if !nativeID(credential.Provider) || !nativeID(credential.AccountID) || !nativeID(credential.CredentialRef) || n.Provider != credential.Provider {
		return Record{}, nativeError("unverified_account_mapping")
	}
	if n.AuthID == "" && n.AuthIndex == "" || n.AuthID != "" && n.AuthID != credential.AuthID || n.AuthIndex != "" && n.AuthIndex != credential.AuthIndex {
		return Record{}, nativeError("credential_identity_mismatch")
	}
	if n.AccessTokenSHA256 != "" && !nativeSHA(n.AccessTokenSHA256) || credential.AccessTokenSHA256 != "" && (!nativeSHA(credential.AccessTokenSHA256) || credential.AccessTokenSHA256 != n.AccessTokenSHA256) {
		return Record{}, nativeError("credential_fingerprint_mismatch")
	}
	if !validTime(n.RequestedAt) || n.Latency == nil || *n.Latency < 0 {
		return Record{}, nativeError("invalid_request_clock")
	}
	end := utc(n.RequestedAt.Add(*n.Latency))
	if !validTime(end) || end.Before(n.RequestedAt) {
		return Record{}, nativeError("invalid_end_clock")
	}
	m := &NativeMetadata{AccountingVersion: n.AccountingVersion, CredentialBindingRef: "sha256:" + key(credential), AuthIndex: n.AuthIndex, AuthType: n.AuthType, ExecutorType: n.ExecutorType,
		ParentSessionID: n.ParentSessionID, SessionID: n.SessionID, NodeKind: n.NodeKind, IsFork: n.IsFork, IsCompaction: n.IsCompaction, Generate: n.Generate, Stream: n.Stream, Latency: *n.Latency, TTFT: n.TTFT,
		RequestedServiceTier: n.ServiceTier, ResponseServiceTier: n.ResponseServiceTier, ExecutionModel: n.Model, ResponseModel: n.ResponseModel}
	if n.AccessTokenSHA256 != "" {
		m.AccessTokenRef = "sha256:" + n.AccessTokenSHA256
	}
	a := Attribution{Provider: n.Provider, AccountID: credential.AccountID, CredentialRef: credential.CredentialRef, SessionID: n.SessionID, Model: n.ResponseModel, RequestedModel: n.Alias, ServiceTier: n.ResponseServiceTier, Client: "cpa-native"}
	if a.Model == "" {
		a.Model = n.Model
	}
	if a.RequestedModel == "" {
		a.RequestedModel = n.Model
	}
	if a.ServiceTier == "" {
		a.ServiceTier = n.ServiceTier
	}
	if apiKey := strings.TrimSpace(n.APIKey); apiKey != "" {
		a.KeyRef = "sha256:" + digest([]byte(apiKey))
		// Matches CPA sdk/cliproxy/session.CallerScope; selected host scope wins.
		a.CallerID = digest([]byte("cli-proxy-api:caller-scope:v1\x00" + apiKey))
	}
	r := Record{EventID: "cpa-native:" + key([]string{sourceID, n.RequestID}), SourceID: sourceID, Revision: 1, RequestID: n.LifecycleRequestID, AttemptID: n.RequestID, ExecutionID: n.RequestID, TraceID: n.TraceID,
		Attribution: a, Kind: "other", RequestedAt: utc(n.RequestedAt), StartedAt: utc(n.RequestedAt), StartTimeBasis: "requested_at_unverified", EndedAt: end, Final: true, Success: !*n.Failed, Native: m, Activity: Activity{Partial: true}}
	if n.IsCompaction || n.NodeKind == "compaction" || n.NodeKind == "summary" {
		r.Kind = "summary"
	} else if n.NodeKind == "title" {
		r.Kind = "title"
	}
	if p := n.CachePolicy; p != nil {
		m.Dispatched, m.BreakpointCount = p.Dispatched, p.BreakpointCount
		// Optional evidence: absent (or JSON null) is silent; anything present but
		// invalid is dropped with a fixed code and never rejects the record.
		if raw := p.Prefix; len(raw) > 0 && string(raw) != "null" {
			if m.PrefixSnapshot = decodeNativePrefix(raw); m.PrefixSnapshot == nil {
				m.Diagnostics = append(m.Diagnostics, "prefix_snapshot_invalid")
			}
		}
		r.Policy.EffectiveTTL = TTL(p.EffectiveTTL)
		if p.Dispatched && !p.UpstreamStartedAt.IsZero() {
			if !validTime(p.UpstreamStartedAt) || p.UpstreamStartedAt.Before(n.RequestedAt) || p.UpstreamStartedAt.After(end) {
				return Record{}, nativeError("invalid_dispatch_clock")
			}
			r.StartedAt = utc(p.UpstreamStartedAt)
			r.StartTimeBasis = "upstream"
			r.Policy.RequestStartedAt = r.StartedAt
		}
		if selected := p.Selected; selected != nil {
			if !validNativePolicy(*selected) {
				return Record{}, nativeError("invalid_selected_policy")
			}
			r.Policy.ContextEpoch, r.Policy.Revision, r.Policy.SelectedTTL, r.Policy.Kind = selected.ContextEpoch, selected.Revision, TTL(selected.TTL), selected.Kind
			r.Attribution.CallerID = selected.Scope.CallerScope
			r.Attribution.InstallationID = selected.Scope.InstallationID
			r.Attribution.SessionID = selected.Scope.SessionID
			r.Attribution.ProviderRoute = selected.Scope.ProviderRoute
			if n.SessionID != "" && n.SessionID != selected.Scope.SessionID {
				m.Diagnostics = append(m.Diagnostics, "native_session_differs_from_selected_scope")
			}
			if r.Kind == "other" && selected.Kind != "auxiliary" {
				r.Kind = selected.Kind
			} else if r.Kind != "other" && selected.Kind != "auxiliary" {
				m.Diagnostics = append(m.Diagnostics, "auxiliary_kind_conflicts_with_selected")
			}
		}
	}
	if err := applyNativeSource(&r, n, credential, sourceID); err != nil {
		return Record{}, err
	}
	if !knownStart(r) {
		m.Diagnostics = append(m.Diagnostics, "unverified_upstream_start")
	}
	r.Usage = nativeUsage(n, m)
	// Marker TTL and aggregate cache tokens do not identify a cache entry or its
	// relationship to a reusable primary prefix. Keep the marker observation in
	// Policy, but Activity has no positive prefix/TTL authority (managed or not).
	// AllWritesCovered stays false, and the unknown remainder stays partial.
	m.Diagnostics = unique(append(m.Diagnostics, "native_prefix_coverage_partial"))
	raw, err := json.Marshal(struct {
		Detail         *nativeDetail    `json:"Detail"`
		TokenBreakdown *nativeBreakdown `json:"TokenBreakdown"`
	}{n.Detail, n.TokenBreakdown})
	if err != nil {
		return Record{}, nativeError("usage_projection_failed")
	}
	r.RawUsage = raw
	r, _, _, err = canonicalRecord(r)
	if err != nil {
		return Record{}, nativeError("invalid_record_metadata")
	}
	return r, nil
}

func nativeID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}
func nativeSHA(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func validNativePolicy(p nativeSelectedPolicy) bool {
	if p.Protocol != 1 || p.Revision < 1 || !nativeSHA(p.Scope.CallerScope) || !nativeID(p.Scope.InstallationID) || !nativeID(p.Scope.SessionID) || !nativeID(p.Scope.ProviderRoute) || !nativeID(p.ContextEpoch) {
		return false
	}
	return p.Kind == "auxiliary" && p.TTL == "" || (p.Kind == "primary" || p.Kind == "warming") && (p.TTL == "5m" || p.TTL == "1h")
}

func nativeUsage(n nativeEnvelope, m *NativeMetadata) Usage {
	u := Usage{AccountingVersion: "cpa-token-accounting-v" + strconv.Itoa(n.AccountingVersion), Quality: "unknown"}
	b := n.TokenBreakdown
	if b != nil {
		m.BreakdownVersion = b.SchemaVersion
		b.Quality = nativeQuality(b.Quality)
	}
	if n.Detail != nil && n.Detail.CacheCreation != nil {
		n.Detail.CacheCreation.Quality = nativeQuality(n.Detail.CacheCreation.Quality)
		m.CacheCreationQuality = n.Detail.CacheCreation.Quality
	}
	if n.AccountingVersion != 2 || b == nil || b.SchemaVersion != 2 {
		m.Diagnostics = append(m.Diagnostics, "unsupported_accounting_version")
		return u
	}
	u.NonCacheInput, u.CacheRead, u.CacheCreate = counter(b.Input.UncachedTokens), counter(b.Input.CacheReadTokens), counter(b.Input.CacheWriteTokens)
	u.Output, u.Reasoning = counter(b.Output.TotalTokens), counter(b.Output.ReasoningTokens)
	u.TotalInput, u.Total = b.Input.TotalTokens, b.TotalTokens
	complete := b.Quality == "complete"
	for _, p := range []*int64{b.TotalTokens, b.Input.TotalTokens, b.Input.UncachedTokens, b.Input.CacheReadTokens, b.Input.CacheWriteTokens, b.Output.TotalTokens, b.Output.NonReasoningTokens, b.Output.ReasoningTokens, b.UnclassifiedTokens} {
		if p == nil {
			complete = false
			m.Diagnostics = append(m.Diagnostics, "missing_normalized_counter")
		}
	}
	if b.Quality != "complete" {
		m.Diagnostics = append(m.Diagnostics, "incomplete_normalized_accounting")
	}
	out, ok := sumChecked(counter(b.Output.NonReasoningTokens), u.Reasoning)
	if !ok || out != u.Output || counter(b.UnclassifiedTokens) != 0 {
		complete = false
		m.Diagnostics = append(m.Diagnostics, "inconsistent_or_unclassified_output")
	}
	if c := n.Detail; c != nil && c.CacheCreation != nil {
		w := c.CacheCreation
		u.CacheCreate5m, u.CacheCreate1h = w.Ephemeral5mInputTokens, w.Ephemeral1hInputTokens
		known, valid := sumChecked(counter(u.CacheCreate5m), counter(u.CacheCreate1h))
		if !valid || known > u.CacheCreate || w.TotalTokens != nil && *w.TotalTokens != u.CacheCreate || w.UnknownTokens != nil && *w.UnknownTokens != u.CacheCreate-known {
			complete = false
			m.Diagnostics = append(m.Diagnostics, "cache_creation_counter_mismatch")
		}
		if w.Quality == "inconsistent" || w.Quality == "unknown" || w.Quality == "complete" && (w.TotalTokens == nil || known != u.CacheCreate) || w.Quality == "partial" && (w.TotalTokens == nil || known >= u.CacheCreate) {
			complete = false
			m.Diagnostics = append(m.Diagnostics, "cache_creation_quality_incomplete")
		}
	}
	if _, valid := normalized(u); !valid {
		complete = false
		m.Diagnostics = append(m.Diagnostics, "invalid_normalized_counters")
	}
	u.Quality = "partial"
	if complete {
		u.Quality = "complete"
	}
	return u
}

// The projection keeps numeric evidence but cannot carry arbitrary text hidden
// in a malformed accounting quality field.
func nativeQuality(s string) string {
	switch s {
	case "complete", "partial", "unclassified", "inconsistent":
		return s
	default:
		return "unknown"
	}
}
