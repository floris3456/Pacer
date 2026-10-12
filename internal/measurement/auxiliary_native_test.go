package measurement

import (
	"slices"
	"testing"
)

func TestNativeAuxiliaryPreservesUsageAndScopeWithoutCacheWarmth(t *testing.T) {
	for _, kind := range []string{"", "compaction", "title"} {
		for _, failed := range []bool{false, true} {
			n, credential := nativeFixture()
			n.NodeKind, n.Failed = kind, failed
			n.CachePolicy.Selected.Kind, n.CachePolicy.Selected.TTL = "auxiliary", ""
			n.CachePolicy.EffectiveTTL, n.CachePolicy.BreakpointCount = "", 0
			n.TokenBreakdown.TotalTokens = 600
			n.TokenBreakdown.Input.TotalTokens = 300
			n.TokenBreakdown.Input.CacheWriteTokens = 0
			n.Detail.TotalTokens, n.Detail.CacheCreationTokens = 600, 0
			n.Detail.CacheCreation = &cpaCreationFixture{TotalTokens: integer(0), Ephemeral5mInputTokens: integer(0), Ephemeral1hInputTokens: integer(0), UnknownTokens: integer(0), Quality: "complete"}
			r := convertNative(t, n, credential)
			wantKind := kind
			if kind == "compaction" {
				wantKind = "summary"
			} else if kind == "" {
				wantKind = "other"
			}
			if r.Kind != wantKind || r.Success == failed || r.Policy.Kind != "auxiliary" || r.Policy.ContextEpoch != n.CachePolicy.Selected.ContextEpoch || r.Policy.Revision != n.CachePolicy.Selected.Revision {
				t.Fatal("auxiliary identity or outcome lost", r.Kind, r.Policy, r.Success)
			}
			if r.Attribution.CallerID != n.CachePolicy.Selected.Scope.CallerScope || r.Attribution.ProviderRoute != n.CachePolicy.Selected.Scope.ProviderRoute || r.Attribution.SessionID != n.CachePolicy.Selected.Scope.SessionID || r.Attribution.AccountID != credential.AccountID {
				t.Fatal("auxiliary usage attributed to the wrong scope/account", r.Attribution)
			}
			if r.Policy.SelectedTTL != "" || r.Policy.EffectiveTTL != "" || r.Policy.AllWritesCovered || r.Activity.PrimaryPrefix || r.Activity.PrefixRef != "" || r.Activity.ProtectionTTL != "" || !r.Activity.Partial || r.Native.PrefixSnapshot != nil {
				t.Fatal("auxiliary accounting fabricated cache evidence", r.Policy, r.Activity)
			}
			if r.Usage.NonCacheInput != 100 || r.Usage.CacheRead != 200 || r.Usage.CacheCreate != 0 || r.Usage.Output != 300 || counter(r.Usage.Total) != 600 || !ValueRecord(r, DefaultPriceBook()).Exact() {
				t.Fatal("auxiliary tokens/cost were dropped", r.Usage)
			}
			if slices.Contains(r.Native.Diagnostics, "auxiliary_kind_conflicts_with_selected") {
				t.Fatal("legitimate helper kind was reported as conflicting", r.Native.Diagnostics)
			}
		}
	}
}

func TestNativeAuxiliaryDoesNotBroadenOtherPolicyKindsOrTTLs(t *testing.T) {
	for _, pair := range [][2]string{{"auxiliary", "5m"}, {"auxiliary", "1h"}, {"primary", ""}, {"warming", ""}, {"compaction", ""}, {"unknown", ""}} {
		n, credential := nativeFixture()
		n.CachePolicy.Selected.Kind, n.CachePolicy.Selected.TTL = pair[0], pair[1]
		if _, err := FromNative(encodeNative(t, n), credential, "host-installation"); err == nil {
			t.Fatal("invalid selected kind/TTL accepted", pair)
		}
	}
}
