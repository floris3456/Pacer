// Package usagecoverage consumes CPA's account-qualified source proof on a
// background path. It performs no host RPC, provider calls, polling or routing.
package usagecoverage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type Options struct {
	SourceID string
	Clock    func() time.Time
}

type Consumer struct {
	mu             sync.Mutex
	store          *measurement.Store
	physical       *measurement.PhysicalProof
	id, source     string
	pendingFailure bool
	// physicalOff: physical proof is unavailable for the rest of this process
	// (it failed to start or a physical write failed); physicalErr says why.
	physicalOff bool
	physicalErr error
}

// New always creates a new durable consumer incarnation. The caller must install
// this object in its usage hook before requesting any host checkpoint for it.
func New(ctx context.Context, store *measurement.Store, options Options) (*Consumer, error) {
	if store == nil {
		return nil, measurement.ErrInvalid
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	c := &Consumer{store: store, id: hex.EncodeToString(id[:]), source: options.SourceID}
	if err := store.StartSourceConsumer(ctx, c.id, c.source, options.Clock().Round(0).UTC()); err != nil {
		return nil, err
	}
	// Physical proof only feeds capacity estimates. Without it the consumer still
	// records usage and request-domain coverage, so it never stops Pacer's start.
	physical, err := store.NewPhysicalProof(ctx, c.id)
	if err != nil {
		c.physicalOff, c.physicalErr = true, err
		return c, nil
	}
	c.physical = physical
	return c, nil
}

func (c *Consumer) ID() string { return c.id }

// PhysicalError is why physical proof is off in this process, or nil.
func (c *Consumer) PhysicalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.physicalErr
}

// physicalDo runs an optional physical write. A failed one never reaches the
// request domain or the host hook: the physical consumer is marked failed
// (durably when that write works) and skipped for the rest of this process, so
// no proof can span the lost write. Semantically unusable input stays unknown.
func (c *Consumer) physicalDo(ctx context.Context, op func(*measurement.PhysicalProof) error) {
	if c.physicalOff {
		return
	}
	err := op(c.physical)
	if err == nil || errors.Is(err, measurement.ErrInvalid) || errors.Is(err, measurement.ErrConflict) || errors.Is(err, measurement.ErrStaleSource) {
		return
	}
	c.physicalOff, c.physicalErr = true, err
	_ = c.physical.Poison(ctx)
}
func (c *Consumer) physicalFault(ctx context.Context, account string) {
	c.physicalDo(ctx, func(p *measurement.PhysicalProof) error { return p.Fault(ctx, account) })
}
func (c *Consumer) physicalInvalidate(ctx context.Context, ref, manager, sink string) {
	c.physicalDo(ctx, func(p *measurement.PhysicalProof) error { return p.Invalidate(ctx, ref, manager, sink) })
}

// A failed write cannot be followed by a successful proof in this process. The
// next operation first persists a sticky failure; on restart New's lower bound
// excludes the previous consumer's interval. Parent must propagate callback errors.
func (c *Consumer) ready(ctx context.Context) error {
	if !c.pendingFailure {
		return nil
	}
	if err := c.store.PoisonSourceConsumer(ctx, c.id); err != nil {
		return err
	}
	c.physicalDo(ctx, func(p *measurement.PhysicalProof) error { return p.Poison(ctx) })
	c.pendingFailure = false
	return nil
}
func (c *Consumer) storageResult(err error) error {
	if err != nil && !errors.Is(err, measurement.ErrInvalid) && !errors.Is(err, measurement.ErrConflict) && !errors.Is(err, measurement.ErrStaleSource) {
		c.pendingFailure = true
	}
	return err
}
func (c *Consumer) fault(ctx context.Context, account string) error {
	return c.storageResult(c.store.FaultSourceAccount(ctx, c.id, account, nil))
}

type Member struct {
	Scope         pluginapi.UsageSourceScope
	CredentialRef string
}
type Membership struct {
	ID, Provider, AccountID string
	Revision                int64
	From, Through           time.Time
	Verified, Complete      bool
	MappingEvidenceRef      string
	Members                 []Member
}

func scope(s pluginapi.UsageSourceScope) measurement.SourceScope {
	return measurement.SourceScope{Provider: s.Provider, AuthID: s.AuthID, AuthIndex: s.AuthIndex, Kind: s.Kind, Version: s.Version}
}
func (c *Consumer) reference(s pluginapi.UsageSourceScope) string {
	return measurement.SourceReference(c.source, scope(s))
}

