// Package measurement persists independent native usage evidence and computes
// API-equivalent capacity and TTL-compatible warm-stretch estimates. It does not
// poll providers, consume CPAMP's queue, or change routing/cache policy.
package measurement

import (
	"encoding/json"
	"errors"
	"time"
)

const ValuationVersion = "pacer-native-value-v1"

var (
	ErrConflict = errors.New("measurement: immutable evidence conflict")
	ErrInvalid  = errors.New("measurement: invalid input")
	ErrLimit    = errors.New("measurement: analysis record limit exceeded")
)

type TTL string

const (
	TTL5m  TTL = "5m"
	TTL1h  TTL = "1h"
	TTL30m TTL = "30m" // Explicit Codex protection behavior, not Claude billing TTL.
)

func (t TTL) Duration() time.Duration {
	switch t {
	case TTL5m:
		return 5 * time.Minute
	case TTL1h:
		return time.Hour
	case TTL30m:
		return 30 * time.Minute
	default:
		return 0
	}
}

// Attribution contains references, never credentials. Model is the actual
// resolved pricing model; RequestedModel is retained but is not a price fallback.
type Attribution struct {
	Provider       string `json:"provider"`
	AccountID      string `json:"account_id"`
	CredentialRef  string `json:"credential_ref,omitempty"`
	CallerID       string `json:"caller_id,omitempty"`
	KeyRef         string `json:"key_ref,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	ProviderRoute  string `json:"provider_route,omitempty"`
	Model          string `json:"model"`
	RequestedModel string `json:"requested_model,omitempty"`
	ServiceTier    string `json:"service_tier,omitempty"`
	Client         string `json:"client,omitempty"`
	NodeID         string `json:"node_id,omitempty"`
	ForkID         string `json:"fork_id,omitempty"`
	CompactionID   string `json:"compaction_id,omitempty"`
}

// PolicySnapshot belongs to this physical attempt, not the chat's current policy.
// AllWritesCovered is executor evidence that there cannot be additional billed
// server-tool/mixed writes. Selection by itself never authorizes write inference.
type PolicySnapshot struct {
	ContextEpoch     string    `json:"context_epoch,omitempty"`
	Revision         int64     `json:"revision,omitempty"`
	SelectedTTL      TTL       `json:"selected_ttl,omitempty"`
	EffectiveTTL     TTL       `json:"effective_ttl,omitempty"`
	RequestStartedAt time.Time `json:"request_started_at"`
	AllWritesCovered bool      `json:"all_writes_covered"`
}

// Usage is an explicit, disjoint normalized domain, not CPA's legacy input
// convention. Output includes reasoning once. Nil split/total counters are
// absent, not zero. Invalid/partial evidence is retained but cannot give an exact
// cost. RawUsage on Record separately preserves the original source evidence.
type Usage struct {
	AccountingVersion string `json:"accounting_version"`
	Quality           string `json:"quality"` // complete | partial | unknown
	NonCacheInput     int64  `json:"noncache_input"`
	CacheRead         int64  `json:"cache_read"`
	CacheCreate       int64  `json:"cache_create"`
	CacheCreate5m     *int64 `json:"cache_create_5m,omitempty"`
	CacheCreate1h     *int64 `json:"cache_create_1h,omitempty"`
	Output            int64  `json:"output"`
	Reasoning         int64  `json:"reasoning"`
	TotalInput        *int64 `json:"total_input,omitempty"`
	Total             *int64 `json:"total,omitempty"`
}

// Activity is positive executor evidence about the relevant primary prefix.
// ProtectionTTL is the longest confirmed relevant prefix TTL, including mixed
// prefixes; it is not inferred from SelectedTTL. Partial coverage remains unknown
// warmth after the known prefix expires. A title/summary cannot qualify.
type Activity struct {
	PrimaryPrefix bool   `json:"primary_prefix"`
	PrefixRef     string `json:"prefix_ref,omitempty"`
	ProtectionTTL TTL    `json:"protection_ttl,omitempty"`
	Partial       bool   `json:"partial"`
}

// Record represents one cumulative usage snapshot for one physical attempt.
// EventID is globally stable across delivery/restart. SourceID namespaces host
// AttemptID values. Retries have different EventIDs/AttemptIDs even if RequestID
// is shared. Higher Revision replaces the current snapshot, never adds a delta.
// Identity, attribution, start time, kind, policy and activity are immutable.
type Record struct {
	EventID             string          `json:"event_id"`
	SourceID            string          `json:"source_id"`
	Revision            int64           `json:"revision"`
	RequestID           string          `json:"request_id,omitempty"`
	AttemptID           string          `json:"attempt_id,omitempty"`
	StreamID            string          `json:"stream_id,omitempty"`
	TraceID             string          `json:"trace_id,omitempty"`
	ExecutionID         string          `json:"execution_id,omitempty"`
	Attribution         Attribution     `json:"attribution"`
	Policy              PolicySnapshot  `json:"policy"`
	Activity            Activity        `json:"activity"`
	Kind                string          `json:"kind"`       // primary | warming | title | summary | other
	StartedAt           time.Time       `json:"started_at"` // Upstream dispatch, never completion.
	EndedAt             time.Time       `json:"ended_at"`
	Final               bool            `json:"final"`
	Success             bool            `json:"success"`
	Usage               Usage           `json:"usage"`
	CapturedCostNanoUSD *int64          `json:"captured_cost_nano_usd,omitempty"`
	RawUsage            json.RawMessage `json:"-"` // Local evidence only; excluded from reports.
}

type StoredRecord struct {
	Record
	ReceivedAt time.Time `json:"received_at"`
}

type PutResult struct {
	Inserted  bool `json:"inserted"`
	Revised   bool `json:"revised"`
	Duplicate bool `json:"duplicate"`
}

// Observation is an immutable source-backed meter reading. Scope is the exact
// quota pool (e.g. all or opus); Models is its explicit resolved-model membership.
// Empty Models means every model in the provider/account. CycleID must come from
// the observation service: elapsed clock time is never cycle closure evidence.
type Observation struct {
	ID          string        `json:"id"`
	Provider    string        `json:"provider"`
	AccountID   string        `json:"account_id"`
	Scope       string        `json:"scope"`
	CycleID     string        `json:"cycle_id"`
	Duration    time.Duration `json:"duration"`
	Models      []string      `json:"models,omitempty"`
	At          time.Time     `json:"at"`
	UsedPercent float64       `json:"used_percent"`
	Source      string        `json:"source"`
	Usable      bool          `json:"usable"`
}

type CycleRef struct {
	Provider  string        `json:"provider"`
	AccountID string        `json:"account_id"`
	Scope     string        `json:"scope"`
	CycleID   string        `json:"cycle_id"`
	Duration  time.Duration `json:"duration"`
}

// Closure is separate from a reading: a reset need not include a final 100% meter.
type Closure struct {
	CycleRef
	At     time.Time `json:"at"` // Observed actual end, including evidenced early reset.
	Source string    `json:"source"`
}

type DeliveryRun struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	Through   time.Time `json:"through"`
	Closed    bool      `json:"closed"`
}

type Gap struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id,omitempty"` // Empty means all accounts.
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Reason    string    `json:"reason"`
}

