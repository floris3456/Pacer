package pacer

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
)

type forwardTuple struct{ membership, provider, account string }
type forwardCycle struct {
	selection historyreports.ForwardSeal
	phase     string
	keys      []forwardTuple
	next      int
	proof     historyreports.ForwardProvenance
}

func (w *forwardPublisher) run() {
	defer w.wg.Done()
	ticks := w.opts.ticks
	var ticker *time.Ticker
	if ticks == nil {
		ticker = time.NewTicker(30 * time.Second)
		ticks = ticker.C
		defer ticker.Stop()
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.wake:
		case <-ticks:
		case <-timer.C:
		}
		if w.ctx.Err() != nil {
			return
		}
		if w.opts.beforeTurn != nil {
			w.opts.beforeTurn(w.ctx)
		}
		if w.ctx.Err() != nil {
			return
		}
		w.update(func(s *ForwardRuntimeStatus) {
			s.Active = true
			if s.State == "error" {
				if w.bootstrap {
					s.State = "lag"
				} else {
					s.State = "initializing"
				}
			}
		})
		more, code := w.turn()
		if w.ctx.Err() != nil {
			return
		}
		w.update(func(s *ForwardRuntimeStatus) {
			s.Active = false
			if code != "" {
				s.State = "error"
				s.ErrorCode = code
				if s.Failures < ^uint64(0) {
					s.Failures++
				}
			} else {
				s.ErrorCode = ""
			}
		})
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if code != "" {
			// Drain coalesced hints before backoff; new hints cannot hot-loop a
			// persistent error. All durable changes are retried after this pause.
			wait := time.NewTimer(w.opts.retry)
			select {
			case <-w.ctx.Done():
				wait.Stop()
				return
			case <-wait.C:
			}
			select {
			case <-w.wake:
			default:
			}
			timer.Reset(0)
		} else if more {
			timer.Reset(w.opts.yield)
		}
	}
}

