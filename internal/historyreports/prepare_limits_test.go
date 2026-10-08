package historyreports

import (
	"context"
	"errors"
	"testing"
)

// TestPreparationCeilingValues pins the production ceilings. The values are a
// deliberate decision sized for a real 5-week CPAMP backup (278,399
// observations); changing them requires changing this table too.
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
	// The runtime map must start from the same constants, so no table can drift.
	for table, want := range map[string]int64{
		"analytics_events":            maxEvents,
		"analytics_observations":      maxObservations,
		"analytics_accounts":          maxAccounts,
		"analytics_profile_revisions": maxProfileRevisions,
		"analytics_forecasts":         maxForecasts,
		"analytics_forecast_results":  maxForecastResults,
	} {
		if got, ok := preparationCeilings[table]; !ok || got != want {
			t.Errorf("preparationCeilings[%s] = %d (present=%t), want %d", table, got, ok, want)
		}
	}
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

	const table = "analytics_observations"
	orig := preparationCeilings[table]
	t.Cleanup(func() { preparationCeilings[table] = orig })

	preparationCeilings[table] = rows
	r, e := Prepare(ctx, s, t.TempDir())
	if e != nil {
		t.Fatalf("source at exactly the ceiling (%d rows) was refused: %v", rows, e)
	}
	r.Close()

	preparationCeilings[table] = rows - 1
	if r, e = Prepare(ctx, s, t.TempDir()); !errors.Is(e, ErrLimit) || r != nil {
		t.Fatalf("source above the ceiling: got reader=%v err=%v, want ErrLimit", r != nil, e)
	}
}
