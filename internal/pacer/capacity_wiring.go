package pacer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/capacity"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/usagecoverage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Capacity lower bounds (docs/capacity-lower-bound.md). Pacer brackets the
// observer's own scheduled provider read with host inventory and quiescent-cut
// reads, and the background worker later judges stored sample pairs. Nothing
// here makes a provider call, changes routing or the measurement report, or
// knows an account size: every result stays capacity_known=false.

// Enough samples for the newest interval and HistoryLimit earlier ones in each
// of the two shared windows.
const capacitySampleLimit = 2 * (capacity.HistoryLimit + 2)

type capacityState struct {
	intervals []capacity.Interval
	reasons   []string
}

func capacityEvidencePath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "capacity-evidence.sqlite")
}

// openCapacityEvidence opens the ledger in its own file, so the guarded state
// database keeps its schema. Any failure returns (nil, nil): capacity is then
// unknown, and startup, routing and measurement are unaffected.
func openCapacityEvidence(statePath string, now func() time.Time) (*sql.DB, *capacity.Store) {
	if statePath == "" {
		return nil, nil
	}
	db := openOwnerOnlySQLite(capacityEvidencePath(statePath))
	if db == nil {
		return nil, nil
	}
	store, err := capacity.Open(db, now)
	if err != nil {
		db.Close()
		return nil, nil
	}
	return db, store
}

type capacityTuple struct {
	ref   string
	scope pluginapi.UsageSourceScope
}

// capacitySandwich is one bracketing of one scheduled provider read.
type capacitySandwich struct {
	store               *capacity.Store
	consumer            *usagecoverage.Consumer
	host                Host
	account, membership string
	tuples              []capacityTuple
	sample              capacity.Sample
	skip                bool
}

// capacityBefore reads inventory c1 and cut a for every Pacer tuple of the
// account, just before the provider read. An old host without the inventory
// RPC gets no cut reads: membership can never hold there.
func (p *Plugin) capacityBefore(c credential) *capacitySandwich {
	if c.Provider != "claude" {
		return nil
	}
	p.mu.Lock()
	s := &capacitySandwich{store: p.capacityStore, consumer: p.coverage, host: p.Host, account: c.Account}
	group, grouped := p.coverageGroups[c.Account]
	p.mu.Unlock()
	if s.store == nil {
		return nil
	}
	s.membership = group.MembershipID
	s.sample.Account = c.Account
	refs := []string{}
	for _, scope := range group.Members {
		ref := capacity.TupleRef(scope.AuthID, scope.AuthIndex)
		s.tuples = append(s.tuples, capacityTuple{ref, scope})
		refs = append(refs, ref)
	}
	switch {
	case !grouped || s.consumer == nil || len(s.tuples) == 0:
		s.sample.Reasons = append(s.sample.Reasons, "pacer_membership_unavailable")
	case len(s.tuples) > maxCoverageCalls:
		s.sample.Reasons = append(s.sample.Reasons, "pacer_membership_limit")
	}
	before := p.capacityInventory(s)
	s.sample.Before = before.ID
	if before.Reason == "inventory_unsupported" {
		s.skip = true
		s.sample.After = before.ID
		s.sample.Reasons = append(s.sample.Reasons, "inventory_unsupported")
		return s
	}
	s.sample.AccountRef, _ = capacity.ResolveAccountRef(before, refs)
	if len(s.sample.Reasons) == 0 {
		s.sample.A = p.capacityCuts(s)
	}
	return s
}

// capacityAfter reads cut b for every tuple, then inventory c2.
func (p *Plugin) capacityAfter(s *capacitySandwich) {
	if s == nil || s.skip {
		return
	}
	if len(s.sample.Reasons) == 0 {
		s.sample.B = p.capacityCuts(s)
	}
	s.sample.After = p.capacityInventory(s).ID
}

