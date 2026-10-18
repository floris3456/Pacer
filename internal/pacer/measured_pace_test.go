package pacer

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
)

func compareView(t *testing.T, raw string) historyreports.CompareView {
	t.Helper()
	var v historyreports.CompareView
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The value is the weekly ("all") average of the profile an account belongs to
// now; demand is each provider's recorded value per hour.
func TestMeasureReadsProfileRatesAndRecordedSpend(t *testing.T) {
	now := time.Date(2026, 10, 18, 13, 30, 0, 0, time.UTC)
	from := now.AddDate(0, 0, -demandDays).Truncate(24 * time.Hour)
	all := compareView(t, `{"profiles":[
		{"reference":"profile:pro","provider":"claude","pools":[
			{"kind":"5h","scope":"all","average":{"status":"available","usd_per_percent":{"low":0.37,"high":0.39}}},
			{"kind":"weekly","scope":"all","average":{"status":"available","usd_per_percent":{"low":2.7,"high":3.0}}}]},
		{"reference":"profile:max","provider":"claude","pools":[
			{"kind":"weekly","scope":"fable","average":{"status":"available","usd_per_percent":{"low":6,"high":8}}},
			{"kind":"weekly","scope":"all","average":{"status":"available","usd_per_percent":{"low":22,"high":26}}}]},
		{"reference":"profile:new","provider":"claude","pools":[
			{"kind":"weekly","scope":"all","average":{"status":"unavailable","usd_per_percent":null}}]},
		{"reference":"profile:old","provider":"claude","archived":true,"pools":[
			{"kind":"weekly","scope":"all","average":{"status":"available","usd_per_percent":{"low":1,"high":1}}}]}]}`)
	recent := compareView(t, `{"profiles":[
		{"reference":"profile:pro","provider":"claude","selected":{"value_low_nano_usd":70000000000,"value_high_nano_usd":80000000000}},
		{"reference":"profile:max","provider":"claude","selected":{"value_low_nano_usd":300000000000,"value_high_nano_usd":340000000000}},
		{"reference":"unassigned","provider":"","selected":{"value_low_nano_usd":900000000000,"value_high_nano_usd":900000000000}},
		{"reference":"profile:codex","provider":"codex","selected":{"value_low_nano_usd":10000000000,"value_high_nano_usd":10000000000}}]}`)
	ms := now.UnixMilli()
	profiles := []usageProfile{
		{Reference: "profile:pro", Provider: "claude", Accounts: []usageMembership{{Account: "acct:ilse", FromMS: 0}, {Account: "acct:gone", FromMS: 0, ToMS: ms - 1}}},
		{Reference: "profile:max", Provider: "claude", Accounts: []usageMembership{{Account: "acct:minke", FromMS: ms - 1000}}},
		{Reference: "profile:new", Provider: "claude", Accounts: []usageMembership{{Account: "acct:fresh", FromMS: 0}}},
		{Reference: "profile:old", Provider: "claude", Archived: true, Accounts: []usageMembership{{Account: "acct:gone", FromMS: 0}}},
	}
	creds := []credential{{Account: "claude:ilse"}, {Account: "claude:minke"}, {Account: "claude:gone"}, {Account: "claude:fresh"}, {Account: "claude:unknown"}}
	refs := map[string]string{"claude:ilse": "acct:ilse", "claude:minke": "acct:minke", "claude:gone": "acct:gone", "claude:fresh": "acct:fresh"}
	m := measure(now, from, all, recent, profiles, creds, func(c credential) (string, bool) {
		ref, ok := refs[c.Account]
		return ref, ok
	})
	if len(m.value) != 2 || math.Abs(m.value["claude:ilse"]-2.85) > 1e-9 || m.value["claude:minke"] != 24 {
		t.Fatalf("values %v", m.value)
	}
	hours := now.Sub(from).Hours()
	if math.Abs(m.demand["claude"]-(75+320)/hours) > 1e-9 || math.Abs(m.demand["codex"]-10/hours) > 1e-9 || len(m.demand) != 2 || !m.at.Equal(now) {
		t.Fatalf("demand %v over %v h", m.demand, hours)
	}
}

// With measured sizes and spend, a small account whose balance the spend needs
// about the time to its reset takes the new chat; the large one is the reserve.
// Without measurements, or with old ones, pace decides.
func TestHandleSendsNewChatsToTheDueAccount(t *testing.T) {
	f := newRoutingFixture(t, "on")
	// a: 30 % left, b: 80 % left, both reset in 96 h. By pace b wins.
	r := f.before(t, "unmeasured", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "b" {
		t.Fatal("pace", got)
	}
	f.p.mu.Lock()
	// a: $3 per 1 %, 30 % = $90, 56 h of $1.6 an hour with 96 h left: due.
	// b: $24 per 1 %, 80 % = $1,920, 1,200 h: more than can be spent, the reserve.
	f.p.measured = measuredPace{at: f.p.now(), value: map[string]float64{f.p.creds["a"].Account: 3, f.p.creds["b"].Account: 24}, demand: map[string]float64{"claude": 1.6}}
	f.p.mu.Unlock()
	r = f.before(t, "measured", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" {
		t.Fatal("due account not chosen", got, f.p.requests[r.RequestID].Decision)
	}
	d := f.p.requests[r.RequestID].Decision
	for _, c := range d.Candidates {
		due := c.Auth == "a"
		if c.Due != due || c.USDPerPercent == 0 || c.NeedHours == 0 {
			t.Fatalf("trace: %+v", d.Candidates)
		}
	}
	f.clock.Add(int64(measuredAge))
	f.observe(t)
	r = f.before(t, "aged", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "b" {
		t.Fatal("old measurements used", got)
	}
}
