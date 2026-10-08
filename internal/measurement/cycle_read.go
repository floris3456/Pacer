package measurement

import (
	"context"
	"time"
)

func (s *Store) Closures(ctx context.Context, limit int) ([]Closure, error) { return nil, nil }

func (s *Store) LastReading(ctx context.Context, ref CycleRef, at time.Time) (Observation, bool, error) {
	return Observation{}, false, nil
}

func (s *Store) RecordsStarted(ctx context.Context, provider string, from, to time.Time, limit int) ([]Record, error) {
	return nil, nil
}

func (s *Store) ObservedCycles(ctx context.Context, provider, account, scope string, from, to time.Time, limit int) ([]CycleRef, error) {
	return nil, nil
}
