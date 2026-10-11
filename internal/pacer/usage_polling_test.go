package pacer

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
)

func TestUsagePollingAPIAndFreshness(t *testing.T) {
	p := usageFixture(t)
	var view historyreports.CompareView
	r := usageCall(p, "POST", "profiles", `{}`)
	if r.StatusCode != 200 || json.Unmarshal(r.Body, &view) != nil {
		t.Fatal(r.StatusCode)
	}
	ref := ""
	for _, prof := range view.Profiles {
		if prof.PollingSeconds > 0 {
			ref = prof.Reference
			break
		}
	}
	if ref == "" {
		t.Fatal("no pollable profile")
	}
	edit := fmt.Sprintf(`{"profile":%q,"seconds":900,"revision":0}`, ref)
	if r = usageCall(p, "PATCH", "polling", edit); r.StatusCode != 200 {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	if r = usageCall(p, "PATCH", "polling", edit); r.StatusCode != 409 {
		t.Fatal("lost update accepted", r.StatusCode)
	}
	if r = usageCall(p, "PATCH", "polling", `{"profile":"unknown","seconds":900,"revision":0}`); r.StatusCode != 400 {
		t.Fatal("unknown profile accepted", r.StatusCode)
	}
	view = historyreports.CompareView{Profiles: []historyreports.CompareProfile{
		{Reference: ref, Provider: "antigravity", Current: []historyreports.CompareMeter{{LastObservedMS: p.now().Add(-20 * time.Minute).UnixMilli()}}},
		{Reference: "default", Provider: "antigravity", Current: []historyreports.CompareMeter{{LastObservedMS: p.now().Add(-20 * time.Minute).UnixMilli(), Stale: true}, {LastObservedMS: p.now().Add(-32 * time.Minute).UnixMilli()}}},
	}}
	if err := p.annotatePolling(&view); err != nil {
		t.Fatal(err)
	}
	if view.Profiles[0].PollingSeconds != 900 || !view.Profiles[0].Current[0].Stale || view.Profiles[1].Current[0].Stale || !view.Profiles[1].Current[1].Stale {
		t.Fatalf("freshness does not follow polling cadence: %+v", view.Profiles)
	}
}
