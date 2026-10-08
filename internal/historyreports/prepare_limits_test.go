package historyreports

import (
	"context"
	"errors"
	"testing"
)

// TestPreparationCeilingValues pins the production ceilings and their fixed
// check order. The values are a deliberate decision sized for a real 5-week
// CPAMP backup (278,399 observations); changing them requires changing this
// table too.
func TestPreparationCeilingValues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want int64
	}{
		{"maxEvents", maxEvents, 2000000},
		{"maxObservations", maxObservations, 2000000},
		{"maxAccounts", maxAccounts, 1000},
		{"maxProfileRevisions", maxProfileRevisions, 10000},
		{"maxForecasts", maxForecasts, 100000},
		{"maxForecastResults", maxForecastResults, 200000},
		{"maxLatestProfiles", maxLatestProfiles, 1000},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	// The runtime list must start from the same constants, in this exact order,
	// so no table can drift and the error precedence is fixed.
	want := []preparationCeiling{
		{"analytics_events", maxEvents},
		{"analytics_observations", maxObservations},
		{"analytics_accounts", maxAccounts},
		{"analytics_profile_revisions", maxProfileRevisions},
		{"analytics_forecasts", maxForecasts},
		{"analytics_forecast_results", maxForecastResults},
	}
	if len(preparationCeilings) != len(want) {
		t.Fatalf("preparationCeilings has %d entries, want %d", len(preparationCeilings), len(want))
	}
	for i := range want {
		if preparationCeilings[i] != want[i] {
			t.Errorf("preparationCeilings[%d] = %+v, want %+v", i, preparationCeilings[i], want[i])
		}
	}
}

// ceilingEntry returns the runtime entry for table and restores its original
// value when the test ends. It fails the test if the table is not listed.
func ceilingEntry(t *testing.T, table string) *preparationCeiling {
	t.Helper()
	for i := range preparationCeilings {
		if preparationCeilings[i].table == table {
			e := &preparationCeilings[i]
			orig := *e
			t.Cleanup(func() { *e = orig })
			return e
		}
	}
	t.Fatalf("table %s is not in preparationCeilings", table)
	return nil
}

// withCeilings replaces the whole runtime list for one test and restores it after.
func withCeilings(t *testing.T, list ...preparationCeiling) {
	t.Helper()
	orig := preparationCeilings
	t.Cleanup(func() { preparationCeilings = orig })
	preparationCeilings = list
}

// TestPreparationCeilingBoundary lowers one runtime ceiling to the exact row
// count of the frozen fixture and then one below it. At the ceiling Prepare
// must succeed; one row over must refuse with ErrLimit and no partial reader.
func TestPreparationCeilingBoundary(t *testing.T) {
	s, _ := frozenFixture(t)
	ctx := context.Background()

	base, e := Prepare(ctx, s, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	var rows int64
	if e = base.db.QueryRow(`SELECT count(*) FROM analytics_observations`).Scan(&rows); e != nil {
		t.Fatal(e)
	}
	base.Close()
	if rows == 0 {
		t.Fatal("fixture has no observations to bound")
	}

	ceilingEntry(t, "analytics_observations").max = rows
	r, e := Prepare(ctx, s, t.TempDir())
	if e != nil {
		t.Fatalf("source at exactly the ceiling (%d rows) was refused: %v", rows, e)
	}
	r.Close()

	ceilingEntry(t, "analytics_observations").max = rows - 1
	if r, e = Prepare(ctx, s, t.TempDir()); !errors.Is(e, ErrLimit) || r != nil {
		t.Fatalf("source above the ceiling: got reader=%v err=%v, want ErrLimit", r != nil, e)
	}
}

// TestPreparationCeilingPrecedence proves the first table in the fixed order
// that fails decides the error. The "missing" table makes its count query fail
// (ErrSource); the other is over its ceiling (ErrLimit). Swapping the order
// swaps the reported error, so the result never depends on map iteration.
func TestPreparationCeilingPrecedence(t *testing.T) {
	s, _ := frozenFixture(t)
	ctx := context.Background()

	withCeilings(t,
		preparationCeiling{"analytics_events", -1}, // every count is over -1
		preparationCeiling{"analytics_missing_table", maxEvents},
	)
	if r, e := Prepare(ctx, s, t.TempDir()); !errors.Is(e, ErrLimit) || r != nil {
		t.Fatalf("over-limit table first: got reader=%v err=%v, want ErrLimit", r != nil, e)
	}

	withCeilings(t,
		preparationCeiling{"analytics_missing_table", maxEvents},
		preparationCeiling{"analytics_events", -1},
	)
	if r, e := Prepare(ctx, s, t.TempDir()); !errors.Is(e, ErrSource) || r != nil {
		t.Fatalf("failing count table first: got reader=%v err=%v, want ErrSource", r != nil, e)
	}
}
