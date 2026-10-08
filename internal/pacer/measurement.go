package pacer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
)

func (p *Plugin) openMeasurements(db *sql.DB) (*measurement.Store, error) {
	ctx := context.Background()
	m, err := measurement.Open(ctx, db, measurement.Options{Clock: p.now})
	if err != nil {
		return nil, err
	}
	if err = m.RegisterPriceBook(ctx, measurement.DefaultPriceBook()); err != nil {
		return nil, err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return nil, err
	}
	// No timer-based checkpoint: the native callback transport has not supplied
	// a drained event-time watermark. Unknown coverage must stay unknown.
	if err = m.StartDelivery(ctx, hex.EncodeToString(id[:]), p.now()); err != nil {
		return nil, err
	}
	return m, nil
}

func (p *Plugin) metricError(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	p.measurementErrors++
	p.mu.Unlock()
}

func (p *Plugin) storeObservations(c credential, samples []observation.Window) {
	if len(samples) == 0 {
		return
	}
	p.observationMu.Lock()
	defer p.observationMu.Unlock()
	before, err := p.observer.ReadWindows(c.Account)
	if err != nil {
		p.metricError(err)
		return
	}
	if err = p.observer.Observe(c.Account, c.Provider, samples); err != nil {
		p.metricError(err)
		return
	}
	after, err := p.observer.ReadWindows(c.Account)
	if err != nil {
		p.metricError(err)
		return
	}
	previous := map[string]observation.Window{}
	for _, w := range before {
		previous[w.Key()] = w
	}
	for _, w := range after {
		// Named scopes need exact catalogue membership, not guessed substrings.
		if w.Scope != "all" || w.Remaining == nil || w.Cycle == "" || w.Usage.At.IsZero() {
			continue
		}
		o := measurement.Observation{Provider: c.Provider, AccountID: c.Account, Scope: w.Scope, CycleID: w.Cycle, Duration: time.Duration(w.Seconds) * time.Second, At: w.Usage.At, UsedPercent: 100 - *w.Remaining, Source: w.Usage.Source, Usable: w.Complete(w.Usage.At) && !w.Dormant && !w.NotApplicable && !w.Usage.At.After(p.now())}
		body, _ := json.Marshal(o)
		o.ID = tracker.Digest(string(body))
		p.metricError(p.measurement.PutObservation(context.Background(), o))
		old := previous[w.Key()]
		if old.Cycle != "" && old.Cycle != w.Cycle && !w.Conflict && !w.Ambiguous && !w.CycleAt.IsZero() {
			p.metricError(p.measurement.CloseCycle(context.Background(), measurement.Closure{CycleRef: measurement.CycleRef{Provider: c.Provider, AccountID: c.Account, Scope: w.Scope, CycleID: old.Cycle, Duration: time.Duration(old.Seconds) * time.Second}, At: w.CycleAt, Source: "pacer-observer:" + w.ResetEvidence.Source}))
		}
	}
}

// Analyze outside scheduler.pick. Missing, over-limit or stale reports never
// become zero burn or an invented account size.
func (p *Plugin) refreshMeasurements() {
	p.mu.Lock()
	creds := map[string]credential{}
	for _, c := range p.creds {
		creds[c.Account] = c
	}
	p.mu.Unlock()
	reports := map[string]measurement.Report{}
	for account, c := range creds {
		r, err := p.measurement.Measure(context.Background(), measurement.Query{Provider: c.Provider, AccountID: account, Scope: "all", BookID: measurement.DefaultPriceBook().ID, Since: p.now().Add(-21 * 24 * time.Hour), AsOf: p.now(), MaxRecords: 10000})
		if err != nil {
			p.metricError(err)
			continue
		}
		reports[account] = r
	}
	p.mu.Lock()
	p.measurements = reports
	p.mu.Unlock()
}
