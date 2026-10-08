package usagecoverage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func physicalResponse(id string, seq, lower uint64, through time.Time, initial bool) pluginapi.HostUsageCheckpointResponse {
	r := response(id, seq)
	r.Through = at(through)
	r.Physical = &pluginapi.UsagePhysicalCheckpoint{Version: 1, Domain: pluginapi.ClaudePhysicalProofDomain, Coverage: "known", FromCut: &pluginapi.UsagePhysicalCut{Sequence: lower, At: epoch}, ThroughCut: &pluginapi.UsagePhysicalCut{Sequence: seq, At: through}}
	if initial {
		r.Physical.Coverage = "unknown"
		r.Physical.Reason = "first_quiescent_cut"
		r.Physical.ThroughCut = nil
	}
	return r
}
func physicalSelection(f *fixture, m string, r pluginapi.HostUsageCheckpointResponse) measurement.PhysicalSelection {
	cut := func(v *pluginapi.UsagePhysicalCut) measurement.PhysicalCutIdentity {
		return measurement.PhysicalCutIdentity{SourceID: "host-installation", ConsumerID: f.c.ID(), MembershipID: m, SourceRef: measurement.SourceReference("host-installation", scope(r.Scope)), ManagerID: r.ManagerIncarnationID, SinkID: r.IncarnationID, Sequence: v.Sequence, At: v.At}
	}
	return measurement.PhysicalSelection{From: cut(r.Physical.FromCut), Through: cut(r.Physical.ThroughCut)}
}
func physicalUsage(t *testing.T, event, id string, admission, dispatch uint64) ([]byte, Capture) {
	t.Helper()
	raw, capture := rawUsage(event, id, epoch.Add(time.Second))
	var m map[string]json.RawMessage
	must(t, json.Unmarshal(raw, &m))
	var e pluginapi.UsageSourceEvidence
	must(t, json.Unmarshal(m["SourceEvidence"], &e))
	e.Physical = &pluginapi.UsagePhysicalDispatch{Version: 1, AdmissionSequence: admission, DispatchSequence: dispatch, StartedAt: e.UpstreamStartedAt.Add(time.Nanosecond)}
	m["SourceEvidence"], _ = json.Marshal(e)
	raw, err := json.Marshal(m)
	must(t, err)
	return raw, capture
}
func physicalSetup(t *testing.T) (*fixture, Membership) {
	t.Helper()
	f := newFixture(t)
	m := membership("members-a", "a", "a")
	m.Complete = false
	m.Through = m.From
	must(t, f.c.PutMembership(ctx, m))
	return f, m
}
func selected(t *testing.T, f *fixture, s measurement.PhysicalSelection) measurement.PhysicalResult {
	t.Helper()
	v, err := f.c.SelectPhysical(ctx, s)
	must(t, err)
	return v
}
func beginPhysical(t *testing.T, f *fixture, m Membership, seq uint64) {
	t.Helper()
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), physicalResponse("a", seq, seq, epoch, true)))
}

