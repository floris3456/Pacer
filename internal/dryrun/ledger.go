package dryrun

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"
)

// MaxMarkGap is the longest interval two consecutive marks of one run may span
// and still prove that the mode they record stayed in effect.
const MaxMarkGap = 10 * time.Minute

// ModeDryRun is the effective routing mode that marks record as dry-run.
const ModeDryRun = "dry-run"

const ledgerVersion = 1

// The ledger lives in its own SQLite file (dry-run-evidence.sqlite), never in
// the guarded state database whose DDL the measurement feed witness pins.
const ledgerSchema = `
CREATE TABLE IF NOT EXISTS dry_run_meta (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL, secret BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS dry_run_marks (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, run TEXT NOT NULL,
 mode TEXT NOT NULL, providers TEXT NOT NULL, decision_errors INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS dry_run_marks_at ON dry_run_marks(at,seq);
`

// Ledger holds the installation's chat_ref secret and the mode marks that prove
// when dry-run was active. A run is one ledger opening (one plugin process
// configuration); marks from different runs never prove continuity.
type Ledger struct {
	db     *sql.DB
	clock  func() time.Time
	run    string
	secret []byte
}

func OpenLedger(db *sql.DB, clock func() time.Time) (*Ledger, error) {
	if db == nil {
		return nil, errors.New("dryrun: nil ledger database")
	}
	if clock == nil {
		clock = time.Now
	}
	if _, err := db.Exec(ledgerSchema); err != nil {
		return nil, err
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO dry_run_meta(id,version,secret) VALUES(1,?,?) ON CONFLICT(id) DO NOTHING`, ledgerVersion, fresh); err != nil {
		return nil, err
	}
	l := &Ledger{db: db, clock: clock}
	var version int
	if err := db.QueryRow(`SELECT version,secret FROM dry_run_meta WHERE id=1`).Scan(&version, &l.secret); err != nil {
		return nil, err
	}
	if version != ledgerVersion || len(l.secret) != 32 {
		return nil, errors.New("dryrun: incompatible evidence ledger")
	}
	run := make([]byte, 16)
	if _, err := rand.Read(run); err != nil {
		return nil, err
	}
	l.run = hex.EncodeToString(run)
	return l, nil
}

// Secret returns a copy of the installation's chat_ref key.
func (l *Ledger) Secret() []byte { return append([]byte(nil), l.secret...) }

// Mark records the effective mode, the routed providers and this run's
// cumulative decision write failures at the current time.
func (l *Ledger) Mark(mode string, providers []string, decisionErrors int64) error {
	list := append([]string(nil), providers...)
	sort.Strings(list)
	_, err := l.db.Exec(`INSERT INTO dry_run_marks(at,run,mode,providers,decision_errors) VALUES(?,?,?,?,?)`, l.clock().UnixNano(), l.run, mode, strings.Join(unique(list), ","), decisionErrors)
	return err
}

type ledgerMark struct {
	at        time.Time
	run, mode string
	providers []string
	errors    int64
}

// Coverage derives, from consecutive marks of one run at most MaxMarkGap apart,
// the per-provider intervals in which dry-run was in effect, and the intervals
// in which the run's decision write failure count increased. Intervals are not
// clipped to [from,to].
func (l *Ledger) Coverage(ctx context.Context, from, to time.Time) (map[string][]Interval, []Interval, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT at,run,mode,providers,decision_errors FROM dry_run_marks WHERE at>=? AND at<=? ORDER BY at,seq`, from.Add(-MaxMarkGap).UnixNano(), to.Add(MaxMarkGap).UnixNano())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	marks := []ledgerMark{}
	for rows.Next() {
		var m ledgerMark
		var at int64
		var providers string
		if err = rows.Scan(&at, &m.run, &m.mode, &providers, &m.errors); err != nil {
			return nil, nil, err
		}
		m.at = time.Unix(0, at).UTC()
		if providers != "" {
			m.providers = strings.Split(providers, ",")
		}
		marks = append(marks, m)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	covered := map[string][]Interval{}
	errs := []Interval{}
	for i := 1; i < len(marks); i++ {
		m, n := marks[i-1], marks[i]
		if m.run != n.run || !n.at.After(m.at) || n.at.Sub(m.at) > MaxMarkGap {
			continue
		}
		segment := Interval{From: m.at, To: n.at}
		if m.mode == ModeDryRun {
			for _, p := range m.providers {
				covered[p] = appendMerged(covered[p], segment)
			}
		}
		if n.errors > m.errors {
			errs = appendMerged(errs, segment)
		}
	}
	return covered, errs, nil
}

func appendMerged(list []Interval, iv Interval) []Interval {
	if n := len(list); n > 0 && !iv.From.After(list[n-1].To) {
		list[n-1].To = later(list[n-1].To, iv.To)
		return list
	}
	return append(list, iv)
}

// Prune deletes marks older than before.
func (l *Ledger) Prune(before time.Time) error {
	_, err := l.db.Exec(`DELETE FROM dry_run_marks WHERE at<?`, before.UnixNano())
	return err
}
