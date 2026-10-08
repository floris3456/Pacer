package measurement

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func sourceNativeFixture(t *testing.T) (map[string]any, NativeCredential) {
	t.Helper()
	n, c := nativeFixture()
	n.CachePolicy.Selected.TTL = "5m"
	n.CachePolicy.EffectiveTTL = "5m"
	n.TokenBreakdown.Input.CacheWriteTokens = 100
	n.TokenBreakdown.Input.TotalTokens = 400
	n.TokenBreakdown.TotalTokens = 700
	n.Detail.CacheCreation.TotalTokens = integer(100)
	n.Detail.CacheCreation.Ephemeral5mInputTokens = integer(25)
	n.Detail.CacheCreation.Ephemeral1hInputTokens = integer(75)
	var obj map[string]any
	if err := json.Unmarshal(encodeNative(t, n), &obj); err != nil {
		t.Fatal(err)
	}
	obj["SourceEvidence"] = map[string]any{"scope": map[string]any{"provider": "claude", "auth_id": n.AuthID, "auth_index": n.AuthIndex, "kind": ClaudeHTTPSource, "version": 1}, "manager_incarnation_id": "manager-one", "tracked": true, "requested_at": n.RequestedAt, "upstream_started_at": n.CachePolicy.UpstreamStartedAt.Add(time.Nanosecond), "final_usage_at": n.RequestedAt.Add(n.Latency + time.Microsecond)}
	return obj, c
}

func TestNativeSourceClocksSanitizedAndSelected5mKeepsObserved25And75(t *testing.T) {
	obj, c := sourceNativeFixture(t)
	r, err := FromNative(encodeNative(t, obj), c, "host-installation")
	if err != nil {
		t.Fatal(err)
	}
	if r.Policy.SelectedTTL != TTL5m || counter(r.Usage.CacheCreate5m) != 25 || counter(r.Usage.CacheCreate1h) != 75 || r.Usage.CacheCreate != 100 {
		t.Fatal(r)
	}
	if r.Native.SourceEvidence == nil || r.Native.SourceEvidence.SourceRef != SourceReference("host-installation", SourceScope{Provider: "claude", AuthID: c.AuthID, AuthIndex: c.AuthIndex, Kind: ClaudeHTTPSource, Version: 1}) || !r.StartedAt.Equal(base.Add(time.Minute+time.Nanosecond)) || !r.Policy.RequestStartedAt.Equal(r.StartedAt) {
		t.Fatal(r)
	}
	if !r.EndedAt.Equal(base.Add(10*time.Minute+time.Microsecond)) || r.EndTimeBasis != "source_final_usage_observation" {
		t.Fatal(r)
	}
	if strings.Contains(string(encodeNative(t, r)), c.AuthID) {
		t.Fatal("raw qualifier escaped sanitized projection")
	}
	// Source clocks work independently of managed policy, without inventing
	// selected TTL, primary-prefix evidence or complete pricing/counter coverage.
	delete(obj, "CachePolicy")
	r, err = FromNative(encodeNative(t, obj), c, "host-installation")
	if err != nil {
		t.Fatal(err)
	}
	if r.StartTimeBasis != "upstream" || !r.Native.Dispatched || r.Activity.PrimaryPrefix || r.Policy.SelectedTTL != "" || r.Kind != "other" {
		t.Fatal(r)
	}
}

func TestNativeSourceRejectsMismatchedQualifierAndClockEvidence(t *testing.T) {
	for _, mode := range []string{"auth_id", "auth_index", "provider", "kind", "version", "requested", "dispatch_before", "dispatch_after", "final_before", "manager", "partial_tuple", "policy_clock"} {
		t.Run(mode, func(t *testing.T) {
			obj, c := sourceNativeFixture(t)
			e := obj["SourceEvidence"].(map[string]any)
			s := e["scope"].(map[string]any)
			switch mode {
			case "auth_id", "auth_index", "provider", "kind":
				s[mode] = "wrong"
			case "version":
				s[mode] = 2
			case "requested":
				e["requested_at"] = base.Add(time.Second)
			case "dispatch_before":
				e["upstream_started_at"] = base.Add(-time.Second)
			case "dispatch_after":
				e["upstream_started_at"] = base.Add(time.Hour)
			case "final_before":
				e["final_usage_at"] = base.Add(time.Second)
			case "manager":
				e["manager_incarnation_id"] = ""
			case "partial_tuple":
				s["auth_id"] = ""
				obj["AuthID"] = ""
			case "policy_clock":
				e["upstream_started_at"] = base.Add(time.Second)
			}
			if _, err := FromNative(encodeNative(t, obj), c, "host-installation"); !errors.Is(err, ErrUnrecordable) {
				t.Fatal("invalid source accepted", err)
			}
		})
	}
}