// capacityInventory stores every inventory read, known or not. Host failures
// are expected on old hosts and are not measurement errors.
func (p *Plugin) capacityInventory(s *capacitySandwich) capacity.Checkpoint {
	var raw json.RawMessage
	err := errors.New("host unavailable")
	if s.host != nil {
		raw, err = s.host(pluginabi.MethodHostAuthInventoryCheckpoint, pluginapi.HostAuthInventoryCheckpointRequest{Provider: "claude"})
	}
	stored, err := s.store.PutCheckpoint(context.Background(), capacity.ParseInventory(raw, err).Checkpoint)
	if err != nil {
		p.metricError(err)
		return capacity.Checkpoint{Coverage: "unknown", Reason: "inventory_unrecorded"}
	}
	return stored
}

// capacityCuts reads one side of the sandwich through the same consumer path
// as the background refresh, holding coverageMu so no other read interleaves.
func (p *Plugin) capacityCuts(s *capacitySandwich) []capacity.CutRead {
	p.coverageMu.Lock()
	defer p.coverageMu.Unlock()
	reads := make([]capacity.CutRead, 0, len(s.tuples))
	for _, t := range s.tuples {
		response, failure := p.sourceCheckpoint(s.consumer, s.host, s.account, s.membership, t.scope)
		if failure != "" {
			reads = append(reads, capacity.CutRead{Tuple: t.ref, Reason: failure})
			continue
		}
		identity := measurement.PhysicalCutIdentity{SourceID: p.measurementSource, ConsumerID: s.consumer.ID(), MembershipID: s.membership, SourceRef: measurement.SourceReference(p.measurementSource, measurement.SourceScope{Provider: t.scope.Provider, AuthID: t.scope.AuthID, AuthIndex: t.scope.AuthIndex, Kind: t.scope.Kind, Version: t.scope.Version}), ManagerID: response.ManagerIncarnationID, SinkID: response.IncarnationID}
		read := capacityCutRead(t.ref, identity, t.scope, response)
		if read.Reason == "" {
			read = withStoredLower(read, s.consumer)
		}
		reads = append(reads, read)
	}
	return reads
}

// withStoredLower replaces the host's lower cut with the lower edge this
// consumer stored, which is where every durable proof of the epoch starts. It
// can be later than the host's cut after a membership change or when this
// consumer missed the sink's first cut. Without a stored edge there is no cut.
func withStoredLower(read capacity.CutRead, consumer *usagecoverage.Consumer) capacity.CutRead {
	lower, err := consumer.PhysicalLower(context.Background(), *read.Cut)
	switch {
	case err != nil:
		read.Reason = "physical_lower_unreadable"
	case lower == nil || lower.Sequence > read.Cut.Sequence || read.Lower == nil || lower.Sequence < read.Lower.Sequence:
		read.Reason = "physical_lower_unavailable"
	default:
		read.Lower = lower
		return read
	}
	read.Lower, read.Cut = nil, nil
	return read
}

// capacityCutRead keeps a cut only if the read is a quiescent cut without any
// gap. The durable proof re-read at judgment time remains authoritative.
func capacityCutRead(tuple string, identity measurement.PhysicalCutIdentity, scope pluginapi.UsageSourceScope, r pluginapi.HostUsageCheckpointResponse) capacity.CutRead {
	read := capacity.CutRead{Tuple: tuple}
	cut := func(c *pluginapi.UsagePhysicalCut) *measurement.PhysicalCutIdentity {
		i := identity
		i.Sequence, i.At = c.Sequence, c.At.Round(0).UTC()
		return &i
	}
	v := r.Physical
	switch {
	case r.Scope != scope:
		read.Reason = "checkpoint_scope_mismatch"
	case v == nil || v.Version != pluginapi.ClaudePhysicalProofVersion || v.Domain != pluginapi.ClaudePhysicalProofDomain:
		read.Reason = "physical_cut_unsupported"
	case r.GapCount > 0 || len(r.Gaps) > 0 || r.GapsTruncated || r.InvalidFrom != nil || v.GapCount > 0 || v.InvalidFromSequence != nil:
		read.Reason = "physical_incarnation_gap"
	case v.Outstanding != 0 || v.Pending != 0:
		read.Reason = "cut_not_quiescent"
	case v.Coverage == "known" && v.FromCut != nil && v.ThroughCut != nil && v.ThroughCut.Sequence == r.Sequence && v.FromCut.Sequence < v.ThroughCut.Sequence:
		read.Lower, read.Cut = cut(v.FromCut), cut(v.ThroughCut)
	case v.Coverage == "unknown" && v.Reason == "first_quiescent_cut" && v.FromCut != nil && v.ThroughCut == nil && v.FromCut.Sequence == r.Sequence:
		read.Lower, read.Cut = cut(v.FromCut), cut(v.FromCut)
	default:
		read.Reason = "cut_unavailable"
		if reasonToken(v.Reason) {
			read.Reason = v.Reason
		}
	}
	if v != nil && v.LastProducerClosedAt != nil {
		closed := v.LastProducerClosedAt.Round(0).UTC()
		read.LastProducerClosedAt = &closed
	}
	return read
}

