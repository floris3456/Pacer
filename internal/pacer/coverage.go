package pacer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sort"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/usagecoverage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxCoverageInventory = 128
const maxCoverageCalls = 8
const maxCheckpointBytes = 16 << 10

type coverageGroup struct {
	MembershipID string
	Members      []pluginapi.UsageSourceScope
}

// Private bounded diagnostics: no credentials, callback bodies or capacity.
type coverageDiagnostics struct {
	Calls, Errors, Stale uint64
	Reason               string
}

func sourceScope(c credential) pluginapi.UsageSourceScope {
	return pluginapi.UsageSourceScope{Provider: c.Provider, AuthID: c.ID, AuthIndex: c.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
}

func (p *Plugin) clearMeasurementCache() {
	p.mu.Lock()
	p.coverageGeneration++
	p.measurements = map[string]measurement.Report{}
	p.mu.Unlock()
}

func (p *Plugin) coverageFailure(reason, account string) {
	p.mu.Lock()
	p.coverageStats.Errors++
	p.coverageStats.Reason = reason
	consumer := p.coverage
	p.coverageGeneration++
	p.measurements = map[string]measurement.Report{}
	p.mu.Unlock()
	if consumer != nil {
		p.metricError(consumer.Invalidate(context.Background(), account))
	}
}

func (p *Plugin) discoveryFailed() {
	p.coverageFailure("inventory_unavailable", "")
	p.mu.Lock()
	p.coverageGroups = map[string]coverageGroup{}
	// Routing may retain its last inventory, but subsequent captures cannot
	// claim a verified mapping from a failed refresh.
	for id, c := range p.creds {
		c.AccountVerified = false
		c.CoverageMembership = ""
		p.creds[id] = c
	}
	p.mu.Unlock()
}

// persistCoverageInventory is called with discoverMu held, never p.mu. The
// host list/get pair gives point observations only: Complete is ALWAYS false.
// Unchanged point snapshots are reused, avoiding one metadata row per poll.
func (p *Plugin) persistCoverageInventory(found map[string]credential) map[string]coverageGroup {
	out := map[string]coverageGroup{}
	if p.coverage == nil || p.measurement == nil {
		return out
	}
	groups := map[string][]credential{}
	for _, c := range found {
		if c.Provider != "claude" || !c.ExactIdentity {
			continue
		}
		s := sourceScope(c)
		if !measurement.ValidSourceScope(measurement.SourceScope{Provider: s.Provider, AuthID: s.AuthID, AuthIndex: s.AuthIndex, Kind: s.Kind, Version: s.Version}) {
			continue
		}
		groups[c.Account] = append(groups[c.Account], c)
	}
	accounts := make([]string, 0, len(groups))
	for account := range groups {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		members := groups[account]
		sort.Slice(members, func(i, j int) bool {
			if members[i].ID == members[j].ID {
				return members[i].Index < members[j].Index
			}
			return members[i].ID < members[j].ID
		})
		at := p.now().Round(0).UTC()
		m := usagecoverage.Membership{Provider: "claude", AccountID: account, Revision: 1, From: at, Through: at, Verified: true, Complete: false}
		projected := []measurement.SourceMember{}
		for _, c := range members {
			s := sourceScope(c)
			ref := tracker.Digest(c.Provider + "\x00" + c.ID + "\x00" + c.Index)
			m.Members = append(m.Members, usagecoverage.Member{Scope: s, CredentialRef: ref})
			projected = append(projected, measurement.SourceMember{SourceRef: measurement.SourceReference(p.measurementSource, measurement.SourceScope{Provider: s.Provider, AuthID: s.AuthID, AuthIndex: s.AuthIndex, Kind: s.Kind, Version: s.Version}), CredentialRef: ref})
			m.Verified = m.Verified && c.AccountVerified
		}
		sort.Slice(projected, func(i, j int) bool { return projected[i].SourceRef < projected[j].SourceRef })
		if m.Verified {
			m.MappingEvidenceRef = "host-auth-metadata:" + tracker.Digest(account)
		}
		old, err := p.measurement.CurrentSourceMembership(context.Background(), p.measurementSource, "claude", account)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			p.coverageFailure("inventory_storage_failed", account)
			continue
		}
		unchanged := err == nil && !old.Complete && old.Verified == m.Verified && old.MappingEvidenceRef == m.MappingEvidenceRef && reflect.DeepEqual(old.Members, projected)
		if unchanged {
			m.ID = old.ID
		} else {
			if err == nil {
				m.Revision = old.Revision + 1
			}
			raw, _ := json.Marshal(struct {
				Account  string
				Revision int64
				Members  []measurement.SourceMember
				Verified bool
			}{account, m.Revision, projected, m.Verified})
			m.ID = "inventory:" + tracker.Digest(p.measurementSource+string(raw))
			if err = p.coverage.PutMembership(context.Background(), m); err != nil {
				p.coverageFailure("inventory_snapshot_failed", account)
				continue
			}
		}
		g := coverageGroup{MembershipID: m.ID}
		for _, c := range members {
			g.Members = append(g.Members, sourceScope(c))
			c.CoverageMembership = m.ID
			found[c.ID] = c
		}
		out[account] = g
	}
	p.mu.Lock()
	oldGroups := p.coverageGroups
	p.mu.Unlock()
	for account, old := range oldGroups {
		if current, ok := out[account]; !ok || old.MembershipID != current.MembershipID {
			p.coverageFailure("inventory_membership_changed", account)
		}
	}
	return out
}

