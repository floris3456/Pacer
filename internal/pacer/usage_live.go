package pacer

// The Usage Quota page's continuous record (docs/usage-unification.md): after
// the import boundary H, Pacer's own requests (forward store) and meter
// readings (observation store) continue every account with CPAMP's derivation.
// One background loop follows both stores and rebuilds the live facts outside
// the report slots; usage reads stay in memory.

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
)

const (
	usageLiveEvery   = 30 * time.Second
	usageLiveRefresh = 5 * time.Minute // staleness marks move even without new evidence
	usageLivePage    = 5000
)

type usageLive struct {
	mu sync.Mutex
	s  usageLiveState // guarded by mu; replaced whole on a new artifact
}

type usageLiveState struct {
	reader        *historyreports.Reader
	h             time.Time
	events        map[string]historyreports.ForwardEvent // highest revision per event ID
	through       int64
	origins       historyreports.ForwardOrigins
	readings      []historyreports.NativeReading
	readingsAfter int64
	dirty         bool
	built         time.Time
	next          int64
	status        historyreports.LiveStatus
}

func (p *Plugin) startUsageLive() {
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
			}
		}
	}()
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

// refreshUsageLive follows both stores and rebuilds the live facts when
// evidence arrived, a window opened or reset, or usageLiveRefresh passed.
func (p *Plugin) refreshUsageLive(ctx context.Context) error {
	p.mu.Lock()
	reader, observer, source := p.reports, p.observer, p.measurementSource
	labels := map[string]string{}
	for _, c := range p.creds {
		if c.Account != "" && c.Alias != "" {
			labels[c.Account] = c.Alias
		}
	}
	p.mu.Unlock()
	p.usageLive.mu.Lock()
	defer p.usageLive.mu.Unlock()
	u := &p.usageLive.s
	if reader == nil {
		*u = usageLiveState{status: historyreports.LiveStatus{State: "off"}}
		return nil
	}
	if u.reader != reader {
		*u = usageLiveState{reader: reader, events: map[string]historyreports.ForwardEvent{}, dirty: true}
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
	err := p.withForwardStore(ctx, func(c context.Context, s *historyreports.ForwardStore) error {
		for {
			page, err := s.UsageNative(c, u.h, u.through, usageLivePage)
			if err != nil {
				return err
			}
			u.origins, u.through = page.Origins, page.Through
			for _, e := range page.Events {
				if old, ok := u.events[e.ID]; !ok || e.Revision > old.Revision {
					u.events[e.ID] = e
					u.dirty = true
				}
			}
			if !page.More {
				return nil
			}
		}
	})
	if err != nil && !errors.Is(err, errUsageExclusionsUnavailable) {
		return fail(err)
	}
	if observer != nil {
		for {
			page, err := observer.HistorySince(u.readingsAfter, usageLivePage)
			if err != nil {
				return fail(err)
			}
			for _, e := range page {
				u.readingsAfter = e.ID
				if r, ok := nativeReading(e); ok && !r.At.Before(u.h) {
					u.readings = append(u.readings, r)
					u.dirty = true
				}
			}
			if len(page) < usageLivePage {
				break
			}
		}
	}
	now := p.now().UTC()
	due := u.dirty || u.built.IsZero() || u.next != 0 && now.UnixMilli() >= u.next || now.Sub(u.built) >= usageLiveRefresh
	if !due {
		return nil
	}
	if len(u.events) == 0 && len(u.readings) == 0 {
		reader.UseLive(nil)
		u.dirty, u.built, u.next = false, now, 0
		u.status = historyreports.LiveStatus{State: "waiting", ImportedThroughMS: u.h.UnixMilli(), BuiltAtMS: now.UnixMilli()}
		return nil
	}
	events := make([]historyreports.ForwardEvent, 0, len(u.events))
	for _, e := range u.events {
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	facts, stats, err := reader.BuildLive(ctx, historyreports.LiveInput{Now: now, Native: historyreports.UsageNative{Events: events, Origins: u.origins}, Readings: u.readings, Source: source, Labels: labels})
	if err != nil {
		return fail(err)
	}
	reader.UseLive(facts)
	u.dirty, u.built, u.next = false, now, facts.NextTransition
	u.status = historyreports.LiveStatus{State: "live", ImportedThroughMS: u.h.UnixMilli(), BuiltAtMS: now.UnixMilli(), Stats: stats}
	return nil
}
