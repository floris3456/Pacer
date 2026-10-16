package pacer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/usagecoverage"
)

func (p *Plugin) openMeasurements(db *sql.DB) (*measurement.Store, error) {
	ctx := context.Background()
	// Finish the independent compiled namespace before Open establishes the
	// feed's durable schema witness. Existing guarded stores still reject drift;
	// this ordering does not refresh or repair a previously recorded witness.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pacer_usage_source(id INTEGER PRIMARY KEY CHECK(id=1), source TEXT NOT NULL); CREATE TABLE IF NOT EXISTS pacer_usage_credentials(request_id TEXT NOT NULL,auth TEXT NOT NULL,body BLOB NOT NULL,PRIMARY KEY(request_id,auth)); CREATE TABLE IF NOT EXISTS pacer_usage_capture_metadata(request_id TEXT NOT NULL,auth TEXT NOT NULL,body BLOB NOT NULL,PRIMARY KEY(request_id,auth))`); err != nil {
		return nil, err
	}
	// This process's tables above, and any table a newer Pacer version added
	// before this one, changed the DDL generation the feed records as its
	// schema witness. Re-anchor the witness before Open checks it; the feed
	// objects themselves must still be intact, so a database a DDL runaway left
	// inconsistent still refuses.
	if err := measurement.SettleReportFeedSchema(ctx, db); err != nil {
		return nil, err
	}
	m, err := measurement.Open(ctx, db, measurement.Options{Clock: p.now})
	if err != nil {
		return nil, err
	}
	var source [16]byte
	if _, err = rand.Read(source[:]); err != nil {
		return nil, err
	}
	if _, err = db.Exec(`INSERT INTO pacer_usage_source(id,source) VALUES(1,?) ON CONFLICT(id) DO NOTHING`, hex.EncodeToString(source[:])); err != nil {
		return nil, err
	}
	if err = db.QueryRow(`SELECT source FROM pacer_usage_source WHERE id=1`).Scan(&p.measurementSource); err != nil {
		return nil, err
	}
	if err = m.RegisterPriceBook(ctx, measurement.DefaultPriceBook()); err != nil {
		return nil, err
	}
	// The native path has its own source-scoped incarnation. Legacy global runs
	// are never started, checkpointed or consulted by this integration.
	p.coverage, err = usagecoverage.New(ctx, m, usagecoverage.Options{SourceID: p.measurementSource, Clock: p.now})
	if err != nil {
		return nil, err
	}
	p.coverageGroups = map[string]coverageGroup{}
	return m, nil
}

func (p *Plugin) captureUsageCredential(request string, c credential) error {
	err := p.captureUsageCredentialTx(request, c)
	if err != nil {
		p.coverageFailure("capture_unavailable", c.Account)
	}
	return err
}

type usageCaptureMetadata struct {
	Version         int
	MembershipID    string
	AccountVerified bool
	ExactIdentity   bool
	CapturedAt      time.Time
}

func (p *Plugin) captureUsageCredentialTx(request string, c credential) error {
	if request == "" || len(request) > 256 || c.ID == "" || len(c.ID) > 512 || c.Index == "" || len(c.Index) > 128 {
		return errors.New("invalid usage correlation")
	}
	value := measurement.NativeCredential{Provider: c.Provider, AccountID: c.Account, CredentialRef: tracker.Digest(c.Provider + "\x00" + c.ID + "\x00" + c.Index), AuthID: c.ID, AuthIndex: c.Index}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tx, err := p.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO pacer_usage_credentials(request_id,auth,body) VALUES(?,?,?) ON CONFLICT(request_id,auth) DO NOTHING`, request, c.ID, raw)
	if err != nil {
		return err
	}
	var stored []byte
	if err = tx.QueryRow(`SELECT body FROM pacer_usage_credentials WHERE request_id=? AND auth=?`, request, c.ID).Scan(&stored); err != nil {
		return err
	}
	var original measurement.NativeCredential
	if json.Unmarshal(stored, &original) != nil || original != value {
		return errors.New("credential identity changed within logical dispatch")
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 1 {
		meta := usageCaptureMetadata{Version: 1, MembershipID: c.CoverageMembership, AccountVerified: c.AccountVerified, ExactIdentity: c.ExactIdentity, CapturedAt: p.now().Round(0).UTC()}
		body, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO pacer_usage_capture_metadata VALUES(?,?,?)`, request, c.ID, body); err != nil {
			return err
		}
	}
	// Existing legacy captures deliberately remain without verified metadata.
	return tx.Commit()
}