// Called only from refreshMeasurements on the existing background worker.
// No queue drain, host call or wait occurs in scheduler/usage/request paths.
// Native Host supplies its RPC deadline; this worker has at most one outstanding
// call and a fixed per-pass budget, with a sorted round-robin cursor.
func (p *Plugin) refreshSourceCoverage() {
	if !p.coverageMu.TryLock() {
		return
	}
	defer p.coverageMu.Unlock()
	type task struct {
		key, account, membership string
		scope                    pluginapi.UsageSourceScope
	}
	p.mu.Lock()
	consumer, host, stopped := p.coverage, p.Host, p.stopped
	tasks := []task{}
	for account, g := range p.coverageGroups {
		for _, s := range g.Members {
			tasks = append(tasks, task{account + "\x00" + s.AuthID + "\x00" + s.AuthIndex, account, g.MembershipID, s})
		}
	}
	p.mu.Unlock()
	if stopped || consumer == nil || len(tasks) == 0 {
		return
	}
	if len(tasks) > maxCoverageInventory {
		p.coverageFailure("inventory_limit", "")
		return
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].key < tasks[j].key })
	start := sort.Search(len(tasks), func(i int) bool { return tasks[i].key > p.coverageCursor })
	for i := 0; i < len(tasks) && i < maxCoverageCalls; i++ {
		select {
		case <-p.workerDone:
			return
		default:
		}
		t := tasks[(start+i)%len(tasks)]
		p.coverageCursor = t.key
		var raw json.RawMessage
		var err error
		if host == nil {
			err = errors.New("host unavailable")
		} else {
			raw, err = host(pluginabi.MethodHostUsageCheckpoint, pluginapi.HostUsageCheckpointRequest{Scope: t.scope})
		}
		p.mu.Lock()
		p.coverageStats.Calls++
		p.mu.Unlock()
		var response pluginapi.HostUsageCheckpointResponse
		if err == nil && (len(raw) == 0 || len(raw) > maxCheckpointBytes) {
			err = errors.New("invalid checkpoint size")
		}
		if err == nil {
			err = json.Unmarshal(raw, &response)
		}
		if err != nil {
			p.metricError(consumer.CheckpointFailed(context.Background(), t.membership))
			p.coverageFailure("checkpoint_unavailable", t.account)
			continue
		}
		err = consumer.ApplyCheckpoint(context.Background(), t.membership, t.scope, response)
		// Cached reports cannot outlive any new invalidation, including stale
		// responses which persist gaps before rejecting sequence advancement.
		p.clearMeasurementCache()
		if errors.Is(err, measurement.ErrStaleSource) {
			p.mu.Lock()
			p.coverageStats.Stale++
			p.mu.Unlock()
		} else if err != nil {
			p.coverageFailure("checkpoint_rejected", t.account)
		}
	}
}