func (w *forwardPublisher) turn() (bool, string) {
	ctx, cancel := context.WithTimeout(w.ctx, w.opts.batchTimeout)
	defer cancel()
	if !w.bootstrap {
		w.update(func(s *ForwardRuntimeStatus) { s.State = "initializing"; s.Phase = "bootstrap" })
		done, err := w.feed.BootstrapReportFeed(ctx, measurement.ReportBudget{})
		if err != nil {
			return false, "forward_bootstrap_failed"
		}
		w.bootstrap = done
		head, e := w.feed.ReportFeedHead(ctx)
		if e != nil {
			return false, "forward_source_unavailable"
		}
		w.update(func(s *ForwardRuntimeStatus) { s.BootstrapComplete = done })
		w.update(func(s *ForwardRuntimeStatus) { s.SourceHead = head.Through })
		return true, ""
	}
	if w.cycle == nil {
		head, err := w.feed.ReportFeedHead(ctx)
		if err != nil {
			return false, "forward_source_unavailable"
		}
		selection, err := w.store.NativeForwardSelection(ctx, w.book)
		if err != nil {
			return false, "forward_selection_unavailable"
		}
		status, err := w.store.Status(ctx)
		if err != nil {
			return false, "forward_store_unavailable"
		}
		selection.Head = head
		selection.At = w.now().Round(0).UTC()
		w.cycle = &forwardCycle{selection: selection, phase: "projection", proof: historyreports.ForwardProvenance{ConsumerID: w.consumer, Reasons: []string{"native_runtime_recorded_only", "physical_coverage_unverified", "outside_usage_unknown"}}}
		w.update(func(s *ForwardRuntimeStatus) {
			s.State = "lag"
			s.Phase = "projection"
			s.SourceHead = head.Through
			s.Projected = status.Through
			s.Latest = status.Latest
			s.TargetAt = selection.At
		})
		return true, ""
	}
	c := w.cycle
	switch c.phase {
	case "projection":
		done, err := w.store.AdvanceForward(ctx, w.feed, c.selection.Head, measurement.ReportBudget{})
		if err != nil {
			return false, "forward_projection_failed"
		}
		status, err := w.store.Status(ctx)
		if err != nil {
			return false, "forward_store_unavailable"
		}
		w.update(func(s *ForwardRuntimeStatus) { s.Projected = status.Through })
		if done {
			c.phase = "provenance_feed"
		}
		return true, ""
	case "provenance_feed":
		w.update(func(s *ForwardRuntimeStatus) { s.Phase = c.phase })
		page, err := w.feed.ReadReportFeed(ctx, c.selection.Head, w.proofCursor, measurement.ReportBudget{})
		if err != nil {
			return false, "forward_provenance_feed_failed"
		}
		for _, entry := range page.Entries {
			r := entry.Record
			if r.StartedAt.After(w.maxStart) {
				w.maxStart = r.StartedAt
			}
			if r.Native == nil || r.Native.SourceMembershipID == "" {
				w.unattributed = true
				continue
			}
			key := forwardTuple{r.Native.SourceMembershipID, r.Attribution.Provider, r.Attribution.AccountID}
			from := r.RequestedAt
			if from.IsZero() {
				from = r.StartedAt
			}
			old, exists := w.tuples[key]
			if !exists && len(w.tuples) >= 128 {
				w.omitted = true
				continue
			}
			if !exists || from.Before(old) {
				w.tuples[key] = from
			}
		}
		w.proofCursor = page.Next
		if page.Done {
			for key := range w.tuples {
				c.keys = append(c.keys, key)
			}
			sort.Slice(c.keys, func(i, j int) bool {
				a, b := c.keys[i], c.keys[j]
				if a.membership != b.membership {
					return a.membership < b.membership
				}
				if a.provider != b.provider {
					return a.provider < b.provider
				}
				return a.account < b.account
			})
			if w.omitted {
				c.proof.Reasons = append(c.proof.Reasons, "source_membership_limit")
			}
			if w.unattributed {
				c.proof.Reasons = append(c.proof.Reasons, "source_membership_absent")
			}
			c.phase = "provenance"
		}
		return true, ""
	case "provenance":
		w.update(func(s *ForwardRuntimeStatus) { s.Phase = c.phase })
		// One exact local snapshot per turn. No discovery or checkpoint RPC.
		if c.next < len(c.keys) {
			key := c.keys[c.next]
			from := w.tuples[key]
			through := c.selection.At
			if !from.Before(through) {
				through = from.Add(time.Nanosecond)
			}
			v, err := w.feed.SourceSnapshot(ctx, w.consumer, key.membership, key.provider, key.account, from, through)
			if err != nil {
				c.proof.Reasons = append(c.proof.Reasons, "source_snapshot_unavailable")
			} else {
				candidate := append(c.proof.Snapshots, v)
				raw, e := json.Marshal(candidate)
				if e != nil || len(raw) > 512<<10 {
					c.proof.Reasons = append(c.proof.Reasons, "source_snapshot_limit")
				} else {
					c.proof.Snapshots = candidate
				}
			}
			c.next++
			return true, ""
		}
		w.plugin.mu.Lock()
		faults := w.plugin.coverageStats.Errors
		w.plugin.mu.Unlock()
		if faults > 0 {
			c.proof.Reasons = append(c.proof.Reasons, "local_source_fault")
		}
		sort.Strings(c.proof.Reasons)
		unique := c.proof.Reasons[:0]
		for _, reason := range c.proof.Reasons {
			if len(unique) == 0 || unique[len(unique)-1] != reason {
				unique = append(unique, reason)
			}
		}
		c.proof.Reasons = unique
		c.selection.Provenance = c.proof
		c.phase = "seal"
		return true, ""
	case "seal":
		encoded, _ := json.Marshal(struct {
			Head    measurement.ReportFeedHead
			Book    string
			Profile int64
			Proof   historyreports.ForwardProvenance
		}{c.selection.Head, c.selection.BookDigest, c.selection.ProfileRevision, c.selection.Provenance})
		fingerprint := tracker.Digest(string(encoded))
		status, _ := w.snapshot()
		if fingerprint == w.lastFingerprint && (w.maxStart.Before(status.ReportAt) || !c.selection.At.After(status.ReportAt)) {
			w.cycle = nil
			w.update(func(s *ForwardRuntimeStatus) { s.State = "ready"; s.Phase = ""; s.TargetAt = time.Time{} })
			return false, ""
		}
		w.update(func(s *ForwardRuntimeStatus) { s.Phase = "seal" })
		// Do not inherit the short batch deadline: 100k sealing is measured ~31s
		// with -race. This work remains cancellable and owns one shared slot.
		buildCtx, stop := context.WithTimeout(w.ctx, w.opts.sealTimeout)
		defer stop()
		select {
		case w.slots <- struct{}{}:
			defer func() { <-w.slots }()
		case <-buildCtx.Done():
			return false, "forward_compute_unavailable"
		}
		pin, err := w.opts.seal(buildCtx, w.store, c.selection)
		if err != nil {
			return false, "forward_seal_failed"
		}
		w.lastFingerprint = fingerprint
		w.cycle = nil
		w.update(func(s *ForwardRuntimeStatus) {
			s.State = "ready"
			s.Phase = ""
			s.Latest = pin.Generation
			s.ReportAt = c.selection.At
			s.TargetAt = time.Time{}
		})
		// Recheck the durable head after publication without creating an idle
		// self-wake loop for records with future starts relative to frozen T.
		recheck, done := context.WithTimeout(w.ctx, w.opts.batchTimeout)
		defer done()
		head, e := w.feed.ReportFeedHead(recheck)
		if e != nil {
			return false, "forward_source_unavailable"
		}
		if head.Through > c.selection.Head.Through {
			w.update(func(s *ForwardRuntimeStatus) { s.State = "lag"; s.SourceHead = head.Through })
			select {
			case w.wake <- struct{}{}:
			default:
			}
		}
		return false, ""
	}
	return false, "forward_phase_invalid"
}
