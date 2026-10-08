package measurement

import (
	"errors"
	"testing"
	"time"
)

func TestSourceReloadEqualAndRegressedClockPersistsPoisonWithoutLosingIngestion(t *testing.T) {
	for _, delta := range []time.Duration{0, -time.Second} {
		t.Run(delta.String(), func(t *testing.T) {
			s, db := fixture(t)
			if err := s.StartSourceConsumer(ctx, "original", "host", base); err != nil {
				t.Fatal(err)
			}
			if err := s.StartSourceConsumer(ctx, "new", "host", base.Add(delta)); err != nil {
				t.Fatal("reload disabled persistence", err)
			}
			var at int64
			var active, poisoned bool
			if err := db.QueryRow(`SELECT started_at,active,poisoned FROM measurement_source_consumers WHERE id='new'`).Scan(&at, &active, &poisoned); err != nil {
				t.Fatal(err)
			}
			if at != base.UnixNano() || !active || !poisoned {
				t.Fatal("regressed startup borrowed proof", at, active, poisoned)
			}
			if err := s.StartSourceConsumer(ctx, "new", "host", base.Add(time.Second)); !errors.Is(err, ErrConflict) {
				t.Fatal("old ID reuse allowed", err)
			}
		})
	}
}

func TestSourceIncompletePointSnapshotCannotBecomeIntervalEvidence(t *testing.T) {
	s, _ := fixture(t)
	m := SourceMembership{ID: "point", SourceID: "host", Provider: "claude", AccountID: "account", Revision: 1, From: base, Through: base, Complete: false}
	if err := s.PutSourceMembership(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := s.CurrentSourceMembership(ctx, "host", "claude", "account")
	if err != nil || got.ID != m.ID || !got.From.Equal(got.Through) {
		t.Fatal(got, err)
	}
	m.ID = "complete"
	m.Revision = 2
	m.Complete = true
	if err = s.PutSourceMembership(ctx, m); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero-width complete membership accepted", err)
	}
	if err = s.StartSourceConsumer(ctx, "consumer", "host", base); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.SourceSnapshot(ctx, "consumer", "point", "claude", "account", base, base.Add(time.Second))
	if err != nil || snapshot.Known {
		t.Fatal("point snapshot proved interval", snapshot, err)
	}
}
