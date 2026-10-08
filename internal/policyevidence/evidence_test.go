package policyevidence

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	_ "github.com/mattn/go-sqlite3"
)

var testTime = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
var testContext = context.Background()
var caller = strings.Repeat("a", 64)

type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) Now() time.Time  { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Set(t time.Time) { c.ns.Store(t.UnixNano()) }

func openTest(t *testing.T, path string, clock Clock) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })
	s, err := Open(db, clock)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}
func fixture(t *testing.T) (*Store, *sql.DB, *fakeClock, string) {
	t.Helper()
	c := &fakeClock{}
	c.Set(testTime.Add(time.Hour))
	path := filepath.Join(t.TempDir(), "evidence.sqlite")
	s, db := openTest(t, path, c.Now)
	return s, db, c, path
}
func record(id string, start time.Time, ttl measurement.TTL) measurement.Record {
	return measurement.Record{EventID: "cpa-native:" + id, SourceID: "host", Revision: 1, RequestID: "lifecycle-" + id, AttemptID: "attempt-" + id, ExecutionID: "attempt-" + id,
		Attribution: measurement.Attribution{Provider: "claude", AccountID: "account-a", CredentialRef: "credential-a", CallerID: caller, Client: "cpa-native", InstallationID: "install", SessionID: "session", ProviderRoute: "claude-main", Model: "claude-opus-5-5"},
		Policy:      measurement.PolicySnapshot{ContextEpoch: "epoch", Revision: 3, SelectedTTL: ttl, EffectiveTTL: ttl, Kind: "primary", RequestStartedAt: start},
		Kind:        "primary", StartTimeBasis: "upstream", RequestedAt: start.Add(-time.Second), StartedAt: start, EndedAt: start.Add(2 * time.Second), Final: true, Success: true,
		Native:   &measurement.NativeMetadata{ExecutorType: "ClaudeExecutor", CredentialBindingRef: "sha256:" + strings.Repeat("b", 64), Dispatched: true, BreakpointCount: 3, Latency: 3 * time.Second},
		Activity: measurement.Activity{Partial: true}, Usage: measurement.Usage{AccountingVersion: "cpa-token-accounting-v0", Quality: "unknown"}}
}
func state(epoch, ttl string, revision int64) []byte {
	return []byte(fmt.Sprintf("{\n  \"protocol\":1, \"installation_id\":\"install\", \"session_id\":\"session\", \"provider_route\":\"claude-main\", \"revision\":%d,\n  \"active\":{\"context_epoch\":%q, \"ttl\":%q, \"used\":true, \"checkpoint\":\"checkpoint\"},\n  \"warming_paused\":true, \"policy_hash\":\"ORIGINAL-HASH\",\n  \"last_commit\": { \"receipt\" : \"ORIGINAL-RECEIPT\", \"number\":1e3, \"opaque\" : [ 1, 2 ] }\n}\n", revision, epoch, ttl))
}
func observe(t *testing.T, s *Store, r measurement.Record) ObserveResult {
	t.Helper()
	result, err := s.Observe(testContext, r)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func augmentState(t *testing.T, s *Store, digest string, raw []byte) ([]byte, map[string]json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testContext, 3*time.Second)
	defer cancel()
	out, err := s.AugmentAuthorized(ctx, digest, raw)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err = json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err, string(out))
	}
	return out, decoded
}
func wantTTL(t *testing.T, s *Store, digest string, raw []byte, want string) map[string]json.RawMessage {
	t.Helper()
	_, decoded := augmentState(t, s, digest, raw)
	var got string
	if value, ok := decoded[ttlField]; ok {
		if err := json.Unmarshal(value, &got); err != nil {
			t.Fatal(err)
		}
	}
	if got != want {
		t.Fatalf("last TTL=%q want %q; %s", got, want, decoded[evidenceField])
	}
	return decoded
}
func reason(t *testing.T, decoded map[string]json.RawMessage) string {
	t.Helper()
	var p provenance
	if err := json.Unmarshal(decoded[evidenceField], &p); err != nil {
		t.Fatal(err)
	}
	return p.Reason
}

