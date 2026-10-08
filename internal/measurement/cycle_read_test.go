package measurement

import (
	"errors"
	"testing"
	"time"
)

func TestCycleReadsForDryRunReport(t *testing.T) {
	s, db := fixture(t)
	var schema int64
	if err := db.QueryRow(`PRAGMA schema_version`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	o1 := observation(t, s, "o1", "cycle-1", 5*time.Hour, base.Add(time.Hour), 20)
	observation(t, s, "o2", "cycle-1", 5*time.Hour, base.Add(2*time.Hour), 35)
	unusable := Observation{ID: "o3", Provider: "claude", AccountID: "a", Scope: "all", CycleID: "cycle-1", Duration: 5 * time.Hour, At: base.Add(3 * time.Hour), UsedPercent: 90, Source: "response-header"}
	if err := s.PutObservation(ctx, unusable); err != nil {
		t.Fatal(err)
	}
	closeCycle(t, s, o1, base.Add(5*time.Hour))
	o4 := observation(t, s, "o4", "cycle-2", 5*time.Hour, base.Add(6*time.Hour), 5)
	closeCycle(t, s, o4, base.Add(10*time.Hour))

	closures, err := s.Closures(ctx, 10)
	if err != nil || len(closures) != 2 {
		t.Fatal(closures, err)
	}
	if closures[0].CycleID != "cycle-1" || !closures[0].At.Equal(base.Add(5*time.Hour)) || closures[1].CycleID != "cycle-2" {
		t.Fatalf("closures not in time order: %+v", closures)
	}
	if _, err = s.Closures(ctx, 1); !errors.Is(err, ErrLimit) {
		t.Fatal("closure limit must fail, never truncate", err)
	}

	last, ok, err := s.LastReading(ctx, o1.ref(), base.Add(5*time.Hour))
	if err != nil || !ok || last.ID != "o2" || last.UsedPercent != 35 {
		t.Fatal("newest usable reading", last, ok, err)
	}
	if last, ok, err = s.LastReading(ctx, o1.ref(), base.Add(90*time.Minute)); err != nil || !ok || last.ID != "o1" {
		t.Fatal("reading at or before the bound", last, ok, err)
	}
	missing := o1.ref()
	missing.CycleID = "cycle-3"
	if _, ok, err = s.LastReading(ctx, missing, base.Add(5*time.Hour)); err != nil || ok {
		t.Fatal("unknown cycle", ok, err)
	}

	put(t, s, record("r1", base.Add(time.Hour), 2))
	put(t, s, record("r2", base.Add(3*time.Hour), 1))
	codex := record("r3", base.Add(time.Hour), 1)
	codex.Attribution.Provider = "codex"
	put(t, s, codex)
	records, err := s.RecordsStarted(ctx, "claude", base, base.Add(2*time.Hour), 10)
	if err != nil || len(records) != 1 || records[0].EventID != "r1" || records[0].RequestID != "request-r1" {
		t.Fatal(records, err)
	}
	if _, err = s.RecordsStarted(ctx, "claude", base, base.Add(4*time.Hour), 1); !errors.Is(err, ErrLimit) {
		t.Fatal("record limit must fail, never truncate", err)
	}
	var after int64
	if err = db.QueryRow(`PRAGMA schema_version`).Scan(&after); err != nil || after != schema {
		t.Fatal("reads changed the schema", schema, after, err)
	}
}

// Unclosed cycles are found from their own readings, ordered by first reading.
func TestObservedCyclesForDryRunReport(t *testing.T) {
	s, _ := fixture(t)
	observation(t, s, "o1", "cycle-1", 5*time.Hour, base.Add(time.Hour), 20)
	observation(t, s, "o2", "cycle-1", 5*time.Hour, base.Add(2*time.Hour), 35)
	observation(t, s, "o3", "cycle-2", 5*time.Hour, base.Add(6*time.Hour), 5)
	other := Observation{ID: "o4", Provider: "claude", AccountID: "b", Scope: "all", CycleID: "cycle-9", Duration: 5 * time.Hour, At: base.Add(time.Hour), UsedPercent: 1, Source: "response-header", Usable: true}
	if err := s.PutObservation(ctx, other); err != nil {
		t.Fatal(err)
	}
	refs, err := s.ObservedCycles(ctx, "claude", "a", "all", base, base.Add(3*time.Hour), 10)
	if err != nil || len(refs) != 1 || refs[0].CycleID != "cycle-1" || refs[0].Duration != 5*time.Hour || refs[0].AccountID != "a" {
		t.Fatal(refs, err)
	}
	if refs, err = s.ObservedCycles(ctx, "claude", "a", "all", base, base.Add(7*time.Hour), 10); err != nil || len(refs) != 2 || refs[0].CycleID != "cycle-1" || refs[1].CycleID != "cycle-2" {
		t.Fatal(refs, err)
	}
	if _, err = s.ObservedCycles(ctx, "claude", "a", "all", base, base.Add(7*time.Hour), 1); !errors.Is(err, ErrLimit) {
		t.Fatal("cycle limit must fail, never truncate", err)
	}
}
