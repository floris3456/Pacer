package pacer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

// ForwardRuntimeStatus is detached bounded metadata for a future authorized v8
// adapter. It is deliberately absent from legacy management registration/state.
type ForwardRuntimeStatus struct {
	State             string    `json:"state"`
	Active            bool      `json:"active"`
	Phase             string    `json:"phase"`
	SourceHead        int64     `json:"source_head,string"`
	Projected         int64     `json:"projected,string"`
	BootstrapComplete bool      `json:"bootstrap_complete"`
	Latest            string    `json:"latest_generation,omitempty"`
	ReportAt          time.Time `json:"report_at,omitzero"`
	TargetAt          time.Time `json:"target_at,omitzero"`
	ErrorCode         string    `json:"error_code,omitempty"`
	Failures          uint64    `json:"failures,string"`
	Capacity          string    `json:"capacity"`
	// Present only for an accepted prepared baseline store: H, N0, an optional
	// declared coverage gap, and the latest sealed generation's cutover counts.
	Cutover *historyreports.ForwardCutoverStatus `json:"cutover,omitempty"`
}

var ErrForwardUnavailable = errors.New("forward reports unavailable")

type forwardOptions struct {
	beforeTurn                              func(context.Context)
	ticks                                   <-chan time.Time // optional deterministic test clock; production owns ticker
	batchTimeout, sealTimeout, retry, yield time.Duration
	seal                                    func(context.Context, *historyreports.ForwardStore, historyreports.ForwardSeal) (historyreports.ForwardPin, error)
}

func defaultForwardOptions() forwardOptions {
	return forwardOptions{batchTimeout: 2 * time.Second, sealTimeout: 2 * time.Minute, retry: 2 * time.Second, yield: 2 * time.Millisecond, seal: func(ctx context.Context, s *historyreports.ForwardStore, q historyreports.ForwardSeal) (historyreports.ForwardPin, error) {
		return s.SealForward(ctx, q)
	}}
}

type forwardPublisher struct {
	plugin           *Plugin
	store            *historyreports.ForwardStore
	feed             *measurement.Store
	consumer         string
	book             string
	now              func() time.Time
	opts             forwardOptions
	slots            chan struct{}
	wake             chan struct{}
	ctx              context.Context
	cancel           context.CancelFunc
	life             sync.Mutex
	started, stopped bool
	wg               sync.WaitGroup
	closeOnce        sync.Once
	mu               sync.Mutex
	status           ForwardRuntimeStatus
	changed          chan struct{}
	// The following cursor/state is owned exclusively by the one worker.
	bootstrap             bool
	cycle                 *forwardCycle
	proofCursor           int64
	tuples                map[forwardTuple]time.Time
	omitted, unattributed bool
	maxStart              time.Time
	lastFingerprint       string
}

func newForwardPublisher(p *Plugin, s *historyreports.ForwardStore, book string) *forwardPublisher {
	ctx, cancel := context.WithCancel(context.Background())
	opts := defaultForwardOptions()
	if p.forwardOptions != nil {
		opts = *p.forwardOptions
	}
	return &forwardPublisher{plugin: p, store: s, book: book, now: p.now, opts: opts, slots: p.reportSlots, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, changed: make(chan struct{}), status: ForwardRuntimeStatus{State: "initializing", Capacity: "unavailable"}, tuples: map[forwardTuple]time.Time{}}
}

