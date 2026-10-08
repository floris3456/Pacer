package historyreports

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func validForwardTime(t time.Time) bool {
	return !t.IsZero() && t.UnixNano() > 0 && time.Unix(0, t.UnixNano()).Equal(t)
}

// SealForward validates a fixed version vector and compiles its default summary
// before publication. Staging failure leaves the previous latest generation live.
func (s *ForwardStore) SealForward(ctx context.Context, q ForwardSeal) (ForwardPin, error) {
	if !validForwardTime(q.At) || !q.Head.BootstrapComplete || !validHash(q.Head.ID) || q.Head.Through < 0 {
		return ForwardPin{}, ErrInput
	}
	status, err := s.Status(ctx)
	if err != nil {
		return ForwardPin{}, err
	}
	if status.FeedID != q.Head.ID || status.Through < q.Head.Through || q.Head.BootstrapThrough != status.BootstrapThrough || q.Head.Through < status.BootstrapThrough {
		return ForwardPin{}, ErrForwardPending
	}
	base, err := s.loadForwardBase(ctx, q.BaseDigest)
	if err != nil {
		return ForwardPin{}, err
	}
	var book ForwardBook
	if err = s.object(ctx, "book", q.BookDigest, &book); err != nil {
		return ForwardPin{}, err
	}
	if h := book.Book.Historical; h != nil && q.BaseDigest != "" && (h.At != base.Boundary.HMS || h.SourceCheckpoint != base.Boundary.CheckpointSHA256 || h.EventsThrough != base.Boundary.EventsThrough || h.ObservationsThrough != base.Boundary.ObservationsThrough) {
		return ForwardPin{}, ErrForwardConflict
	}
	_, pd, err := s.ProfilesAt(ctx, q.ProfileRevision)
	if err != nil {
		return ForwardPin{}, err
	}
	if q.OriginDigest != "" {
		if q.BaseDigest == "" {
			return ForwardPin{}, ErrInput
		}
		var ready bool
		if err = s.reader.QueryRowContext(ctx, `SELECT ready FROM forward_origin_state WHERE digest=?`, q.OriginDigest).Scan(&ready); err != nil {
			return ForwardPin{}, err
		}
		if !ready {
			return ForwardPin{}, ErrForwardPending
		}
	}
	q.Provenance.Reasons = append(append([]string{}, q.Provenance.Reasons...), "native_normalization_unavailable", "outside_usage_unknown", "physical_coverage_unverified")
	if len(q.Provenance.Snapshots) == 0 {
		q.Provenance.Reasons = append(q.Provenance.Reasons, "source_proof_absent")
	}
	q.Provenance.Reasons = forwardUnique(q.Provenance.Reasons)
	m := forwardManifest{Version: ForwardVersion, Converter: ForwardConverterVersion, Head: q.Head, BaseDigest: q.BaseDigest, BookDigest: q.BookDigest, OriginDigest: q.OriginDigest, ProfileRevision: q.ProfileRevision, ProfileDigest: pd, At: q.At.UTC(), Provenance: q.Provenance}
	body, err := json.Marshal(m)
	if err != nil {
		return ForwardPin{}, ErrInput
	}
	if len(body) > 2<<20 {
		return ForwardPin{}, ErrLimit
	}
	id := hash(string(body))
	pin := ForwardPin{Version: ForwardVersion, Generation: id, Filter: ForwardFilter{Domain: "combined"}}
	var already bool
	if e := s.reader.QueryRowContext(ctx, `SELECT sealed FROM forward_generations WHERE id=?`, id).Scan(&already); e == nil && already {
		return pin, nil // Idempotent replay never moves the latest pointer backward.
	} else if e != nil && e != sql.ErrNoRows {
		return ForwardPin{}, e
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO forward_generations(id,body) VALUES(?,?) ON CONFLICT DO NOTHING`, id, body)
		return e
	})
	if err != nil {
		return ForwardPin{}, err
	}
	r, err := s.forwardReport(ctx, pin, true)
	if err != nil {
		return ForwardPin{}, err
	}
	summary, err := json.Marshal(r)
	if err != nil {
		return ForwardPin{}, err
	}
	if len(summary) > 8<<20 {
		return ForwardPin{}, ErrLimit
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `UPDATE forward_generations SET sealed=1,summary=? WHERE id=? AND sealed=0`, summary, id); e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, `UPDATE forward_meta SET latest=? WHERE id=1 AND (latest=? OR latest=?)`, id, status.Latest, id)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrForwardConflict
		}
		return nil
	})
	return pin, err
}

func (s *ForwardStore) PinForward(ctx context.Context, generation string, filter ForwardFilter) (ForwardPin, error) {
	if generation == "" {
		status, e := s.Status(ctx)
		if e != nil {
			return ForwardPin{}, e
		}
		generation = status.Latest
	}
	p := ForwardPin{Version: ForwardVersion, Generation: generation, Filter: filter}
	_, p, err := s.forwardContext(ctx, p, false)
	return p, err
}

type forwardReadContext struct {
	manifest         forwardManifest
	book             ForwardBook
	config           ForwardProfileConfig
	origins          ForwardOrigins
	baseResolved     bool
	base             *ForwardBase
	provenanceDigest string
	snapshots        map[[3]string][]measurement.SourceSnapshot
}

func (s *ForwardStore) forwardContext(ctx context.Context, p ForwardPin, staging bool) (c forwardReadContext, pin ForwardPin, err error) {
	pin = p
	if p.Version != ForwardVersion || !validHash(p.Generation) {
		return c, pin, ErrInput
	}
	var raw []byte
	var sealed bool
	if err = s.reader.QueryRowContext(ctx, `SELECT body,sealed FROM forward_generations WHERE id=?`, p.Generation).Scan(&raw, &sealed); err != nil {
		return
	}
	if !sealed && !staging {
		return c, pin, ErrForwardPending
	}
	if hash(string(raw)) != p.Generation || json.Unmarshal(raw, &c.manifest) != nil || c.manifest.Version != ForwardVersion || c.manifest.Converter != ForwardConverterVersion {
		return c, pin, ErrSource
	}
	f := &pin.Filter
	if f.Domain == "" {
		f.Domain = "combined"
	}
	if f.Domain != "combined" && f.Domain != "native" && f.Domain != "baseline" {
		return c, pin, ErrInput
	}
	if !f.From.IsZero() {
		if !validForwardTime(f.From) {
			return c, pin, ErrInput
		}
		f.From = f.From.UTC()
	}
	if !f.To.IsZero() {
		if !validForwardTime(f.To) {
			return c, pin, ErrInput
		}
		f.To = f.To.UTC()
	}
	to := c.manifest.At
	if !f.To.IsZero() && f.To.Before(to) {
		to = f.To
	}
	if !f.From.IsZero() && !f.From.Before(to) {
		return c, pin, ErrInput
	}
	if f.KeyHash != "" && !validHash(f.KeyHash) {
		return c, pin, ErrInput
	}
	if len(f.Account) > 1024 || len(f.Provider) > 256 || len(f.Model) > 256 || len(f.Profile) > 256 {
		return c, pin, ErrInput
	}
	if err = s.object(ctx, "book", c.manifest.BookDigest, &c.book); err != nil {
		return
	}
	var pd string
	c.config, pd, err = s.ProfilesAt(ctx, c.manifest.ProfileRevision)
	if err != nil {
		return
	}
	if pd != c.manifest.ProfileDigest {
		return c, pin, ErrForwardConflict
	}
	if f.Profile != "" && f.Profile != "unassigned" {
		found := false
		for _, p := range c.config.Profiles {
			if p.ID == f.Profile {
				found = true
			}
		}
		if !found {
			return c, pin, ErrInput
		}
	}
	base, e := s.loadForwardBase(ctx, c.manifest.BaseDigest)
	if e != nil {
		err = e
		return
	}
	if c.manifest.BaseDigest != "" {
		c.base = &base
	}
	c.provenanceDigest = forwardDigest(c.manifest.Provenance)
	c.snapshots = map[[3]string][]measurement.SourceSnapshot{}
	for _, v := range c.manifest.Provenance.Snapshots {
		key := [3]string{v.Provider, v.AccountID, v.MembershipID}
		c.snapshots[key] = append(c.snapshots[key], v)
	}
	c.baseResolved = c.manifest.BaseDigest == ""
	if c.manifest.OriginDigest != "" {
		if err = s.object(ctx, "origins", c.manifest.OriginDigest, &c.origins); err != nil {
			return
		}
		if c.origins.BaseDigest != c.manifest.BaseDigest {
			return c, pin, ErrForwardConflict
		}
	}
	if c.manifest.BaseDigest != "" {
		var missing int64
		if c.manifest.OriginDigest != "" {
			err = s.reader.QueryRowContext(ctx, `SELECT missing_count FROM forward_origin_state WHERE digest=? AND ready=1`, c.manifest.OriginDigest).Scan(&missing)
		} else {
			err = s.reader.QueryRowContext(ctx, `SELECT event_count FROM forward_base_meta WHERE digest=? AND complete=1`, c.manifest.BaseDigest).Scan(&missing)
		}
		c.baseResolved = missing == 0
	}
	return
}

// Fixed-head SQL never reads measurement_records or a mutable "latest" value.
// Existing indices support latest accepted revision and stable event-time order.
type forwardJoinedRow struct {
	id                string
	at                int64
	native, baseline  []byte
	proof             sql.NullString
	qualifierConflict bool
}

const forwardQualifies = `n.account=l.account AND n.provider=l.provider AND n.credential=l.credential AND n.started>=l.start_ns AND (l.end_ns=0 OR n.started<l.end_ns)`
const forwardBatchSQL = `SELECT n.sort_key AS key,n.started AS started,n.body AS native_body,b.body AS base_body,l.physical AS proof,(l.physical IS NOT NULL AND b.id IS NULL) AS qualifier_conflict
 FROM forward_native n LEFT JOIN forward_links l ON l.origin=? AND l.physical=n.physical LEFT JOIN forward_base_events b ON b.id=l.baseline AND ` + forwardQualifies + `
 WHERE ? AND n.sequence<=? AND n.revision=(SELECT max(x.revision) FROM forward_native x WHERE x.event_id=n.event_id AND x.sequence<=?)
 AND (n.started,n.sort_key)>(?,?) AND n.started<?
 UNION ALL
 SELECT b.id AS key,b.started,NULL AS native_body,b.body AS base_body,l.physical AS proof,
 (? AND EXISTS(SELECT 1 FROM forward_native n WHERE n.physical=l.physical AND n.sequence<=? AND NOT (` + forwardQualifies + `))) AS qualifier_conflict
 FROM forward_base_events b LEFT JOIN forward_links l ON l.origin=? AND l.baseline=b.id
 WHERE ? AND (NOT ? OR NOT EXISTS(SELECT 1 FROM forward_native n WHERE n.physical=l.physical AND n.sequence<=? AND ` + forwardQualifies + `))
 AND (b.started,b.id)>(?,?) AND b.started<?
 ORDER BY started,key LIMIT 250`

func (s *ForwardStore) readForwardBatch(ctx context.Context, c forwardReadContext, p ForwardPin, afterTime int64, afterID string) ([]forwardJoinedRow, error) {
	includeNative := p.Filter.Domain != "baseline"
	includeBase := p.Filter.Domain != "native" && c.manifest.BaseDigest != ""
	linkOrigin := ""
	if includeBase {
		linkOrigin = c.manifest.OriginDigest
	}
	if !p.Filter.From.IsZero() && afterTime < p.Filter.From.UnixNano() {
		afterTime, afterID = p.Filter.From.UnixNano(), ""
	}
	to := c.manifest.At.UnixNano()
	if !p.Filter.To.IsZero() {
		to = min(to, p.Filter.To.UnixNano())
	}
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, forwardBatchSQL, linkOrigin, includeNative, c.manifest.Head.Through, c.manifest.Head.Through, afterTime, afterID, to, includeNative, c.manifest.Head.Through, linkOrigin, includeBase, includeNative, c.manifest.Head.Through, afterTime, afterID, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := []forwardJoinedRow{}
	used := 0
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		var r forwardJoinedRow
		if err = rows.Scan(&r.id, &r.at, &r.native, &r.baseline, &r.proof, &r.qualifierConflict); err != nil {
			return nil, err
		}
		n := len(r.native) + len(r.baseline)
		if n > 2<<20-used {
			if len(page) == 0 {
				return nil, ErrLimit
			}
			break
		}
		used += n
		page = append(page, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return page, nil
}

func (s *ForwardStore) walkForward(ctx context.Context, c forwardReadContext, p ForwardPin, afterTime int64, afterID string, visit func(ForwardEvidence, []string, time.Time) (bool, error)) error {
	seen := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := s.readForwardBatch(ctx, c, p, afterTime, afterID)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			if err = ctx.Err(); err != nil {
				return err
			}
			seen++
			if seen > 500000 {
				return ErrLimit
			}
			id, at, nb, bb, proof := row.id, row.at, row.native, row.baseline, row.proof
			afterTime, afterID = at, id
			var n, b ForwardEvent
			if len(nb) > 0 && json.Unmarshal(nb, &n) != nil || len(bb) > 0 && json.Unmarshal(bb, &b) != nil {
				return ErrSource
			}
			e := b
			origins := []ForwardEvent{b}
			account := b.Event.Account
			resolved := proof.Valid
			if len(nb) > 0 {
				e = n
				origins = []ForwardEvent{n}
				account = ForwardAccountRef(n.SourceID, n.Event.Provider, n.Event.Account)
				resolved = n.PhysicalKey != "" && (p.Filter.Domain == "native" || c.baseResolved)
				cert, er := originForNative(c.origins, n)
				if er != nil {
					resolved = false
					e.Issues = append(e.Issues, "account_mapping_ambiguous")
				} else if cert != nil {
					account = cert.LegacyAccount
				}
				if len(bb) > 0 {
					origins = append(origins, b)
					resolved = n.PhysicalKey != "" && cert != nil && cert.LegacyAccount == b.Event.Account && n.Event.Provider == b.Event.Provider
					nativeCategories, nativeValid := cpamp.ForwardCategories(n.Event)
					baseCategories, baseValid := cpamp.ForwardCategories(b.Event)
					if !nativeValid || !baseValid || nativeCategories.Input != baseCategories.Input || nativeCategories.Cached != baseCategories.Cached || n.Event.Tokens.CacheCreation != b.Event.Tokens.CacheCreation || nativeCategories.Output != baseCategories.Output || nativeCategories.Reasoning != baseCategories.Reasoning || n.Event.Model != b.Event.Model || n.Event.Failed != b.Event.Failed {
						e.Issues = append(e.Issues, "origin_usage_conflict")
					}
					if normalizedForwardTier(n.Event.ServiceTier) != normalizedForwardTier(b.Event.ServiceTier) {
						e.Issues = append(e.Issues, "origin_tier_conflict")
					}
					source := b.Event.SourceTokens
					if b.Event.TokenEvidence != nil {
						source = &b.Event.TokenEvidence.Details
					}
					if source != nil && e.Event.SourceTokens != nil {
						copy := *e.Event.SourceTokens
						e.Event.SourceTokens = &copy
						for _, pair := range [][2]**int64{{&e.Event.SourceTokens.Write5m, &source.Write5m}, {&e.Event.SourceTokens.Write1h, &source.Write1h}} {
							if *pair[0] == nil {
								*pair[0] = *pair[1]
							} else if *pair[1] != nil && **pair[0] != **pair[1] {
								e.Issues = append(e.Issues, "origin_split_conflict")
							}
						}
					}
				}
			}
			if row.qualifierConflict {
				resolved = false
				e.Issues = append(e.Issues, "origin_qualifier_conflict")
			}
			for _, issue := range e.Issues {
				if issue == "physical_identity_conflict" || issue == "physical_identity_missing" {
					resolved = false
				}
			}
			members := forwardMemberships(c.config, account, e.Event.Provider, time.Unix(0, at).UnixMilli())
			if f := p.Filter; f.Account != "" && f.Account != account || f.Provider != "" && f.Provider != e.Event.Provider || f.Model != "" && f.Model != e.Event.Model || f.KeyHash != "" && f.KeyHash != e.Event.APIKeyHash {
				continue
			}
			if p.Filter.Profile != "" {
				found := false
				for _, m := range members {
					if m == p.Filter.Profile {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			value, issues := valueForward(e, c.book)
			captured, costBasis, costConflict := forwardCapturedCost(origins)
			if costConflict {
				issues = append(issues, "invalid_captured_cost")
			}
			if !resolved {
				issues = append(issues, "identity_unresolved")
				value = nil
			}
			out := ForwardEvidence{Pin: p, ID: id, Account: account, Origins: origins, Value: value, IdentityResolved: resolved, Issues: forwardUnique(issues), Valuation: "profile-api-value-v3/factual", PriceBookID: c.book.ID, PriceBookDigest: c.manifest.BookDigest, ProvenanceDigest: c.provenanceDigest, Capacity: "unavailable"}
			if c.base != nil {
				out.HistoricalHMS = c.base.Boundary.HMS
			}
			out.CoverageReasons = forwardCoverage(c, e)
			out.CapturedCost, out.CapturedCostBasis = captured, costBasis
			stop, err := visit(out, members, time.Unix(0, at).UTC())
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
	}
}

func normalizedForwardTier(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "default" || s == "standard" {
		return ""
	}
	return s
}

// Accounting-corrected CPAMP CostNano remains available in the event input, but
// never replaces its original captured cost in the explicitly raw-cost subtotal.
func forwardCapturedCost(origins []ForwardEvent) (*int64, string, bool) {
	var value *int64
	basis := "unavailable"
	for _, origin := range origins {
		cost := origin.Event.CostNano
		candidateBasis := "native_captured_cost"
		if origin.Origin == "baseline" {
			candidateBasis = "cpamp_original_captured_cost"
			if origin.Event.OriginalAccounting != nil {
				cost = origin.Event.OriginalAccounting.CostNano
			}
		}
		if cost == nil {
			continue
		}
		if *cost < 0 || value != nil && *value != *cost {
			return nil, "conflicting_captured_cost", true
		}
		if value != nil {
			basis = "corroborated_captured_cost"
		} else {
			basis = candidateBasis
		}
		n := *cost
		value = &n
	}
	return value, basis, false
}

func forwardCoverage(c forwardReadContext, e ForwardEvent) []string {
	reasons := append([]string{}, c.manifest.Provenance.Reasons...)
	if e.Origin == "baseline" {
		return forwardUnique(append(reasons, "frozen_checkpoint_not_account_coverage"))
	}
	if e.Native == nil || e.Native.SourceMembershipID == "" {
		return forwardUnique(append(reasons, "source_membership_absent"))
	}
	if source := e.Native.SourceEvidence; source != nil && source.Gap {
		reasons = append(reasons, "source_gap")
	}
	values := c.snapshots[[3]string{e.Event.Provider, e.Event.Account, e.Native.SourceMembershipID}]
	if len(values) == 0 {
		return forwardUnique(append(reasons, "source_membership_snapshot_missing"))
	}
	if len(values) != 1 {
		return forwardUnique(append(reasons, "source_snapshot_conflict"))
	}
	v := values[0]
	consumerVerified := c.manifest.Provenance.ConsumerID != "" && len(v.Runs) > 0
	for _, run := range v.Runs {
		if run.ConsumerID != c.manifest.Provenance.ConsumerID {
			consumerVerified = false
		}
	}
	if !consumerVerified {
		reasons = append(reasons, "source_consumer_unverified")
	}
	reasons = append(reasons, v.Reasons...)
	if !v.Known {
		reasons = append(reasons, "source_coverage_unknown")
	}
	if v.HasGap {
		reasons = append(reasons, "source_gap")
	}
	if !v.PhysicalCarryInProven {
		reasons = append(reasons, "physical_carry_in_unknown")
	}
	if e.Native.SourceEvidence == nil || e.Native.SourceEvidence.RequestedAt.Before(v.From) || !e.Native.SourceEvidence.RequestedAt.Before(v.Through) {
		reasons = append(reasons, "outside_source_proof_window")
	}
	return forwardUnique(reasons)
}

func addForwardTotals(t *ForwardTotals, e ForwardEvidence) error {
	if t.Reasons == nil {
		t.Reasons = map[string]int64{}
	}
	t.Rows++
	t.SourceRecords += int64(len(e.Origins))
	for _, reason := range e.Issues {
		t.Reasons[reason]++
	}
	if t.CoverageReasons == nil {
		t.CoverageReasons = map[string]int64{}
	}
	for _, reason := range e.CoverageReasons {
		t.CoverageReasons[reason]++
	}
	if e.IdentityResolved && e.Value != nil {
		if err := addForwardValue(&t.ValuedSubset, *e.Value); err != nil {
			return err
		}
		t.ValuedCount++
	}
	cost := e.CapturedCost
	if e.IdentityResolved && cost != nil && *cost >= 0 {
		conflict := false
		for _, i := range e.Issues {
			if i == "invalid_captured_cost" {
				conflict = true
			}
		}
		if !conflict {
			n, ok := forwardSum(t.CapturedCostNano, *cost)
			if !ok {
				return ErrLimit
			}
			t.CapturedCostNano = n
			t.CapturedCount++
		}
	}
	return nil
}
func finishForwardTotals(t ForwardTotals) ForwardTotals {
	t.CapturedMissing = t.Rows - t.CapturedCount
	if t.Reasons == nil {
		t.Reasons = map[string]int64{}
	}
	if t.CoverageReasons == nil {
		t.CoverageReasons = map[string]int64{}
	}
	if t.Reasons["identity_unresolved"] == 0 {
		n := t.Rows
		t.UniqueAttempts = &n
		if t.CapturedCount == t.Rows {
			cost := t.CapturedCostNano
			t.CapturedSelection = &cost
		}
		if t.ValuedCount == t.Rows {
			v := t.ValuedSubset
			t.SelectionValue = &v
		}
	}
	return t
}
func (s *ForwardStore) ForwardReport(ctx context.Context, p ForwardPin) (ForwardReport, error) {
	return s.forwardReport(ctx, p, false)
}
func (s *ForwardStore) forwardReport(ctx context.Context, p ForwardPin, staging bool) (r ForwardReport, err error) {
	c, p, err := s.forwardContext(ctx, p, staging)
	if err != nil {
		return r, err
	}
	// The only cache is the immutable, size-bounded compiled default summary.
	// Decode per read: callers cannot mutate a shared map or change future replay.
	if !staging && p.Filter == (ForwardFilter{Domain: "combined"}) {
		var raw []byte
		if err = s.reader.QueryRowContext(ctx, `SELECT summary FROM forward_generations WHERE id=? AND sealed=1`, p.Generation).Scan(&raw); err != nil {
			return r, err
		}
		if len(raw) > 8<<20 || json.Unmarshal(raw, &r) != nil || r.Pin != p || forwardDigest(r.Manifest) != p.Generation {
			return ForwardReport{}, ErrSource
		}
		return r, nil
	}
	r = ForwardReport{Pin: p, Manifest: c.manifest, Base: c.base, PriceBook: c.book, Daily: map[string]ForwardTotals{}, Models: map[string]ForwardTotals{}, Accounts: map[string]ForwardTotals{}, Profiles: map[string]ForwardTotals{}, Capacity: "unavailable"}
	err = s.walkForward(ctx, c, p, 0, "", func(e ForwardEvidence, profiles []string, at time.Time) (bool, error) {
		if er := addForwardTotals(&r.Totals, e); er != nil {
			return false, er
		}
		buckets := []struct {
			m   map[string]ForwardTotals
			key string
		}{{r.Daily, at.Format("2006-01-02")}, {r.Models, e.Origins[0].Event.Model}, {r.Accounts, e.Account}}
		for _, name := range profiles {
			buckets = append(buckets, struct {
				m   map[string]ForwardTotals
				key string
			}{r.Profiles, name})
		}
		for _, b := range buckets {
			v := b.m[b.key]
			if er := addForwardTotals(&v, e); er != nil {
				return false, er
			}
			b.m[b.key] = v
			if len(b.m) > 10000 {
				return false, ErrLimit
			}
		}
		return false, nil
	})
	if err != nil {
		return ForwardReport{}, err
	}
	r.Totals = finishForwardTotals(r.Totals)
	for _, m := range []map[string]ForwardTotals{r.Daily, r.Models, r.Accounts, r.Profiles} {
		for k, v := range m {
			m[k] = finishForwardTotals(v)
		}
	}
	encoded, e := json.Marshal(r)
	if e != nil {
		return ForwardReport{}, e
	}
	if len(encoded) > 8<<20 {
		return ForwardReport{}, ErrLimit
	}
	return r, nil
}

type forwardCursor struct {
	Binding string
	At      int64 `json:",string"`
	ID      string
}

func ForwardCacheKey(p ForwardPin, view string, options any) string {
	return forwardDigest([]any{p, view, options})
}
func (s *ForwardStore) ForwardRequests(ctx context.Context, p ForwardPin, limit int, cursor string) (page ForwardPage, err error) {
	if limit < 1 || limit > 100 || len(cursor) > 128<<10 {
		return page, ErrInput
	}
	c, p, err := s.forwardContext(ctx, p, false)
	if err != nil {
		return page, err
	}
	page.Pin = p
	page.Rows = []ForwardEvidence{}
	binding := ForwardCacheKey(p, "requests", limit)
	after := forwardCursor{Binding: binding}
	if cursor != "" {
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 {
			return page, ErrCursor
		}
		body, e := base64.RawURLEncoding.DecodeString(parts[0])
		if e != nil {
			return page, ErrCursor
		}
		mac, e := base64.RawURLEncoding.DecodeString(parts[1])
		if e != nil {
			return page, ErrCursor
		}
		h := hmac.New(sha256.New, s.cursorKey)
		h.Write(body)
		if !hmac.Equal(mac, h.Sum(nil)) || json.Unmarshal(body, &after) != nil || after.Binding != binding {
			return page, ErrCursor
		}
	}
	more := false
	pageBytes := 0
	last := after
	err = s.walkForward(ctx, c, p, after.At, after.ID, func(e ForwardEvidence, _ []string, at time.Time) (bool, error) {
		if len(page.Rows) == limit {
			more = true
			return true, nil
		}
		body, e2 := json.Marshal(e)
		if e2 != nil {
			return false, e2
		}
		pageBytes += len(body)
		if pageBytes > 7<<20 {
			return false, ErrLimit
		}
		page.Rows = append(page.Rows, e)
		last.At, last.ID = at.UnixNano(), e.ID
		return false, nil
	})
	if err != nil {
		return ForwardPage{}, err
	}
	if more {
		body, _ := json.Marshal(last)
		h := hmac.New(sha256.New, s.cursorKey)
		h.Write(body)
		page.Next = base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	}
	return page, nil
}
func (s *ForwardStore) ForwardEvidence(ctx context.Context, p ForwardPin, id string) (out ForwardEvidence, err error) {
	if id == "" || len(id) > 2048 {
		return out, ErrInput
	}
	c, p, err := s.forwardContext(ctx, p, false)
	if err != nil {
		return out, err
	}
	found := false
	err = s.walkForward(ctx, c, p, 0, "", func(e ForwardEvidence, _ []string, _ time.Time) (bool, error) {
		if e.ID == id {
			out = e
			found = true
			return true, nil
		}
		return false, nil
	})
	if err == nil && !found {
		err = ErrInput
	}
	return
}
