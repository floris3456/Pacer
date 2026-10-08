package prefixevidence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const evidenceField = "prefix_cache_evidence"

var (
	observedKeys   = []string{"version", "status", "basis", "coverage", "relation", "context_epoch", "policy_revision", "primary_snapshot_ref", "binding_ref", "entry_ref", "effective_ttl", "upstream_started_at", "protection_estimate_until", "remaining_prefixes"}
	unverifiedKeys = []string{"version", "status", "reasons", "remaining_prefixes"}
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.now = t }

// openDB mirrors the production pool shape: one connection, borrowed by the store.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

type harness struct {
	s    *Store
	db   *sql.DB
	clk  *fakeClock
	path string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prefix.sqlite")
	clk := &fakeClock{now: t0}
	db := openDB(t, path)
	s, err := Open(db, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{s: s, db: db, clk: clk, path: path}
}

func (h *harness) at(d time.Duration) { h.clk.Set(t0.Add(d)) }

func (h *harness) observe(t *testing.T, f Fact) {
	t.Helper()
	if err := h.s.Observe(context.Background(), f); err != nil {
		t.Fatalf("Observe(%s): %v", f.AttemptID, err)
	}
}

func (h *harness) settle(t *testing.T, id string, o Outcome) {
	t.Helper()
	if err := h.s.Settle(context.Background(), id, o); err != nil {
		t.Fatalf("Settle(%s,%s): %v", id, o, err)
	}
}

// augment calls the readback with the fixed caller digest.
func (h *harness) augment(t *testing.T, state []byte) []byte {
	t.Helper()
	out, err := h.s.AugmentAuthorized(context.Background(), caller, state)
	if err != nil {
		t.Fatalf("AugmentAuthorized: %v", err)
	}
	return out
}

// policyState is a canonical authorized GET body with a receipt-like member
// whose bytes must survive augmentation unchanged.
func policyState(revision int64, epoch string) []byte {
	return []byte(fmt.Sprintf(`{"protocol":1,"revision":%d,"installation_id":"inst-1","session_id":"sess-1","provider_route":"route-1","active":{"context_epoch":%q,"ttl":"5m","used":true},"receipt":{"hash":"sha256:%s","note":"kept"}}`, revision, epoch, hex64('9')))
}

func evidenceMember(t *testing.T, out []byte) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return m[evidenceField]
}

func keysOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("evidence is not an object: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, raw []byte, want []string) {
	t.Helper()
	w := append([]string(nil), want...)
	sort.Strings(w)
	if got := keysOf(t, raw); strings.Join(got, ",") != strings.Join(w, ",") {
		t.Fatalf("keys=%v, want exactly %v", got, w)
	}
}

type evidence struct {
	Version                 int       `json:"version"`
	Status                  string    `json:"status"`
	Basis                   string    `json:"basis"`
	Coverage                string    `json:"coverage"`
	Relation                string    `json:"relation"`
	ContextEpoch            string    `json:"context_epoch"`
	PolicyRevision          int64     `json:"policy_revision"`
	PrimarySnapshotRef      string    `json:"primary_snapshot_ref"`
	BindingRef              string    `json:"binding_ref"`
	EntryRef                string    `json:"entry_ref"`
	EffectiveTTL            string    `json:"effective_ttl"`
	UpstreamStartedAt       time.Time `json:"upstream_started_at"`
	ProtectionEstimateUntil time.Time `json:"protection_estimate_until"`
	RemainingPrefixes       string    `json:"remaining_prefixes"`
	Reasons                 []string  `json:"reasons"`
}

func decodeEvidence(t *testing.T, raw []byte) evidence {
	t.Helper()
	var ev evidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	return ev
}