type Coverage struct {
	Observed bool     `json:"observed"`
	Reasons  []string `json:"reasons,omitempty"`
	// In-memory delivery cannot prove a complete provider/CPA history, even when
	// every observed interval is inside an acknowledged delivery run.
	Delivery string `json:"delivery"`
}

// Query is an explicit analytical read boundary. Call off the scheduler critical
// path and cache the resulting immutable report. Since bounds work and history;
// a truncated first stretch cannot become population evidence.
type Query struct {
	Provider   string
	AccountID  string
	Scope      string
	BookID     string
	Since      time.Time
	AsOf       time.Time
	MaxRecords int // 0 = 100000; over the limit fails, never silently truncates.
}

type ChatRef struct {
	Provider       string `json:"provider"`
	CallerID       string `json:"caller_id"`
	InstallationID string `json:"installation_id"`
	SessionID      string `json:"session_id"`
	ProviderRoute  string `json:"provider_route"`
	AccountID      string `json:"account_id"`
	ContextEpoch   string `json:"context_epoch"`
}

func (r Record) Chat() ChatRef {
	a := r.Attribution
	return ChatRef{a.Provider, a.CallerID, a.InstallationID, a.SessionID, a.ProviderRoute, a.AccountID, r.Policy.ContextEpoch}
}
