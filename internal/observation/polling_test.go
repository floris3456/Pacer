package observation

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func TestPollingMigrationPreservesGuardedFeedAndRefusesPriorDDL(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "valid-feed"
		if corrupt {
			name = "unrecognized-DDL"
		}
		t.Run(name, func(t *testing.T) {
			s, c, _ := testStore(t)
			// Reproduce an incumbent database: the observation tables predate
			// the guarded feed; the two polling tables do not exist yet.
			if _, err := s.db.Exec(`DROP TABLE observation_poll_profiles; DROP TABLE observation_poll_accounts`); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			feed, err := measurement.Open(ctx, s.db, measurement.Options{})
			if err != nil {
				t.Fatal(err)
			}
			before, err := feed.ReportFeedHead(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if _, err = s.db.Exec(`CREATE TABLE unrelated_schema_change(id INTEGER)`); err != nil {
					t.Fatal(err)
				}
			}
			_, err = Open(s.db, c.Now)
			if corrupt {
				if err == nil {
					t.Fatal("migration blessed a pre-existing schema change")
				}
				var n int
				s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'observation_poll_%'`).Scan(&n)
				if n != 0 {
					t.Fatal("failed migration left partial schema")
				}
				if _, err = feed.ReportFeedHead(ctx); !errors.Is(err, measurement.ErrReportFeedIntegrity) {
					t.Fatal("feed guard weakened", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := feed.ReportFeedHead(ctx)
			if err != nil || before != after {
				t.Fatalf("feed identity/head changed: %v %v %v", before, after, err)
			}
			if _, err = measurement.Open(ctx, s.db, measurement.Options{}); err != nil {
				t.Fatal("restart failed", err)
			}
			if _, err = Open(s.db, c.Now); err != nil {
				t.Fatal("second observer open failed", err)
			}
		})
	}
}

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