func wantUnverified(t *testing.T, raw []byte, reasons ...string) {
	t.Helper()
	if raw == nil {
		t.Fatalf("prefix_cache_evidence absent, want unverified %v", reasons)
	}
	assertKeys(t, raw, unverifiedKeys)
	ev := decodeEvidence(t, raw)
	if ev.Version != 1 || ev.Status != "unverified" || ev.RemainingPrefixes != "unknown" {
		t.Fatalf("unverified shape wrong: %+v", ev)
	}
	if strings.Join(ev.Reasons, ",") != strings.Join(reasons, ",") {
		t.Fatalf("reasons=%v, want %v", ev.Reasons, reasons)
	}
}

func wantAbsent(t *testing.T, raw []byte) {
	t.Helper()
	if raw != nil {
		t.Fatalf("prefix_cache_evidence present, want absent: %s", raw)
	}
}

// domainRef is the spec's hashed-ref shape: first 32 hex of SHA-256(domain || value).
func domainRef(domain, v string) string {
	sum := sha256.Sum256([]byte(domain + v))
	return hex.EncodeToString(sum[:])[:32]
}

func attemptRefWant(id string) string { return domainRef("prefix-evidence/attempt-v1\x00", id) }
func bindingRefWant(b string) string  { return domainRef("prefix-evidence/binding-v1\x00", b) }

func obsRow(t *testing.T, db *sql.DB, id string) (status, reason, relation string) {
	t.Helper()
	if err := db.QueryRow(`SELECT obs_status, obs_reason, obs_relation FROM prefix_attempts WHERE attempt_id=?`, id).Scan(&status, &reason, &relation); err != nil {
		t.Fatalf("observation row %s: %v", id, err)
	}
	return
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Item 1 end-to-end: a sole-endpoint write joins a succeeded settlement and
// reads back as the full observed_partial shape (5m case).
func TestPrimaryObservedReadbackShape(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)

	h.at(2 * time.Minute)
	raw := evidenceMember(t, h.augment(t, policyState(8, "epoch-1")))
	assertKeys(t, raw, observedKeys)
	ev := decodeEvidence(t, raw)
	if ev.Version != 1 || ev.Status != "observed_partial" || ev.Basis != "unique_write_destination" || ev.Coverage != "named_entry_only" {
		t.Fatalf("observed header wrong: %+v", ev)
	}
	if ev.Relation != "primary_anchor" || ev.ContextEpoch != "epoch-1" || ev.PolicyRevision != 7 {
		t.Fatalf("relation/epoch/revision wrong: %+v", ev)
	}
	if ev.PrimarySnapshotRef != attemptRefWant("att-1") || ev.BindingRef != bindingRefWant("bind-1") {
		t.Fatalf("refs wrong: primary=%q binding=%q", ev.PrimarySnapshotRef, ev.BindingRef)
	}
	if ev.EntryRef != strings.Repeat("d", 32) || ev.EffectiveTTL != "5m" || ev.RemainingPrefixes != "unknown" {
		t.Fatalf("entry/ttl/remaining wrong: %+v", ev)
	}
	if !ev.UpstreamStartedAt.Equal(t0) || !ev.ProtectionEstimateUntil.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("start=%v until=%v, want t0 and t0+5m", ev.UpstreamStartedAt, ev.ProtectionEstimateUntil)
	}
}

// Item 4: the deadline is t0+TTL. Read before it: observed with until=t0+TTL.
// Read at or after it: unverified/estimate_elapsed. Settlement time never extends it.
func TestSettlementAfterTTLNeverExtendsDeadline(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(4 * time.Minute)
	h.settle(t, "att-1", OutcomeSucceeded)

	h.at(4*time.Minute + 30*time.Second)
	ev := decodeEvidence(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))))
	if ev.Status != "observed_partial" || !ev.ProtectionEstimateUntil.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("before deadline: %+v", ev)
	}

	h.settle(t, "att-1", OutcomeSucceeded) // identical duplicate, no effect
	h.at(6 * time.Minute)
	wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "estimate_elapsed")
}

