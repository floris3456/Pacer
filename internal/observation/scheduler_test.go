package observation

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func queue(t *testing.T, s *Store, a, endpoint, reason string, due time.Time) {
	t.Helper()
	if err := s.Queue(a, endpoint, reason, due); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, s *Store, a string) Job {
	t.Helper()
	j, ok, err := s.Status(a)
	if err != nil || !ok {
		t.Fatalf("status %s %v %v", a, ok, err)
	}
	return j
}

func acquire(t *testing.T, s *Store, a string, want bool) *Job {
	t.Helper()
	j, err := s.Acquire(a)
	if err != nil {
		t.Fatal(err)
	}
	if (j != nil) != want {
		t.Fatalf("Acquire %s = %#v, want start=%v; status=%v", a, j, want, status(t, s, a))
	}
	return j
}

func finish(t *testing.T, s *Store, j *Job, code int, h http.Header) {
	t.Helper()
	if err := s.Finish(*j, code, h); err != nil {
		t.Fatal(err)
	}
}

func TestFreshCompleteSuppressesCallsAndHourlyIdle(t *testing.T) {
	s, c, _ := testStore(t)
	queue(t, s, "a", "codex", ReasonSeed, time.Time{})
	seed := status(t, s, "a")
	if seed.Next.Before(c.Now().Add(time.Minute)) || seed.Next.After(c.Now().Add(5*time.Minute)) {
		t.Fatal("seed jitter out of bounds")
	}
	observe(t, s, "a", "codex", sample(c.Now(), c.Now().Add(7*24*time.Hour), 50))
	if j := status(t, s, "a"); j.State != "waiting" || j.Cancelled != 1 {
		t.Fatalf("not cancelled: %v", j)
	}
	for i := 0; i < 20; i++ {
		c.Add(time.Minute)
		observe(t, s, "a", "codex", sample(c.Now(), instant("2026-10-14T07:34:00Z"), 50-float64(i)))
		queue(t, s, "a", "codex:usage", ReasonManual, time.Time{})
		acquire(t, s, "a", false)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM observation_attempts").Scan(&count); err != nil || count != 0 {
		t.Fatalf("passive traffic caused %d direct starts: %v", count, err)
	}
	due := status(t, s, "a").Next
	if !due.Equal(c.Now().Add(time.Hour)) {
		t.Fatalf("hourly idle wake-up = %s", due)
	}
	c.Set(due)
	job := acquire(t, s, "a", true)
	observe(t, s, "a", "codex", sample(c.Now(), instant("2026-10-14T07:34:00Z"), 30))
	finish(t, s, job, 200, nil)
	if !status(t, s, "a").Next.Equal(c.Now().Add(time.Hour)) {
		t.Fatal("successful complete response not hourly")
	}
}

func TestClaudeMissingRequiredWindowCannotCancelSeed(t *testing.T) {
	s, c, _ := testStore(t)
	queue(t, s, "a", "claude", ReasonSeed, time.Time{})
	w := sample(c.Now(), c.Now().Add(5*time.Hour), 90)
	w.Provider = "claude"
	w.Seconds = 18000
	observe(t, s, "a", "claude", w)
	if status(t, s, "a").State != "queued" {
		t.Fatal("one Claude window falsely satisfied coverage")
	}
	c.Set(status(t, s, "a").Next)
	j := acquire(t, s, "a", true)
	finish(t, s, j, 200, nil)
	queue(t, s, "a", "claude", ReasonManual, time.Time{})
	acquire(t, s, "a", false)
	if !status(t, s, "a").Next.Equal(j.Started.Add(AccountMinGap)) {
		t.Fatal("partial response floor bypassed")
	}
}

func TestConcurrentConsumersAndSharedEndpointAcrossPools(t *testing.T) {
	s, c, path := testStore(t)
	other, _ := openTest(t, path, c)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 != 0 {
				store = other
			}
			errs <- store.Queue("verified-account", "codex:usage", ReasonManual, c.Now())
		}(i)
	}
	wg.Wait()
	jobs := make(chan *Job, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 != 0 {
				store = other
			}
			j, err := store.Acquire("verified-account")
			errs <- err
			jobs <- j
		}(i)
	}
	wg.Wait()
	close(errs)
	close(jobs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var acquired *Job
	count := 0
	for j := range jobs {
		if j != nil {
			acquired = j
			count++
		}
	}
	if count != 1 || status(t, s, "verified-account").Joined != 19 {
		t.Fatalf("%d starts, status %v", count, status(t, s, "verified-account"))
	}
	queue(t, other, "sibling", "codex:usage", ReasonManual, c.Now())
	acquire(t, other, "sibling", false)
	if status(t, other, "sibling").Blocked != "endpoint-inflight" {
		t.Fatal("shared endpoint not gated")
	}
	finish(t, s, acquired, 200, nil)
	next := status(t, other, "sibling").Next
	if next.Before(acquired.Started.Add(30*time.Second)) || next.After(acquired.Started.Add(35*time.Second)) {
		t.Fatal("endpoint start spacing wrong")
	}
	c.Set(next)
	sibling := acquire(t, other, "sibling", true)
	finish(t, other, sibling, 200, nil)
	acquire(t, s, "verified-account", false)
	if err := s.Queue("verified-account", "another-host:usage", ReasonManual, c.Now()); err == nil {
		t.Fatal("same account budget escaped through alternate endpoint")
	}
	if err := s.Queue("third", "https://alternate.example/wham/usage", ReasonManual, c.Now()); err == nil {
		t.Fatal("URL masqueraded as shared endpoint key")
	}
}

func TestPersistentRetryAfterNumericDateAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		minimum   time.Duration
	}{
		{"numeric", "43200", 12 * time.Hour},
		{"fractional", "900.125", 900125 * time.Millisecond},
		{"http-date", "Thu, 08 Oct 2026 07:34:00 GMT", 24 * time.Hour},
		{"malformed", "later", 15 * time.Minute},
		{"missing", "", 15 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, path := testStore(t)
			queue(t, s, "a", "codex", ReasonManual, c.Now())
			job := acquire(t, s, "a", true)
			before := sample(c.Now(), c.Now().Add(7*24*time.Hour), 42)
			observe(t, s, "a", "codex", before)
			finish(t, s, job, 429, http.Header{"retry-after": {tc.raw}})
			s, _ = openTest(t, path, c)
			queue(t, s, "sibling", "codex", ReasonManual, c.Now())
			j := status(t, s, "sibling")
			if j.Next.Before(c.Now().Add(tc.minimum)) || j.EndpointNotBefore.Before(c.Now().Add(tc.minimum)) {
				t.Fatalf("provider backoff shortened: %v", j)
			}
			got := s.Windows("a")[0]
			if *got.Remaining != 42 || !got.Reset.Equal(before.Reset) || got.State(c.Now()) != "current" {
				t.Fatal("analytics 429 changed inference evidence")
			}
			acquire(t, s, "sibling", false)
			c.Set(j.Next.Add(-time.Nanosecond))
			acquire(t, s, "sibling", false)
			c.Add(time.Nanosecond)
			probe := acquire(t, s, "sibling", true)
			queue(t, s, "another", "codex", ReasonManual, c.Now())
			acquire(t, s, "another", false)
			finish(t, s, probe, 200, nil)
			acquire(t, s, "another", false)
			var stored string
			if err := s.db.QueryRow("SELECT retry_after FROM observation_attempts WHERE token=?", job.Token).Scan(&stored); err != nil || stored != tc.raw {
				t.Fatal("Retry-After audit lost")
			}
		})
	}
}

func TestFallbackEscalatesAcrossRestartAndCapsAtSixHours(t *testing.T) {
	s, c, path := testStore(t)
	queue(t, s, "a", "codex", ReasonManual, c.Now())
	for attempt := 1; attempt <= 8; attempt++ {
		j := acquire(t, s, "a", true)
		finish(t, s, j, 429, nil)
		s, _ = openTest(t, path, c)
		next := status(t, s, "a")
		base := AccountMinGap * time.Duration(1<<uint(attempt-1))
		if base > MaxBackoff {
			base = MaxBackoff
		}
		delay := next.Next.Sub(c.Now())
		if delay < base || delay > MaxBackoff || (base < MaxBackoff && delay > base+base/10) || next.Failures != attempt {
			t.Fatalf("attempt %d delay %v failures %d", attempt, delay, next.Failures)
		}
		c.Set(next.Next)
	}
}

func TestDeadlineGraceAndAccountFloor(t *testing.T) {
	s, c, _ := testStore(t)
	queue(t, s, "a", "codex", ReasonManual, c.Now())
	j := acquire(t, s, "a", true)
	reset := c.Now().Add(5 * time.Minute)
	observe(t, s, "a", "codex", sample(c.Now(), reset, 0))
	finish(t, s, j, 200, nil)
	queue(t, s, "a", "codex", ReasonDeadline, reset)
	q := status(t, s, "a")
	if q.Due.Before(reset.Add(30*time.Second)) || q.Due.After(reset.Add(90*time.Second)) {
		t.Fatalf("deadline jitter = %s", q.Due)
	}
	if q.Next.Before(j.Started.Add(15 * time.Minute)) {
		t.Fatal("deadline bypassed account spacing")
	}
	c.Set(reset.Add(90 * time.Second))
	acquire(t, s, "a", false)
	if s.Windows("a")[0].State(c.Now()) != "awaiting-confirmation" || *s.Windows("a")[0].Remaining != 0 {
		t.Fatal("passed reset granted capacity")
	}
	c.Set(j.Started.Add(AccountMinGap))
	acquire(t, s, "a", true)
}