func TestPhysicalExactPointMembershipSelectionAndImmutableDispatch(t *testing.T) {
	f, m := physicalSetup(t)
	beginPhysical(t, f, m, 10)
	raw, capture := physicalUsage(t, "attempt-a", "a", 11, 12)
	observed, err := f.c.ObserveUsageWithRecord(ctx, raw, capture)
	must(t, err)
	r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	s := physicalSelection(f, m.ID, r)
	got := selected(t, f, s)
	if !got.Known || len(got.Facts) != 1 {
		t.Fatalf("exact selection: %+v", got)
	}
	fact := got.Facts[0]
	if fact.EventID != observed.Record.EventID || fact.ConsumerID != f.c.ID() || fact.AccountID != "a" || fact.CredentialRef != "credential-a" || fact.DispatchSequence != 12 || fact.AdmissionSequence != 11 || !fact.StartedAt.Equal(epoch.Add(time.Second+2*time.Nanosecond)) {
		t.Fatalf("incorrect immutable join: %+v", fact)
	}
	if observed.Record.Native.BillingMappingVerified || !observed.Record.StartedAt.Equal(epoch.Add(time.Second+time.Nanosecond)) || observed.Record.EndTimeBasis != "source_final_usage_observation" {
		t.Fatal("physical payload changed accounting clocks/mapping", observed.Record)
	}
	ss := snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Minute))
	if ss.Known || ss.PhysicalCarryInProven {
		t.Fatal("point membership promoted time coverage", ss)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "PRIVATE-") || strings.Contains(string(b), "final_usage_at") {
		t.Fatal("selection exported identity or invented end")
	}
	for _, mutate := range []func(*measurement.PhysicalSelection){func(s *measurement.PhysicalSelection) { s.Through.SinkID = "other" }, func(s *measurement.PhysicalSelection) { s.From.ConsumerID = "other" }, func(s *measurement.PhysicalSelection) { s.From.At = s.From.At.Add(time.Nanosecond) }, func(s *measurement.PhysicalSelection) { s.From.Sequence++ }, func(s *measurement.PhysicalSelection) { s.Through.Sequence-- }} {
		bad := s
		mutate(&bad)
		if selected(t, f, bad).Known {
			t.Fatal("arbitrary cut identity was accepted")
		}
	}
	// Replay cannot duplicate or reattribute the persisted physical event.
	must(t, func() error { _, err := f.c.ObserveUsage(ctx, raw, capture); return err }())
	if again := selected(t, f, s); !again.Known || len(again.Facts) != 1 || again.Facts[0] != fact {
		t.Fatal("replay changed physical evidence", again)
	}
}

func TestPhysicalZeroIsLocalOnlyAndKnownNeedsObservedInitialCut(t *testing.T) {
	for _, initial := range []bool{false, true} {
		f, m := physicalSetup(t)
		if initial {
			beginPhysical(t, f, m, 10)
		}
		r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
		must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
		v := selected(t, f, physicalSelection(f, m.ID, r))
		if v.Known != initial || len(v.Facts) != 0 {
			t.Fatal(initial, v)
		}
		if snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Minute)).PhysicalCarryInProven {
			t.Fatal("local zero claimed account/time proof")
		}
	}
}

func TestPhysicalOptionalInvalidOrMissingPreservesPrimaryUsage(t *testing.T) {
	for _, mode := range []string{"missing", "version", "negative", "overflow", "float", "string", "duplicate", "reversed-clock", "gap", "failed"} {
		t.Run(mode, func(t *testing.T) {
			f, m := physicalSetup(t)
			beginPhysical(t, f, m, 10)
			raw, capture := physicalUsage(t, "optional-"+mode, "a", 11, 12)
			var body map[string]json.RawMessage
			must(t, json.Unmarshal(raw, &body))
			var e map[string]json.RawMessage
			must(t, json.Unmarshal(body["SourceEvidence"], &e))
			switch mode {
			case "missing":
				delete(e, "physical")
			case "version":
				e["physical"] = []byte(`{"version":2}`)
			case "negative":
				e["physical"] = []byte(`{"version":1,"dispatch_sequence":-1}`)
			case "overflow":
				e["physical"] = []byte(`{"version":1,"dispatch_sequence":18446744073709551615}`)
			case "float":
				e["physical"] = []byte(`{"version":1,"dispatch_sequence":12.5}`)
			case "string":
				e["physical"] = []byte(`"bad optional extension"`)
			case "duplicate":
				e["physical"] = []byte(`{"version":1,"dispatch_sequence":12,"dispatch_sequence":13}`)
			case "reversed-clock":
				var p pluginapi.UsagePhysicalDispatch
				must(t, json.Unmarshal(e["physical"], &p))
				p.StartedAt = epoch
				e["physical"], _ = json.Marshal(p)
			case "gap":
				e["gap"], _ = json.Marshal(pluginapi.UsageDeliveryGap{Sequence: 13, Reason: "late", ObservedAt: epoch.Add(time.Second)})
			case "failed":
				body["Failed"] = []byte(`true`)
			}
			body["SourceEvidence"], _ = json.Marshal(e)
			raw, _ = json.Marshal(body)
			o, err := f.c.ObserveUsageWithRecord(ctx, raw, capture)
			must(t, err)
			stored, err := f.store.Record(ctx, o.Record.EventID, 1)
			must(t, err)
			if stored.AttemptID != "optional-"+mode || stored.Usage.NonCacheInput != 250000 {
				t.Fatal("primary usage dropped or altered", stored)
			}
			r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
			must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
			if selected(t, f, physicalSelection(f, m.ID, r)).Known {
				t.Fatal("optional invalid data gained proof")
			}
		})
	}
}