// Item 4: failed and canceled settlements never promote.
func TestFailedAndCanceledNotSucceeded(t *testing.T) {
	for _, o := range []Outcome{OutcomeFailed, OutcomeCanceled} {
		t.Run(string(o), func(t *testing.T) {
			h := newHarness(t)
			h.at(30 * time.Second)
			h.observe(t, baseFact(KindPrimary, "5m"))
			h.at(time.Minute)
			h.settle(t, "att-1", o)
			h.at(2 * time.Minute)
			wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "not_succeeded")
		})
	}
}

// Item 4: no settlement is pending; after 15 minutes it is settlement_timeout.
// A late success never promotes a timed-out attempt.
func TestPendingThenSettlementTimeoutNeverPromotes(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(time.Minute)
	wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "pending_settlement")

	h.at(15*time.Minute + time.Second)
	if err := h.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "settlement_timeout")

	h.at(15*time.Minute + 2*time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "settlement_timeout")
}

// Item 4: usage before settlement does not promote; only Settle(succeeded) does.
func TestUsageBeforeSettlementDoesNotPromote(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(40 * time.Second)
	wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "pending_settlement")
	h.settle(t, "att-1", OutcomeSucceeded)
	if ev := decodeEvidence(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1")))); ev.Status != "observed_partial" {
		t.Fatalf("after success: %+v", ev)
	}
}

// Item 4: duplicate identical Observe/Settle are idempotent; the same instant in
// another zone is the same fact.
func TestDuplicateIdenticalObserveSettleIdempotent(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	f := baseFact(KindPrimary, "5m")
	h.observe(t, f)
	g := f
	g.StartedAt = t0.In(time.FixedZone("plus-one", 3600))
	h.observe(t, g)
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.settle(t, "att-1", OutcomeSucceeded)
	if n := countRows(t, h.db, "prefix_attempts"); n != 1 {
		t.Fatalf("rows=%d, want 1", n)
	}
	h.at(2 * time.Minute)
	ev := decodeEvidence(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))))
	if ev.Status != "observed_partial" || !ev.UpstreamStartedAt.Equal(t0) {
		t.Fatalf("duplicates changed the observation: %+v", ev)
	}
}

// Item 4: a retry is a new physical attempt with its own row. The newest start
// is the one shown; the failed earlier attempt stays unverified.
func TestRetryIsIndependentAttempt(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(time.Minute)
	h.settle(t, "att-1", OutcomeFailed)

	retry := baseFact(KindPrimary, "5m")
	retry.AttemptID = "att-2"
	retry.StartedAt = t0.Add(70 * time.Second)
	h.at(70 * time.Second)
	h.observe(t, retry)
	h.at(80 * time.Second)
	h.settle(t, "att-2", OutcomeSucceeded)

	if n := countRows(t, h.db, "prefix_attempts"); n != 2 {
		t.Fatalf("rows=%d, want 2 independent attempts", n)
	}
	h.at(2 * time.Minute)
	ev := decodeEvidence(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))))
	if ev.Status != "observed_partial" || ev.PrimarySnapshotRef != attemptRefWant("att-2") {
		t.Fatalf("retry not shown: %+v", ev)
	}
	if s, r, _ := obsRow(t, h.db, "att-1"); s != "unverified" || r != "not_succeeded" {
		t.Fatalf("failed attempt = %s/%s, want unverified/not_succeeded", s, r)
	}
}

// Item 4: four distinct clocks. admission (t0-2m) is not an input; t0 is the
// StartedAt; usage collection is the Observe clock; settlement is the Settle clock.
// The estimate is t0+TTL, not settlement+TTL.
func TestFourDistinctClocks(t *testing.T) {
	h := newHarness(t)
	admission := t0.Add(-2 * time.Minute)
	f := baseFact(KindPrimary, "1h")
	f.StartedAt = t0
	h.clk.Set(admission.Add(time.Second)) // not visible to the store
	h.clk.Set(t0.Add(30 * time.Second))   // usage collection
	h.observe(t, f)
	h.clk.Set(t0.Add(90 * time.Second)) // settlement
	h.settle(t, "att-1", OutcomeSucceeded)

	ev := decodeEvidence(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))))
	if ev.Status != "observed_partial" || ev.EffectiveTTL != "1h" {
		t.Fatalf("status=%s ttl=%s", ev.Status, ev.EffectiveTTL)
	}
	if !ev.UpstreamStartedAt.Equal(t0) || !ev.ProtectionEstimateUntil.Equal(t0.Add(time.Hour)) {
		t.Fatalf("start=%v until=%v, want t0 and t0+1h", ev.UpstreamStartedAt, ev.ProtectionEstimateUntil)
	}
}

