package pacer

// The Usage Quota page's continuous record (docs/usage-record.md): after the
// import boundary H, Pacer's own requests (forward store, and the usage-only
// providers' requests) and meter readings (observation store) continue every
// account with CPAMP's derivation. One background loop copies new evidence
// into the record file and builds the record; closed windows are sealed on
// disk, so a build derives only what changed. Usage reads stay in memory.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
)

const (
	usageLiveEvery   = 30 * time.Second
	usageLiveRefresh = time.Minute // staleness marks move even without new evidence
	usageLivePage    = 5000
	usageBuildBudget = 20 * time.Second
)

type usageLive struct {
	mu   sync.Mutex
	s    usageLiveState // guarded by mu; replaced whole on a new artifact
	pmu  sync.Mutex
	pend historyreports.Changes // evidence written by callbacks, guarded by pmu
	kick chan struct{}
}

type usageLiveState struct {
	reader  *historyreports.Reader
	h       time.Time
	origins historyreports.ForwardOrigins
	models  map[string]string // model -> provider, for the merged price book
	built   time.Time
	next    int64
	status  historyreports.LiveStatus
	inputs  string // settings the last build used
}

// changed notes evidence written outside the loop.
func (u *usageLive) changed(c historyreports.Changes) {
	if len(c) == 0 {
		return
	}
	u.pmu.Lock()
	if u.pend == nil {
		u.pend = historyreports.Changes{}
	}
	u.pend.Merge(c)
	u.pmu.Unlock()
}

func (u *usageLive) take() historyreports.Changes {
	u.pmu.Lock()
	defer u.pmu.Unlock()
	c := u.pend
	u.pend = nil
	if c == nil {
		c = historyreports.Changes{}
	}
	return c
}

// wake asks the loop to build now (after an edit).
func (p *Plugin) wakeUsageLive() {
	p.usageLive.pmu.Lock()
	k := p.usageLive.kick
	p.usageLive.pmu.Unlock()
	if k != nil {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

func usageRecordPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "usage-record.sqlite")
}

func (p *Plugin) startUsageLive() {
	p.usageLive.pmu.Lock()
	p.usageLive.kick = make(chan struct{}, 1)
	kick := p.usageLive.kick
	p.usageLive.pmu.Unlock()
	p.workerWait.Add(1)
	go func() {
		defer p.workerWait.Done()
		ticker := time.NewTicker(usageLiveEvery)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			_ = p.refreshUsageLive(ctx)
			cancel()
			select {
			case <-p.workerDone:
				return
			case <-ticker.C:
			case <-kick:
			}
		}
	}()
	p.startPriceSync()
}

func (p *Plugin) usageLiveStatus() *historyreports.LiveStatus {
	p.usageLive.mu.Lock()
	defer p.usageLive.mu.Unlock()
	s := p.usageLive.s.status
	if s.State == "" {
		s.State = "starting"
	}
	return &s
}

func nativeReading(e observation.AccountHistoryEntry) (historyreports.NativeReading, bool) {
	w := e.Sample
	if w.Remaining == nil || w.Usage.At.IsZero() || w.Reset.IsZero() {
		return historyreports.NativeReading{}, false
	}
	remaining := *w.Remaining
	return historyreports.NativeReading{ID: e.ID, Account: e.Account, Provider: w.Provider, Scope: w.Scope, Seconds: w.Seconds, Remaining: &remaining, Reset: w.Reset, At: w.Usage.At, Source: w.Usage.Source}, true
}

type recordCursor struct {
	Store string `json:"store"`
	After int64  `json:"after"`
}

func readCursor(ctx context.Context, s *historyreports.RecordStore, name, identity string) (int64, error) {
	raw, ok, err := s.Meta(ctx, name)
	if err != nil || !ok {
		return 0, err
	}
	var c recordCursor
	if json.Unmarshal([]byte(raw), &c) != nil || c.Store != identity {
		return 0, nil // another store: copy it from the start (events are keyed by ID)
	}
	return c.After, nil
}

func writeCursor(ctx context.Context, s *historyreports.RecordStore, name, identity string, after int64) error {
	b, _ := json.Marshal(recordCursor{Store: identity, After: after})
	return s.SetMeta(ctx, name, string(b))
}

// usageSettings are the record's inputs kept in the forward store.
type usageSettings struct {
	profiles historyreports.ForwardProfileConfig
	links    historyreports.AccountLinks
	prices   historyreports.ModelPrices
	sync     historyreports.PriceSync
	ok       bool
}

