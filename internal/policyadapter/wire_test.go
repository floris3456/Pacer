package policyadapter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExactCPAJSONRejectsAmbiguousOrInjectedFields(t *testing.T) {
	f := newFixture(t)
	state := f.enroll()
	raw, err := json.Marshal(f.reference(state, "logical"))
	if err != nil {
		t.Fatal(err)
	}
	valid := string(raw)
	for _, bad := range []string{
		strings.Replace(valid, `"model":`, `"Model":`, 1),
		strings.Replace(valid, `"protocol":1`, `"protocol":null`, 1),
		strings.Replace(valid, `"session_id":"session"`, `"session_id":"session","session_id":"foreign"`, 1),
		strings.TrimSuffix(valid, "}") + `,"caller_identity":"untrusted"}`,
		valid + ` {}`,
		`null`,
		strings.Repeat(" ", MaxRPCBytes+1),
	} {
		_, err = f.a.Dispatch(testContext, pluginabi.MethodCachePolicyResolve, []byte(bad))
		checkError(t, err, 400, "invalid")
	}
	if f.read().ActivePrimary != 0 {
		t.Fatal("malformed native calls created a request guard")
	}
	prepare := f.operation("prepare", state, "1h")
	control, _ := json.Marshal(pluginapi.CachePolicyRequest{Method: "POST", Scope: f.scope, Capability: f.capability, Operation: prepare})
	missing := strings.Replace(string(control), `"expected_revision":2,`, "", 1)
	_, err = f.a.Dispatch(testContext, pluginabi.MethodCachePolicyHandle, []byte(missing))
	checkError(t, err, 400, "invalid")
	badReceipt := strings.TrimSuffix(string(control), "}") + `,"capability":"duplicate"}`
	_, err = f.a.Dispatch(testContext, pluginabi.MethodCachePolicyHandle, []byte(badReceipt))
	checkError(t, err, 400, "invalid")
	_, err = f.a.Dispatch(testContext, pluginabi.MethodRequestComplete, []byte(`{"request_id":"logical","Outcome":"failed"}`))
	checkError(t, err, 400, "invalid")
	// Existing completion fields are PascalCase and extensible Metadata is not authority.
	completion, _ := json.Marshal(pluginapi.RequestCompletion{RequestID: "unmanaged", Outcome: pluginapi.RequestCompletionSucceeded, Metadata: map[string]any{"anything": []any{"value", nil, map[string]any{"new_field": 1}}}})
	if !strings.Contains(string(completion), `"RequestID"`) || strings.Contains(string(completion), `"request_id"`) {
		t.Fatal("completion JSON casing changed")
	}
	if _, err = f.a.Dispatch(testContext, pluginabi.MethodRequestComplete, completion); err != nil {
		t.Fatal(err)
	}
	// Actual native CPA rpcRequestCompletion flattens this callback field beside
	// the public PascalCase fields. It must not be mistaken for a capability.
	policy := f.resolve(f.reference(state, "native-completion"))
	if policy.TTL != "5m" {
		t.Fatal(policy)
	}
	wrapped, _ := json.Marshal(struct {
		pluginapi.RequestCompletion
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}{pluginapi.RequestCompletion{RequestID: "native-completion", Outcome: pluginapi.RequestCompletionRejected}, "native-callback-token"})
	if _, err = f.a.Dispatch(testContext, pluginabi.MethodRequestComplete, wrapped); err != nil {
		t.Fatal(err)
	}
	if f.read().ActivePrimary != 0 || len(f.a.requests) != 0 {
		t.Fatal("native callback wrapper prevented logical settlement")
	}
}

