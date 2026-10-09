package historyreports

import (
	"context"
	"testing"
	"time"
)

func TestUsageNativePagesEveryRevisionAfterTheCutoff(t *testing.T) {
	ctx := context.Background()
	s, _ := forwardLedger(t)
	feed, _ := forwardFeed(t)
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		forwardPut(t, feed, forwardNative(t, id, at.Add(time.Duration(i)*time.Minute)))
	}
	forwardAdvance(t, s, feed)
	all, err := s.UsageNative(ctx, time.Time{}, 0, 10000)
	if err != nil || all.More || len(all.Events) != 5 || len(all.Origins.Certificates) != 0 || all.Events[0].Event.Model != "claude-opus-5-5" {
		t.Fatal(len(all.Events), all.More, err)
	}
	// Paging by sequence sees every row exactly once.
	seen, after := 0, int64(0)
	for {
		page, err := s.UsageNative(ctx, time.Time{}, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(page.Events)
		after = page.Through
		if !page.More {
			break
		}
	}
	if seen != len(all.Events) || after != all.Through {
		t.Fatal("paging", seen, len(all.Events), after, all.Through)
	}
	// The cutoff drops earlier requests.
	if cut, _ := s.UsageNative(ctx, at.Add(150*time.Second), 0, 10000); len(cut.Events) != 2 {
		t.Fatal("cutoff", len(cut.Events))
	}
	// A cutoff after every request returns none but still advances the cursor.
	late, err := s.UsageNative(ctx, all.Events[len(all.Events)-1].StartedAt.Add(time.Hour), 0, 10000)
	if err != nil || len(late.Events) != 0 || late.Through != all.Through {
		t.Fatal(len(late.Events), late.Through, err)
	}
	if _, err = s.UsageNative(ctx, time.Time{}, 0, 0); err == nil {
		t.Fatal("unbounded page")
	}
}
