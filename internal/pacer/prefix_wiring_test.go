package pacer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/prefixevidence"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	prefixEpoch = time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)
	// prefixStart is the upstream start (t0) of the single 5m dispatch. The
	// CachePolicy and SourceEvidence clocks carry the same value so the
	// expected protection_estimate_until is unambiguous.
	prefixStart = prefixEpoch.Add(-1500 * time.Millisecond)
)

// prefixTestRecord is a valid primary 5m record with a single-endpoint snapshot
// and a positive 5m write. Tests change one member at a time.
func prefixTestRecord(start time.Time) measurement.Record {
	five, one := int64(25), int64(0)
	return measurement.Record{
		AttemptID: "physical",
		Kind:      "primary",
		StartedAt: start,
		Attribution: measurement.Attribution{
			AccountID: "account", CallerID: strings.Repeat("c", 64), InstallationID: "installation",
			SessionID: "session", ProviderRoute: "claude-main",
		},
		Policy: measurement.PolicySnapshot{ContextEpoch: "epoch", Revision: 7, SelectedTTL: measurement.TTL5m},
		Usage:  measurement.Usage{CacheCreate5m: &five, CacheCreate1h: &one},
		Native: &measurement.NativeMetadata{
			CredentialBindingRef: "sha256:binding",
			Dispatched:           true,
			PrefixSnapshot: &measurement.PrefixSnapshot{
				Version: 1, KeyID: "0123456789abcdef", SettingsTag: strings.Repeat("a", 64),
				Endpoints: []measurement.PrefixEndpoint{{Ordinal: 0, Section: "tools", TTL: "5m", Tag: strings.Repeat("b", 64)}},
				Shape:     "text_single_pass",
			},
		},
	}
}

func TestPrefixFactMapsEveryField(t *testing.T) {
	r := prefixTestRecord(prefixStart)
	five, zero := int64(25), int64(0)
	want := prefixevidence.Fact{
		AttemptID: "physical", Account: "account", BindingRef: "sha256:binding",
		Caller: strings.Repeat("c", 64), Installation: "installation", Session: "session", Route: "claude-main",
		Epoch: "epoch", Revision: 7, SelectedTTL: "5m", Kind: prefixevidence.KindPrimary,
		Dispatched: true, StartedAt: prefixStart, Write5m: &five, Write1h: &zero,
		Snapshot: &prefixevidence.Snapshot{
			Version: 1, KeyID: "0123456789abcdef", SettingsTag: strings.Repeat("a", 64),
			Endpoints: []prefixevidence.Endpoint{{Ordinal: 0, Section: "tools", TTL: "5m", Tag: strings.Repeat("b", 64)}},
			Shape:     "text_single_pass",
		},
	}
	if got := prefixFact(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("prefixFact() = %+v, want %+v", got, want)
	}
	for kind, mapped := range map[string]string{
		"primary": prefixevidence.KindPrimary, "warming": prefixevidence.KindWarming,
		"title": "other", "summary": "other", "other": "other",
	} {
		r.Kind = kind
		if got := prefixFact(r).Kind; got != mapped {
			t.Fatalf("kind %q mapped to %q, want %q", kind, got, mapped)
		}
	}
}

func TestPrefixFactNonUpstreamStartAndAbsentCountersStayAbsent(t *testing.T) {
	// requested_at_unverified is a fallback, not a measured interval start.
	r := prefixTestRecord(prefixStart)
	r.StartTimeBasis = "requested_at_unverified"
	if got := prefixFact(r).StartedAt; !got.IsZero() {
		t.Fatalf("unverified basis leaked start %v into evidence", got)
	}
	for _, basis := range []string{"", "upstream"} {
		r := prefixTestRecord(prefixStart)
		r.StartTimeBasis = basis
		if got := prefixFact(r).StartedAt; !got.Equal(prefixStart) {
			t.Fatalf("basis %q dropped upstream start: %v", basis, got)
		}
	}
	r = prefixTestRecord(prefixStart)
	r.Usage.CacheCreate5m, r.Usage.CacheCreate1h = nil, nil
	if f := prefixFact(r); f.Write5m != nil || f.Write1h != nil {
		t.Fatalf("absent counters became zero: 5m=%v 1h=%v", f.Write5m, f.Write1h)
	}
}