// Item 5: both join orders give the identical observation.
func TestJoinOrdersIdentical(t *testing.T) {
	obsFirst := newHarness(t)
	obsFirst.at(30 * time.Second)
	obsFirst.observe(t, baseFact(KindPrimary, "5m"))
	obsFirst.at(90 * time.Second)
	obsFirst.settle(t, "att-1", OutcomeSucceeded)

	setFirst := newHarness(t)
	setFirst.at(90 * time.Second)
	setFirst.settle(t, "att-1", OutcomeSucceeded)
	setFirst.at(30 * time.Second)
	setFirst.observe(t, baseFact(KindPrimary, "5m"))

	state := policyState(8, "epoch-1")
	a := evidenceMember(t, obsFirst.augment(t, state))
	b := evidenceMember(t, setFirst.augment(t, state))
	if a == nil || string(a) != string(b) {
		t.Fatalf("join orders differ:\n%s\n%s", a, b)
	}
}

// Item 5: close and reopen the store on the same DB between Observe and Settle.
func TestReopenBetweenObserveAndSettle(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	if err := h.db.Close(); err != nil {
		t.Fatal(err)
	}

	db := openDB(t, h.path)
	s, err := Open(db, h.clk.Now)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	h.clk.Set(t0.Add(90 * time.Second))
	if err := s.Settle(context.Background(), "att-1", OutcomeSucceeded); err != nil {
		t.Fatalf("settle after reopen: %v", err)
	}
	h.s, h.db = s, db
	h.at(2 * time.Minute)
	if ev := decodeEvidence(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1")))); ev.Status != "observed_partial" {
		t.Fatalf("after reopen: %+v", ev)
	}
}

