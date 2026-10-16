package observation

import (
	"testing"
	"time"
)

// claudeReply stores the two required Claude windows from one reading.
func claudeReply(t *testing.T, s *Store, account string, at time.Time, five, week float64) {
	t.Helper()
	short := sample(at, at.Add(3*time.Hour), five)
	short.Provider, short.Seconds = "claude", 18000
	long := sample(at, at.Add(96*time.Hour), week)
	long.Provider = "claude"
	observe(t, s, account, "claude", short, long)
}

// seedRead makes the seed read of a Claude account and returns its time.
func seedRead(t *testing.T, s *Store, c *fakeClock, account string) time.Time {
	t.Helper()
	queue(t, s, account, "claude", ReasonSeed, time.Time{})
	c.Set(status(t, s, account).Next)
	j := acquire(t, s, account, true)
	claudeReply(t, s, account, c.Now(), 65, 95)
	finish(t, s, j, 200, nil)
	return c.Now()
}

// A large request's own reply showed the meter from before it was charged
// (2026-10-15: 64% shown for 4 minutes while the account was at 54%).
func TestSpendReadsTheMeterOnceAfterALargeRequest(t *testing.T) {
	s, c, _ := testStore(t)
	read := seedRead(t, s, c, "a")

	// Replies keep a used account current: no direct read.
	c.Set(read.Add(20 * time.Minute))
	claudeReply(t, s, "a", c.Now(), 64, 95)
	if j := status(t, s, "a"); j.State != "waiting" || j.Reason == ReasonSpend {
		t.Fatalf("a reply asked for a read: %v", j)
	}
	ended := c.Now().Add(18 * time.Second)
	c.Set(ended)
	if err := s.Spend("a", "claude", ended); err != nil {
		t.Fatal(err)
	}
	if j := status(t, s, "a"); j.Reason != ReasonSpend || !j.Next.Equal(ended.Add(SpendQuiet)) || !j.Activity.Equal(ended) {
		t.Fatalf("after a large request: %v (want a read a minute after it ended)", j)
	}
	c.Set(ended.Add(SpendQuiet - time.Nanosecond))
	acquire(t, s, "a", false)
	c.Set(ended.Add(SpendQuiet))
	j := acquire(t, s, "a", true)
	claudeReply(t, s, "a", c.Now(), 54, 93)
	finish(t, s, j, 200, nil)
	read = c.Now()
	if j := status(t, s, "a"); j.Reason != ReasonIdle || !j.Next.Equal(read.Add(IdleInterval)) {
		t.Fatalf("spend still pending after the read: %v", j)
	}

	// The chat's next step brings a newer reply first: no direct read.
	c.Set(read.Add(AccountMinGap))
	if err := s.Spend("a", "claude", c.Now()); err != nil {
		t.Fatal(err)
	}
	c.Add(10 * time.Second)
	claudeReply(t, s, "a", c.Now(), 50, 92)
	if j := status(t, s, "a"); j.Reason == ReasonSpend {
		t.Fatalf("a newer reply did not cover the spend: %v", j)
	}
	c.Add(SpendQuiet)
	acquire(t, s, "a", false)
}

func TestSpendKeepsTheAccountFloor(t *testing.T) {
	s, c, _ := testStore(t)
	read := seedRead(t, s, c, "a")
	c.Set(read.Add(2 * time.Minute))
	if err := s.Spend("a", "claude", c.Now()); err != nil {
		t.Fatal(err)
	}
	if j := status(t, s, "a"); j.Reason != ReasonSpend || !j.Next.Equal(read.Add(AccountMinGap)) || j.Blocked != "account-spacing" {
		t.Fatalf("spend bypassed the account floor: %v", j)
	}
	c.Set(read.Add(AccountMinGap))
	acquire(t, s, "a", true)
}

func TestSpendFirstSightAndProvidersItLeavesAlone(t *testing.T) {
	s, c, _ := testStore(t)
	// A Claude account Pacer has no job for yet: one read a minute later.
	if err := s.Spend("new", "claude", c.Now()); err != nil {
		t.Fatal(err)
	}
	if j := status(t, s, "new"); j.Endpoint != "claude:usage" || j.Reason != ReasonSpend || !j.Activity.Equal(c.Now()) {
		t.Fatalf("first large request: %v", j)
	}
	// Direct-only providers are read after their requests anyway (Activity);
	// a provider without a meter adapter is never read.
	for _, provider := range []string{"antigravity", "opencode-go", "openai"} {
		if err := s.Spend("x-"+provider, provider, c.Now()); err != nil {
			t.Fatal(err)
		}
		if _, exists, err := s.Status("x-" + provider); err != nil || exists {
			t.Fatalf("%s spend scheduled a read: %v %v", provider, exists, err)
		}
	}
	if err := s.Spend("", "claude", c.Now()); err == nil {
		t.Fatal("spend without an account accepted")
	}
}
