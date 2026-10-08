package pacer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestForwardStartupDeferredAndFixedTHorizon(t *testing.T) {
	f := preparedRuntime(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	p := f.open(t, func(o *forwardOptions) {
		o.beforeTurn = func(ctx context.Context) {
			once.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if n := hookCount(t, p, `SELECT complete FROM measurement_report_meta WHERE id=1`); n != 0 {
		t.Fatal("startup performed bootstrap")
	}
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "future", c.ID)
	invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "future", "future-start", hookEpoch))
	close(release)
	first := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	old := forwardReportAt(t, p, first.Latest)
	if old.Totals.Rows != 0 || !first.ReportAt.Equal(hookEpoch) {
		t.Fatal("fixed T ignored", old.Totals, first.ReportAt)
	}
	f.h.clock.Add(int64(2 * time.Second))
	f.ticks <- f.h.now()
	later := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active && v.Latest != first.Latest })
	if later.Projected != first.Projected {
		t.Fatal("fixture requires same durable head")
	}
	if r := forwardReportAt(t, p, later.Latest); r.Totals.Rows != 1 || r.Totals.SelectionValue.Low != 725e7 {
		t.Fatal("post-T fact discarded for lifetime", r.Totals)
	}
	if r := forwardReportAt(t, p, first.Latest); r.Totals.Rows != 0 {
		t.Fatal("saved horizon advanced")
	}
}

func TestForwardErrorBackoffKeepsOldPinAndIngestion(t *testing.T) {
	f := preparedRuntime(t)
	var fail atomic.Bool
	var attempts atomic.Int64
	p := f.open(t, func(o *forwardOptions) {
		o.retry = time.Hour
		o.seal = func(ctx context.Context, s *historyreports.ForwardStore, q historyreports.ForwardSeal) (historyreports.ForwardPin, error) {
			if fail.Load() {
				attempts.Add(1)
				return historyreports.ForwardPin{}, errors.New("sensitive-sql-path")
			}
			return s.SealForward(ctx, q)
		}
	})
	initial := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	fail.Store(true)
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "backoff", c.ID)
	f.h.clock.Add(int64(2 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "backoff", "one", hookEpoch))
	broken := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "error" })
	if broken.Latest != initial.Latest || broken.ErrorCode != "forward_seal_failed" || broken.Active {
		t.Fatal(broken)
	}
	for i := 0; i < 1000; i++ {
		p.WakeForward()
	}
	if len(p.forward.wake) != 1 || attempts.Load() != 1 {
		t.Fatal("error hot-loop/unbounded wake", len(p.forward.wake), attempts.Load())
	}
	admitCoverage(t, p, "second", c.ID)
	invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "second", "two", hookEpoch))
	if n := hookCount(t, p, `SELECT count(*) FROM measurement_records`); n != 2 {
		t.Fatal("publisher failure lost usage", n)
	}
	if r := forwardReportAt(t, p, initial.Latest); r.Totals.Rows != 0 {
		t.Fatal("old sealed report changed")
	}
	invoke(t, p, pluginabi.MethodPluginQuiesce, struct{}{})
	if attempts.Load() != 1 {
		t.Fatal("wake bypassed error backoff")
	}
}

func TestForwardCloseCancelsAndJoinsReadLeaseBeforeDatabaseClose(t *testing.T) {
	f := preparedRuntime(t)
	p := f.open(t, nil)
	ready := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	entered := make(chan struct{})
	release := make(chan struct{})
	cancelled := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- p.WithForwardReports(context.Background(), func(ctx context.Context, s *historyreports.ForwardStore) error {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			select {
			case <-release:
			case <-time.After(5 * time.Second):
				return errors.New("test release timeout")
			}
			q, c := context.WithTimeout(context.Background(), time.Second)
			defer c()
			v, e := s.Status(q)
			if e != nil {
				return e
			}
			if v.Latest != ready.Latest {
				return errors.New("read lease changed generation")
			}
			return nil
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("read lease not cancelled")
	}
	select {
	case <-closed:
		t.Fatal("closed store before lease returned")
	default:
	}
	close(release)
	select {
	case e := <-readDone:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("Close did not join")
	}
}

func TestForwardReopenExposesOldHeadDuringPausedBootstrap(t *testing.T) {
	f := preparedRuntime(t)
	p := f.open(t, nil)
	before := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	p.Close()
	release := make(chan struct{})
	p = f.open(t, func(o *forwardOptions) {
		o.beforeTurn = func(ctx context.Context) {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	})
	v := p.ForwardStatus()
	if v.Latest != before.Latest || !v.ReportAt.Equal(before.ReportAt) {
		t.Fatal("reopen hid retained head", v, before)
	}
	if r := forwardReportAt(t, p, v.Latest); r.Totals.Rows != 0 {
		t.Fatal(r)
	}
	close(release)
}

func TestForwardBootstrapOneBatchPerTurnResumesAfterQuiesce(t *testing.T) {
	f := preparedRuntime(t)
	start := make(chan struct{})
	paused := make(chan struct{})
	var turns atomic.Int64
	p := f.open(t, func(o *forwardOptions) {
		o.beforeTurn = func(ctx context.Context) {
			switch turns.Add(1) {
			case 1:
				select {
				case <-start:
				case <-ctx.Done():
				}
			case 2:
				close(paused)
				<-ctx.Done()
			}
		}
	})
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "bootstrap", c.ID)
	f.h.clock.Add(int64(2 * time.Second))
	for i := 0; i < 251; i++ {
		invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "bootstrap", fmt.Sprintf("bootstrap-%03d", i), hookEpoch))
	}
	if hookCount(t, p, `SELECT complete FROM measurement_report_meta WHERE id=1`) != 0 {
		t.Fatal("startup or callback bootstrapped")
	}
	close(start)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var after string
	if e := p.db.QueryRow(`SELECT after_event FROM measurement_report_meta WHERE id=1`).Scan(&after); e != nil || after == "" {
		t.Fatal("batch progress not durable", e)
	}
	if hookCount(t, p, `SELECT complete FROM measurement_report_meta WHERE id=1`) != 0 || p.ForwardStatus().Latest != "" {
		t.Fatal("one batch consumed >250 or exposed partial report")
	}
	invoke(t, p, pluginabi.MethodPluginQuiesce, struct{}{})
	p.Close()
	p = f.open(t, nil)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	r := forwardReportAt(t, p, "")
	if r.Totals.Rows != 251 || r.Totals.SelectionValue == nil || r.Totals.SelectionValue.Low != 251*int64(725e7) {
		t.Fatal("journal resume lost attempts", r.Totals)
	}
}

func TestForwardComputeUsesSharedTwoSlotAdmission(t *testing.T) {
	f := preparedRuntime(t)
	p := f.open(t, nil)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	entered := make(chan struct{}, 2)
	finished := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			finished <- p.WithForwardReports(context.Background(), func(ctx context.Context, _ *historyreports.ForwardStore) error {
				entered <- struct{}{}
				<-ctx.Done()
				return nil
			})
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "slots", c.ID)
	f.h.clock.Add(int64(2 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "slots", "slots", hookEpoch))
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.Active && v.Phase == "seal" })
	if len(p.reportSlots) != 2 {
		t.Fatal("publisher exceeded report admissions")
	}
	invoke(t, p, pluginabi.MethodPluginQuiesce, struct{}{})
	for i := 0; i < 2; i++ {
		select {
		case e := <-finished:
			if e != nil {
				t.Fatal(e)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if len(p.reportSlots) != 0 {
		t.Fatal("report admission leaked")
	}
}