func TestPhysicalLateEntireIncarnationRevocationPrecedesStaleOrder(t *testing.T) {
	for _, mode := range []string{"physical-gap", "top-truncated", "equal-conflict", "unknown-version", "oversized-gaps", "oversized-domain"} {
		t.Run(mode, func(t *testing.T) {
			f, m := physicalSetup(t)
			beginPhysical(t, f, m, 10)
			r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
			must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
			s := physicalSelection(f, m.ID, r)
			if !selected(t, f, s).Known {
				t.Fatal("initial edge unavailable")
			}
			bad := physicalResponse("a", 19, 10, epoch.Add(time.Minute), false)
			switch mode {
			case "oversized-gaps":
				bad.Gaps = make([]pluginapi.UsageDeliveryGap, 1000)
			case "oversized-domain":
				bad.Physical.Domain = strings.Repeat("private-invalid-domain", 50)
			case "physical-gap":
				bad.Physical.Coverage = "unknown"
				bad.Physical.ThroughCut = nil
				bad.Physical.GapCount = 1
			case "top-truncated":
				bad.Coverage = "unknown"
				bad.Through = nil
				bad.GapCount = 100
				bad.GapsTruncated = true
				bad.Physical.GapCount = 100
				bad.Physical.Coverage = "unknown"
				bad.Physical.ThroughCut = nil
			case "equal-conflict":
				bad.Sequence = 20
				bad.Physical.ThroughCut.Sequence = 20
				bad.Physical.ThroughCut.At = bad.Physical.ThroughCut.At.Add(time.Nanosecond)
			case "unknown-version":
				bad.Physical.Version = 55
			}
			err := f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), bad)
			if err != nil && !errors.Is(err, measurement.ErrStaleSource) && !errors.Is(err, measurement.ErrConflict) && !errors.Is(err, measurement.ErrInvalid) {
				t.Fatal(err)
			}
			if selected(t, f, s).Known {
				t.Fatal("late negative did not revoke earlier proof")
			}
			later := physicalResponse("a", 30, 10, epoch.Add(2*time.Minute), false)
			must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), later))
			if selected(t, f, physicalSelection(f, m.ID, later)).Known {
				t.Fatal("later known healed gap")
			}
		})
	}
}

func TestPhysicalSequencesRemainExactAndSignedOverflowRefuses(t *testing.T) {
	f, m := physicalSetup(t)
	const lower uint64 = 1<<53 + 101
	beginPhysical(t, f, m, lower)
	raw, capture := physicalUsage(t, "large-sequence", "a", lower+1, lower+2)
	_, err := f.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	r := physicalResponse("a", lower+3, lower, epoch.Add(time.Minute), false)
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	v := selected(t, f, physicalSelection(f, m.ID, r))
	if !v.Known || len(v.Facts) != 1 || v.Facts[0].DispatchSequence != lower+2 {
		t.Fatal("lossy sequence", v)
	}
	r.Sequence = math.MaxUint64
	r.Physical.ThroughCut.Sequence = r.Sequence
	if err = f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r); !errors.Is(err, measurement.ErrInvalid) {
		t.Fatal("signed overflow not rejected", err)
	}
	old := physicalResponse("a", lower+3, lower, epoch.Add(time.Minute), false)
	if selected(t, f, physicalSelection(f, m.ID, old)).Known {
		t.Fatal("overflow retained physical proof")
	}
}