func (p *Plugin) readUsageSettings(ctx context.Context) (usageSettings, error) {
	out, err := p.readForwardSettings(ctx)
	if err != nil {
		return out, err
	}
	p.mu.Lock()
	store := p.recordStore
	p.mu.Unlock()
	if store != nil {
		if out.sync, err = store.PriceSync(ctx); err != nil {
			return out, err
		}
		out.prices.Catalog, out.prices.SyncedAtMS = out.sync.Catalog, out.sync.SyncedAtMS
	}
	return out, nil
}

func (p *Plugin) readForwardSettings(ctx context.Context) (usageSettings, error) {
	var out usageSettings
	err := p.withForwardStore(ctx, func(c context.Context, s *historyreports.ForwardStore) error {
		var e error
		if out.profiles, e = s.LatestProfiles(c); e != nil {
			return e
		}
		if out.links, e = s.AccountLinks(c); e != nil {
			return e
		}
		out.prices, e = s.ModelPrices(c)
		return e
	})
	if errors.Is(err, errUsageExclusionsUnavailable) {
		return usageSettings{prices: historyreports.ModelPrices{Manual: map[string]historyreports.ForwardModelPrice{}}}, nil
	}
	out.ok = err == nil
	return out, err
}

// refreshUsageLive copies new evidence into the record file and builds the
// record when evidence arrived, a window opens or resets, a throttled segment
// is due, or usageLiveRefresh passed.
func (p *Plugin) refreshUsageLive(ctx context.Context) error {
	p.mu.Lock()
	reader, observer, source, store, record := p.reports, p.observer, p.measurementSource, p.recordStore, p.record
	p.mu.Unlock()
	p.usageLive.mu.Lock()
	defer p.usageLive.mu.Unlock()
	u := &p.usageLive.s
	if reader == nil {
		*u = usageLiveState{status: historyreports.LiveStatus{State: "off"}}
		return nil
	}
	if u.reader != reader {
		*u = usageLiveState{reader: reader}
		u.h = time.UnixMilli(reader.ImportedThrough(ctx)).UTC()
	}
	if u.h.UnixMilli() <= 0 {
		u.status = historyreports.LiveStatus{State: "waiting", Error: "No usage history is prepared yet."}
		return nil
	}
	fail := func(err error) error {
		u.status.State, u.status.Error = "error", "Pacer's own record could not be read; the page shows the last good state."
		return err
	}
	if store == nil || record == nil {
		u.status = historyreports.LiveStatus{State: "error", ImportedThroughMS: u.h.UnixMilli(), Error: "Pacer's usage record file could not be opened; the page shows the imported history only."}
		return nil
	}
	changes := p.usageLive.take()
	keep := func() { p.usageLive.changed(changes) } // retried next time
	// Requests from the forward ledger.
	err := p.withForwardStore(ctx, func(c context.Context, s *historyreports.ForwardStore) error {
		id := s.Identity()
		through, err := readCursor(c, store, "forward_cursor", id)
		if err != nil {
			return err
		}
		for {
			page, err := s.UsageNative(c, u.h, through, usageLivePage)
			if err != nil {
				return err
			}
			u.origins = page.Origins
			events := make([]historyreports.RecordEvent, 0, len(page.Events))
			for _, e := range page.Events {
				events = append(events, historyreports.RecordEventFromForward(e))
				if u.models != nil && e.Event.Model != "" {
					u.models[e.Event.Model] = e.Event.Provider
				}
			}
			ch, err := store.PutEvents(c, events)
			if err != nil {
				return err
			}
			changes.Merge(ch)
			through = page.Through
			if err = writeCursor(c, store, "forward_cursor", id, through); err != nil {
				return err
			}
			if !page.More {
				return nil
			}
		}
	})
	if err != nil && !errors.Is(err, errUsageExclusionsUnavailable) {
		keep()
		return fail(err)
	}
	// Meter readings of every provider.
	if observer != nil {
		after, err := readCursor(ctx, store, "readings_cursor", source)
		if err != nil {
			keep()
			return fail(err)
		}
		for {
			page, err := observer.HistorySince(after, usageLivePage)
			if err != nil {
				keep()
				return fail(err)
			}
			readings, refs := []historyreports.NativeReading{}, []string{}
			for _, e := range page {
				after = e.ID
				if r, ok := nativeReading(e); ok && !r.At.Before(u.h) {
					readings = append(readings, r)
					refs = append(refs, historyreports.ForwardAccountRef(source, r.Provider, r.Account))
				}
			}
			ch, err := store.PutReadings(ctx, readings, refs)
			if err != nil {
				keep()
				return fail(err)
			}
			changes.Merge(ch)
			if err = writeCursor(ctx, store, "readings_cursor", source, after); err != nil {
				keep()
				return fail(err)
			}
			if len(page) < usageLivePage {
				break
			}
		}
	}
	settings, err := p.readUsageSettings(ctx)
	if err != nil {
		keep()
		return fail(err)
	}
	inputs := historyreports.Hash(string(mustJSON([]any{settings.profiles.Revision, settings.links.Revision, settings.prices.Revision, settings.sync.SyncedAtMS, settings.ok})))
	now := p.now().UTC()
	due := len(changes) > 0 || u.built.IsZero() || inputs != u.inputs || u.next != 0 && now.UnixMilli() >= u.next || now.Sub(u.built) >= usageLiveRefresh
	if !due {
		return nil
	}
	importedConfig, importedBook, err := reader.Imported(ctx)
	if err != nil {
		keep()
		return fail(err)
	}
	if u.models == nil {
		models, err := store.Models(ctx)
		if err != nil {
			keep()
			return fail(err)
		}
		imported, err := reader.ImportedModels(ctx)
		if err != nil {
			keep()
			return fail(err)
		}
		for m, provider := range imported {
			if _, ok := models[m]; !ok {
				models[m] = provider
			}
		}
		u.models = models
	}
	config := historyreports.EffectiveProfiles(importedConfig, settings.profiles)
	book := historyreports.MergedBook(importedBook, settings.prices, u.models)
	if u.status.State == "" || u.status.State == "starting" {
		u.status = historyreports.LiveStatus{State: "building", ImportedThroughMS: u.h.UnixMilli()}
	}
	facts, st, err := record.Build(ctx, historyreports.RecordInput{Now: now, Source: source, Origins: u.origins, Config: &config, Links: settings.links.Links, Book: &book, Labels: p.usageLabels(), Changes: changes, Budget: usageBuildBudget})
	if errors.Is(err, historyreports.ErrRecordPending) {
		// Derived segments are kept; the next run continues. The page keeps
		// showing the last complete record.
		if u.built.IsZero() {
			u.status = historyreports.LiveStatus{State: "building", ImportedThroughMS: u.h.UnixMilli(), Record: st}
		}
		return nil
	}
	if err != nil {
		keep()
		return fail(err)
	}
	reader.UseLive(facts)
	p.refreshVerified(ctx, store, record, reader, source)
	u.built, u.next, u.inputs = now, facts.NextTransition, inputs
	if st.WakeMS != 0 && (u.next == 0 || st.WakeMS < u.next) {
		u.next = st.WakeMS
	}
	u.status = historyreports.LiveStatus{State: "live", ImportedThroughMS: u.h.UnixMilli(), BuiltAtMS: now.UnixMilli(), Record: st}
	if !settings.ok {
		u.status.Error = "Profile edits, links and prices need Pacer's usage store; it is not configured."
	}
	return nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// refreshVerified keeps every judged capacity interval (a later judgment can
// revoke one) and marks the record's windows with the proven ones.
func (p *Plugin) refreshVerified(ctx context.Context, store *historyreports.RecordStore, record *historyreports.Record, reader *historyreports.Reader, source string) {
	p.mu.Lock()
	judged := []historyreports.CapacityInterval{}
	for _, c := range p.creds {
		state, ok := p.capacityViews[c.Account]
		if !ok {
			continue
		}
		for _, i := range state.intervals {
			if i.From.IsZero() || i.To.IsZero() {
				continue
			}
			lower := 0.0
			if i.Value != nil {
				lower = i.Value.LowerBoundUSDPerPercent
			}
			judged = append(judged, historyreports.CapacityInterval{Native: c.Account, Provider: c.Provider, Window: i.Window, FromMS: i.From.UnixMilli(), ToMS: i.To.UnixMilli(), Status: i.Status, LowerUSDPerPercent: lower})
		}
	}
	p.mu.Unlock()
	if err := store.PutCapacity(ctx, judged); err != nil {
		p.metricError(err)
		return
	}
	proven, err := store.Capacity(ctx)
	if err != nil {
		p.metricError(err)
		return
	}
	reader.UseVerifications(record.Verifications(source, proven))
}