func TestPrefixFactCopiesSnapshotAndCounters(t *testing.T) {
	r := prefixTestRecord(prefixStart)
	r.Native.PrefixSnapshot.Reasons = []string{"kept"}
	f := prefixFact(r)
	// Mutate every shared pointer or slice in the source after mapping.
	r.Native.PrefixSnapshot.Version = 9
	r.Native.PrefixSnapshot.Endpoints[0].Tag = strings.Repeat("z", 64)
	r.Native.PrefixSnapshot.Reasons[0] = "mutated"
	*r.Usage.CacheCreate5m = 99
	if f.Snapshot == nil {
		t.Fatal("snapshot not mapped")
	}
	if f.Snapshot.Version != 1 || f.Snapshot.Endpoints[0].Tag != strings.Repeat("b", 64) || f.Snapshot.Reasons[0] != "kept" {
		t.Fatalf("snapshot changed after source mutation: %+v", f.Snapshot)
	}
	if f.Write5m == nil || *f.Write5m != 25 {
		t.Fatalf("write counter aliased the usage record: %v", f.Write5m)
	}
}

// prefixFlow selects one managed 5m primary dispatch and its CPA-side evidence.
type prefixFlow struct {
	snapshot    bool   // native usage carries a valid single-endpoint 5m snapshot
	outcome     string // physical finish outcome; empty sends no finish
	finishFirst bool   // send finish before the native usage record
	unwired     bool   // prefixEvidence nil: the no-wiring comparison plugin
}

// prefixRun is everything an ABI observer can see after one flow.
type prefixRun struct {
	posts   [][]byte       // POST bodies in order: ensure, then the idempotent retry after dispatch
	get     map[string]any // authorized GET body after the flow
	tracker string         // tracker tables after the flow
}

func runPrefixFlow(t *testing.T, flow prefixFlow) prefixRun {
	t.Helper()
	now := prefixEpoch
	clock := func() time.Time { return now }
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cachepolicy.Create(context.Background(), db, cachepolicy.Options{Clock: clock, Routes: []cachepolicy.Route{{ProviderRoute: "claude-main", TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}}}}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	scope := pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "trusted-plugin", SessionID: "chat", ProviderRoute: "claude-main"}
	config := "state_path: " + path + "\ncache_policy:\n  enabled: true\n  routes:\n    - id: claude-main\n      models: [claude-opus-5-5]\n  trusted_empty_installations:\n    - caller_digest: " + scope.CallerScope + "\n      installation_id: trusted-plugin\n"
	p := New(testHost)
	p.now = clock
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte(config), "schema_version": 7})
	t.Cleanup(p.Close)
	if flow.unwired {
		p.prefixEvidence = nil
	}

	var run prefixRun
	capability := strings.Repeat("a", 42) + "A"
	ensure := pluginapi.CachePolicyRequest{Scope: scope, Capability: capability, Method: "POST", Operation: pluginapi.CachePolicyOperation{Protocol: 1, Operation: "ensure", InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute, ContextEpoch: "empty", TTL: "5m", OperationID: "enroll", Enrollment: "new_empty"}}
	post := func() {
		t.Helper()
		r := invoke(t, p, pluginabi.MethodCachePolicyHandle, ensure).(pluginapi.CachePolicyResponse)
		if r.StatusCode != http.StatusOK {
			t.Fatal(r.StatusCode, string(r.Body))
		}
		run.posts = append(run.posts, r.Body)
	}
	post()
	selected := invoke(t, p, pluginabi.MethodCachePolicyResolve, pluginapi.CachePolicyResolveRequest{Protocol: 1, Scope: scope, Capability: capability, ContextEpoch: "empty", Revision: 1, Kind: "primary", RequestID: "logical", Model: "claude-opus-5-5", SourceFormat: "claude"}).(pluginapi.CachePolicy)
	if selected.TTL != "5m" {
		t.Fatal("selection is not the 5m policy", selected.TTL)
	}
	attempt := pluginapi.CachePolicyAttemptRequest{Operation: "begin", RequestID: "logical", AttemptID: "physical", Capability: capability, Policy: selected}
	started := invoke(t, p, pluginabi.MethodCachePolicyAttempt, attempt).(pluginapi.CachePolicyAttemptResponse)
	attempt.Operation, attempt.StartedAt = "validate", started.StartedAt
	invoke(t, p, pluginabi.MethodCachePolicyAttempt, attempt)
	if err = p.captureUsageCredential("logical", p.creds["a"]); err != nil {
		t.Fatal(err)
	}
	finish := attempt
	finish.Operation, finish.Outcome, finish.Dispatched = "finish", flow.outcome, true
	settle := func() {
		if flow.outcome != "" {
			invoke(t, p, pluginabi.MethodCachePolicyAttempt, finish)
		}
	}
	if flow.finishFirst {
		settle()
	}
	raw := prefixUsage(t, p.creds["a"], selected, now, flow.snapshot)
	if err = p.recordUsage(raw); err != nil {
		t.Fatal(err)
	}
	if !flow.finishFirst {
		settle()
	}
	post()
	r := invoke(t, p, pluginabi.MethodCachePolicyHandle, pluginapi.CachePolicyRequest{Scope: scope, Capability: capability, Method: "GET", Operation: pluginapi.CachePolicyOperation{Protocol: 1, Operation: "read", InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute}}).(pluginapi.CachePolicyResponse)
	if r.StatusCode != http.StatusOK || json.Unmarshal(r.Body, &run.get) != nil {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	run.tracker = prefixTrackerDump(t, p)
	return run
}