func TestPhysicalMissingJoinedRowsCannotBeMistakenForZero(t *testing.T) {
	for _, table := range []string{"measurement_record_revisions", "measurement_records", "measurement_physical_records"} {
		t.Run(table, func(t *testing.T) {
			f, m := physicalSetup(t)
			beginPhysical(t, f, m, 10)
			raw, capture := physicalUsage(t, "missing-row", "a", 11, 12)
			o, err := f.c.ObserveUsageWithRecord(ctx, raw, capture)
			must(t, err)
			r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
			must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
			s := physicalSelection(f, m.ID, r)
			if !selected(t, f, s).Known {
				t.Fatal("baseline unavailable")
			}
			_, err = f.db.Exec(`DELETE FROM `+table+` WHERE event_id=?`, o.Record.EventID)
			must(t, err)
			if selected(t, f, s).Known {
				t.Fatal("missing row interpreted as zero")
			}
			r = physicalResponse("a", 30, 10, epoch.Add(2*time.Minute), false)
			must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
			if selected(t, f, physicalSelection(f, m.ID, r)).Known {
				t.Fatal("new cut hid missing row")
			}
		})
	}
}

func TestPhysicalChangedReplayAndDispatchCollisionRevokeWithoutLosingPrimary(t *testing.T) {
	for _, collision := range []bool{false, true} {
		f, m := physicalSetup(t)
		beginPhysical(t, f, m, 10)
		raw, capture := physicalUsage(t, "original", "a", 11, 12)
		_, err := f.c.ObserveUsage(ctx, raw, capture)
		must(t, err)
		r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
		must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
		s := physicalSelection(f, m.ID, r)
		if !selected(t, f, s).Known {
			t.Fatal("original join missing")
		}
		if collision {
			raw, capture = physicalUsage(t, "other-event", "a", 11, 12)
		} else {
			raw, capture = physicalUsage(t, "original", "a", 11, 13)
		}
		o, err := f.c.ObserveUsageWithRecord(ctx, raw, capture)
		must(t, err)
		if _, err = f.store.Record(ctx, o.Record.EventID, 1); err != nil {
			t.Fatal("conflicting optional proof dropped primary", err)
		}
		if selected(t, f, s).Known {
			t.Fatal("changed/colliding physical identity retained coverage")
		}
	}
}

func TestPhysicalConcurrentUsageAndOutOfOrderGapCannotRepublishKnown(t *testing.T) {
	f, m := physicalSetup(t)
	beginPhysical(t, f, m, 10)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		raw, capture := physicalUsage(t, fmt.Sprintf("concurrent-%d", i), "a", uint64(11+i*2), uint64(12+i*2))
		wg.Add(1)
		go func() { defer wg.Done(); _, err := f.c.ObserveUsage(ctx, raw, capture); errs <- err }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent physical ingestion blocked")
	}
	for range n {
		must(t, <-errs)
	}
	r := physicalResponse("a", 100, 10, epoch.Add(time.Minute), false)
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	s := physicalSelection(f, m.ID, r)
	v := selected(t, f, s)
	if !v.Known || len(v.Facts) != n {
		t.Fatal("acknowledged concurrent joins incomplete", v)
	}
	for i, fact := range v.Facts {
		if fact.DispatchSequence != uint64(12+i*2) || fact.ConsumerID != f.c.ID() {
			t.Fatal("physical ordering lost", fact)
		}
	}
	negative := physicalResponse("a", 99, 10, epoch.Add(time.Minute), false)
	negative.Physical.Coverage = "unknown"
	negative.Physical.ThroughCut = nil
	negative.Physical.GapCount = 1
	later := physicalResponse("a", 101, 10, epoch.Add(2*time.Minute), false)
	start := make(chan struct{})
	for _, reply := range []pluginapi.HostUsageCheckpointResponse{negative, later} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), reply)
			errs <- err
		}()
	}
	close(start)
	done = make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent checkpoint blocked")
	}
	for range 2 {
		err := <-errs
		if err != nil && !errors.Is(err, measurement.ErrStaleSource) {
			t.Fatal(err)
		}
	}
	if selected(t, f, s).Known || selected(t, f, physicalSelection(f, m.ID, later)).Known {
		t.Fatal("concurrent newer success erased late gap")
	}
}