func TestSuccessfulMarkerReadbackPreservesPolicyAndUnknownWarmth(t *testing.T) {
	for _, ttl := range []measurement.TTL{measurement.TTL5m, measurement.TTL1h} {
		t.Run(string(ttl), func(t *testing.T) {
			s, _, _, _ := fixture(t)
			r := record("verified", testTime, ttl)
			// Missing/unknown usage and partial prefix proof do not change what the
			// final executor positively recorded about caller-controlled markers.
			if result := observe(t, s, r); result.Disposition != "recorded" || !result.HeadChanged {
				t.Fatal(result)
			}
			original := state("epoch", string(ttl), 3)
			copyOriginal := bytes.Clone(original)
			out, decoded := augmentState(t, s, caller, original)
			wantTTL(t, s, caller, out, string(ttl))
			if !bytes.Equal(original, copyOriginal) {
				t.Fatal("input state mutated")
			}
			before, _ := object(original)
			after, _ := object(out)
			for _, m := range before {
				var raw json.RawMessage
				if !field(after, m.name, &raw) || !bytes.Equal(raw, m.value) {
					t.Fatalf("original field changed: %s", m.name)
				}
			}
			for _, forbidden := range []string{"effective_ttl", "cache_refresh_at", "protected_until", "warm", "warmth", "cache_hit", "account_id", "caller_digest", "request_id"} {
				if _, exists := decoded[forbidden]; exists {
					t.Fatal("invented or private optional field", forbidden)
				}
			}
			var p provenance
			if err := json.Unmarshal(decoded[evidenceField], &p); err != nil {
				t.Fatal(err)
			}
			if p.Status != "observed" || p.Basis != "successful_native_caller_cpa_markers_only" || !p.UpstreamStartedAt.Equal(testTime) || p.PolicyRevision != 3 {
				t.Fatal(p)
			}
		})
	}
}