// meterRereadTokens: a request that processed this many input tokens without a
// cache read moves a 5-hour meter by a few points (a 574k-token cache write
// moved one by 10 on 2026-10-15). Its own reply showed the meter from before,
// so the account's meter is read again shortly after (observation.Spend).
const meterRereadTokens = 100_000

func (p *Plugin) recordUsage(raw []byte) error {
	_, err := p.recordUsageCaptured(raw)
	return err
}

func (p *Plugin) recordUsageCaptured(raw []byte) (captured credential, err error) {
	defer func() {
		p.clearMeasurementCache()
		if err != nil {
			p.metricError(err)
			p.coverageFailure("usage_unrecorded", captured.Account)
		}
	}()
	if len(raw) == 0 || len(raw) > 8<<20 {
		return captured, errors.New("invalid native usage envelope")
	}
	var identity struct {
		LifecycleRequestID string
		AuthID             string
		AuthIndex          string
		Provider           string
		SourceEvidence     json.RawMessage
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		return captured, errors.New("invalid native usage envelope")
	}
	if identity.Provider == "" {
		return captured, errors.New("native usage provider missing")
	}
	if identity.Provider != "claude" && identity.Provider != "codex" {
		if len(identity.SourceEvidence) > 0 && string(identity.SourceEvidence) != "null" {
			return captured, errors.New("unsupported native source")
		}
		// Outside Pacer's source contract; recorded for the usage page only.
		p.recordUsageOnly(raw, identity.AuthID, identity.AuthIndex, identity.Provider)
		return captured, nil
	}
	// Older hosts cannot correlate final callback to the immutable per-request
	// inventory. No mutable-discovery fallback: its record would be misattributed.
	if identity.LifecycleRequestID == "" {
		return captured, errors.New("native usage correlation missing")
	}
	var body, metadata []byte
	err = p.db.QueryRow(`SELECT c.body,m.body FROM pacer_usage_credentials c LEFT JOIN pacer_usage_capture_metadata m ON m.request_id=c.request_id AND m.auth=c.auth WHERE c.request_id=? AND c.auth=?`, identity.LifecycleRequestID, identity.AuthID).Scan(&body, &metadata)
	if err != nil {
		return captured, err
	}
	var capture usagecoverage.Capture
	if err = json.Unmarshal(body, &capture.Credential); err != nil {
		return captured, errors.New("native usage capture invalid")
	}
	captured = credential{ID: capture.Credential.AuthID, Index: capture.Credential.AuthIndex, Provider: capture.Credential.Provider, Account: capture.Credential.AccountID}
	if len(metadata) > 0 {
		var meta usageCaptureMetadata
		if json.Unmarshal(metadata, &meta) != nil || meta.Version != 1 {
			return captured, errors.New("native usage capture metadata invalid")
		}
		capture.MembershipID = meta.MembershipID
		capture.Verified = meta.AccountVerified && meta.ExactIdentity
		captured.AccountVerified = meta.AccountVerified
		captured.ExactIdentity = meta.ExactIdentity
		captured.CoverageMembership = meta.MembershipID
	}
	if p.coverage == nil {
		return captured, errors.New("native source consumer unavailable")
	}
	observed, err := p.coverage.ObserveUsageWithRecord(context.Background(), raw, capture)
	if err != nil {
		return captured, err
	}
	record := observed.Record
	if record.Attribution.AccountID != captured.Account {
		return captured, errors.New("persisted native attribution mismatch")
	}
	if p.observer != nil && record.Final && record.Success && record.Usage.NonCacheInput+record.Usage.CacheCreate >= meterRereadTokens {
		p.metricError(p.observer.Spend(captured.Account, captured.Provider, p.now()))
	}
	if p.policyEvidence != nil && record.Policy.SelectedTTL != "" {
		_, evidenceErr := p.policyEvidence.Observe(context.Background(), record)
		p.metricError(evidenceErr) // Optional projection cannot acknowledge delivery.
	}
	// Optional projection cannot acknowledge delivery; the tracker below is untouched by it.
	p.observePrefix(record)
	if !record.Activity.Partial {
		err = p.chats.CacheActivityForAuth(record.RequestID, identity.AuthID, record.StartedAt, record.Success, record.Activity.PrimaryPrefix, record.Activity.ProtectionTTL.Duration())
	}
	return captured, err
}