func TestPhysicalRestartRetainsHistoryNeverBorrowsAndOldGapStaysAtOrigin(t *testing.T) {
	f, m := physicalSetup(t)
	beginPhysical(t, f, m, 10)
	r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	old := physicalSelection(f, m.ID, r)
	must(t, f.db.Close())
	f2 := openFixture(t, f.path, epoch.Add(2*time.Minute))
	history, err := f2.store.SelectPhysical(ctx, old)
	must(t, err)
	if !history.Known {
		t.Fatal("historical proof lost", history)
	}
	if selected(t, f2, old).Known {
		t.Fatal("new listener borrowed history")
	}
	// The same sink cannot assign its earlier lower cut to this new listener.
	_ = f2.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r)
	borrow := physicalSelection(f2, m.ID, r)
	if selected(t, f2, borrow).Known {
		t.Fatal("new consumer reused sink")
	}
	initial := physicalResponse("a", 50, 50, epoch.Add(3*time.Minute), true)
	initial.ManagerIncarnationID = "manager-two"
	initial.IncarnationID = "sink-two"
	initial.Physical.FromCut.At = epoch.Add(3 * time.Minute)
	must(t, f2.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), initial))
	next := initial
	next.Sequence = 60
	next.Through = at(epoch.Add(4 * time.Minute))
	cp := *initial.Physical
	cp.Coverage = "known"
	cp.Reason = ""
	cp.ThroughCut = &pluginapi.UsagePhysicalCut{Sequence: 60, At: *next.Through}
	next.Physical = &cp
	must(t, f2.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), next))
	current := physicalSelection(f2, m.ID, next)
	if !selected(t, f2, current).Known {
		t.Fatal("new independent epoch unavailable")
	}
	raw, capture := physicalUsage(t, "old-gap", "a", 11, 12)
	var wire map[string]json.RawMessage
	must(t, json.Unmarshal(raw, &wire))
	var e pluginapi.UsageSourceEvidence
	must(t, json.Unmarshal(wire["SourceEvidence"], &e))
	e.Gap = &pluginapi.UsageDeliveryGap{Reason: "late", ObservedAt: epoch.Add(time.Minute)}
	wire["SourceEvidence"], _ = json.Marshal(e)
	raw, _ = json.Marshal(wire)
	_, err = f2.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	history, err = f2.store.SelectPhysical(ctx, old)
	must(t, err)
	if history.Known {
		t.Fatal("old-origin proof not revoked")
	}
	if !selected(t, f2, current).Known {
		t.Fatal("old manager gap poisoned unrelated current manager")
	}
}

func TestPhysicalSQLFailureIsNotAcknowledgedAndLatches(t *testing.T) {
	f, m := physicalSetup(t)
	beginPhysical(t, f, m, 10)
	_, err := f.db.Exec(`CREATE TRIGGER physical_failure BEFORE INSERT ON measurement_physical_records BEGIN SELECT RAISE(ABORT,'dummy physical failure'); END`)
	must(t, err)
	raw, capture := physicalUsage(t, "failed-store", "a", 11, 12)
	if _, err = f.c.ObserveUsage(ctx, raw, capture); err == nil {
		t.Fatal("physical persistence failure acknowledged")
	}
	_, err = f.db.Exec(`DROP TRIGGER physical_failure`)
	must(t, err)
	_, err = f.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
	err = f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r)
	if err != nil && !errors.Is(err, measurement.ErrStaleSource) {
		t.Fatal(err)
	}
	if selected(t, f, physicalSelection(f, m.ID, r)).Known {
		t.Fatal("write failure was healed by retry")
	}
}

