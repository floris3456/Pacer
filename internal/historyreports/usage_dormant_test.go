package historyreports

import (
	"context"
	"testing"
	"time"
)

// Pacer records a window that reset and has not started (dormant) as an idle
// reading: 100 % left, starting at the read (pacer.nativeReading). The newest
// is the account's current window; earlier idle readings do not pile up as
// cycles, and the expired window before them stays closed.
func TestDormantReadingsShowTheCurrentUnstartedWindow(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := time.UnixMilli(frozen.Filter.At)
	idle := func(id int64, at time.Time) NativeReading {
		reading := liveReading(id, "nx", 5*3600, at.Add(5*time.Hour), at, 0)
		reading.Source = "api"
		return reading
	}
	used := liveReading(1, "nx", 5*3600, h.Add(time.Hour), h.Add(10*time.Minute), 90)
	readings := []NativeReading{used, idle(2, h.Add(2*time.Hour)), idle(3, h.Add(150*time.Minute))}
	rec, store := openTestRecord(t, r, t.TempDir())
	events := []ForwardEvent{liveEvent(1, "nx", "cred-x", "claude-opus-5-5", h.Add(5*time.Minute), 500, 50)}
	ch := putLive(t, store, events, readings)
	live, _, err := rec.Build(ctx, RecordInput{Now: h.Add(3 * time.Hour), Source: liveSource, Changes: ch})
	if err != nil {
		t.Fatal(err)
	}
	ref := ForwardAccountRef(liveSource, "claude", "nx")
	var starts []int64
	for _, w := range live.Windows {
		if w.Account != ref || w.Base.Kind != "5h" {
			continue
		}
		starts = append(starts, w.Base.Start)
		switch w.Base.Start {
		case h.Add(time.Hour).Add(-5 * time.Hour).UnixMilli():
			if w.Latest || w.Base.Status != "closed" || *w.Base.Used != 90 {
				t.Fatalf("expired window: latest %v status %s used %v", w.Latest, w.Base.Status, *w.Base.Used)
			}
		case h.Add(150 * time.Minute).UnixMilli():
			if !w.Latest || w.Base.Status != "current" || *w.Base.Used != 0 {
				t.Fatalf("unstarted window: latest %v status %s used %v", w.Latest, w.Base.Status, *w.Base.Used)
			}
		default:
			t.Fatal("superseded idle reading kept as a window", w.Base.Start)
		}
	}
	if len(starts) != 2 {
		t.Fatal("windows", starts)
	}
}