func TestCPAResponseBodyAndUsageTypesKeepExactWireEvidence(t *testing.T) {
	f := newFixture(t)
	state := f.enroll()
	read := pluginapi.CachePolicyRequest{Method: "GET", Scope: f.scope, Capability: f.capability, Operation: pluginapi.CachePolicyOperation{Protocol: 1, Operation: "read", InstallationID: f.scope.InstallationID, SessionID: f.scope.SessionID, ProviderRoute: f.scope.ProviderRoute}}
	resp, err := f.a.HandleCachePolicy(testContext, read)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"body":{"protocol":1`) || strings.Contains(string(raw), f.capability) {
		t.Fatal("body was base64 or contained capability", string(raw))
	}
	p := f.resolve(f.reference(state, "logical"))
	attempt := f.begin(p, "logical", "physical")
	var zero int64
	total := int64(100)
	observed := f.now().Add(time.Second)
	usage := pluginapi.UsageRecord{
		RequestID: "physical", LifecycleRequestID: "logical", Provider: "claude", AccountingVersion: 2,
		CachePolicy:               &pluginapi.CachePolicyEvidence{Selected: &p, AttemptStartedAt: attempt.StartedAt, EffectiveTTL: "5m", BreakpointCount: 2, Dispatched: true, UpstreamStartedAt: observed},
		Detail:                    pluginapi.UsageDetail{CacheCreation: &pluginapi.CacheCreationUsage{TotalTokens: &total, Ephemeral5mInputTokens: &zero, Quality: "partial"}},
		TokenBreakdown:            pluginapi.UsageTokenBreakdown{SchemaVersion: 2, Input: pluginapi.UsageTokenInput{TotalTokens: 100, CacheWriteTokens: 100}},
		ResponseHeadersObservedAt: map[string]time.Time{"Anthropic-Ratelimit-Tokens-Remaining": observed},
	}
	raw, err = json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"RequestID":"physical"`, `"LifecycleRequestID":"logical"`, `"CachePolicy":`, `"attempt_started_at":`, `"selected":`, `"CacheCreation":`, `"ephemeral_5m_input_tokens":0`, `"ResponseHeadersObservedAt":`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("missing CPA usage field %s: %s", field, raw)
		}
	}
	if strings.Contains(string(raw), `"ephemeral_1h_input_tokens"`) || strings.Contains(string(raw), f.capability) {
		t.Fatal("missing split was fabricated as zero, or capability leaked")
	}
	var roundTrip pluginapi.UsageRecord
	if err = json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.CachePolicy.Selected == nil || *roundTrip.CachePolicy.Selected != p || roundTrip.Detail.CacheCreation.Ephemeral1hInputTokens != nil || roundTrip.Detail.CacheCreation.Ephemeral5mInputTokens == nil || !roundTrip.ResponseHeadersObservedAt["Anthropic-Ratelimit-Tokens-Remaining"].Equal(observed) {
		t.Fatal("usage evidence changed through SDK wire types")
	}
	_, err = f.a.Dispatch(testContext, pluginabi.MethodUsageHandle, raw)
	checkError(t, err, 400, "invalid")
	if f.read().ActiveAttempts != 1 || f.read().ActivePrimary != 1 {
		t.Fatal("usage delivery settled a physical/logical request")
	}
}

func TestInvalidConfigurationAndRefusedPolicyControls(t *testing.T) {
	f := newFixture(t)
	for _, opts := range []Options{
		{},
		{Clock: f.now, Routes: []Route{{ProviderRoute: "route", Models: []string{"gpt-6"}, TTLs: []cachepolicy.TTL{cachepolicy.TTL5m}}}},
		{Clock: f.now, Routes: []Route{{ProviderRoute: "route", Models: []string{"claude-*"}, TTLs: []cachepolicy.TTL{cachepolicy.TTL5m}}}},
		{Clock: f.now, Routes: []Route{{ProviderRoute: "route", Models: []string{"plugin/claude-model"}, TTLs: []cachepolicy.TTL{cachepolicy.TTL5m}}}},
		{Clock: f.now, Routes: []Route{{ProviderRoute: "route", Models: []string{"claude-model"}, TTLs: []cachepolicy.TTL{"2h"}}}},
		{Clock: f.now, Routes: []Route{{ProviderRoute: "route", TTLs: []cachepolicy.TTL{cachepolicy.TTL5m}}}},
	} {
		_, err := PolicyOptions(opts)
		checkError(t, err, 400, "invalid")
	}
	cmd := f.prepareExisting()
	request := pluginapi.CachePolicyRequest{Method: "POST", Scope: f.scope, Capability: f.capability, Operation: cmd}
	for _, tc := range []struct {
		capability string
		status     int
		code       string
	}{{"", 403, "scope_denied"}, {"not-base64", 400, "invalid"}} {
		r := request
		r.Capability = tc.capability
		response, err := f.a.HandleCachePolicy(testContext, r)
		if err != nil {
			t.Fatal(err)
		}
		controlState(t, response, tc.status, tc.code)
	}
	r := request
	r.Scope.CallerScope = "client-supplied-key"
	response, err := f.a.HandleCachePolicy(testContext, r)
	if err != nil {
		t.Fatal(err)
	}
	controlState(t, response, 403, "scope_denied")
	r = request
	r.Method = "PUT"
	response, err = f.a.HandleCachePolicy(testContext, r)
	if err != nil {
		t.Fatal(err)
	}
	controlState(t, response, 405, "method_not_allowed")
	pending := f.control("POST", cmd, 200, "")
	legacy := f.commit(pending, "new-checkpoint")
	legacy.CheckpointReceipt = nil
	f.control("POST", legacy, 422, "checkpoint_receipt_required")
	wrong := f.commit(pending, "new-checkpoint")
	wrong.CheckpointReceipt.PreviousCheckpoint = "different"
	f.control("POST", wrong, 409, "checkpoint_required")
	cancel := f.operation("cancel", pending, "5m")
	cancel.TransitionID = pending.Pending.ID
	state := f.control("POST", cancel, 200, "")
	if !state.EnrollmentRequired || !state.WarmingPaused {
		t.Fatal("canceled enrollment became fresh")
	}
}