// reasonToken admits a host reason into readback only as a short plain token.
func reasonToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// capacitySample stores one sample per shared window of the provider read.
// Model-scoped pools are skipped: CPA value counts every model, so dividing it
// by a model pool's change would not be a lower bound.
func (p *Plugin) capacitySample(s *capacitySandwich, at time.Time, windows []observation.Window) {
	if s == nil {
		return
	}
	for _, w := range windows {
		if w.Scope != "all" || w.Remaining == nil || w.Dormant || w.NotApplicable || w.Usage.Source != observation.APISource {
			continue
		}
		used, err := strconv.ParseFloat(strings.TrimSpace(w.Usage.Raw), 64)
		if err != nil {
			continue
		}
		x := s.sample
		x.Window = capacity.Window{Scope: w.Scope, Seconds: w.Seconds}
		x.At, x.UsedPercent, x.Quantum, x.Reset = at, used, capacity.Quantum(w.Usage.Raw), w.Reset
		if _, err = s.store.PutSample(context.Background(), x); err != nil {
			p.metricError(err)
		}
	}
}

// refreshCapacity re-judges the newest stored sample pairs on the background
// worker. Proofs are re-read every time, so a late gap or invalidation revokes
// an earlier lower bound. It makes no host or provider call.
func (p *Plugin) refreshCapacity() {
	p.mu.Lock()
	store, stopped := p.capacityStore, p.stopped
	accounts := map[string]bool{}
	for _, c := range p.creds {
		if c.Provider == "claude" {
			accounts[c.Account] = true
		}
	}
	p.mu.Unlock()
	if store == nil || stopped || p.measurement == nil {
		return
	}
	ctx := context.Background()
	j := &capacityJudge{p: p, store: store, proofs: map[string]capacity.Proof{}, records: map[string]capacity.RecordValue{}}
	out := map[string]capacityState{}
	for account := range accounts {
		samples, err := store.Samples(ctx, account, capacitySampleLimit)
		if err != nil {
			p.metricError(err)
			out[account] = capacityState{reasons: []string{"capacity_ledger_unavailable"}}
			continue
		}
		state := capacityState{}
		for _, pair := range capacity.Pairs(samples, capacity.HistoryLimit+1) {
			state.intervals = append(state.intervals, j.judge(ctx, pair[0], pair[1]))
		}
		out[account] = state
	}
	p.mu.Lock()
	p.capacityViews = out
	p.mu.Unlock()
}

type capacityJudge struct {
	p       *Plugin
	store   *capacity.Store
	proofs  map[string]capacity.Proof
	records map[string]capacity.RecordValue
}