// Item 5: a conflicting second Observe or Settle quarantines the attempt. Readback
// never shows observed_partial for it, and a different attempt is not downgraded.
func TestConflictingDuplicateQuarantined(t *testing.T) {
	t.Run("counter conflict", func(t *testing.T) {
		h := newHarness(t)
		h.at(30 * time.Second)
		h.observe(t, baseFact(KindPrimary, "5m"))
		h.at(90 * time.Second)
		h.settle(t, "att-1", OutcomeSucceeded)
		f := baseFact(KindPrimary, "5m")
		f.Write5m = ptr(2)
		if err := h.s.Observe(context.Background(), f); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflicting Observe err=%v, want ErrConflict", err)
		}
		h.at(2 * time.Minute)
		wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "conflicting_evidence")
		if s, r, _ := obsRow(t, h.db, "att-1"); s != "uncertain" || r != "conflicting_evidence" {
			t.Fatalf("row = %s/%s, want uncertain/conflicting_evidence", s, r)
		}
	})
	t.Run("endpoint tag conflict", func(t *testing.T) {
		h := newHarness(t)
		h.at(30 * time.Second)
		h.observe(t, baseFact(KindPrimary, "5m"))
		f := baseFact(KindPrimary, "5m")
		f.Snapshot.Endpoints[0].Tag = hex64('f')
		if err := h.s.Observe(context.Background(), f); !errors.Is(err, ErrConflict) {
			t.Fatalf("tag conflict err=%v, want ErrConflict", err)
		}
		h.at(90 * time.Second)
		if err := h.s.Settle(context.Background(), "att-1", OutcomeSucceeded); err != nil {
			t.Fatalf("settle: %v", err)
		}
		h.at(2 * time.Minute)
		wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "conflicting_evidence")
	})
	t.Run("outcome conflict", func(t *testing.T) {
		h := newHarness(t)
		h.at(30 * time.Second)
		h.observe(t, baseFact(KindPrimary, "5m"))
		h.at(90 * time.Second)
		h.settle(t, "att-1", OutcomeSucceeded)
		if err := h.s.Settle(context.Background(), "att-1", OutcomeFailed); !errors.Is(err, ErrConflict) {
			t.Fatalf("outcome conflict err=%v, want ErrConflict", err)
		}
		h.at(2 * time.Minute)
		wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "conflicting_evidence")
	})
	t.Run("other attempt not downgraded", func(t *testing.T) {
		h := newHarness(t)
		h.at(30 * time.Second)
		h.observe(t, baseFact(KindPrimary, "5m"))
		h.at(90 * time.Second)
		h.settle(t, "att-1", OutcomeSucceeded)
		b := baseFact(KindPrimary, "5m")
		b.AttemptID = "att-b"
		b.StartedAt = t0.Add(time.Minute)
		h.at(100 * time.Second)
		h.observe(t, b)
		h.at(110 * time.Second)
		h.settle(t, "att-b", OutcomeSucceeded)

		conflict := baseFact(KindPrimary, "5m")
		conflict.Write5m = ptr(3)
		if err := h.s.Observe(context.Background(), conflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("err=%v, want ErrConflict", err)
		}
		h.at(2 * time.Minute)
		ev := decodeEvidence(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))))
		if ev.Status != "observed_partial" || ev.PrimarySnapshotRef != attemptRefWant("att-b") {
			t.Fatalf("other attempt downgraded: %+v", ev)
		}
	})
}

// Item 5: a warming observation matches only a primary anchor with equal account,
// binding, key_id, scope, epoch, endpoint tag and settings tag, and whose start is
// not later than the warming start. Each mismatch yields no_primary_anchor.
func TestWarmingAnchorMatrix(t *testing.T) {
	type mut struct {
		name     string
		warm     func(*Fact)
		primary  func(*Fact)
		positive bool
	}
	cases := []mut{
		{name: "equal everything", positive: true},
		{name: "equal start anchor", positive: true, primary: func(f *Fact) { f.StartedAt = t0.Add(time.Minute) }},
		{name: "other account", warm: func(f *Fact) { f.Account = "acct-2" }},
		{name: "other binding", warm: func(f *Fact) { f.BindingRef = "bind-2" }},
		{name: "other key_id", warm: func(f *Fact) { f.Snapshot.KeyID = "fedcba9876543210" }},
		{name: "other settings tag", warm: func(f *Fact) { f.Snapshot.SettingsTag = hex64('a') }},
		{name: "other endpoint tag", warm: func(f *Fact) { f.Snapshot.Endpoints[0].Tag = hex64('f') }},
		{name: "other epoch", warm: func(f *Fact) { f.Epoch = "epoch-2" }},
		{name: "other caller", warm: func(f *Fact) { f.Caller = strings.Repeat("b", 64) }},
		{name: "other installation", warm: func(f *Fact) { f.Installation = "inst-2" }},
		{name: "other session", warm: func(f *Fact) { f.Session = "sess-2" }},
		{name: "other route", warm: func(f *Fact) { f.Route = "route-2" }},
		{name: "later-start anchor", primary: func(f *Fact) { f.StartedAt = t0.Add(2 * time.Minute) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			p := baseFact(KindPrimary, "5m")
			p.AttemptID = "prim-1"
			if c.primary != nil {
				c.primary(&p)
			}
			h.clk.Set(p.StartedAt)
			h.observe(t, p)
			h.settle(t, "prim-1", OutcomeSucceeded)

			w := baseFact(KindWarming, "5m")
			w.AttemptID = "warm-1"
			w.StartedAt = t0.Add(time.Minute)
			if c.warm != nil {
				c.warm(&w)
			}
			h.clk.Set(w.StartedAt)
			h.observe(t, w)
			h.settle(t, "warm-1", OutcomeSucceeded)

			status, reason, relation := obsRow(t, h.db, "warm-1")
			if c.positive {
				if status != "observed_partial" || relation != "matches_primary_anchor" {
					t.Fatalf("status=%s relation=%s, want observed_partial/matches_primary_anchor", status, relation)
				}
				return
			}
			if status != "unverified" || reason != "no_primary_anchor" {
				t.Fatalf("status=%s reason=%s, want unverified/no_primary_anchor", status, reason)
			}
		})
	}
}