// PutMembership accepts an explicit immutable inventory snapshot, never discovers
// account identity from AuthIndex/aliases and never turns fallback IDs into trust.
func (c *Consumer) PutMembership(ctx context.Context, m Membership) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ready(ctx); err != nil {
		return err
	}
	stored := measurement.SourceMembership{ID: m.ID, SourceID: c.source, Provider: m.Provider, AccountID: m.AccountID, Revision: m.Revision, From: m.From, Through: m.Through, Verified: m.Verified, Complete: m.Complete, MappingEvidenceRef: m.MappingEvidenceRef}
	if len(m.Members) > measurement.MaxSourceMembers {
		return c.badMembership(ctx, m.AccountID, measurement.ErrLimit)
	}
	for _, v := range m.Members {
		if !measurement.ValidSourceScope(scope(v.Scope)) || v.Scope.Provider != m.Provider {
			return c.badMembership(ctx, m.AccountID, measurement.ErrInvalid)
		}
		stored.Members = append(stored.Members, measurement.SourceMember{SourceRef: c.reference(v.Scope), CredentialRef: v.CredentialRef})
	}
	if err := c.store.PutSourceMembership(ctx, stored); err != nil {
		return c.badMembership(ctx, m.AccountID, err)
	}
	return nil
}
func (c *Consumer) badMembership(ctx context.Context, account string, cause error) error {
	c.storageResult(cause)
	c.physicalFault(ctx, account)
	if err := c.fault(ctx, account); err != nil {
		return err
	}
	return cause
}

// Capture is supplied by the parent from immutable attempt-time correlation.
// Verified requires actual billing-account mapping, not a credential fallback.
type Capture struct {
	Credential   measurement.NativeCredential
	MembershipID string
	Verified     bool
}

// ObserveUsage returns nil only after the native record AND any invalidations
// have been durably accepted. Missing correlation or unrecordable input is an
// error for the acknowledged host hook, never a successful dropped callback.
func (c *Consumer) ObserveUsage(ctx context.Context, raw []byte, capture Capture) (measurement.PutResult, error) {
	observed, err := c.ObserveUsageWithRecord(ctx, raw, capture)
	return observed.Result, err
}

// ObservedUsage contains the very record accepted by the authoritative store.
// Optional projections must use it, not reconstruct a different native identity.
type ObservedUsage struct {
	Record measurement.Record
	Result measurement.PutResult
}

func (c *Consumer) ObserveUsageWithRecord(ctx context.Context, raw []byte, capture Capture) (ObservedUsage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ready(ctx); err != nil {
		return ObservedUsage{}, err
	}
	r, err := measurement.FromNative(raw, capture.Credential, c.source)
	if err != nil {
		c.physicalFault(ctx, "")
		if failure := c.fault(ctx, ""); failure != nil {
			return ObservedUsage{}, failure
		}
		return ObservedUsage{}, err
	}
	se := r.Native.SourceEvidence
	verified := false
	if capture.Verified && capture.MembershipID != "" && se != nil && se.SourceRef != "" {
		m, e := c.store.SourceMembership(ctx, capture.MembershipID)
		if e != nil {
			return ObservedUsage{}, c.storageResult(e)
		}
		if m.SourceID == c.source && m.Provider == capture.Credential.Provider && m.AccountID == capture.Credential.AccountID && m.Verified && !r.RequestedAt.Before(m.From) && r.RequestedAt.Before(m.Through) {
			for _, member := range m.Members {
				if member.SourceRef == se.SourceRef && member.CredentialRef == capture.Credential.CredentialRef {
					verified = true
				}
			}
		}
	}
	r.Native.SourceMembershipID = capture.MembershipID
	r.Native.BillingMappingVerified = verified
	if !verified || se == nil || !se.Tracked || se.SourceRef == "" {
		if err = c.fault(ctx, r.Attribution.AccountID); err != nil {
			return ObservedUsage{}, err
		}
	}
	if se != nil && se.SourceRef != "" && (se.Gap || !se.Tracked) {
		c.physicalInvalidate(ctx, se.SourceRef, se.ManagerID, "")
		if err = c.store.InvalidateSource(ctx, se.SourceRef, se.ManagerID, "", se.InvalidFrom); err != nil {
			return ObservedUsage{}, c.storageResult(err)
		}
	}
	result, err := c.store.PutRecord(ctx, r)
	if err != nil {
		c.storageResult(err)
		if se != nil && se.SourceRef != "" {
			c.physicalInvalidate(ctx, se.SourceRef, se.ManagerID, "")
		} else {
			c.physicalFault(ctx, r.Attribution.AccountID)
		}
		if failure := c.fault(ctx, r.Attribution.AccountID); failure != nil {
			return ObservedUsage{}, failure
		}
		return ObservedUsage{}, err
	}
	c.physicalDo(ctx, func(p *measurement.PhysicalProof) error { return p.ObserveRecord(ctx, raw, r) })
	return ObservedUsage{Record: r, Result: result}, nil
}

// Invalidate is for an ingestion/discovery failure before record correlation is
// available. Empty account faults this entire consumer; failures remain latched.
func (c *Consumer) Invalidate(ctx context.Context, account string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ready(ctx); err != nil {
		return err
	}
	c.physicalFault(ctx, account)
	return c.fault(ctx, account)
}