// prefixUsage builds the native usage record for the physical attempt "physical"
// of logical request "logical". Snapshot is the only optional member.
func prefixUsage(t *testing.T, c credential, selected pluginapi.CachePolicy, now time.Time, snapshot bool) []byte {
	t.Helper()
	policy := map[string]any{"selected": selected, "effective_ttl": "5m", "breakpoint_count": 1, "dispatched": true, "upstream_started_at": prefixStart}
	if snapshot {
		policy["prefix"] = map[string]any{
			"version": 1, "key_id": "0123456789abcdef", "settings_tag": strings.Repeat("a", 64),
			"endpoints": []map[string]any{{"ordinal": 0, "section": "tools", "ttl": "5m", "tag": strings.Repeat("b", 64)}},
			"automatic": false, "shape": "text_single_pass",
		}
	}
	usage := map[string]any{"Provider": "claude", "Model": "claude-opus-5-5", "LifecycleRequestID": "logical", "RequestID": "physical", "AuthID": c.ID, "AuthIndex": c.Index, "ExecutorType": "ClaudeExecutor", "SessionID": "pck:independent-affinity-topology", "NodeKind": "primary", "RequestedAt": now.Add(-2 * time.Second), "Latency": int64(time.Second), "Failed": false, "AccountingVersion": 2, "CachePolicy": policy,
		// 25 cache-write tokens, all in the 5m bucket: a positive 5m write and a zero 1h write.
		"Detail":         map[string]any{"CacheCreation": map[string]any{"total_tokens": 25, "ephemeral_5m_input_tokens": 25, "ephemeral_1h_input_tokens": 0, "unknown_tokens": 0, "quality": "complete"}},
		"TokenBreakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 25, "input": map[string]any{"total_tokens": 25, "uncached_tokens": 0, "cache_read_tokens": 0, "cache_write_tokens": 25}, "output": map[string]any{"total_tokens": 0, "non_reasoning_tokens": 0, "reasoning_tokens": 0}, "unclassified_tokens": 0}}
	usage["SourceEvidence"] = map[string]any{"scope": map[string]any{"provider": "claude", "auth_id": c.ID, "auth_index": c.Index, "kind": "cpa.claude.http.first-party", "version": 1}, "manager_incarnation_id": "host-incarnation", "tracked": true, "requested_at": now.Add(-2 * time.Second), "upstream_started_at": prefixStart, "final_usage_at": now.Add(-900 * time.Millisecond)}
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// prefixTrackerDump serialises the tracker tables so two runs can be compared
// for identical cache-activity state.
func prefixTrackerDump(t *testing.T, p *Plugin) string {
	t.Helper()
	var out strings.Builder
	rows, err := p.db.Query(`SELECT key, body FROM pacer_chats ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		var body []byte
		if err = rows.Scan(&key, &body); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "chat %s %s\n", key, body)
	}
	rows.Close()
	rows, err = p.db.Query(`SELECT request_id, auth, body FROM pacer_activity_contexts ORDER BY request_id, auth`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, auth string
		var body []byte
		if err = rows.Scan(&id, &auth, &body); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "activity %s %s %s\n", id, auth, body)
	}
	rows.Close()
	return out.String()
}

func withoutPrefixEvidence(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != "prefix_cache_evidence" {
			out[k] = v
		}
	}
	return out
}

func TestPrefixEvidenceReadbackFollowsFinishAndUsageInEitherOrder(t *testing.T) {
	baseline := runPrefixFlow(t, prefixFlow{outcome: "succeeded"})
	if baseline.get["last_effective_ttl"] != "5m" {
		t.Fatal("baseline run did not persist the 5m effective TTL", baseline.get)
	}
	for _, finishFirst := range []bool{false, true} {
		name := "usage_then_finish"
		if finishFirst {
			name = "finish_then_usage"
		}
		t.Run(name, func(t *testing.T) {
			run := runPrefixFlow(t, prefixFlow{snapshot: true, outcome: "succeeded", finishFirst: finishFirst})
			evidence, ok := run.get["prefix_cache_evidence"].(map[string]any)
			if !ok || evidence["status"] != "observed_partial" {
				t.Fatal("prefix evidence not observed", run.get["prefix_cache_evidence"])
			}
			until, err := time.Parse(time.RFC3339Nano, fmt.Sprint(evidence["protection_estimate_until"]))
			if err != nil || !until.Equal(prefixStart.Add(5*time.Minute)) {
				t.Fatalf("protection_estimate_until = %v (%v), want upstream start + 5m", until, err)
			}
			if !reflect.DeepEqual(withoutPrefixEvidence(run.get), baseline.get) {
				t.Fatalf("prefix evidence changed other GET members:\n got  %v\n want %v", withoutPrefixEvidence(run.get), baseline.get)
			}
		})
	}
}

func TestPrefixEvidenceAbsentWithoutSnapshotAndUnverifiedWhenNotSucceeded(t *testing.T) {
	none := runPrefixFlow(t, prefixFlow{outcome: "succeeded"})
	if _, ok := none.get["prefix_cache_evidence"]; ok {
		t.Fatal("snapshot-less record produced prefix evidence", none.get["prefix_cache_evidence"])
	}
	failed := runPrefixFlow(t, prefixFlow{snapshot: true, outcome: "failed"})
	evidence, ok := failed.get["prefix_cache_evidence"].(map[string]any)
	if !ok || evidence["status"] != "unverified" || !reflect.DeepEqual(evidence["reasons"], []any{"not_succeeded"}) {
		t.Fatal("failed finish was not unverified/not_succeeded", failed.get["prefix_cache_evidence"])
	}
}

func TestPrefixEvidencePostResponsesAreByteIdenticalWithAndWithoutWiring(t *testing.T) {
	wired := runPrefixFlow(t, prefixFlow{snapshot: true, outcome: "succeeded"})
	unwired := runPrefixFlow(t, prefixFlow{snapshot: true, outcome: "succeeded", unwired: true})
	if _, ok := wired.get["prefix_cache_evidence"]; !ok {
		t.Fatal("wired run did not exercise prefix evidence; comparison would be vacuous")
	}
	if _, ok := unwired.get["prefix_cache_evidence"]; ok {
		t.Fatal("unwired run still produced prefix evidence")
	}
	if len(wired.posts) != len(unwired.posts) {
		t.Fatalf("POST count differs: %d vs %d", len(wired.posts), len(unwired.posts))
	}
	for i := range wired.posts {
		if !bytes.Equal(wired.posts[i], unwired.posts[i]) {
			t.Fatalf("POST %d differs with wiring:\n with    %s\n without %s", i, wired.posts[i], unwired.posts[i])
		}
	}
}

func TestPrefixEvidenceDoesNotChangeTrackerActivity(t *testing.T) {
	without := runPrefixFlow(t, prefixFlow{outcome: "succeeded"})
	with := runPrefixFlow(t, prefixFlow{snapshot: true, outcome: "succeeded"})
	if _, ok := with.get["prefix_cache_evidence"]; !ok {
		t.Fatal("snapshot run did not exercise prefix evidence; comparison would be vacuous")
	}
	if with.tracker != without.tracker {
		t.Fatalf("tracker state changed by prefix evidence:\n with    %q\n without %q", with.tracker, without.tracker)
	}
}

// Regression (lab reload 2026-10-08): prefix tables created inside the state
// database changed its DDL generation after the guarded measurement feed
// boundary, and the next start refused with "report feed integrity
// unavailable". The evidence must live in its own file and a restart must leave
// the state database's schema generation untouched.
func TestPrefixEvidenceNeverChangesStateSchemaAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	if p.prefixEvidence == nil {
		t.Fatal("prefix evidence store not opened")
	}
	p.Close()
	version := func() (v int64, prefixTables int64) {
		db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err = db.QueryRow(`PRAGMA schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'prefix%'`).Scan(&prefixTables); err != nil {
			t.Fatal(err)
		}
		return
	}
	before, tables := version()
	if tables != 0 {
		t.Fatalf("state database contains %d prefix objects", tables)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "prefix-evidence.sqlite")); err != nil {
		t.Fatal("dedicated prefix evidence database missing", err)
	}
	p = startPlugin(t, path)
	defer p.Close()
	if p.prefixEvidence == nil || p.measurement == nil {
		t.Fatal("restart lost prefix evidence or measurement")
	}
	after, tables := version()
	if after != before || tables != 0 {
		t.Fatalf("state schema generation changed across restart: %d -> %d (prefix objects %d)", before, after, tables)
	}
}
