package usagecoverage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (c *Consumer) applyPhysicalCheckpoint(ctx context.Context, membership string, expected pluginapi.UsageSourceScope, r pluginapi.HostUsageCheckpointResponse) error {
	m, err := c.store.SourceMembership(ctx, membership)
	if err != nil {
		return err
	}
	ref := c.reference(expected)
	member := false
	for _, v := range m.Members {
		member = member || v.SourceRef == ref
	}
	if !member || m.SourceID != c.source || m.Provider != expected.Provider || !measurement.ValidSourceScope(scope(expected)) || r.Scope != expected {
		return c.physical.Fault(ctx, m.AccountID)
	}
	// Bound typed callers too, before iteration or marshaling. Native hooks also
	// cap raw RPC bytes, but that is not a substitute for this adapter's boundary.
	if !boundedPhysicalResponse(r) {
		return c.physical.Invalidate(ctx, ref, r.ManagerIncarnationID, r.IncarnationID)
	}
	identity := measurement.PhysicalCutIdentity{SourceID: c.source, ConsumerID: c.id, MembershipID: membership, SourceRef: ref, ManagerID: r.ManagerIncarnationID, SinkID: r.IncarnationID, Sequence: r.Sequence}
	p := measurement.PhysicalCheckpoint{Identity: identity, Missing: r.Physical == nil}
	p.HasGap = r.GapCount > 0 || len(r.Gaps) > 0 || r.GapsTruncated || r.InvalidFrom != nil
	p.Invalid = len(r.Gaps) > 16 || r.GapCount < uint64(len(r.Gaps)) || r.GapsTruncated != (r.GapCount > uint64(len(r.Gaps))) || len(r.Reason) > 256 || len(r.Coverage) > 32 || r.Coverage != "known" && r.Coverage != "unknown" && r.Coverage != "unsupported"
	if r.Coverage == "known" && (r.From == nil || r.Through == nil || !validTime(*r.From) || !validTime(*r.Through) || r.Through.Before(*r.From)) || r.Coverage != "known" && r.Through != nil {
		p.Invalid = true
	}
	for _, g := range r.Gaps {
		if g.Sequence == 0 || g.Sequence > r.Sequence || len(g.Reason) > 256 || !validTime(g.ObservedAt) || g.From != nil && (!validTime(*g.From) || g.From.After(g.ObservedAt)) || g.Through != nil && (!validTime(*g.Through) || g.From != nil && g.Through.Before(*g.From)) {
			p.Invalid = true
		}
	}
	if v := r.Physical; v != nil {
		// Producer physical gaps are manager/source-wide in CPA. A newer sink's
		// observation must also revoke older sink cuts from that origin.
		if v.GapCount > 0 || v.InvalidFromSequence != nil {
			if err = c.physical.Invalidate(ctx, ref, r.ManagerIncarnationID, ""); err != nil {
				return err
			}
		}
		p.HasGap = p.HasGap || v.GapCount > 0 || v.InvalidFromSequence != nil
		p.Invalid = p.Invalid || v.Version != pluginapi.ClaudePhysicalProofVersion || v.Domain != pluginapi.ClaudePhysicalProofDomain || len(v.Reason) > 256 || v.GapCount < r.GapCount || v.Coverage != "known" && v.Coverage != "unknown"
		p.Coverage, p.Reason = v.Coverage, v.Reason
		cut := func(v *pluginapi.UsagePhysicalCut) *measurement.PhysicalCutIdentity {
			if v == nil {
				return nil
			}
			i := identity
			i.Sequence = v.Sequence
			i.At = v.At.Round(0).UTC()
			return &i
		}
		p.From, p.Through = cut(v.FromCut), cut(v.ThroughCut)
		if v.Coverage == "known" {
			p.Invalid = p.Invalid || p.From == nil || p.Through == nil || v.Outstanding != 0 || v.Pending != 0 || p.HasGap || r.Coverage != "known"
		}
		if v.Coverage != "known" && v.ThroughCut != nil {
			p.Invalid = true
		}
		if v.Reason == "first_quiescent_cut" && (v.Coverage != "unknown" || v.Outstanding != 0 || v.Pending != 0 || p.From == nil) {
			p.Invalid = true
		}
		if v.LastProducerClosedAt != nil && !validTime(*v.LastProducerClosedAt) {
			p.Invalid = true
		}
		if v.LastProducerClosedAt != nil && v.ThroughCut != nil && v.LastProducerClosedAt.After(v.ThroughCut.At) {
			p.Invalid = true
		}
		for _, b := range []*measurement.PhysicalCutIdentity{p.From, p.Through} {
			if b != nil && (b.Sequence == 0 || b.Sequence > math.MaxInt64 || b.Sequence > r.Sequence || !validTime(b.At)) {
				p.Invalid = true
			}
		}
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) > 16384 {
		p.Invalid = true
	}
	h := sha256.Sum256(b)
	p.Digest = hex.EncodeToString(h[:])
	err = c.physical.Apply(ctx, p)
	// Semantically unusable OPTIONAL proof is unknown. It cannot discard or
	// relabel primary request-domain evidence. SQL/limit failures must propagate.
	if errors.Is(err, measurement.ErrInvalid) || errors.Is(err, measurement.ErrConflict) || errors.Is(err, measurement.ErrStaleSource) {
		return nil
	}
	return err
}

func boundedPhysicalResponse(r pluginapi.HostUsageCheckpointResponse) bool {
	if len(r.Gaps) > 16 || len(r.ManagerIncarnationID) > 256 || len(r.IncarnationID) > 256 || len(r.Reason) > 256 || len(r.Coverage) > 32 {
		return false
	}
	for _, g := range r.Gaps {
		if len(g.Reason) > 256 {
			return false
		}
	}
	if p := r.Physical; p != nil && (len(p.Domain) > 128 || len(p.Reason) > 256 || len(p.Coverage) > 32) {
		return false
	}
	return true
}

// PhysicalLower returns this listener's stored lower edge for cut c's epoch.
func (c *Consumer) PhysicalLower(ctx context.Context, cut measurement.PhysicalCutIdentity) (*measurement.PhysicalCutIdentity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	if cut.ConsumerID != c.id {
		return nil, nil
	}
	lower, err := c.store.PhysicalLower(ctx, cut)
	return lower, c.storageResult(err)
}

// SelectPhysical accepts only this listener's exact persisted cut identities.
// Historical listeners can be inspected explicitly with Store.SelectPhysical.
func (c *Consumer) SelectPhysical(ctx context.Context, s measurement.PhysicalSelection) (measurement.PhysicalResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ready(ctx); err != nil {
		return measurement.PhysicalResult{}, err
	}
	if s.From.ConsumerID != c.id || s.Through.ConsumerID != c.id {
		return measurement.PhysicalResult{Version: measurement.PhysicalVersion, Domain: measurement.PhysicalDomain, Selection: s, Reason: "foreign_consumer", Facts: []measurement.PhysicalFact{}}, nil
	}
	r, err := c.store.SelectPhysical(ctx, s)
	return r, c.storageResult(err)
}