// ApplyCheckpoint is called with the exact requested tuple, not a scope selected
// from the response. Gap aggregation precedes sequence/coverage acceptance.
func (c *Consumer) ApplyCheckpoint(ctx context.Context, membership string, expected pluginapi.UsageSourceScope, response pluginapi.HostUsageCheckpointResponse) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ready(ctx); err != nil {
		return err
	}
	// Optional physical negatives are durable before either domain can advance.
	c.physicalDo(ctx, func(*measurement.PhysicalProof) error {
		return c.applyPhysicalCheckpoint(ctx, membership, expected, response)
	})
	// The request-domain digest retains its original payload. The optional
	// extension has its own bounded hash and must not alter request-time identity.
	response.Physical = nil
	m, err := c.store.SourceMembership(ctx, membership)
	if err != nil {
		return c.storageResult(err)
	}
	ref := c.reference(expected)
	member := false
	for _, v := range m.Members {
		member = member || v.SourceRef == ref
	}
	if !measurement.ValidSourceScope(scope(expected)) || !member || m.SourceID != c.source || m.Provider != expected.Provider || response.Scope != expected {
		if err = c.fault(ctx, m.AccountID); err != nil {
			return err
		}
		return measurement.ErrInvalid
	}
	if len(response.Gaps) > 16 || len(response.ManagerIncarnationID) > 256 || len(response.IncarnationID) > 256 || len(response.Reason) > 256 || len(response.Coverage) > 32 {
		if err = c.fault(ctx, m.AccountID); err != nil {
			return err
		}
		return measurement.ErrInvalid
	}
	for _, g := range response.Gaps {
		if len(g.Reason) > 256 {
			if err = c.fault(ctx, m.AccountID); err != nil {
				return err
			}
			return measurement.ErrInvalid
		}
	}
	raw, err := json.Marshal(response)
	if err != nil {
		if e := c.fault(ctx, m.AccountID); e != nil {
			return e
		}
		return measurement.ErrInvalid
	}
	hash := sha256.Sum256(raw)
	p := measurement.SourceCheckpoint{ConsumerID: c.id, MembershipID: membership, SourceRef: ref, ManagerID: response.ManagerIncarnationID, SinkID: response.IncarnationID, Sequence: response.Sequence, Coverage: response.Coverage, From: copyTime(response.From), Through: copyTime(response.Through), PayloadDigest: hex.EncodeToString(hash[:])}
	p.HasGap = response.GapCount > 0 || len(response.Gaps) > 0 || response.GapsTruncated || response.InvalidFrom != nil
	p.InvalidFrom = copyTime(response.InvalidFrom)
	p.Invalid = len(response.Gaps) > 16 || len(raw) > 16384 || len(response.Reason) > 256 || response.GapCount < uint64(len(response.Gaps)) || response.GapsTruncated != (response.GapCount > uint64(len(response.Gaps)))
	unknownBegin := p.HasGap && response.InvalidFrom == nil
	for _, g := range response.Gaps {
		if g.From == nil {
			unknownBegin = true
		} else if p.InvalidFrom == nil || g.From.Before(*p.InvalidFrom) {
			p.InvalidFrom = copyTime(g.From)
		}
		if g.Sequence == 0 || g.Sequence > response.Sequence || !validTime(g.ObservedAt) || len(g.Reason) > 256 || g.From != nil && (!validTime(*g.From) || g.From.After(g.ObservedAt)) || g.Through != nil && (!validTime(*g.Through) || g.From != nil && g.Through.Before(*g.From)) {
			p.Invalid = true
		}
	}
	if unknownBegin {
		p.InvalidFrom = nil
	}
	err = c.store.ApplySourceCheckpoint(ctx, p)
	if errors.Is(err, measurement.ErrInvalid) || errors.Is(err, measurement.ErrConflict) {
		c.physicalInvalidate(ctx, ref, response.ManagerIncarnationID, response.IncarnationID)
	}
	return c.storageResult(err)
}

func validTime(t time.Time) bool {
	return !t.IsZero() && t.UnixNano() > 0 && time.Unix(0, t.UnixNano()).Equal(t)
}
func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.Round(0).UTC()
	return &v
}

// CheckpointFailed is required for host RPC/decode/registration errors. No last
// successful checkpoint is silently reused after a failed observation.
func (c *Consumer) CheckpointFailed(ctx context.Context, membership string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ready(ctx); err != nil {
		return err
	}
	m, err := c.store.SourceMembership(ctx, membership)
	if err != nil {
		return c.storageResult(err)
	}
	c.physicalFault(ctx, m.AccountID)
	return c.fault(ctx, m.AccountID)
}

// BeforeMeasure copies the explicit account->membership selection and forces the
// scoped path. It does not advance proof and never mixes in legacy global runs.
func (c *Consumer) BeforeMeasure(ctx context.Context, q measurement.Query, memberships map[string]string) (measurement.Query, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q.RequireSourceCoverage = true
	q.SourceConsumerID = c.id
	q.AccountScopes = map[string]string{}
	if err := c.ready(ctx); err != nil {
		return q, err
	}
	if len(memberships) > measurement.MaxSourceMembers {
		return q, measurement.ErrLimit
	}
	for account, id := range memberships {
		q.AccountScopes[account] = id
	}
	return q, nil
}

func (c *Consumer) Snapshot(ctx context.Context, membership, provider, account string, from, through time.Time) (measurement.SourceSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ready(ctx); err != nil {
		return measurement.SourceSnapshot{}, err
	}
	v, err := c.store.SourceSnapshot(ctx, c.id, membership, provider, account, from, through)
	return v, c.storageResult(err)
}