// Item 5: an anchor that settles after its warming dependent still qualifies it,
// and the result is identical for either settlement order.
func TestWarmingAnchorLateArrivalMatchesEitherOrder(t *testing.T) {
	warm := func() Fact {
		w := baseFact(KindWarming, "5m")
		w.AttemptID = "warm-1"
		w.StartedAt = t0.Add(time.Minute)
		return w
	}
	prim := func() Fact {
		p := baseFact(KindPrimary, "5m")
		p.AttemptID = "prim-1"
		return p
	}

	warmFirst := newHarness(t)
	warmFirst.clk.Set(t0.Add(70 * time.Second))
	warmFirst.observe(t, warm())
	warmFirst.settle(t, "warm-1", OutcomeSucceeded)
	if s, r, _ := obsRow(t, warmFirst.db, "warm-1"); s != "unverified" || r != "no_primary_anchor" {
		t.Fatalf("before anchor: %s/%s", s, r)
	}
	warmFirst.clk.Set(t0)
	warmFirst.observe(t, prim())
	warmFirst.settle(t, "prim-1", OutcomeSucceeded)

	primFirst := newHarness(t)
	primFirst.clk.Set(t0)
	primFirst.observe(t, prim())
	primFirst.settle(t, "prim-1", OutcomeSucceeded)
	primFirst.clk.Set(t0.Add(70 * time.Second))
	primFirst.observe(t, warm())
	primFirst.settle(t, "warm-1", OutcomeSucceeded)

	for _, h := range []*harness{warmFirst, primFirst} {
		s, r, rel := obsRow(t, h.db, "warm-1")
		if s != "observed_partial" || r != "" || rel != "matches_primary_anchor" {
			t.Fatalf("warming = %s/%s/%s, want observed_partial/matches_primary_anchor", s, r, rel)
		}
	}
}

// Item 5: a non-matching warming result reads as unverified and never as a
// primary-anchored estimate.
func TestWarmingWithoutAnchorNeverObserved(t *testing.T) {
	h := newHarness(t)
	h.clk.Set(t0.Add(70 * time.Second))
	w := baseFact(KindWarming, "5m")
	w.AttemptID = "warm-1"
	w.StartedAt = t0.Add(time.Minute)
	h.observe(t, w)
	h.settle(t, "warm-1", OutcomeSucceeded)
	if s, r, _ := obsRow(t, h.db, "warm-1"); s != "unverified" || r != "no_primary_anchor" {
		t.Fatalf("%s/%s, want unverified/no_primary_anchor", s, r)
	}
}

// Item 5: a fact starting more than 60s after the collection clock is future_evidence.
func TestFutureEvidence(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	far := baseFact(KindPrimary, "5m")
	far.AttemptID = "att-far"
	far.StartedAt = t0.Add(61 * time.Second)
	h.observe(t, far)
	h.settle(t, "att-far", OutcomeSucceeded)
	if s, r, _ := obsRow(t, h.db, "att-far"); s != "unverified" || r != "future_evidence" {
		t.Fatalf("61s future: %s/%s, want unverified/future_evidence", s, r)
	}

	edge := baseFact(KindPrimary, "5m")
	edge.AttemptID = "att-edge"
	edge.StartedAt = t0.Add(60 * time.Second)
	h.observe(t, edge)
	h.settle(t, "att-edge", OutcomeSucceeded)
	if s, _, _ := obsRow(t, h.db, "att-edge"); s != "observed_partial" {
		t.Fatalf("60s boundary: status=%s, want observed_partial", s)
	}
}

