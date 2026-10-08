package pacer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/dryrun"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The daily dry-run report (docs/dry-run-report.md). The route only reads
// persisted evidence: it never calls the host or a provider, never writes the
// state or evidence databases, and never changes routing.

const (
	dryRunReportRoute = "/v0/management/plugins/pacer/dry-run-report"
	// The background worker marks the effective mode this often; consecutive
	// marks at most dryrun.MaxMarkGap apart prove the mode stayed in effect.
	dryRunHeartbeatEvery = time.Minute
	// Reports cover at most the last dryRunMaxAge days. Marks are kept long
	// enough for the oldest such day plus its simulation lookback.
	dryRunMaxAge        = 31 * 24 * time.Hour
	dryRunMarkRetention = dryRunMaxAge + dryrun.MaxLookback + 24*time.Hour
	dryRunPruneEvery    = 24 * time.Hour
	// Read bounds. Decisions beyond the limit shorten the lookback; every other
	// limit fails or makes the affected projections unknown, never truncates.
	dryRunMaxDecisions = 200000
	dryRunMaxClosures  = 100000
	dryRunMaxCycles    = 10000
	dryRunMaxAttempts  = 200000
)

var errDryRunDayTooLarge = errors.New("dry-run decisions of the day exceed the read limit")

func dryRunEvidencePath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "dry-run-evidence.sqlite")
}

// openDryRunEvidence opens the ledger in its own owner-only file, so the guarded
// state database keeps its schema. Any failure returns (nil, nil): the report
// route then refuses, and startup and routing are unaffected.
func openDryRunEvidence(statePath string, now func() time.Time) (*sql.DB, *dryrun.Ledger) {
	if statePath == "" {
		return nil, nil
	}
	db := openOwnerOnlySQLite(dryRunEvidencePath(statePath))
	if db == nil {
		return nil, nil
	}
	ledger, err := dryrun.OpenLedger(db, now)
	if err != nil {
		db.Close()
		return nil, nil
	}
	return db, ledger
}

// dryRunMarkLocked records the effective mode. The caller holds p.mu, so a
// mark can never be ordered after a later mode change. A failed mark leaves
// the interval unproven, which the report shows as missing coverage.
func (p *Plugin) dryRunMarkLocked() {
	if p.dryRun == nil {
		return
	}
	mode := p.cfg.Routing.Mode
	switch {
	case p.stopped:
		mode = "stopped"
	case !p.cfg.Enabled:
		mode = "disabled"
	}
	_ = p.dryRun.Mark(mode, p.cfg.Routing.Providers, p.decisionErrors)
}

func (p *Plugin) dryRunHeartbeat() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.dryRun == nil {
		return
	}
	p.dryRunMarkLocked()
	if now := p.now(); now.Sub(p.dryRunPruned) >= dryRunPruneEvery {
		if p.dryRun.Prune(now.Add(-dryRunMarkRetention)) == nil {
			p.dryRunPruned = now
		}
	}
}

func (p *Plugin) startDryRunHeartbeat() {
	p.workerWait.Add(1)
	go func() {
		defer p.workerWait.Done()
		ticker := time.NewTicker(dryRunHeartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-p.workerDone:
				return
			case <-ticker.C:
				p.dryRunHeartbeat()
			}
		}
	}()
}

// parseDryRunDay accepts exactly one strict YYYY-MM-DD UTC date from the
// retention range up to today; no parameter means yesterday.
func parseDryRunDay(query url.Values, now time.Time) (time.Time, bool) {
	today := now.UTC().Truncate(24 * time.Hour)
	values, given := query["day"]
	if !given {
		return today.Add(-24 * time.Hour), true
	}
	if len(values) != 1 {
		return time.Time{}, false
	}
	day, err := time.Parse("2006-01-02", values[0])
	if err != nil || day.Format("2006-01-02") != values[0] || day.After(today) || day.Before(today.Add(-dryRunMaxAge)) {
		return time.Time{}, false
	}
	return day, true
}