func TestVerifiedNativeConversionAndUsagePersistenceBeforeEvidence(t *testing.T) {
	s, db, clock, _ := fixture(t)
	raw := []byte(fmt.Sprintf(`{"RequestID":"physical-native","LifecycleRequestID":"logical-native","Provider":"claude","Model":"claude-opus-5-5","AuthID":"auth-id","AuthIndex":"auth-index","APIKey":"SECRET-RAW-KEY","Source":"SECRET-PROVIDER-KEY","ExecutorType":"ClaudeExecutor","SessionID":"session","RequestedAt":%q,"Latency":3000000000,"Failed":false,"AccountingVersion":0,"Detail":{"InputTokens":1},"CachePolicy":{"selected":{"protocol":1,"scope":{"caller_scope":%q,"installation_id":"install","session_id":"session","provider_route":"claude-main"},"context_epoch":"epoch","revision":3,"ttl":"1h","kind":"warming"},"effective_ttl":"1h","breakpoint_count":2,"dispatched":true,"attempt_started_at":%q,"upstream_started_at":%q}}`, testTime.Add(-time.Second).Format(time.RFC3339Nano), caller, testTime.Add(-500*time.Millisecond).Format(time.RFC3339Nano), testTime.Format(time.RFC3339Nano)))
	r, err := measurement.FromNative(raw, measurement.NativeCredential{Provider: "claude", AccountID: "account", CredentialRef: "credential", AuthID: "auth-id", AuthIndex: "auth-index"}, "host")
	if err != nil {
		t.Fatal(err)
	}
	m, err := measurement.Open(testContext, db, measurement.Options{Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.PutRecord(testContext, r); err != nil {
		t.Fatal(err)
	}
	if r.Activity.PrimaryPrefix || !r.Activity.Partial {
		t.Fatal("fixture must have unknown prefix/usage evidence", r.Activity)
	}
	observe(t, s, r)
	wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
	var runs int
	if err = db.QueryRow(`SELECT count(*) FROM measurement_delivery_runs`).Scan(&runs); err != nil || runs != 0 {
		t.Fatal("invented delivery coverage", runs, err)
	}
}

func TestRestartOutOfOrderAccountChangeAndEpochIsolation(t *testing.T) {
	s, db, clock, path := fixture(t)
	old := record("old-account", testTime, measurement.TTL5m)
	newer := record("new-account", testTime.Add(time.Minute), measurement.TTL1h)
	newer.Attribution.AccountID = "account-b"
	newer.Attribution.CredentialRef = "credential-b"
	observe(t, s, newer)
	if result := observe(t, s, old); result.HeadChanged || result.Reason != "older_dispatch" {
		t.Fatal(result)
	}
	wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ = openTest(t, path, clock.Now)
	if result := observe(t, s, old); result.Disposition != "duplicate" || result.HeadChanged {
		t.Fatal(result)
	}
	wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
	next := record("next-epoch", testTime.Add(2*time.Minute), measurement.TTL5m)
	next.Policy.ContextEpoch = "epoch-2"
	next.Policy.Revision = 4
	before := state("epoch-2", "5m", 4)
	out, _ := augmentState(t, s, caller, before)
	if !bytes.Equal(out, before) {
		t.Fatal("new epoch inherited old telemetry", string(out))
	}
	observe(t, s, next)
	late := record("late-old-epoch", testTime.Add(3*time.Minute), measurement.TTL1h)
	observe(t, s, late)
	wantTTL(t, s, caller, state("epoch-2", "5m", 4), "5m")
}

func TestExactForeignScopesAndHostDigestNotRehashed(t *testing.T) {
	s, _, _, _ := fixture(t)
	observe(t, s, record("scope", testTime, measurement.TTL1h))
	for _, test := range []struct{ field, value string }{{"installation_id", "elsewhere"}, {"session_id", "other-session"}, {"provider_route", "other-route"}} {
		t.Run(test.field, func(t *testing.T) {
			var obj map[string]any
			json.Unmarshal(state("epoch", "1h", 3), &obj)
			obj[test.field] = test.value
			b, _ := json.Marshal(obj)
			wantTTL(t, s, caller, b, "")
		})
	}
	wantTTL(t, s, strings.Repeat("c", 64), state("epoch", "1h", 3), "")
	wantTTL(t, s, hash(caller), state("epoch", "1h", 3), "")
	wantTTL(t, s, caller, state("different-epoch", "1h", 3), "")
	if _, err := s.AugmentAuthorized(testContext, "sha256:"+caller, state("epoch", "1h", 3)); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
}

func TestNativeAffinityTopologyCannotRebindAuthenticatedPolicyScope(t *testing.T) {
	s, _, _, _ := fixture(t)
	r := record("different-affinity-session", testTime, measurement.TTL1h)
	r.Native.SessionID = "pck:server-generated-affinity-id"
	if result := observe(t, s, r); result.Disposition != "recorded" || !result.HeadChanged {
		t.Fatal(result)
	}
	wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
	var foreign map[string]any
	json.Unmarshal(state("epoch", "1h", 3), &foreign)
	foreign["session_id"] = r.Native.SessionID
	raw, _ := json.Marshal(foreign)
	wantTTL(t, s, caller, raw, "")
	wantTTL(t, s, strings.Repeat("c", 64), state("epoch", "1h", 3), "")
}

func TestIgnoredDispatchSourceAndClockEvidenceNeverPromotes(t *testing.T) {
	tests := map[string]func(*measurement.Record){
		"failed":                func(r *measurement.Record) { r.Success = false },
		"not-final":             func(r *measurement.Record) { r.Final = false },
		"not-dispatched":        func(r *measurement.Record) { r.Native.Dispatched = false },
		"no-native":             func(r *measurement.Record) { r.Native = nil },
		"unmanaged":             func(r *measurement.Record) { r.Policy.Kind = "" },
		"wrong-executor":        func(r *measurement.Record) { r.Native.ExecutorType = "OpenAICompatExecutor" },
		"wrong-provider":        func(r *measurement.Record) { r.Attribution.Provider = "codex" },
		"wrong-model":           func(r *measurement.Record) { r.Attribution.Model = "gpt-other" },
		"wrong-source":          func(r *measurement.Record) { r.Attribution.Client = "unverified" },
		"bad-binding":           func(r *measurement.Record) { r.Native.CredentialBindingRef = "PRIVATE-RAW-CREDENTIAL" },
		"title":                 func(r *measurement.Record) { r.Kind = "title" },
		"summary":               func(r *measurement.Record) { r.Native.IsCompaction = true },
		"kind-mismatch":         func(r *measurement.Record) { r.Policy.Kind = "warming" },
		"node-kind-mismatch":    func(r *measurement.Record) { r.Native.NodeKind = "warming" },
		"explicit-auxiliary":    func(r *measurement.Record) { r.Native.NodeKind = "auxiliary" },
		"request-time-fallback": func(r *measurement.Record) { r.StartTimeBasis = "requested_at_unverified" },
		"missing-basis":         func(r *measurement.Record) { r.StartTimeBasis = "" },
		"admission-time":        func(r *measurement.Record) { r.Policy.RequestStartedAt = r.RequestedAt },
		"missing-start":         func(r *measurement.Record) { r.StartedAt = time.Time{} },
		"requested-after-start": func(r *measurement.Record) { r.RequestedAt = r.StartedAt.Add(time.Second) },
		"end-before-start":      func(r *measurement.Record) { r.EndedAt = r.StartedAt.Add(-time.Second) },
		"latency-mismatch":      func(r *measurement.Record) { r.Native.Latency = time.Hour },
		"future-end": func(r *measurement.Record) {
			r.EndedAt = testTime.Add(2 * time.Hour)
			r.Native.Latency = r.EndedAt.Sub(r.RequestedAt)
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s, _, _, _ := fixture(t)
			r := record(name, testTime, measurement.TTL1h)
			change(&r)
			if result := observe(t, s, r); result.Disposition != "ignored" || result.HeadChanged || result.Reason == "" {
				t.Fatal(result)
			}
			original := state("epoch", "1h", 3)
			out, _ := augmentState(t, s, caller, original)
			if !bytes.Equal(out, original) {
				t.Fatal(string(out))
			}
		})
	}
}

func TestFutureClockQuarantineIsDurableAndReadsNeverTrustFuture(t *testing.T) {
	s, db, clock, path := fixture(t)
	future := record("future", clock.Now().Add(time.Minute), measurement.TTL1h)
	if result := observe(t, s, future); result.Reason != "unverified_dispatch_clock" {
		t.Fatal(result)
	}
	clock.Set(clock.Now().Add(time.Hour))
	db.Close()
	s, _ = openTest(t, path, clock.Now)
	if result := observe(t, s, future); result.Disposition != "duplicate" || result.HeadChanged || result.Reason != "unverified_dispatch_clock" {
		t.Fatal(result)
	}
	wantTTL(t, s, caller, state("epoch", "1h", 3), "")
	good := record("good", testTime, measurement.TTL1h)
	observe(t, s, good)
	wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
	clock.Set(testTime.Add(-time.Second))
	if r := reason(t, wantTTL(t, s, caller, state("epoch", "1h", 3), "")); r != "unverified_read_clock" {
		t.Fatal(r)
	}
	clock.Set(testTime.Add(time.Hour))
	wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
}

func TestEqualClockContradictionsAreOrderIndependentAndSticky(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			s, _, _, _ := fixture(t)
			a := record("same-a", testTime, measurement.TTL1h)
			b := record("same-b", testTime, measurement.TTL5m)
			if reverse {
				observe(t, s, b)
				observe(t, s, a)
			} else {
				observe(t, s, a)
				observe(t, s, b)
			}
			if r := reason(t, wantTTL(t, s, caller, state("epoch", "1h", 3), "")); r != "equal_start_conflict" {
				t.Fatal(r)
			}
			observe(t, s, a)
			third := record("same-c", testTime, measurement.TTL1h)
			observe(t, s, third)
			if r := reason(t, wantTTL(t, s, caller, state("epoch", "1h", 3), "")); r != "equal_start_conflict" {
				t.Fatal(r)
			}
			later := record("strictly-later", testTime.Add(time.Minute), measurement.TTL1h)
			observe(t, s, later)
			wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
		})
	}
	// Different physical attempts at the same clock can agree on the same TTL.
	s, _, _, _ := fixture(t)
	observe(t, s, record("one", testTime, measurement.TTL1h))
	observe(t, s, record("two", testTime, measurement.TTL1h))
	wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
}

