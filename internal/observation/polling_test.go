package observation

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestProfilePollingPersistsAndReschedulesWithoutBypassingBackoff(t *testing.T) {
	s, c, _ := testStore(t)
	if err := s.SetPolling("profile:test", 900, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPolling("profile:test", 300, 1); !errors.Is(err, ErrPollingInput) {
		t.Fatal(err)
	}
	if err := s.SetPolling("profile:test", 3600, 0); !errors.Is(err, ErrPollingConflict) {
		t.Fatal(err)
	}
	reopened, err := Open(s.db, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := reopened.PollingSettings()
	if err != nil || settings["profile:test"].Seconds != 900 || settings["profile:test"].Revision != 1 {
		t.Fatalf("settings: %v %v", settings, err)
	}
	queue(t, s, "ag", "antigravity", ReasonSeed, time.Time{})
	c.Set(status(t, s, "ag").Next)
	directRead(t, s, c, "ag")
	at := c.Now()
	if err = s.ApplyPolling(map[string]int{"ag": 3600}); err != nil {
		t.Fatal(err)
	}
	if next := status(t, s, "ag").Next; !next.Equal(at.Add(time.Hour)) {
		t.Fatalf("hourly read: %v", next)
	}
	if err = s.ApplyPolling(map[string]int{"ag": 900}); err != nil {
		t.Fatal(err)
	}
	if next := status(t, s, "ag").Next; !next.Equal(at.Add(15 * time.Minute)) {
		t.Fatalf("15 minute read: %v", next)
	}
	c.Set(at.Add(15 * time.Minute))
	job := acquire(t, s, "ag", true)
	finish(t, s, job, 429, http.Header{"Retry-After": []string{"7200"}})
	if err = s.ApplyPolling(map[string]int{"ag": 1800}); err != nil {
		t.Fatal(err)
	}
	if next := status(t, s, "ag").Next; next.Before(c.Now().Add(2 * time.Hour)) {
		t.Fatalf("poll setting bypassed retry-after: %v", next)
	}
}