func (p *Plugin) openForward(cfg Config) (*forwardPublisher, error) {
	if cfg.ForwardReportPath == "" {
		if cfg.ForwardReportBook != "" {
			return nil, errors.New("forward_report_book requires forward_report_path")
		}
		return nil, nil
	}
	if err := historyreports.CheckForwardPath(cfg.ForwardReportPath, cfg.StatePath, cfg.HistoryPath, cfg.ReportPath); err != nil {
		return nil, errors.New("forward_report_path must be a distinct canonical regular file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := historyreports.OpenPreparedForward(ctx, cfg.ForwardReportPath)
	if err != nil {
		return nil, errors.New("configured forward store unavailable; initialize it offline first")
	}
	if _, err = s.NativeForwardSelection(ctx, cfg.ForwardReportBook); err != nil {
		s.Close()
		return nil, errors.New("configured forward book or native-only preparation is unavailable")
	}
	status, at, err := s.PreparedForwardHead(ctx)
	if err != nil {
		s.Close()
		return nil, errors.New("configured forward head is unavailable")
	}
	w := newForwardPublisher(p, s, cfg.ForwardReportBook)
	w.status.Latest = status.Latest
	w.status.ReportAt = at
	w.status.Projected = status.Through
	return w, nil
}

func (p *Plugin) startForward() {
	p.mu.Lock()
	w := p.forward
	if w == nil || p.stopped {
		p.mu.Unlock()
		return
	}
	w.life.Lock()
	if w.started || w.stopped {
		w.life.Unlock()
		p.mu.Unlock()
		return
	}
	w.feed = p.measurement
	if p.coverage != nil {
		w.consumer = p.coverage.ID()
	}
	w.started = true
	w.wg.Add(1)
	w.life.Unlock()
	p.mu.Unlock()
	go w.run()
}

// A hint only. The existing atomic feed is authoritative even when later
// physical persistence/envelope decode fails and usage.handle returns an error.
func (p *Plugin) WakeForward() {
	p.mu.Lock()
	w := p.forward
	p.mu.Unlock()
	if w != nil {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}
func (p *Plugin) ForwardStatus() ForwardRuntimeStatus {
	p.mu.Lock()
	w := p.forward
	p.mu.Unlock()
	if w == nil {
		return ForwardRuntimeStatus{State: "disabled", Capacity: "unavailable"}
	}
	v, _ := w.snapshot()
	return v
}
func (w *forwardPublisher) snapshot() (ForwardRuntimeStatus, <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, w.changed
}
func (w *forwardPublisher) update(fn func(*ForwardRuntimeStatus)) {
	w.mu.Lock()
	fn(&w.status)
	close(w.changed)
	w.changed = make(chan struct{})
	w.mu.Unlock()
}
func (w *forwardPublisher) stop() {
	w.life.Lock()
	w.stopped = true
	w.cancel()
	w.life.Unlock()
	w.wg.Wait()
	w.update(func(s *ForwardRuntimeStatus) { s.State = "stopped"; s.Active = false; s.Phase = "" })
}
func (w *forwardPublisher) close() { w.closeOnce.Do(func() { w.stop(); _ = w.store.Close() }) }
func (p *Plugin) stopForward(closeStore bool) {
	p.mu.Lock()
	w := p.forward
	p.mu.Unlock()
	if w != nil {
		if closeStore {
			w.close()
		} else {
			w.stop()
		}
	}
}

// WithForwardReports shares the existing two-slot report admission and leases
// the owned store until fn returns. fn must honor its cancelled context and must
// not retain the store pointer. No store/database reference escapes as a return.
func (p *Plugin) WithForwardReports(ctx context.Context, fn func(context.Context, *historyreports.ForwardStore) error) error {
	if fn == nil {
		return ErrForwardUnavailable
	}
	p.mu.Lock()
	w := p.forward
	p.mu.Unlock()
	if w == nil {
		return ErrForwardUnavailable
	}
	w.life.Lock()
	if w.stopped {
		w.life.Unlock()
		return ErrForwardUnavailable
	}
	w.wg.Add(1)
	w.life.Unlock()
	defer w.wg.Done()
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	unhook := context.AfterFunc(w.ctx, cancel)
	defer unhook()
	select {
	case w.slots <- struct{}{}:
		defer func() { <-w.slots }()
	case <-c.Done():
		return c.Err()
	}
	if err := c.Err(); err != nil {
		return err
	}
	return fn(c, w.store)
}