// Item 5: the 1025th pending attempt closes the oldest as pending_overflow.
func TestPendingOverflowClosesOldest(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	for i := 0; i <= 1024; i++ {
		f := baseFact(KindPrimary, "5m")
		f.AttemptID = fmt.Sprintf("p-%04d", i)
		h.observe(t, f)
	}
	if s, r, _ := obsRow(t, h.db, "p-0000"); s != "unverified" || r != "pending_overflow" {
		t.Fatalf("oldest = %s/%s, want unverified/pending_overflow", s, r)
	}
	if s, r, _ := obsRow(t, h.db, "p-1024"); s != "unverified" || r != "pending_settlement" {
		t.Fatalf("newest = %s/%s, want unverified/pending_settlement", s, r)
	}
	var open int
	if err := h.db.QueryRow(`SELECT count(*) FROM prefix_attempts WHERE closed='' AND ((fact<>'') <> (outcome<>''))`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1024 {
		t.Fatalf("open pending=%d, want 1024", open)
	}
}

// Item 5: a fact session is a scope key, not a check. A session that matches no
// logging session still qualifies and is not rejected.
func TestSessionIsNotAnInputAtStore(t *testing.T) {
	h := newHarness(t)
	f := baseFact(KindPrimary, "5m")
	f.Session = "session-differs-from-logging"
	h.observe(t, f)
	h.settle(t, "att-1", OutcomeSucceeded)
	if s, r, _ := obsRow(t, h.db, "att-1"); s != "observed_partial" || r != "" {
		t.Fatalf("session-only difference changed outcome: %s/%s", s, r)
	}
}

// Item 6: observed readback carries only the allowlisted keys and no identifiers.
func TestReadbackAllowlistHasNoIdentifiers(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(2 * time.Minute)
	raw := evidenceMember(t, h.augment(t, policyState(8, "epoch-1")))
	for _, banned := range []string{"att-1", "acct-1", "bind-1", hex64('d'), caller, "inst-1", "route-1"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("readback leaks %q: %s", banned, raw)
		}
	}
}

// Item 6: an unverified readback carries only fixed reason codes.
func TestUnverifiedReadbackHasOnlyFixedCodes(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	raw := evidenceMember(t, h.augment(t, policyState(8, "epoch-1")))
	wantUnverified(t, raw, "pending_settlement")
	for _, banned := range []string{"att-1", "acct-1", "bind-1", hex64('d'), caller} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("readback leaks %q: %s", banned, raw)
		}
	}
}

// Item 6: other state members stay byte-identical; the object is appended.
func TestAugmentPreservesOtherMembers(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(2 * time.Minute)

	state := policyState(8, "epoch-1")
	out := h.augment(t, state)
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(state, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	for k, v := range before {
		if string(after[k]) != string(v) {
			t.Fatalf("member %q changed: %s -> %s", k, v, after[k])
		}
	}
	if after[evidenceField] == nil {
		t.Fatalf("evidence not appended: %s", out)
	}
	receipt := `{"hash":"sha256:` + hex64('9') + `","note":"kept"}`
	if !strings.Contains(string(out), receipt) {
		t.Fatalf("receipt bytes not byte-preserved")
	}
}

// Item 6: a stale member is removed when nothing applies, other members remain.
func TestAugmentStripsStaleMemberOnly(t *testing.T) {
	h := newHarness(t)
	state := []byte(`{"protocol":1,"revision":8,"installation_id":"inst-1","session_id":"sess-1","provider_route":"route-1","active":{"context_epoch":"epoch-1","ttl":"5m","used":true},"prefix_cache_evidence":{"status":"old"},"last_effective_ttl":"5m"}`)
	out := h.augment(t, state)
	wantAbsent(t, evidenceMember(t, out))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["last_effective_ttl"]) != `"5m"` || m["active"] == nil {
		t.Fatalf("other members lost: %s", out)
	}
}