func (p *Plugin) dryRunReport(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	now := p.now().UTC()
	day, ok := parseDryRunDay(req.Query, now)
	if !ok {
		return resource(400, map[string]string{"error": "invalid_day", "message": "day must be a single YYYY-MM-DD UTC date from the last 31 days, up to today"})
	}
	p.mu.Lock()
	ledger, db, store, gate := p.dryRun, p.db, p.measurement, p.reportSlots
	creds := make([]credential, 0, len(p.creds))
	for _, c := range p.creds {
		creds = append(creds, c)
	}
	reports := make(map[string]measurement.Report, len(p.measurements))
	for account, r := range p.measurements {
		reports[account] = r
	}
	p.mu.Unlock()
	if ledger == nil || db == nil || store == nil {
		return resource(503, map[string]string{"error": "dry_run_evidence_unavailable"})
	}
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		r := resource(429, map[string]string{"error": "Two report calculations are already running; retry later"})
		r.Headers.Set("Retry-After", "1")
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	in := dryrun.Input{Day: day, Now: now, Secret: ledger.Secret()}
	var err error
	in.Decisions, in.Since, err = readDryRunDecisions(ctx, db, day.Add(-dryrun.MaxLookback), day, day.Add(24*time.Hour), dryRunMaxDecisions)
	if errors.Is(err, errDryRunDayTooLarge) {
		return resource(503, map[string]string{"error": "dry_run_decisions_over_limit"})
	}
	if err != nil {
		return resource(503, map[string]string{"error": "decision_state_unavailable"})
	}
	if in.Coverage, in.ErrorSegments, err = ledger.Coverage(ctx, in.Since, day.Add(24*time.Hour)); err != nil {
		return resource(503, map[string]string{"error": "dry_run_evidence_unavailable"})
	}
	if in.Cycles, err = dryRunCycles(ctx, store, creds, reports, day, now); err != nil {
		return resource(503, map[string]string{"error": "quota_cycles_unavailable"})
	}
	if in.Attempts, in.AttemptsUnavailable, err = dryRunAttempts(ctx, store, in.Cycles); err != nil {
		return resource(503, map[string]string{"error": "usage_records_unavailable"})
	}
	report, err := dryrun.Build(in)
	if err != nil {
		return resource(503, map[string]string{"error": "dry_run_report_unavailable"})
	}
	return resource(200, report)
}

