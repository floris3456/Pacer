package pacer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/dryrun"
)

var errDryRunDayTooLarge = errors.New("dry-run decisions of the day exceed the read limit")

func dryRunEvidencePath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "dry-run-evidence.sqlite")
}

func (p *Plugin) dryRunHeartbeat() {}

func readDryRunDecisions(ctx context.Context, db *sql.DB, since, day, to time.Time, limit int) ([]dryrun.Decision, time.Time, error) {
	return nil, since, nil
}
