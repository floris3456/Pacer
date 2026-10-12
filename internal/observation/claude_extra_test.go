package observation

import (
	"database/sql"
	"net/http"
	"testing"
	"time"
)

func TestClaudeExtraUsageKeepsEnabledCapacitySeparateFromSubscription(t *testing.T) {
	at := instant("2026-10-12T12:00:00Z")
	for _, tc := range []struct{ body, want string }{
		{`{"extra_usage":{"is_enabled":true}}`, "available"},
		{`{"extra_usage":{"is_enabled":true,"used_credits":10}}`, "available"},
		{`{"extra_usage":{"is_enabled":true,"monthly_limit":null,"balance":"999"},"seven_day":{"utilization":100}}`, "available"},
		{`{"extra_usage":{"is_enabled":false,"used_credits":0}}`, "empty"},
		{`{}`, "unknown"}, {`null`, "unknown"}, {`[]`, "unknown"}, {`{"broken"`, "unknown"},
		{`{"extra_usage":null}`, "unknown"}, {`{"extra_usage":[]}`, "unknown"},
		{`{"extra_usage":{}}`, "unknown"}, {`{"extra_usage":{"is_enabled":null}}`, "unknown"},
		{`{"extra_usage":{"is_enabled":"false"}}`, "unknown"},
		{`{"extra_usage":{"used_credits":0}}`, "unknown"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			got := ClaudeExtraUsage([]byte(tc.body), at)
			want := Credits{State: tc.want, ReadMS: at.UnixMilli(), ExpiresMS: at.Add(CreditMaxAge).UnixMilli(), Source: APISource}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestClaudeExtraHeadersRequireExplicitUnambiguousOverageEvidence(t *testing.T) {
	at := instant("2026-10-12T12:00:00Z")
	const status = "Anthropic-Ratelimit-Unified-Overage-Status"
	const reason = "Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"
	for _, tc := range []struct {
		name    string
		headers http.Header
		state   string
		present bool
	}{
		{"absent", nil, "unknown", false},
		{"shared rejection", http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}}, "unknown", false},
		{"fable rejection", http.Header{"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"}}, "unknown", false},
		{"counter only", http.Header{"Anthropic-Ratelimit-Unified-7d-Overage-Utilization": {"1"}}, "unknown", false},
		{"allowed", http.Header{status: {"allowed"}}, "available", true},
		{"warning and casing", http.Header{"anthropic-ratelimit-unified-overage-status": {" ALLOWED_WARNING "}}, "available", true},
		{"rejected", http.Header{status: {"rejected"}}, "empty", true},
		{"disabled only", http.Header{reason: {"member_zero_credit_limit"}}, "empty", true},
		{"rejected and disabled", http.Header{status: {"rejected"}, reason: {"org_spend_cap_reached"}}, "empty", true},
		{"empty reason", http.Header{reason: {" "}}, "unknown", false},
		{"unsupported status", http.Header{status: {"pending"}}, "unknown", true},
		{"empty explicit status", http.Header{status: {""}}, "unknown", true},
		{"empty status list", http.Header{status: nil}, "unknown", true},
		{"malformed reason only", http.Header{reason: {"bad\nvalue"}}, "unknown", true},
		{"allow and disabled", http.Header{status: {"allowed"}, reason: {"org_spend_cap_reached"}}, "unknown", true},
		{"duplicate statuses", http.Header{status: {"allowed", "rejected"}}, "unknown", true},
		{"combined statuses", http.Header{status: {"rejected, allowed"}}, "unknown", true},
		{"case duplicate statuses", http.Header{status: {"rejected"}, "anthropic-ratelimit-unified-overage-status": {"allowed"}}, "unknown", true},
		{"same availability", http.Header{status: {"allowed", "allowed_warning"}}, "available", true},
		{"malformed alongside valid", http.Header{status: {"rejected", "pending"}}, "unknown", true},
		{"malformed reason", http.Header{status: {"rejected"}, reason: {"bad\nvalue"}}, "unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, present := ClaudeExtraHeaders(tc.headers, at)
			want := Credits{State: tc.state, ReadMS: at.UnixMilli(), ExpiresMS: at.Add(CreditMaxAge).UnixMilli(), Source: HeaderSource}
			if present != tc.present || got != want {
				t.Fatalf("got %+v, %v; want %+v, %v", got, present, want, tc.present)
			}
		})
	}
}

