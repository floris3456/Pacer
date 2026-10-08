package measurement

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Independent wire fixtures transcribed from CPA d314c85 SDK structs. Top-level
// fields deliberately have NO JSON tags, as in pluginapi.UsageRecord/UsageDetail;
// nested policy/accounting fields have the SDK's snake_case tags. No SDK import.
type cpaUsageFixture struct {
	LifecycleRequestID                                                               string
	CachePolicy                                                                      *cpaPolicyEvidenceFixture
	AccountingVersion                                                                int
	TokenBreakdown                                                                   cpaBreakdownFixture
	ClientIP, ResolvedClientIP, XForwardedFor, UserAgent, NodeKind                   string
	IsFork, IsCompaction                                                             bool
	AccessTokenSHA256                                                                string
	RequestID, TraceID, Provider, BaseURL, ExecutorType, Model, Alias, APIKey        string
	SessionID, ParentSessionID, AuthID, AuthIndex, AuthType, Source, ReasoningEffort string
	ServiceTier, ResponseServiceTier, ResponseModel                                  string
	Generate, Stream                                                                 bool
	RequestedAt                                                                      time.Time
	Latency, TTFT                                                                    time.Duration
	Failed                                                                           bool
	Failure                                                                          struct {
		StatusCode int
		Body       string
	}
	Detail                    cpaDetailFixture
	ResponseHeaders           map[string][]string
	ResponseHeadersObservedAt map[string]time.Time
}
type cpaDetailFixture struct {
	CacheCreation                                                                                               *cpaCreationFixture
	InputTokens, OutputTokens, ReasoningTokens, CachedTokens, CacheReadTokens, CacheCreationTokens, TotalTokens int64
}
type cpaCreationFixture struct {
	TotalTokens            *int64 `json:"total_tokens,omitempty"`
	Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens,omitempty"`
	UnknownTokens          *int64 `json:"unknown_tokens,omitempty"`
	Quality                string `json:"quality"`
}
type cpaBreakdownFixture struct {
	SchemaVersion int    `json:"schema_version"`
	Quality       string `json:"quality"`
	TotalTokens   int64  `json:"total_tokens"`
	Input         struct {
		TotalTokens      int64 `json:"total_tokens"`
		UncachedTokens   int64 `json:"uncached_tokens"`
		CacheReadTokens  int64 `json:"cache_read_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input"`
	Output struct {
		TotalTokens        int64 `json:"total_tokens"`
		NonReasoningTokens int64 `json:"non_reasoning_tokens"`
		ReasoningTokens    int64 `json:"reasoning_tokens"`
	} `json:"output"`
	UnclassifiedTokens int64 `json:"unclassified_tokens"`
}
type cpaSelectedFixture struct {
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
type cpaPolicyEvidenceFixture struct {
	AttemptStartedAt  time.Time           `json:"attempt_started_at,omitempty"`
	Selected          *cpaSelectedFixture `json:"selected,omitempty"`
	EffectiveTTL      string              `json:"effective_ttl,omitempty"`
	BreakpointCount   int                 `json:"breakpoint_count"`
	Dispatched        bool                `json:"dispatched"`
	UpstreamStartedAt time.Time           `json:"upstream_started_at,omitempty"`
}

func nativeFixture() (cpaUsageFixture, NativeCredential) {
	p := &cpaSelectedFixture{Protocol: 1, ContextEpoch: "epoch-at-dispatch", Revision: 9, TTL: "1h", Kind: "primary"}
	p.Scope.CallerScope = strings.Repeat("a", 64)
	p.Scope.InstallationID = "installation"
	p.Scope.SessionID = "chat"
	p.Scope.ProviderRoute = "claude-main"
	n := cpaUsageFixture{LifecycleRequestID: "logical-request", RequestID: "physical-request-one", TraceID: "trace123", Provider: "claude", Model: "claude-opus-5-5", Alias: "friendly-model", ResponseModel: "claude-opus-5-5",
		APIKey: "CLIENT-KEY-DO-NOT-RETAIN", Source: "PROVIDER-KEY-DO-NOT-RETAIN", AuthID: "PRIVATE-EMAIL-DO-NOT-RETAIN.json", AuthIndex: "auth-index-1", AuthType: "oauth", AccessTokenSHA256: strings.Repeat("b", 64), ExecutorType: "ClaudeExecutor",
		SessionID: "chat", ParentSessionID: "parent-session", NodeKind: "fork", IsFork: true, Generate: true, Stream: true, ServiceTier: "auto", ResponseServiceTier: "default", RequestedAt: base, Latency: 10 * time.Minute, TTFT: time.Minute,
		ClientIP: "192.0.2.11", ResolvedClientIP: "192.0.2.12", XForwardedFor: "192.0.2.13", UserAgent: "PRIVATE-USER-AGENT", BaseURL: "https://PRIVATE-UPSTREAM.invalid/?token=PRIVATE-URL-KEY",
		ResponseHeaders: map[string][]string{"Authorization": {"SECRET-RESPONSE-HEADER"}}, ResponseHeadersObservedAt: map[string]time.Time{"x-rate-limit": base.Add(5 * time.Minute)}, AccountingVersion: 2}
	n.Failure.Body = "SECRET-FAILURE-BODY"
	n.CachePolicy = &cpaPolicyEvidenceFixture{Selected: p, EffectiveTTL: "1h", BreakpointCount: 3, Dispatched: true, AttemptStartedAt: base.Add(time.Second), UpstreamStartedAt: base.Add(time.Minute)}
	n.TokenBreakdown.SchemaVersion = 2
	n.TokenBreakdown.Quality = "complete"
	n.TokenBreakdown.TotalTokens = 1600
	n.TokenBreakdown.Input.TotalTokens = 1300
	n.TokenBreakdown.Input.UncachedTokens = 100
	n.TokenBreakdown.Input.CacheReadTokens = 200
	n.TokenBreakdown.Input.CacheWriteTokens = 1000
	n.TokenBreakdown.Output.TotalTokens = 300
	n.TokenBreakdown.Output.NonReasoningTokens = 200
	n.TokenBreakdown.Output.ReasoningTokens = 100
	n.Detail = cpaDetailFixture{InputTokens: 100, OutputTokens: 300, ReasoningTokens: 100, CachedTokens: 200, CacheReadTokens: 200, CacheCreationTokens: 1000, TotalTokens: 1600,
		CacheCreation: &cpaCreationFixture{TotalTokens: integer(1000), Ephemeral5mInputTokens: integer(250), Ephemeral1hInputTokens: integer(750), UnknownTokens: integer(0), Quality: "complete"}}
	c := NativeCredential{Provider: n.Provider, AccountID: "account-one", CredentialRef: "credential-one", AuthID: n.AuthID, AuthIndex: n.AuthIndex, AccessTokenSHA256: n.AccessTokenSHA256}
	return n, c
}

func encodeNative(t *testing.T, n any) []byte {
	t.Helper()
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func convertNative(t *testing.T, n cpaUsageFixture, c NativeCredential) Record {
	t.Helper()
	r, err := FromNative(encodeNative(t, n), c, "host-installation")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNativeCPAJSONShapeNormalizedV2AndPolicySnapshot(t *testing.T) {
	n, c := nativeFixture()
	raw := encodeNative(t, n)
	for _, expected := range []string{`"RequestedAt":`, `"AccountingVersion":2`, `"TokenBreakdown":{"schema_version":2`, `"CacheCreation":{"total_tokens":1000`, `"caller_scope":`} {
		if !strings.Contains(string(raw), expected) {
			t.Fatal("fixture does not match CPA wire shape", expected)
		}
	}
	r := convertNative(t, n, c)
	if r.RequestID != n.LifecycleRequestID || r.AttemptID != n.RequestID || r.ExecutionID != n.RequestID || r.StreamID != "" || !r.Final || !r.Success || r.Revision != 1 {
		t.Fatal(r)
	}
	if r.Attribution.AccountID != c.AccountID || r.Attribution.CredentialRef != c.CredentialRef || r.Attribution.CallerID != n.CachePolicy.Selected.Scope.CallerScope || r.Attribution.KeyRef != "sha256:"+digest([]byte(n.APIKey)) {
		t.Fatal(r.Attribution)
	}
	if r.Attribution.Model != n.ResponseModel || r.Attribution.RequestedModel != n.Alias || r.Attribution.ServiceTier != "default" {
		t.Fatal(r.Attribution)
	}
	if r.Policy.ContextEpoch != "epoch-at-dispatch" || r.Policy.Revision != 9 || r.Policy.SelectedTTL != TTL1h || r.Policy.EffectiveTTL != TTL1h || r.Policy.Kind != "primary" || r.Policy.AllWritesCovered {
		t.Fatal(r.Policy)
	}
	if !r.StartedAt.Equal(n.CachePolicy.UpstreamStartedAt) || !r.Policy.RequestStartedAt.Equal(r.StartedAt) || r.StartedAt.Equal(n.CachePolicy.AttemptStartedAt) || !r.RequestedAt.Equal(n.RequestedAt) || !r.EndedAt.Equal(n.RequestedAt.Add(n.Latency)) {
		t.Fatal(r.StartedAt, r.EndedAt, r.Policy)
	}
	if !r.Activity.PrimaryPrefix || !r.Activity.Partial || r.Activity.ProtectionTTL != TTL1h {
		t.Fatal(r.Activity)
	}
	if r.Usage.NonCacheInput != 100 || r.Usage.CacheRead != 200 || r.Usage.CacheCreate != 1000 || r.Usage.Output != 300 || r.Usage.Reasoning != 100 || counter(r.Usage.Total) != 1600 {
		t.Fatal(r.Usage)
	}
	v := ValueRecord(r, DefaultPriceBook())
	if !v.Exact() {
		t.Fatal(v)
	}
	near(t, v.LowUSD(), .01369)
	if r.Native.ParentSessionID != "parent-session" || !r.Native.IsFork || r.Native.NodeKind != "fork" || !r.Native.Stream || r.Native.AccessTokenRef != "sha256:"+n.AccessTokenSHA256 {
		t.Fatal(r.Native)
	}
	// Converter is a frozen value, not a late join against this now-mutated policy.
	n.CachePolicy.Selected.ContextEpoch = "later-epoch"
	n.CachePolicy.Selected.TTL = "5m"
	n.Detail.CacheCreation.Ephemeral5mInputTokens = integer(9999)
	if r.Policy.ContextEpoch != "epoch-at-dispatch" || counter(r.Usage.CacheCreate5m) != 250 {
		t.Fatal("snapshot aliased caller state", r)
	}
}

func TestNativeRestrictedRawProjectionAndSecretFreeErrors(t *testing.T) {
	n, c := nativeFixture()
	var obj map[string]any
	if err := json.Unmarshal(encodeNative(t, n), &obj); err != nil {
		t.Fatal(err)
	}
	obj["AccessToken"] = "RAW-ACCESS-TOKEN"
	obj["RequestHeaders"] = map[string]any{"Authorization": "RAW-REQUEST-AUTH"}
	obj["Detail"].(map[string]any)["Secret"] = "NESTED-DETAIL-SECRET"
	obj["TokenBreakdown"].(map[string]any)["Secret"] = "NESTED-BREAKDOWN-SECRET"
	r, err := FromNative(encodeNative(t, obj), c, "host-installation")
	if err != nil {
		t.Fatal(err)
	}
	retained := string(encodeNative(t, r)) + string(r.RawUsage)
	for _, secret := range []string{n.APIKey, n.Source, n.AuthID, n.ClientIP, n.ResolvedClientIP, n.XForwardedFor, n.UserAgent, n.BaseURL, n.Failure.Body, "SECRET-RESPONSE-HEADER", "RAW-ACCESS-TOKEN", "RAW-REQUEST-AUTH", "NESTED-DETAIL-SECRET", "NESTED-BREAKDOWN-SECRET"} {
		if strings.Contains(retained, secret) {
			t.Fatalf("forbidden source field retained: %q", secret)
		}
	}
	var projected map[string]json.RawMessage
	if err = json.Unmarshal(r.RawUsage, &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected) != 2 || projected["Detail"] == nil || projected["TokenBreakdown"] == nil {
		t.Fatal(string(r.RawUsage))
	}
	obj["Detail"].(map[string]any)["InputTokens"] = "SECRET-INVALID-TYPE"
	_, err = FromNative(encodeNative(t, obj), c, "host-installation")
	if !errors.Is(err, ErrUnrecordable) || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
}

func TestNativeOptionalSplitCountersAndQuality(t *testing.T) {
	for _, test := range []string{"absent", "missing", "explicit-zero", "inconsistent", "wrong-total", "wrong-unknown", "quality-secret"} {
		t.Run(test, func(t *testing.T) {
			n, c := nativeFixture()
			w := n.Detail.CacheCreation
			switch test {
			case "absent":
				n.Detail.CacheCreation = nil
			case "missing":
				w.Ephemeral5mInputTokens = nil
				w.Ephemeral1hInputTokens = nil
				w.UnknownTokens = integer(1000)
				w.Quality = "partial"
			case "explicit-zero":
				w.Ephemeral5mInputTokens = integer(0)
				w.Ephemeral1hInputTokens = nil
				w.UnknownTokens = integer(1000)
				w.Quality = "partial"
			case "inconsistent":
				w.Quality = "inconsistent"
			case "wrong-total":
				w.TotalTokens = integer(1001)
			case "wrong-unknown":
				w.UnknownTokens = integer(100)
			case "quality-secret":
				w.Quality = "QUALITY-SECRET"
			}
			r := convertNative(t, n, c)
			v := ValueRecord(r, DefaultPriceBook())
			if v.Exact() || r.Policy.AllWritesCovered {
				t.Fatal("fabricated exact TTL price", v, r.Policy)
			}
			if test == "absent" || test == "missing" {
				if r.Usage.CacheCreate5m != nil || r.Usage.CacheCreate1h != nil || !v.Bounded() {
					t.Fatal(r.Usage, v)
				}
			}
			if test == "explicit-zero" && (r.Usage.CacheCreate5m == nil || *r.Usage.CacheCreate5m != 0 || r.Usage.CacheCreate1h != nil) {
				t.Fatal(r.Usage)
			}
			if test == "quality-secret" && strings.Contains(string(r.RawUsage)+string(encodeNative(t, r)), "QUALITY-SECRET") {
				t.Fatal("arbitrary quality text retained")
			}
		})
	}
}

func TestNativeUnsupportedAndMalformedAccountingCannotAppearExact(t *testing.T) {
	for _, test := range []string{"v1", "v3", "wrong-schema", "unclassified", "inconsistent", "missing-breakdown", "missing-counter", "output-double-count", "zero-legacy"} {
		t.Run(test, func(t *testing.T) {
			n, c := nativeFixture()
			switch test {
			case "v1":
				n.AccountingVersion = 1
			case "v3":
				n.AccountingVersion = 3
			case "wrong-schema":
				n.TokenBreakdown.SchemaVersion = 3
			case "unclassified":
				n.TokenBreakdown.Quality = "unclassified"
				n.TokenBreakdown.UnclassifiedTokens = 2
			case "inconsistent":
				n.TokenBreakdown.Quality = "inconsistent"
			case "output-double-count":
				n.TokenBreakdown.Output.NonReasoningTokens = 300
			case "zero-legacy":
				n.AccountingVersion = 0
				n.Detail = cpaDetailFixture{}
			}
			var obj map[string]any
			if err := json.Unmarshal(encodeNative(t, n), &obj); err != nil {
				t.Fatal(err)
			}
			if test == "missing-breakdown" {
				delete(obj, "TokenBreakdown")
			}
			if test == "missing-counter" {
				delete(obj["TokenBreakdown"].(map[string]any)["input"].(map[string]any), "uncached_tokens")
			}
			r, err := FromNative(encodeNative(t, obj), c, "host-installation")
			if err != nil {
				t.Fatal(err)
			}
			if r.Usage.Quality == "complete" || ValueRecord(r, DefaultPriceBook()).Exact() || r.Activity.PrimaryPrefix {
				t.Fatal(r.Usage, r.Activity)
			}
			if len(r.RawUsage) == 0 || len(r.Native.Diagnostics) == 0 {
				t.Fatal("lost unsupported source evidence", r)
			}
		})
	}
}

func TestNativeUnmanagedOrPreDispatchTimesAreExplicitlyUnknown(t *testing.T) {
	for _, test := range []string{"unmanaged", "attempt-only", "not-dispatched"} {
		t.Run(test, func(t *testing.T) {
			n, c := nativeFixture()
			switch test {
			case "unmanaged":
				n.CachePolicy = nil
			case "attempt-only":
				n.CachePolicy.UpstreamStartedAt = time.Time{}
			case "not-dispatched":
				n.CachePolicy.Dispatched = false
			}
			r := convertNative(t, n, c)
			if knownStart(r) || r.StartTimeBasis != "requested_at_unverified" || !r.StartedAt.Equal(n.RequestedAt) || !r.RequestedAt.Equal(n.RequestedAt) || !r.Policy.RequestStartedAt.IsZero() || r.Activity.PrimaryPrefix || !r.Activity.Partial {
				t.Fatal(r)
			}
			if test == "unmanaged" && r.Attribution.CallerID != digest([]byte("cli-proxy-api:caller-scope:v1\x00"+n.APIKey)) {
				t.Fatal(r.Attribution)
			}
			s, _ := fixture(t)
			put(t, s, r)
			// Account IDs must actually match the credential mapping for this check.
			q := query(base.Add(6 * time.Hour))
			q.AccountID = c.AccountID
			if err := s.PutObservation(ctx, Observation{ID: "mapped-before", Provider: "claude", AccountID: c.AccountID, Scope: "all", CycleID: "mapped", Duration: 5 * time.Hour, At: base, UsedPercent: 0, Source: "header", Usable: true}); err != nil {
				t.Fatal(err)
			}
			mapped := Observation{ID: "mapped-after", Provider: "claude", AccountID: c.AccountID, Scope: "all", CycleID: "mapped", Duration: 5 * time.Hour, At: base.Add(time.Hour), UsedPercent: 10, Source: "header", Usable: true}
			if err := s.PutObservation(ctx, mapped); err != nil {
				t.Fatal(err)
			}
			closeCycle(t, s, mapped, base.Add(5*time.Hour))
			report := measure(t, s, q)
			if report.FiveHour.Known || len(report.Windows) != 1 || !has(report.Windows[0].Reasons, "unverified_upstream_start") {
				t.Fatal(report.Windows)
			}
		})
	}
}

func TestNativeActivityRequiresActualSuccessfulManagedPrimaryPrefixEvidence(t *testing.T) {
	for _, test := range []string{"warming", "failed", "title", "compaction", "no-breakpoints", "mixed-wire", "no-cache", "unmanaged-wire"} {
		t.Run(test, func(t *testing.T) {
			n, c := nativeFixture()
			switch test {
			case "warming":
				n.CachePolicy.Selected.Kind = "warming"
			case "failed":
				n.Failed = true
			case "title":
				n.NodeKind = "title"
			case "compaction":
				n.IsCompaction = true
			case "no-breakpoints":
				n.CachePolicy.BreakpointCount = 0
			case "mixed-wire":
				n.CachePolicy.EffectiveTTL = "mixed"
			case "unmanaged-wire":
				n.CachePolicy.Selected = nil
			case "no-cache":
				n.TokenBreakdown.Input.CacheReadTokens = 0
				n.TokenBreakdown.Input.CacheWriteTokens = 0
				n.TokenBreakdown.Input.TotalTokens = 100
				n.TokenBreakdown.TotalTokens = 400
				n.Detail.CacheCreation = nil
			}
			r := convertNative(t, n, c)
			if r.Activity.PrimaryPrefix != (test == "warming") || !r.Activity.Partial || r.Policy.AllWritesCovered {
				t.Fatal(r.Activity, r.Policy)
			}
		})
	}
}

func TestNativeUnrecordableIdentityClockAndMappingFailures(t *testing.T) {
	for _, test := range []string{"physical-id", "provider", "auth-id", "auth-index", "no-auth", "fingerprint", "missing-account", "bad-caller", "bad-policy", "before-request", "after-end", "negative-latency", "missing-latency", "missing-outcome"} {
		t.Run(test, func(t *testing.T) {
			n, c := nativeFixture()
			switch test {
			case "physical-id":
				n.RequestID = ""
			case "provider":
				c.Provider = "codex"
			case "auth-id":
				c.AuthID = "another"
			case "auth-index":
				c.AuthIndex = "another"
			case "no-auth":
				n.AuthID = ""
				n.AuthIndex = ""
			case "fingerprint":
				n.AccessTokenSHA256 = "RAW-TOKEN"
			case "missing-account":
				c.AccountID = ""
			case "bad-caller":
				n.CachePolicy.Selected.Scope.CallerScope = "RAW-CALLER-KEY"
			case "bad-policy":
				n.CachePolicy.Selected.Revision = 0
			case "before-request":
				n.CachePolicy.UpstreamStartedAt = base.Add(-time.Second)
			case "after-end":
				n.CachePolicy.UpstreamStartedAt = base.Add(time.Hour)
			case "negative-latency":
				n.Latency = -time.Second
			}
			var obj map[string]any
			if err := json.Unmarshal(encodeNative(t, n), &obj); err != nil {
				t.Fatal(err)
			}
			if test == "missing-latency" {
				delete(obj, "Latency")
			}
			if test == "missing-outcome" {
				delete(obj, "Failed")
			}
			_, err := FromNative(encodeNative(t, obj), c, "host-installation")
			var classified *NativeError
			if !errors.Is(err, ErrUnrecordable) || !errors.As(err, &classified) || classified.Code == "" || strings.Contains(err.Error(), "RAW-") {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeDuplicateDelayRetryPersistenceAndNoCoverageWrite(t *testing.T) {
	n, c := nativeFixture()
	r := convertNative(t, n, c)
	duplicate := convertNative(t, n, c)
	if !reflect.DeepEqual(r, duplicate) {
		t.Fatal("conversion depends on callback time or mutable mapping")
	}
	s, _ := openTest(t, filepath.Join(t.TempDir(), "native.sqlite"))
	put(t, s, r)
	if result, err := s.PutRecord(ctx, duplicate); err != nil || !result.Duplicate {
		t.Fatal(result, err)
	}
	n.RequestID = "physical-request-retry"
	retry := convertNative(t, n, c)
	if retry.EventID == r.EventID || retry.AttemptID == r.AttemptID || retry.RequestID != r.RequestID {
		t.Fatal("logical retry collapsed", r, retry)
	}
	put(t, s, retry)
	got, err := s.Record(ctx, r.EventID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.EndedAt.Equal(base.Add(10*time.Minute)) || !got.ReceivedAt.Equal(base.Add(40*24*time.Hour)) || got.Policy.ContextEpoch != "epoch-at-dispatch" {
		t.Fatal(got)
	}
	q := query(base.Add(time.Hour))
	q.AccountID = c.AccountID
	if measure(t, s, q).Coverage.Observed {
		t.Fatal("native callback invented delivery coverage")
	}
	// An inventory remap is not a legitimate revision or duplicate.
	n.RequestID = r.AttemptID
	c.AccountID = "new-account"
	remapped := convertNative(t, n, c)
	if _, err = s.PutRecord(ctx, remapped); !errors.Is(err, ErrConflict) {
		t.Fatal("remapped historical event", err)
	}
}

func TestNativeLegacyV7MissingAdditiveFieldsRetainsUnknownFacts(t *testing.T) {
	n, c := nativeFixture()
	// This older UsageRecord shape has neither lifecycle/cache policy nor v2
	// fields. Counter conventions cannot be reverse-engineered from these totals.
	legacy := struct {
		RequestID, TraceID, Provider, Model, APIKey, SessionID, AuthID, AuthIndex string
		RequestedAt                                                               time.Time
		Latency                                                                   time.Duration
		Failed                                                                    bool
		Detail                                                                    struct{ InputTokens, OutputTokens, ReasoningTokens, CachedTokens, CacheReadTokens, CacheCreationTokens, TotalTokens int64 }
	}{RequestID: n.RequestID, TraceID: n.TraceID, Provider: n.Provider, Model: n.Model, APIKey: n.APIKey, SessionID: n.SessionID, AuthID: n.AuthID, AuthIndex: n.AuthIndex, RequestedAt: n.RequestedAt, Latency: n.Latency}
	legacy.Detail.InputTokens = 1000
	legacy.Detail.OutputTokens = 100
	legacy.Detail.CachedTokens = 500
	legacy.Detail.CacheReadTokens = 500
	legacy.Detail.TotalTokens = 1100
	c.AccessTokenSHA256 = "" // Older wire has no token fingerprint to cross-check.
	r, err := FromNative(encodeNative(t, legacy), c, "host-installation")
	if err != nil {
		t.Fatal(err)
	}
	if r.RequestID != "" || r.AttemptID != n.RequestID || r.Usage.AccountingVersion != "cpa-token-accounting-v0" || r.Usage.Quality != "unknown" || ValueRecord(r, DefaultPriceBook()).Exact() || knownStart(r) {
		t.Fatal(r)
	}
	if !strings.Contains(string(r.RawUsage), `"InputTokens":1000`) || !strings.Contains(string(r.RawUsage), `"CachedTokens":500`) {
		t.Fatal(string(r.RawUsage))
	}
}

func TestNativeZeroWritesNeedNoSplitAndUnknownStartDoesNotDamageKnownCost(t *testing.T) {
	n, c := nativeFixture()
	n.CachePolicy = nil
	n.Detail.CacheCreation = nil
	n.Detail.CacheCreationTokens = 0
	n.TokenBreakdown.Input.CacheWriteTokens = 0
	n.TokenBreakdown.Input.TotalTokens = 300
	n.TokenBreakdown.TotalTokens = 600
	r := convertNative(t, n, c)
	v := ValueRecord(r, DefaultPriceBook())
	if !v.Exact() || v.UnknownWrites != 0 || r.Usage.CacheCreate5m != nil || r.Usage.CacheCreate1h != nil || knownStart(r) {
		t.Fatal(r.Usage, v)
	}
	near(t, v.LowUSD(), .00644)
}

func TestNativeOptionalRecordExtensionPreservesExistingIdentityEncoding(t *testing.T) {
	r := record("pre-native-record", base.Add(time.Minute), 4)
	_, raw, _, err := canonicalRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err = json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"requested_at", "start_time_basis", "native"} {
		if _, exists := obj[field]; exists {
			t.Fatalf("new zero field %s would change an older record's immutable identity", field)
		}
	}
	var policy map[string]json.RawMessage
	if err = json.Unmarshal(obj["policy"], &policy); err != nil {
		t.Fatal(err)
	}
	if _, exists := policy["kind"]; exists {
		t.Fatal("absent selected kind changed older policy encoding")
	}
}