func TestInflightRestartExplicitRecoveryAndStaleFinish(t *testing.T) {
	s, c, path := testStore(t)
	queue(t, s, "a", "claude", ReasonManual, c.Now())
	job := acquire(t, s, "a", true)
	s, _ = openTest(t, path, c)
	c.Add(24 * time.Hour)
	acquire(t, s, "a", false)
	queue(t, s, "sibling", "claude", ReasonManual, c.Now())
	acquire(t, s, "sibling", false)
	if err := s.RecoverInterrupted(*job); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(*job, 200, nil); !errors.Is(err, ErrStaleJob) {
		t.Fatalf("old Finish accepted: %v", err)
	}
	c.Set(status(t, s, "a").Next)
	next := acquire(t, s, "a", true)
	if next.Token == job.Token {
		t.Fatal("reused fencing token")
	}
	if err := s.Finish(*job, 429, nil); !errors.Is(err, ErrStaleJob) {
		t.Fatal("late old completion mutated new flight")
	}
	finish(t, s, next, 200, nil)
}

func TestAuthenticationNeedsExplicitRecovery(t *testing.T) {
	s, c, _ := testStore(t)
	queue(t, s, "a", "claude", ReasonManual, c.Now())
	j := acquire(t, s, "a", true)
	finish(t, s, j, 401, nil)
	c.Add(48 * time.Hour)
	queue(t, s, "a", "claude", ReasonManual, c.Now())
	acquire(t, s, "a", false)
	if status(t, s, "a").Blocked != "authentication" {
		t.Fatal("authentication failure became a retry loop")
	}
	if err := s.Resume("a"); err != nil {
		t.Fatal(err)
	}
	acquire(t, s, "a", true)
}

func TestRetryAfterParser(t *testing.T) {
	now := instant("2026-10-07T07:34:00Z")
	for _, raw := range []string{"NaN", "-1", "later", "Tue, 06 Oct 2026 07:34:00 GMT"} {
		if _, ok := RetryAfter(http.Header{"Retry-After": {raw}}, now); ok {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, seconds := range []int{0, 15, 900, 86400} {
		got, ok := RetryAfter(http.Header{"Retry-After": {fmt.Sprint(seconds)}}, now)
		if !ok || !got.Equal(now.Add(time.Duration(seconds)*time.Second)) {
			t.Fatalf("%d -> %s", seconds, got)
		}
	}
}

func TestManualCannotEraseSeedOrDeadlineJitter(t *testing.T) {
	s, c, _ := testStore(t)
	queue(t, s, "seed", "codex", ReasonSeed, c.Now())
	seed := status(t, s, "seed").Due
	queue(t, s, "seed", "codex", ReasonManual, c.Now())
	if !status(t, s, "seed").Due.Equal(seed) {
		t.Fatal("manual refresh erased seed jitter")
	}
	acquire(t, s, "seed", false)
	queue(t, s, "a", "claude", ReasonSeed, c.Now())
	reset := c.Now().Add(time.Minute)
	for _, seconds := range []int64{18000, 604800} {
		w := sample(c.Now(), reset, 50)
		w.Provider = "claude"
		w.Seconds = seconds
		observe(t, s, "a", "claude", w)
	}
	c.Set(reset.Add(time.Second))
	queue(t, s, "a", "claude", ReasonManual, c.Now())
	acquire(t, s, "a", false)
	next := status(t, s, "a").Next
	if next.Before(reset.Add(30*time.Second)) || next.After(reset.Add(90*time.Second)) {
		t.Fatalf("deadline jitter bypassed: %s", next)
	}
	c.Set(next)
	j := acquire(t, s, "a", true)
	finish(t, s, j, 200, nil)
	a, err := s.Attempts("a", 0, 100)
	if err != nil || len(a) != 1 || a[0].Reason != ReasonDeadline || a[0].Status != 200 || a[0].Finished.IsZero() {
		t.Fatalf("bad attempt audit: %#v %v", a, err)
	}
}

func TestNoSignalSuccessUsesHourlyDefault(t *testing.T) {
	s, c, _ := testStore(t)
	queue(t, s, "a", "known-adapter:usage", ReasonManual, c.Now())
	j := acquire(t, s, "a", true)
	finish(t, s, j, 200, nil)
	if next := status(t, s, "a"); !next.Next.Equal(c.Now().Add(time.Hour)) || next.Reason != ReasonIdle {
		t.Fatalf("no-signal background retry too soon: %v", next)
	}
}