func TestCreditSourceMigrationRetainsLegacyCodexBalance(t *testing.T) {
	_, clock, path := testStore(t)
	db, err := sql.Open("sqlite3", path+".credits.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE observation_credits (account TEXT PRIMARY KEY, state TEXT NOT NULL, observed INTEGER NOT NULL, balance TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO observation_credits VALUES ('codex:legacy','available',?,'12.34')`, clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	store, err := OpenCredits(db, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := store.ReadCredits("codex:legacy")
	if err != nil || legacy.Source != APISource || legacy.State != "available" || legacy.Balance != "12.34" {
		t.Fatal(legacy, err)
	}
	header, present := ClaudeExtraHeaders(http.Header{"Anthropic-Ratelimit-Unified-Overage-Status": {"rejected"}}, clock.Now())
	if !present {
		t.Fatal("missing header evidence")
	}
	if changed, err := store.ObserveCredits("claude:new", header); err != nil || !changed {
		t.Fatal(changed, err)
	}
	reopened, err := OpenCredits(db, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.ReadCredits("claude:new"); err != nil || got != header {
		t.Fatal(got, err)
	}
	if got, err := reopened.ReadCredits("codex:legacy"); err != nil || got != legacy {
		t.Fatal("Claude evidence changed legacy Codex credits", got, err)
	}
}

func TestClaudeExtraEvidenceChronologyExpiryAndCrossSourceConflicts(t *testing.T) {
	_, clock, path := testStore(t)
	store := openTestCredits(t, path+".credits.sqlite", clock)
	api := ClaudeExtraUsage([]byte(`{"extra_usage":{"is_enabled":true}}`), clock.Now())
	if changed, err := store.ObserveCredits("claude:account", api); err != nil || !changed {
		t.Fatal(changed, err)
	}
	clock.Add(time.Second)
	header, _ := ClaudeExtraHeaders(http.Header{"Anthropic-Ratelimit-Unified-Overage-Status": {"rejected"}}, clock.Now())
	if changed, err := store.ObserveCredits("claude:account", header); err != nil || !changed {
		t.Fatal(changed, err)
	}
	for _, old := range []Credits{api, header} {
		if changed, err := store.ObserveCredits("claude:account", old); err != nil || changed {
			t.Fatal("stale or repeated evidence changed credits", changed, err)
		}
	}
	if got, err := store.ReadCredits("claude:account"); err != nil || got != header || got.At(clock.Now().Add(CreditMaxAge)).State != "stale" {
		t.Fatal(got, err)
	}
	api.ReadMS = header.ReadMS
	if changed, err := store.ObserveCredits("claude:account", api); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if got, _ := store.ReadCredits("claude:account"); got.State != "unknown" {
		t.Fatal("equal-time conflicting sources retained certainty", got)
	}
	if changed, err := store.ObserveCredits("claude:account", header); err != nil || changed {
		t.Fatal("same-time replay cleared conflict", changed, err)
	}
	clock.Add(time.Second)
	unknown := ClaudeExtraUsage([]byte(`{}`), clock.Now())
	if changed, err := store.ObserveCredits("claude:account", unknown); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if got, err := store.ReadCredits("claude:account"); err != nil || got != unknown {
		t.Fatal("new unknown evidence not persisted", got, err)
	}
	unknown.Source = EventSource
	if _, err := store.ObserveCredits("claude:account", unknown); err == nil {
		t.Fatal("accepted unsupported credit source")
	}
	clock.Add(time.Second)
	header, _ = ClaudeExtraHeaders(http.Header{"Anthropic-Ratelimit-Unified-Overage-Status": {"rejected"}}, clock.Now())
	if changed, err := store.ObserveCredits("claude:account", header); err != nil || !changed {
		t.Fatal(changed, err)
	}
	clock.Add(time.Second)
	malformed, present := ClaudeExtraHeaders(http.Header{"Anthropic-Ratelimit-Unified-Overage-Status": {"pending"}}, clock.Now())
	if !present || malformed.State != "unknown" {
		t.Fatal(malformed, present)
	}
	if changed, err := store.ObserveCredits("claude:account", malformed); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if got, err := store.ReadCredits("claude:account"); err != nil || got != malformed {
		t.Fatal("new malformed header preserved earlier empty evidence", got, err)
	}
}