func TestNewerUnknownWireEvidenceWithholdsOlderKnownValue(t *testing.T) {
	for _, test := range []string{"unknown", "mixed", "mismatch", "no-markers"} {
		t.Run(test, func(t *testing.T) {
			s, _, _, _ := fixture(t)
			observe(t, s, record("known", testTime, measurement.TTL1h))
			next := record("next", testTime.Add(time.Minute), measurement.TTL1h)
			switch test {
			case "unknown":
				next.Policy.EffectiveTTL = ""
			case "mixed":
				next.Policy.EffectiveTTL = "mixed"
			case "mismatch":
				next.Policy.EffectiveTTL = measurement.TTL5m
			case "no-markers":
				next.Native.BreakpointCount = 0
			}
			result := observe(t, s, next)
			if !result.HeadChanged || result.Reason == "" {
				t.Fatal(result)
			}
			wantTTL(t, s, caller, state("epoch", "1h", 3), "")
		})
	}
}

func TestSameClockUnknownWithholdsKnownAcrossRestart(t *testing.T) {
	for _, unknownFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(unknownFirst), func(t *testing.T) {
			s, db, clock, path := fixture(t)
			known := record("known-at-tie", testTime, measurement.TTL1h)
			unknown := record("unknown-at-tie", testTime, measurement.TTL1h)
			unknown.Policy.EffectiveTTL = ""
			if unknownFirst {
				observe(t, s, unknown)
				observe(t, s, known)
			} else {
				observe(t, s, known)
				observe(t, s, unknown)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s, _ = openTest(t, path, clock.Now)
			observe(t, s, known)
			if got := reason(t, wantTTL(t, s, caller, state("epoch", "1h", 3), "")); got != "equal_start_conflict" {
				t.Fatal(got)
			}
		})
	}
}

