package pacer

import (
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPlacementFitUsesResolvedTTLPriorWithoutCrossingTTLs(t *testing.T) {
	for _, tc := range []struct {
		ttl, want string
		stale     bool
	}{{"1h", "a", false}, {"5m", "b", false}, {"1h", "b", true}} {
		t.Run(tc.ttl+"/"+tc.want, func(t *testing.T) {
			// Eligibility is test-only. No runtime setting can open this gate.
			f := newRoutingFixture(t, "active", true)
			now := f.p.now()
			asOf := now
			if tc.stale {
				asOf = now.Add(-time.Minute)
			}
			for _, id := range []string{"a", "b"} {
				shortValue := 1.0
				if id == "a" {
					shortValue = 2
				}
				report := measurement.Report{Provider: "claude", AccountID: f.p.creds[id].Account, AsOf: asOf, FiveHour: measurement.Ratio{Known: true, USDPerPercent: shortValue}, WarmBurn: measurement.Burn{Bounded: true}}
				for _, sample := range []struct {
					ttl measurement.TTL
					usd int64
				}{{measurement.TTL5m, 10}, {measurement.TTL1h, 90}} {
					report.Stretches = append(report.Stretches, measurement.Stretch{ChatRef: measurement.ChatRef{Provider: "claude", AccountID: report.AccountID, SessionID: "another-compatible-chat"}, TTL: sample.ttl, ClosedAt: now.Add(-time.Hour), PriorEligible: true, Value: measurement.Value{LowNanoUSD: sample.usd * 1e9, HighNanoUSD: sample.usd * 1e9}})
				}
				f.p.measurements[report.AccountID] = report
			}
			r := f.before(t, "ttl-prior", "claude-opus-5-5")
			f.p.selections[r.RequestID] = pluginapi.CachePolicy{TTL: tc.ttl, Kind: "primary", ContextEpoch: "current", Scope: pluginapi.CachePolicyScope{CallerScope: "fixture-caller", InstallationID: "fixture", SessionID: "primary", ProviderRoute: "claude"}}
			choice := f.pick(t, r, "a", "b")
			if !choice.Handled || choice.AuthID != tc.want {
				t.Fatalf("ttl=%s stale=%v choice=%+v", tc.ttl, tc.stale, choice)
			}
		})
	}
}