// readDryRunDecisions returns the trace rows in [since,to) in (at, seq) order.
// When more than the read limit exist, the oldest are dropped at a whole
// timestamp and the returned start moves forward; the report day itself must fit.
func readDryRunDecisions(ctx context.Context, db *sql.DB, since, day, to time.Time, limit int) ([]dryrun.Decision, time.Time, error) {
	rows, err := db.QueryContext(ctx, `SELECT seq,at,body FROM pacer_decisions WHERE at>=? AND at<? ORDER BY at DESC,seq DESC LIMIT ?`, since.UnixNano(), to.UnixNano(), limit+1)
	if err != nil {
		return nil, since, err
	}
	defer rows.Close()
	out := []dryrun.Decision{}
	var cut int64
	truncated := false
	for rows.Next() {
		var seq, at int64
		var body []byte
		if err = rows.Scan(&seq, &at, &body); err != nil {
			return nil, since, err
		}
		if len(out) == limit {
			cut, truncated = at, true
			break
		}
		var e decisionEntry
		if err = json.Unmarshal(body, &e); err != nil {
			return nil, since, err
		}
		out = append(out, dryrun.Decision{Seq: seq, At: time.Unix(0, at).UTC(), Chat: e.Chat, RequestID: e.RequestID, Provider: e.Provider, Model: e.Model, PreviousAccount: e.PreviousAccount, ActualAuth: e.ActualAuth, ActualAccount: e.ActualAccount, Proposed: e.Proposed, Outcome: e.Outcome})
	}
	if err = rows.Err(); err != nil {
		return nil, since, err
	}
	if truncated {
		since = time.Unix(0, cut+1).UTC()
		for len(out) > 0 && out[len(out)-1].At.UnixNano() <= cut {
			out = out[:len(out)-1]
		}
		if since.After(day) {
			return nil, since, errDryRunDayTooLarge
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, since, nil
}

// dryRunCycles collects every quota cycle that ended in the day: recorded
// closures, and cycles whose provider deadline passed without a closure yet.
func dryRunCycles(ctx context.Context, store *measurement.Store, creds []credential, reports map[string]measurement.Report, day, now time.Time) ([]dryrun.Cycle, error) {
	end := day.Add(24 * time.Hour)
	closures, err := store.Closures(ctx, dryRunMaxClosures)
	if err != nil {
		return nil, err
	}
	closed := map[measurement.CycleRef]time.Time{}
	type pool struct{ provider, account string }
	pools := map[pool]bool{}
	for _, c := range closures {
		closed[c.CycleRef] = c.At
		pools[pool{c.Provider, c.AccountID}] = true
	}
	for _, c := range creds {
		if c.Provider != "" && c.Account != "" {
			pools[pool{c.Provider, c.Account}] = true
		}
	}
	candidates := map[measurement.CycleRef]bool{}
	for ref := range closed {
		candidates[ref] = true
	}
	// Pacer stores readings of the shared "all" pool only (storeObservations).
	readTo := end
	if now.Before(readTo) {
		readTo = now
	}
	for pl := range pools {
		refs, err := store.ObservedCycles(ctx, pl.provider, pl.account, "all", day.Add(-dryrun.MaxLookback), readTo, dryRunMaxCycles)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			candidates[ref] = true
		}
	}
	out := []dryrun.Cycle{}
	for ref := range candidates {
		c := dryrun.Cycle{Provider: ref.Provider, Account: ref.AccountID, Scope: ref.Scope, Seconds: int64(ref.Duration / time.Second), CycleID: ref.CycleID, Closed: closed[ref]}
		// The observer names a cycle by its provider reset deadline.
		if scheduled, err := time.Parse(time.RFC3339Nano, ref.CycleID); err == nil {
			c.Scheduled = scheduled.UTC()
		}
		at, _, ok := c.Reset()
		if !ok || at.Before(day) || !at.Before(end) || at.After(now) {
			continue
		}
		last, found, err := store.LastReading(ctx, ref, at)
		if err != nil {
			return nil, err
		}
		if found {
			c.Last = &dryrun.Reading{At: last.At, UsedPercent: last.UsedPercent}
		}
		c.Ratio = dryRunRatio(reports, ref)
		out = append(out, c)
	}
	return out, nil
}

// dryRunRatio is the cycle's own measured dollars per percent from the latest
// background measurement; the report never measures on its own.
func dryRunRatio(reports map[string]measurement.Report, ref measurement.CycleRef) dryrun.Ratio {
	report, ok := reports[ref.AccountID]
	if !ok {
		return dryrun.Ratio{Reasons: []string{"measurement_unavailable"}}
	}
	for _, w := range report.Windows {
		if w.CycleRef != ref {
			continue
		}
		if !w.Eligible {
			reasons := append([]string(nil), w.Reasons...)
			if len(reasons) == 0 {
				reasons = []string{"window_ineligible"}
			}
			return dryrun.Ratio{Reasons: reasons}
		}
		return dryrun.Ratio{Known: true, USDPerPercent: w.USDPerPercent}
	}
	return dryrun.Ratio{Reasons: []string{"cycle_not_measured"}}
}

// dryRunAttempts prices the usage records that can affect a projection: those
// started within an hour of a reported cycle. Exceeding the read limit makes
// every projection unknown rather than silently partial.
func dryRunAttempts(ctx context.Context, store *measurement.Store, cycles []dryrun.Cycle) ([]dryrun.Attempt, string, error) {
	type span struct{ from, to time.Time }
	spans := map[string]span{}
	for _, c := range cycles {
		at, _, _ := c.Reset()
		if c.Scheduled.IsZero() || c.Seconds <= 0 {
			continue
		}
		start := c.Scheduled.Add(-time.Duration(c.Seconds) * time.Second)
		s, ok := spans[c.Provider]
		if !ok || start.Before(s.from) {
			s.from = start
		}
		if !ok || at.After(s.to) {
			s.to = at
		}
		spans[c.Provider] = s
	}
	book := measurement.DefaultPriceBook()
	out := []dryrun.Attempt{}
	for provider, s := range spans {
		remaining := dryRunMaxAttempts - len(out)
		if remaining < 1 {
			return nil, "usage_records_over_limit", nil
		}
		records, err := store.RecordsStarted(ctx, provider, s.from.Add(-time.Hour), s.to.Add(time.Hour), remaining)
		if errors.Is(err, measurement.ErrLimit) {
			return nil, "usage_records_over_limit", nil
		}
		if err != nil {
			return nil, "", err
		}
		for _, r := range records {
			v := measurement.ValueRecord(r, book)
			out = append(out, dryrun.Attempt{RequestID: r.RequestID, Account: r.Attribution.AccountID, StartedAt: r.StartedAt, EndedAt: r.EndedAt, NanoUSD: v.LowNanoUSD, Exact: v.Exact()})
		}
	}
	return out, "", nil
}
