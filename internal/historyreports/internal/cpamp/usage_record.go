package cpamp

// Pacer-owned (not extracted). Helpers for the continuous usage record
// (historyreports/usage_record.go): bounded range reads of imported evidence,
// window kinds shown on the usage page, and profile membership as exact
// intervals. Every calculation still runs in the extracted functions.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// FactKind reports whether a window kind is kept in quota facts: the 5-hour,
// weekly and monthly quota meters.
func FactKind(kind string) bool { return kind == "5h" || kind == "weekly" || kind == "monthly" }

// Range bounds one imported evidence read: events that started in
// [EventsFrom, EventsTo) and meter readings observed in [ObsFrom, ObsTo).
type Range struct {
	EventsFrom, EventsTo int64
	ObsFrom, ObsTo       int64
	// Kinds limits readings to these window kinds (nil: every kind).
	Kinds []string
}

// AccountEvidenceRange reads one imported account's evidence in r at the facts
// pin, exactly as evidence() and BuildQuotaFacts read and enrich it, but only
// within the range. Events are not valued (the caller values them with its book).
func (s *Service) AccountEvidenceRange(ctx context.Context, q *QuotaFacts, account string, r Range) ([]Event, []Observation, error) {
	if err := s.acquireHistory(ctx); err != nil {
		return nil, nil, err
	}
	defer s.releaseHistory()
	f := q.Filter
	events := []Event{}
	activeLimits := map[int64]string{}
	if r.EventsTo > r.EventsFrom {
		rows, err := s.db.QueryContext(ctx, "SELECT a.payload,"+keyValue+" FROM analytics_events a "+keyJoins+" WHERE a.account=? AND a.event_id<=? AND a.started_ms>=? AND a.started_ms<? ORDER BY a.started_ms,a.event_id", account, f.Through, r.EventsFrom, r.EventsTo)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var raw, apiKey string
			var e Event
			if err = rows.Scan(&raw, &apiKey); err == nil {
				err = json.Unmarshal([]byte(raw), &e)
			}
			if err != nil {
				rows.Close()
				return nil, nil, err
			}
			e.APIKeyHash = apiKey
			events = append(events, e)
			activeLimits[e.ID] = e.QuotaHeaders["x-codex-active-limit"]
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
		if err = s.correctEventAccounting(ctx, events); err != nil {
			return nil, nil, err
		}
		if err = s.rangeTokenEvidence(ctx, events, q.PriceBook, f.TokenEvidenceThrough, r); err != nil {
			return nil, nil, err
		}
	}
	obs := []Observation{}
	if r.ObsTo <= r.ObsFrom {
		return events, obs, nil
	}
	query := "SELECT payload FROM analytics_observations WHERE account=? AND rowid<=? AND observed_ms>=? AND observed_ms<?"
	args := []any{account, f.ObservationsThrough, r.ObsFrom, r.ObsTo}
	if r.Kinds != nil {
		query += " AND json_extract(payload,'$.kind') IN (" + strings.TrimSuffix(strings.Repeat("?,", len(r.Kinds)), ",") + ")"
		for _, k := range r.Kinds {
			args = append(args, k)
		}
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY observed_ms,id", args...)
	if err != nil {
		return nil, nil, err
	}
	codex := false
	for rows.Next() {
		var raw string
		var o Observation
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal([]byte(raw), &o)
		}
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		if _, ok := activeLimits[o.EventID]; !ok && o.Provider == "codex" && o.Source == "response_header" && o.Scope == "codex-main" {
			codex = true
		}
		obs = append(obs, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	// evidence() reads the active limit from the reading's own event. A header
	// reading arrives with its request's response, so its event started at
	// most a day before it was observed.
	if codex {
		rows, err = s.db.QueryContext(ctx, "SELECT event_id,coalesce(json_extract(payload,'$.quota_headers.\"x-codex-active-limit\"'),'') FROM analytics_events WHERE account=? AND event_id<=? AND started_ms>=? AND started_ms<?", account, f.Through, r.ObsFrom-dayMS, r.ObsTo)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id int64
			var active string
			if err = rows.Scan(&id, &active); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if _, ok := activeLimits[id]; !ok {
				activeLimits[id] = active
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	for i := range obs {
		o := &obs[i]
		active := activeLimits[o.EventID]
		if o.Provider == "codex" && o.Source == "response_header" && o.Scope == "codex-main" && (active == "codex_bengalfox" || active == "spark") {
			o.Scope = "model:gpt-5.3-codex-spark"
			o.Models = []string{"gpt-5.3-codex-spark"}
			o.WindowID = windowID(*o)
			o.Boundary += "; scope normalized from event x-codex-active-limit"
		}
	}
	return events, obs, nil
}

// rangeTokenEvidence is enrichTokenEvidence restricted to the events read.
func (s *Service) rangeTokenEvidence(ctx context.Context, events []Event, book ComparisonPriceBook, through int64, r Range) error {
	if book.Policy != profileValuePolicy || through == 0 || len(events) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.event_hash,e.revision,e.source_id,e.payload FROM analytics_token_evidence e JOIN analytics_events a ON a.event_hash=e.event_hash WHERE a.account=? AND a.started_ms>=? AND a.started_ms<? AND e.revision<=? ORDER BY e.revision`, events[0].Account, r.EventsFrom, r.EventsTo, through)
	if err != nil {
		return err
	}
	defer rows.Close()
	byHash := map[string]*TokenEvidence{}
	for rows.Next() {
		var hash, raw string
		var e TokenEvidence
		if err = rows.Scan(&hash, &e.Revision, &e.SourceID, &raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &e.Details); err != nil {
			return err
		}
		byHash[hash] = &e
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range events {
		events[i].TokenEvidence = byHash[events[i].Hash]
	}
	return nil
}

// AccountSpan is the first and last time an imported account has evidence.
type AccountSpan struct {
	First, Last int64
}

// AccountSpans returns every imported account's evidence span at the pin.
func (s *Service) AccountSpans(ctx context.Context, q *QuotaFacts) (map[string]AccountSpan, error) {
	if err := s.acquireHistory(ctx); err != nil {
		return nil, err
	}
	defer s.releaseHistory()
	out := map[string]AccountSpan{}
	add := func(account string, first, last int64) {
		sp, ok := out[account]
		if !ok || first < sp.First {
			sp.First = first
		}
		sp.Last = max(sp.Last, last)
		out[account] = sp
	}
	for _, query := range []struct {
		sql   string
		bound int64
	}{
		{"SELECT account,min(started_ms),max(started_ms) FROM analytics_events WHERE event_id<=? GROUP BY account", q.Filter.Through},
		{"SELECT account,min(observed_ms),max(observed_ms) FROM analytics_observations WHERE rowid<=? GROUP BY account", q.Filter.ObservationsThrough},
	} {
		rows, err := s.db.QueryContext(ctx, query.sql, query.bound)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a string
			var first, last int64
			if err = rows.Scan(&a, &first, &last); err != nil {
				rows.Close()
				return nil, err
			}
			add(a, first, last)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CardContains reports whether card index i (profiles in config order, then
// unassigned) owns account at time at, exactly as AccountFactsFor decides.
func CardContains(c ProfileConfig, i int, account string, at int64) bool {
	if i < len(c.Profiles) {
		return quotaProfileContains(c, c.Profiles[i], account, at)
	}
	for _, p := range c.Profiles {
		if quotaProfileContains(c, p, account, at) {
			return false
		}
	}
	return true
}

// ExactMemberships are the intervals in which profile p contains each account
// under c, as explicit memberships. For binding-based profiles this applies the
// first-binding ownership rule, so converting a profile to memberships keeps
// exactly the same contents.
func ExactMemberships(c ProfileConfig, p Profile) []ProfileMembership {
	if p.Memberships != nil || p.Selection {
		return profileMemberships(c, p)
	}
	accounts := []string{}
	seen := map[string]bool{}
	for _, b := range c.Bindings {
		if b.Profile == p.ID && !seen[b.Account] {
			seen[b.Account] = true
			accounts = append(accounts, b.Account)
		}
	}
	out := []ProfileMembership{}
	for _, a := range accounts {
		// Ownership can change only at a binding boundary of this account.
		cuts := []int64{}
		for _, b := range c.Bindings {
			if b.Account == a {
				cuts = append(cuts, b.Start)
				if b.End != 0 {
					cuts = append(cuts, b.End)
				}
			}
		}
		sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
		var open *ProfileMembership
		for i, at := range cuts {
			if i > 0 && at == cuts[i-1] {
				continue
			}
			owned := ownerAt(c, a, at) == p.ID
			switch {
			case owned && open == nil:
				open = &ProfileMembership{Account: a, Start: at}
			case !owned && open != nil:
				open.End = at
				out = append(out, *open)
				open = nil
			}
		}
		if open != nil {
			out = append(out, *open)
		}
	}
	return out
}

// ProfileAccounts lists the accounts a profile can contain.
func ProfileAccounts(c ProfileConfig, p Profile) []string { return quotaProfileAccounts(c, p) }

// ProfileMemberships is profileMemberships: a profile's raw intervals.
func ProfileMemberships(c ProfileConfig, p Profile) []ProfileMembership {
	return profileMemberships(c, p)
}

// AccountKinds lists every imported account's window kinds that quota facts
// keep, from its meter readings at the pin.
func (s *Service) AccountKinds(ctx context.Context, q *QuotaFacts) (map[string][]string, error) {
	if err := s.acquireHistory(ctx); err != nil {
		return nil, err
	}
	defer s.releaseHistory()
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT account,json_extract(payload,'$.kind') FROM analytics_observations WHERE rowid<=?`, q.Filter.ObservationsThrough)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var a string
		var k *string
		if err = rows.Scan(&a, &k); err != nil {
			return nil, err
		}
		if k != nil && FactKind(*k) {
			out[a] = append(out[a], *k)
		}
	}
	return out, rows.Err()
}

// EventModels lists every model of the imported requests with its provider.
func (s *Service) EventModels(ctx context.Context, q *QuotaFacts) (map[string]string, error) {
	if err := s.acquireHistory(ctx); err != nil {
		return nil, err
	}
	defer s.releaseHistory()
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT json_extract(a.payload,'$.model'),c.provider FROM analytics_events a JOIN analytics_accounts c ON c.id=a.account WHERE a.event_id<=?`, q.Filter.Through)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var m, p *string
		if err = rows.Scan(&m, &p); err != nil {
			return nil, err
		}
		if m != nil && *m != "" && p != nil {
			out[*m] = *p
		}
	}
	return out, rows.Err()
}