func TestPhysicalUnseenCallbackBehindAnyCutRevokesOrigin(t *testing.T) {
	for _, mode := range []string{"initial-lower", "known-upper", "later-listener"} {
		t.Run(mode, func(t *testing.T) {
			f, m := physicalSetup(t)
			beginPhysical(t, f, m, 10)
			r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
			s := physicalSelection(f, m.ID, r)
			if mode != "initial-lower" {
				must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
				if !selected(t, f, s).Known {
					t.Fatal("baseline proof missing")
				}
			}
			if mode == "later-listener" {
				must(t, f.db.Close())
				f = openFixture(t, f.path, epoch.Add(2*time.Minute))
			}
			admission, dispatch := uint64(11), uint64(12)
			if mode == "initial-lower" {
				admission, dispatch = 8, 9
			}
			raw, capture := physicalUsage(t, "unseen-"+mode, "a", admission, dispatch)
			o, err := f.c.ObserveUsageWithRecord(ctx, raw, capture)
			must(t, err)
			if _, err = f.store.Record(ctx, o.Record.EventID, 1); err != nil {
				t.Fatal("late evidence lost accounting", err)
			}
			if mode == "initial-lower" {
				must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
			}
			v, err := f.store.SelectPhysical(ctx, s)
			must(t, err)
			if v.Known {
				t.Fatal("unseen callback was excluded as apparent silence", mode, v)
			}
			var gaps int
			must(t, f.db.QueryRow(`SELECT count(*) FROM measurement_physical_gaps WHERE source_ref=? AND manager_id=? AND sink_id=''`, s.From.SourceRef, s.From.ManagerID).Scan(&gaps))
			if gaps != 1 {
				t.Fatal("origin manager was not durably revoked", gaps)
			}
		})
	}
}

func TestPhysicalEqualClocksStaySequenceOrderedAndRegressionRevokes(t *testing.T) {
	f, m := physicalSetup(t)
	beginPhysical(t, f, m, 10)
	equal := physicalResponse("a", 11, 10, epoch, false)
	equal.Physical.Coverage = "unknown"
	equal.Physical.ThroughCut = nil
	equal.Physical.Reason = "nonpositive_cut_interval"
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), equal))
	positive := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
	before := physicalSelection(f, m.ID, positive)
	if selected(t, f, before).Known {
		t.Fatal("elapsed time without exact host edge became proof")
	}
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), positive))
	if !selected(t, f, before).Known {
		t.Fatal("equal-clock unknown destroyed sequence-ordered later edge")
	}
	regressed := physicalResponse("a", 21, 10, epoch.Add(30*time.Second), false)
	err := f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), regressed)
	if err != nil && !errors.Is(err, measurement.ErrConflict) {
		t.Fatal(err)
	}
	if selected(t, f, before).Known {
		t.Fatal("increasing sequence healed regressing cut clock")
	}
}

func TestPhysicalProducerGapAcrossSinkReloadRevokesEarlierSink(t *testing.T) {
	f, m := physicalSetup(t)
	beginPhysical(t, f, m, 10)
	r := physicalResponse("a", 20, 10, epoch.Add(time.Minute), false)
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	old := physicalSelection(f, m.ID, r)
	if !selected(t, f, old).Known {
		t.Fatal("old sink missing baseline")
	}
	initial := physicalResponse("a", 30, 30, epoch.Add(2*time.Minute), true)
	initial.Physical.FromCut.At = epoch.Add(2 * time.Minute)
	initial.IncarnationID = "sink-two"
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), initial))
	gap := initial
	gap.Sequence = 31
	cp := *initial.Physical
	cp.FromCut = nil
	cp.Reason = "physical_incarnation_gap"
	cp.GapCount = 1
	gap.Physical = &cp
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), gap))
	if selected(t, f, old).Known {
		t.Fatal("sink reload isolated an origin-wide producer gap")
	}
}