func TestLaterFailedOrUntrustedCallbackDoesNotRewriteHistoricalMarkerFact(t *testing.T) {
	s, _, _, _ := fixture(t)
	observe(t, s, record("historical-success", testTime, measurement.TTL1h))
	failed := record("later-failed", testTime.Add(time.Minute), measurement.TTL5m)
	failed.Success = false
	observe(t, s, failed)
	untrusted := record("later-no-dispatch-clock", testTime.Add(2*time.Minute), measurement.TTL5m)
	untrusted.StartTimeBasis = "requested_at_unverified"
	observe(t, s, untrusted)
	decoded := wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
	var p provenance
	if err := json.Unmarshal(decoded[evidenceField], &p); err != nil || !p.UpstreamStartedAt.Equal(testTime) {
		t.Fatal(p, err)
	}
}

func TestImmutableDuplicateAndPhysicalAttemptAliasCannotRewriteHead(t *testing.T) {
	s, db, _, _ := fixture(t)
	r := record("immutable", testTime, measurement.TTL1h)
	observe(t, s, r)
	// Late usage enrichment is irrelevant to the immutable marker receipt.
	r.Revision = 2
	r.Usage.CacheCreate5m = new(int64)
	r.Usage.Quality = "complete"
	if result := observe(t, s, r); result.Disposition != "duplicate" || result.HeadChanged {
		t.Fatal(result)
	}
	changed := r
	changed.Policy.EffectiveTTL = measurement.TTL5m
	if _, err := s.Observe(testContext, changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	changed = r
	changed.EventID = "relabelled-event"
	if _, err := s.Observe(testContext, changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	changed = r
	changed.Attribution.CallerID = strings.Repeat("d", 64)
	if _, err := s.Observe(testContext, changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM policy_evidence_events`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestStateRevisionActiveSelectionAndPendingTarget(t *testing.T) {
	s, _, _, _ := fixture(t)
	observe(t, s, record("state", testTime, measurement.TTL1h))
	if r := reason(t, wantTTL(t, s, caller, state("epoch", "1h", 2), "")); r != "state_snapshot_precedes_evidence" {
		t.Fatal(r)
	}
	if r := reason(t, wantTTL(t, s, caller, state("epoch", "5m", 3), "")); r != "active_selection_differs" {
		t.Fatal(r)
	}
	raw := state("epoch", "1h", 4)
	end := bytes.LastIndexByte(raw, '}')
	raw = append(append(bytes.Clone(raw[:end]), []byte(`,"pending":{"ttl":"5m","context_epoch":"future"}`)...), raw[end:]...)
	wantTTL(t, s, caller, raw, "1h")
	wantTTL(t, s, caller, state("epoch", "", 4), "")
	preDispatch := bytes.Replace(state("epoch", "1h", 3), []byte(`"used":true`), []byte(`"used":false`), 1)
	wantTTL(t, s, caller, preDispatch, "")
}

func TestAuthorizedReadIsReadOnlyAndStripsStaleOwnedFields(t *testing.T) {
	s, db, _, _ := fixture(t)
	observe(t, s, record("read", testTime, measurement.TTL1h))
	if _, err := db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	canonical := state("epoch", "1h", 3)
	augmented, _ := augmentState(t, s, caller, canonical)
	// Reusing another scope's optional telemetry cannot leak it into a fresh view.
	clean, _ := augmentState(t, s, strings.Repeat("e", 64), augmented)
	before, _ := object(canonical)
	after, _ := object(clean)
	if len(before) != len(after) {
		t.Fatal(string(clean))
	}
	for _, m := range before {
		var raw json.RawMessage
		if !field(after, m.name, &raw) || !bytes.Equal(raw, m.value) {
			t.Fatal("receipt/hash changed during cleanup", m.name)
		}
	}
	if _, err := db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedUnauthorizedAndAmbiguousStateCannotBeAugmented(t *testing.T) {
	s, _, _, _ := fixture(t)
	observe(t, s, record("guard", testTime, measurement.TTL1h))
	for _, raw := range [][]byte{[]byte(`{"error":{"code":"scope_denied"}}`), []byte(`null`), []byte(`[]`), []byte(`{} {}`), bytes.Replace(state("epoch", "1h", 3), []byte(`"protocol":1`), []byte(`"protocol":1,"protocol":1`), 1), bytes.Replace(state("epoch", "1h", 3), []byte(`"context_epoch":"epoch"`), []byte(`"context_epoch":"epoch","context_epoch":"other"`), 1), bytes.Replace(state("epoch", "1h", 3), []byte(`"session_id":"session"`), []byte(`"session_id":null`), 1), bytes.Repeat([]byte(" "), maxStateBytes+1)} {
		if _, err := s.AugmentAuthorized(testContext, caller, raw); !errors.Is(err, ErrInvalidState) {
			t.Fatal(err, string(raw[:min(len(raw), 128)]))
		}
	}
	bad := record("bad-scope", testTime, measurement.TTL1h)
	bad.Attribution.CallerID = "RAW-KEY"
	if _, err := s.Observe(testContext, bad); !errors.Is(err, ErrInvalidRecord) || strings.Contains(err.Error(), "RAW-KEY") {
		t.Fatal(err)
	}
}

func TestNoRawAccountProviderSecretOrPathIsPersistedOrReturned(t *testing.T) {
	s, db, _, _ := fixture(t)
	r := record("privacy", testTime, measurement.TTL1h)
	r.Attribution.AccountID = "PRIVATE-ACCOUNT"
	r.Attribution.CredentialRef = "/PRIVATE-CREDENTIAL-PATH"
	r.Attribution.KeyRef = "PRIVATE-KEY-REF"
	r.RawUsage = json.RawMessage(`{"secret":"PRIVATE-PROVIDER-RAW"}`)
	r.Native.AccessTokenRef = "PRIVATE-TOKEN-REF"
	r.Native.AuthIndex = "PRIVATE-AUTH-INDEX"
	observe(t, s, r)
	out, _ := augmentState(t, s, caller, state("epoch", "1h", 3))
	var key, attempt, fp, reason string
	if err := db.QueryRow(`SELECT event_key,attempt_key,fingerprint,reason FROM policy_evidence_events`).Scan(&key, &attempt, &fp, &reason); err != nil {
		t.Fatal(err)
	}
	retained := string(out) + key + attempt + fp + reason
	for _, private := range []string{"PRIVATE-ACCOUNT", "PRIVATE-CREDENTIAL-PATH", "PRIVATE-KEY-REF", "PRIVATE-PROVIDER-RAW", "PRIVATE-TOKEN-REF", "PRIVATE-AUTH-INDEX", "cpa-native:privacy", "attempt-privacy"} {
		if strings.Contains(retained, private) {
			t.Fatal("private value retained", private)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := s.AugmentAuthorized(testContext, caller, state("epoch", "1h", 3))
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "sqlite") {
		t.Fatal(err)
	}
}

func TestConcurrentSingleConnectionAndSharedPoolOrder(t *testing.T) {
	s, db, clock, path := fixture(t)
	other, _ := openTest(t, path, clock.Now)
	ctx, cancel := context.WithTimeout(testContext, 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 == 1 {
				store = other
			}
			r := record(fmt.Sprint(i), testTime.Add(time.Duration(i)*time.Second), measurement.TTL1h)
			if _, err := store.Observe(ctx, r); err != nil {
				t.Error(err)
				return
			}
			if _, err := store.AugmentAuthorized(ctx, caller, state("epoch", "1h", 3)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	decoded := wantTTL(t, s, caller, state("epoch", "1h", 3), "1h")
	var p provenance
	json.Unmarshal(decoded[evidenceField], &p)
	if !p.UpstreamStartedAt.Equal(testTime.Add(19 * time.Second)) {
		t.Fatal(p)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM policy_evidence_events`).Scan(&n); err != nil || n != 20 {
		t.Fatal(n, err)
	}
}

func TestUnknownSchemaFailsWithoutChangingOtherStores(t *testing.T) {
	s, db, clock, _ := fixture(t)
	observe(t, s, record("schema", testTime, measurement.TTL1h))
	if _, err := db.Exec(`UPDATE policy_evidence_meta SET version=99`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db, clock.Now); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM policy_evidence_heads`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