// Item 6: readback does not write: row counts are unchanged.
func TestAugmentDoesNotWrite(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.settle(t, "att-1", OutcomeSucceeded)
	before := [2]int{countRows(t, h.db, "prefix_attempts"), countRows(t, h.db, "prefix_anchors")}
	for i := 0; i < 3; i++ {
		h.augment(t, policyState(8, "epoch-1"))
	}
	after := [2]int{countRows(t, h.db, "prefix_attempts"), countRows(t, h.db, "prefix_anchors")}
	if before != after {
		t.Fatalf("row counts changed: %v -> %v", before, after)
	}
}

// Item 6: readback works on the single-connection pool it is handed.
func TestAugmentWorksOnSingleConnectionDB(t *testing.T) {
	h := newHarness(t)
	if got := h.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections=%d, want 1", got)
	}
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(2 * time.Minute)
	raw := evidenceMember(t, h.augment(t, policyState(8, "epoch-1")))
	assertKeys(t, raw, observedKeys)
}

// Item 6: POST/receipt bytes are never inputs: different receipt bytes on the
// same authorized GET yield the identical evidence member.
func TestPostReceiptBytesAreNotInputs(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(2 * time.Minute)

	a := []byte(strings.Replace(string(policyState(8, "epoch-1")), `"note":"kept"`, `"note":"one"`, 1))
	b := []byte(strings.Replace(string(policyState(8, "epoch-1")), `"note":"kept"`, `"note":"two"`, 1))
	if string(evidenceMember(t, h.augment(t, a))) != string(evidenceMember(t, h.augment(t, b))) {
		t.Fatalf("receipt bytes changed the evidence member")
	}
}

// Item 6: an observation newer than the state's revision is not shown.
func TestAugmentNewerRevisionNotShown(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(2 * time.Minute)
	wantAbsent(t, evidenceMember(t, h.augment(t, policyState(6, "epoch-1"))))
}

// A different context_epoch is a different scope: nothing is shown.
func TestAugmentOtherEpochNotShown(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(2 * time.Minute)
	wantAbsent(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-2"))))
}

// A readback clock before the observed start is a clock fault: future_evidence.
func TestAugmentFutureClockUnverified(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(-time.Minute)
	wantUnverified(t, evidenceMember(t, h.augment(t, policyState(8, "epoch-1"))), "future_evidence")
}

// Invalid authorized state is rejected the way policyevidence rejects it.
func TestAugmentRejectsInvalidState(t *testing.T) {
	h := newHarness(t)
	good := string(policyState(8, "epoch-1"))
	cases := map[string]struct {
		digest string
		state  string
	}{
		"uppercase digest": {strings.ToUpper(caller), good},
		"malformed json":   {caller, `{"protocol":1`},
		"duplicate member": {caller, strings.Replace(good, `"protocol":1`, `"protocol":1,"protocol":1`, 1)},
		"wrong protocol":   {caller, strings.Replace(good, `"protocol":1`, `"protocol":2`, 1)},
		"oversized state":  {caller, `{"pad":"` + strings.Repeat("x", 16*1024) + `"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.s.AugmentAuthorized(context.Background(), c.digest, []byte(c.state))
			if !errors.Is(err, ErrInvalidState) {
				t.Fatalf("err=%v, want ErrInvalidState", err)
			}
		})
	}
}

// An unused active policy is not shown, matching policyevidence gating.
func TestAugmentUnusedActiveOmitted(t *testing.T) {
	h := newHarness(t)
	h.at(30 * time.Second)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.at(90 * time.Second)
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(2 * time.Minute)
	state := strings.Replace(string(policyState(8, "epoch-1")), `"used":true`, `"used":false`, 1)
	wantAbsent(t, evidenceMember(t, h.augment(t, []byte(state))))
}