func (j *capacityJudge) judge(ctx context.Context, s1, s2 capacity.Sample) capacity.Interval {
	c1, err1 := j.store.Checkpoint(ctx, s1.Before)
	c2, err2 := j.store.Checkpoint(ctx, s2.After)
	if err := errors.Join(err1, err2); err != nil {
		j.p.metricError(err)
		c1 = capacity.Checkpoint{Coverage: "unknown", Reason: "inventory_unreadable"}
	}
	m := capacity.JudgeMembership(s1.AccountRef, c1, c2)
	if s1.AccountRef != s2.AccountRef {
		m.Holds = false
		m.Reasons = append(m.Reasons, "account_ref_changed")
	}
	// Without membership the result is unknown anyway; judging Pacer's own
	// tuples still reports why carry-in would fail.
	tuples := m.Active
	if !m.Holds {
		tuples = []string{}
		for _, r := range s2.A {
			tuples = append(tuples, r.Tuple)
		}
	}
	proofs := map[string]capacity.Proof{}
	for _, tuple := range tuples {
		if proof, ok := j.proof(ctx, s2, tuple); ok {
			proofs[tuple] = proof
		}
	}
	k := capacity.JudgeCarryIn(capacity.CarryInput{From: s1.At, To: s2.At, S1: s1, S2: s2, Tuples: tuples, Proofs: proofs, Record: j.record})
	return capacity.Project(s1, s2, m, k)
}

func findCutRead(reads []capacity.CutRead, tuple string) (capacity.CutRead, bool) {
	for _, r := range reads {
		if r.Tuple == tuple {
			return r, true
		}
	}
	return capacity.CutRead{}, false
}

// proof re-reads the exact durable selection from s2's lower cut to its cut b,
// which covers both sandwiches and every dispatch between them.
func (j *capacityJudge) proof(ctx context.Context, s2 capacity.Sample, tuple string) (capacity.Proof, bool) {
	a, okA := findCutRead(s2.A, tuple)
	b, okB := findCutRead(s2.B, tuple)
	if !okA || !okB || a.Lower == nil || b.Cut == nil {
		return capacity.Proof{}, false
	}
	selection := measurement.PhysicalSelection{From: *a.Lower, Through: *b.Cut}
	key := selection.From.ID() + "\x00" + selection.Through.ID()
	if proof, ok := j.proofs[key]; ok {
		return proof, true
	}
	r, err := j.p.measurement.SelectPhysical(ctx, selection)
	proof := capacity.Proof{Known: err == nil && r.Known, Reason: r.Reason, From: selection.From.Sequence, Through: selection.Through.Sequence}
	if err != nil {
		j.p.metricError(err)
		proof.Reason = "physical_proof_unreadable"
	}
	if proof.Known {
		for _, f := range r.Facts {
			proof.Facts = append(proof.Facts, capacity.Fact{Sequence: f.DispatchSequence, EventID: f.EventID, Revision: f.Revision})
		}
	}
	j.proofs[key] = proof
	return proof, true
}

// record values the exact joined revision with the registered price book. Its
// low end keeps the ratio a lower bound.
func (j *capacityJudge) record(f capacity.Fact) capacity.RecordValue {
	key := f.EventID + "\x00" + strconv.FormatInt(f.Revision, 10)
	if v, ok := j.records[key]; ok {
		return v
	}
	var v capacity.RecordValue
	r, err := j.p.measurement.Record(context.Background(), f.EventID, f.Revision)
	if err == nil {
		value := measurement.ValueRecord(r.Record, measurement.DefaultPriceBook())
		v = capacity.RecordValue{Delivered: true, Final: r.Final, CounterComplete: value.InvalidCounters == 0 && value.Incomplete == 0, Priced: value.MissingPrices == 0, LowNanoUSD: value.LowNanoUSD, EndedAt: r.EndedAt}
	} else if !errors.Is(err, sql.ErrNoRows) {
		j.p.metricError(err)
	}
	j.records[key] = v
	return v
}

// capacityView is called with p.mu held. Age is computed at read time.
func (p *Plugin) capacityView(c credential) capacity.View {
	switch {
	case c.Provider != "claude":
		return capacity.NewView(nil, p.now(), "provider_unsupported")
	case p.capacityStore == nil:
		return capacity.NewView(nil, p.now(), "capacity_ledger_unavailable")
	}
	state := p.capacityViews[c.Account]
	return capacity.NewView(state.intervals, p.now(), state.reasons...)
}
