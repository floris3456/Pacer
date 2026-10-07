package observation

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type fakeClock struct{ nanos atomic.Int64 }

func newClock() *fakeClock               { c := &fakeClock{}; c.Set(instant("2026-10-07T07:34:00Z")); return c }
func (c *fakeClock) Now() time.Time      { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fakeClock) Set(t time.Time)     { c.nanos.Store(t.UnixNano()) }
func (c *fakeClock) Add(d time.Duration) { c.nanos.Add(int64(d)) }

func openTest(t *testing.T, path string, c *fakeClock) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := Open(db, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func testStore(t *testing.T) (*Store, *fakeClock, string) {
	t.Helper()
	c := newClock()
	path := filepath.Join(t.TempDir(), "observations.sqlite")
	s, _ := openTest(t, path, c)
	return s, c, path
}

func sample(at, reset time.Time, remaining float64) Window {
	return Window{Provider: "codex", Scope: "all", Seconds: 604800, Remaining: &remaining, Reset: reset, Usage: Field{At: at, Source: HeaderSource, Quality: Measured}, ResetEvidence: Field{At: at, Source: HeaderSource, Quality: Absolute}}
}

func observe(t *testing.T, s *Store, account, provider string, w ...Window) {
	t.Helper()
	if err := s.Observe(account, provider, w); err != nil {
		t.Fatal(err)
	}
}

func TestIndependentFieldFreshnessNoSignalAndExpiry(t *testing.T) {
	s, c, _ := testStore(t)
	at := c.Now()
	reset := at.Add(time.Hour)
	w := sample(at, reset, 50)
	observe(t, s, "account", "codex", w)
	c.Add(time.Minute)
	remaining := 45.0
	observe(t, s, "account", "codex", Window{Scope: "all", Seconds: 604800, Remaining: &remaining, Usage: Field{At: c.Now(), Source: HeaderSource, Quality: Measured}})
	got := s.Windows("account")[0]
	if *got.Remaining != 45 || !got.Reset.Equal(reset) || !got.ResetEvidence.At.Equal(at) || !got.Usage.At.Equal(c.Now()) {
		t.Fatalf("partial usage: %#v", got)
	}
	c.Add(time.Minute)
	observe(t, s, "account", "codex", Window{Scope: "all", Seconds: 604800, Reset: reset, ResetEvidence: Field{At: c.Now(), Source: APISource, Quality: Absolute}})
	got = s.Windows("account")[0]
	if *got.Remaining != 45 || !got.Usage.At.Equal(at.Add(time.Minute)) || !got.ResetEvidence.At.Equal(c.Now()) {
		t.Fatalf("partial reset: %#v", got)
	}
	observe(t, s, "account", "codex", Headers("codex", http.Header{"Retry-After": {"30"}}, c.Now())...)
	h, err := s.History("account", 0, 100)
	if err != nil || len(h) != 3 {
		t.Fatalf("no-signal must not create observation: %d %v", len(h), err)
	}
	c.Set(reset.Add(time.Second))
	got = s.Windows("account")[0]
	if got.State(c.Now()) != "awaiting-confirmation" || *got.Remaining != 45 || !got.Reset.Equal(reset) {
		t.Fatal("elapsed time replenished quota")
	}
	// Returned pointers are detached from persisted canonical state.
	*got.Remaining = 99
	if *s.Windows("account")[0].Remaining != 45 {
		t.Fatal("snapshot aliasing")
	}
}

func TestRecordedCodexEarlyResetSequenceAndRetiredCycleRefusal(t *testing.T) {
	s, c, path := testStore(t)
	// Reset dates are the recorded CPAMP ledger (baseline §Reset accuracy).
	// Intermediate balances are synthetic; the final 93% is the recorded sample.
	for _, v := range []struct {
		at, reset string
		remaining float64
	}{
		{"2026-10-02T21:19:00Z", "2026-10-09T21:19:00Z", 20},
		{"2026-10-04T14:57:00Z", "2026-10-11T14:57:00Z", 95},
		{"2026-10-07T07:34:00Z", "2026-10-14T07:34:00Z", 93},
	} {
		c.Set(instant(v.at))
		observe(t, s, "verified-id", "codex", sample(c.Now(), instant(v.reset), v.remaining))
		if !s.Windows("verified-id")[0].Reset.Equal(instant(v.reset)) {
			t.Fatal("early reset pinned to previous deadline")
		}
	}
	s, _ = openTest(t, path, c)
	for _, sourceTime := range []time.Time{instant("2026-10-04T15:00:00Z"), c.Now().Add(time.Minute)} {
		observe(t, s, "verified-id", "codex", sample(sourceTime, instant("2026-10-09T21:19:00Z"), 5))
	}
	got := s.Windows("verified-id")[0]
	if !got.Reset.Equal(instant("2026-10-14T07:34:00Z")) || *got.Remaining != 93 || got.Conflict {
		t.Fatalf("stale cycle replaced current: %#v", got)
	}
	h, _ := s.History("verified-id", 0, 100)
	if len(h) != 5 || h[3].Disposition != "stale-cycle" || h[4].Disposition != "stale-cycle" {
		t.Fatalf("audit missing: %#v", h)
	}
}

func TestJitterConflictAmbiguityAndEvidenceRecovery(t *testing.T) {
	s, c, _ := testStore(t)
	at := c.Now()
	reset := at.Add(7 * 24 * time.Hour)
	observe(t, s, "a", "codex", sample(at, reset, 50))
	first := s.Windows("a")[0]
	c.Add(time.Minute)
	observe(t, s, "a", "codex", sample(c.Now(), reset.Add(-time.Second+123456789*time.Nanosecond), 49))
	got := s.Windows("a")[0]
	if got.Cycle != first.Cycle || got.Reset.Nanosecond() != 123456789 || got.Conflict {
		t.Fatalf("jitter rounded or new cycle invented: %#v", got)
	}
	observe(t, s, "a", "codex", sample(c.Now(), got.Reset, 30))
	if s.Windows("a")[0].State(c.Now()) != "conflicting" {
		t.Fatal("equal-time conflict resolved by callback order")
	}
	c.Add(time.Minute)
	observe(t, s, "a", "codex", sample(c.Now(), got.Reset, 29))
	if s.Windows("a")[0].Conflict {
		t.Fatal("new complete evidence did not resolve conflict")
	}
	c.Add(time.Minute)
	r := 90.0
	observe(t, s, "a", "codex", Window{Scope: "all", Seconds: 604800, Remaining: &r, Usage: Field{At: c.Now(), Source: HeaderSource, Quality: Measured}})
	got = s.Windows("a")[0]
	if !got.Ambiguous || !got.Reset.Equal(reset.Add(-time.Second+123456789*time.Nanosecond)) {
		t.Fatal("percent rebound invented reset")
	}
	c.Add(time.Minute)
	newReset := reset.Add(48 * time.Hour)
	observe(t, s, "a", "codex", Window{Scope: "all", Seconds: 604800, Reset: newReset, ResetEvidence: Field{At: c.Now(), Source: APISource, Quality: Absolute}})
	if !s.Windows("a")[0].Conflict || !s.Windows("a")[0].Reset.Equal(got.Reset) {
		t.Fatal("reset-only incompatible cycle silently inherited usage")
	}
	c.Add(time.Minute)
	observe(t, s, "a", "codex", sample(c.Now(), newReset, 93))
	got = s.Windows("a")[0]
	if got.Conflict || got.Ambiguous || !got.Reset.Equal(newReset) {
		t.Fatal("fresh cycle not accepted")
	}
}

func TestDelayedRelativeResetAndFieldOrdering(t *testing.T) {
	s, c, _ := testStore(t)
	at := c.Now()
	w := Headers("codex", http.Header{"X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Used-Percent": {"5"}, "X-Codex-Primary-Reset-After-Seconds": {"900.125"}}, at)
	c.Add(10 * time.Minute)
	observe(t, s, "a", "codex", w...)
	got := s.Windows("a")[0]
	if !got.Reset.Equal(at.Add(900125*time.Millisecond)) || !got.ResetEvidence.At.Equal(at) || got.State(c.Now()) != "estimated" {
		t.Fatalf("callback shifted source reset: %#v", got)
	}
	w[0].Remaining = nil
	w[0].Usage = Field{}
	w[0].ResetEvidence.At = at.Add(-time.Minute)
	observe(t, s, "a", "codex", w...)
	if !s.Windows("a")[0].ResetEvidence.At.Equal(at) {
		t.Fatal("older reset refreshed field age")
	}
}

func TestNullDormantAndProviderIdentity(t *testing.T) {
	s, c, _ := testStore(t)
	w, err := API("claude", []byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":null}`), c.Now())
	if err != nil {
		t.Fatal(err)
	}
	observe(t, s, "uuid-1", "claude", w...)
	if len(s.Windows("uuid-1")) != 2 || len(s.Windows("uuid-2")) != 0 {
		t.Fatal("speculative account merge")
	}
	for _, v := range s.Windows("uuid-1") {
		if !v.Reset.IsZero() {
			t.Fatal("dormant/null invented a reset")
		}
	}
	if err := s.Observe("uuid-1", "codex", w); err == nil {
		t.Fatal("mismatched provider accepted")
	}
}

func TestNewerConflictCannotBeClearedByDelayedCompleteSample(t *testing.T) {
	s, c, _ := testStore(t)
	at := c.Now()
	reset := at.Add(7 * 24 * time.Hour)
	observe(t, s, "a", "codex", sample(at, reset, 50))
	conflictingAt := at.Add(10 * time.Minute)
	changed := reset.Add(24 * time.Hour)
	observe(t, s, "a", "codex", Window{Scope: "all", Seconds: 604800, Reset: changed, ResetEvidence: Field{At: conflictingAt, Source: APISource, Quality: Absolute}})
	observe(t, s, "a", "codex", sample(at.Add(5*time.Minute), reset, 45))
	got := s.Windows("a")[0]
	if !got.Conflict || !got.ConflictAt.Equal(conflictingAt) || !got.ConflictingReset.Equal(changed) {
		t.Fatalf("delayed reply hid a newer conflict: %#v", got)
	}
	observe(t, s, "a", "codex", sample(at.Add(11*time.Minute), changed, 90))
	got = s.Windows("a")[0]
	if got.Conflict || !got.ConflictingReset.IsZero() || !got.Reset.Equal(changed) {
		t.Fatal("new complete evidence failed to resolve conflict")
	}
}

func TestOlderUnseenExpiredCycleCannotDisplaceFutureCycle(t *testing.T) {
	s, c, _ := testStore(t)
	at := c.Now()
	reset := at.Add(time.Hour)
	observe(t, s, "a", "codex", sample(at, reset, 50))
	observe(t, s, "a", "codex", sample(at.Add(time.Minute), at.Add(-time.Hour), 99))
	got := s.Windows("a")[0]
	if !got.Conflict || !got.Reset.Equal(reset) || *got.Remaining != 50 {
		t.Fatal("expired unseen cycle displaced coherent future evidence")
	}
}