func (p *Plugin) observeUsageCaptured(c credential, headers http.Header, clocks map[string]time.Time, requested time.Time) {
	if c.Account == "" || p.observer == nil {
		return
	}
	var samples []observation.Window
	if len(clocks) > 0 {
		p.observeClaudeExtraCaptured(c, headers, clocks)
		samples = observation.HeadersCaptured(c.Provider, headers, clocks)
	} else if !requested.IsZero() {
		p.observeClaudeExtra(c, headers, requested)
		samples = observation.Headers(c.Provider, headers, requested)
	}
	if len(samples) > 0 {
		p.storeObservations(c, samples)
	}
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
	if p.windowStarts != nil && c.Provider == "claude" {
		p.metricError(p.windowStarts.Confirm(c.Account, after, p.now()))
	}
	// Audit-only replays and refused stale evidence leave the canonical view
	// unchanged. Include evidence times and status, not just quota values:
	// a fresh reading can make an unchanged balance usable again.
	if !reflect.DeepEqual(before, after) {
		p.usageChanges.notify()
	}
	if usageOnlyProvider(c.Provider) {
		return // usage page only: no measurement report, no routing evidence
	}
	previous := map[string]observation.Window{}
	for _, w := range before {
		previous[w.Key()] = w
	}
	for _, w := range after {
		// Named scopes need exact catalogue membership, not guessed substrings.
		if w.Scope != "all" {
			continue
		}
		// A window that reset and has not started has no cycle yet; the one it
		// replaced is closed.
		if old := previous[w.Key()]; w.Dormant && !old.Dormant && old.Cycle != "" {
			p.metricError(p.measurement.CloseCycle(context.Background(), measurement.Closure{CycleRef: measurement.CycleRef{Provider: c.Provider, AccountID: c.Account, Scope: w.Scope, CycleID: old.Cycle, Duration: time.Duration(old.Seconds) * time.Second}, At: w.StatusEvidence.At, Source: "pacer-observer:" + w.StatusEvidence.Source}))
			continue
		}
		if w.Remaining == nil || w.Cycle == "" || w.Usage.At.IsZero() {
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
	p.refreshSourceCoverage()
	p.mu.Lock()
	generation := p.coverageGeneration
	consumer := p.coverage
	memberships := map[string]string{}
	for account, g := range p.coverageGroups {
		memberships[account] = g.MembershipID
	}
	creds := map[string]credential{}
	for _, c := range p.creds {
		creds[c.Account] = c
	}
	p.mu.Unlock()
	reports := map[string]measurement.Report{}
	accounts := make([]string, 0, len(creds))
	for account := range creds {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		c := creds[account]
		q := measurement.Query{Provider: c.Provider, AccountID: account, Scope: "all", BookID: measurement.DefaultPriceBook().ID, Since: p.now().Add(-21 * 24 * time.Hour), AsOf: p.now(), MaxRecords: 10000, RequireSourceCoverage: true}
		if consumer != nil {
			var err error
			q, err = consumer.BeforeMeasure(context.Background(), q, memberships)
			if err != nil {
				p.metricError(err)
				continue
			}
		}
		r, err := p.measurement.Measure(context.Background(), q)
		if err != nil {
			p.metricError(err)
			continue
		}
		reports[account] = r
	}
	p.mu.Lock()
	if generation == p.coverageGeneration {
		p.measurements = reports
	}
	p.mu.Unlock()
	p.refreshCapacity()
}
