package cpamp

// Pacer-owned (not extracted). Quota facts are the filter-independent inputs of
// buildProfileWorkspace, captured once offline at the prepared default pin:
// additive per-event buckets and per-window results. A filtered workspace is then
// rebuilt by summing those facts in the original order, instead of re-reading,
// re-pricing and re-deriving every retained event and observation per request.
//
// Exactness: every value comes from the original functions (valueEvents,
// deriveValuationWindows, windowValuation, valueWindowEvent, normalizeProfileWindow,
// attributeKeyQuota, aggregateProfileRates, ProfileTotals.add). Totals are integer
// sums, so grouping does not change them. The fast workspace is used only where
// that holds: the prepared pin and no date bounds (date bounds clip the
// normalization intervals; those requests keep the original builder). Tests
// compare both builders byte for byte.

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

const QuotaFactsVersion = "pacer-quota-facts-v2"

// Reasons normalizeProfileWindow returns before it reads observations, events,
// filters or periods. A key/model filter cannot change these results.
const (
	reasonNoReference  = "No reference model is configured for this provider."
	reasonUnknownScope = "This quota pool has no verified model scope."
	reasonKeyFiltered  = "Select all API keys and models to compare against this account-wide quota meter."
)

type QuotaFacts struct {
	Version   string                 `json:"version"`
	Filter    ProfileWorkspaceFilter `json:"filter"`
	Config    ProfileConfig          `json:"config"`
	PriceBook ComparisonPriceBook    `json:"price_book"`
	APIKeys   []APIKey               `json:"api_keys"`
	// Credentials, unassigned accounts and the next transition do not depend on filters.
	Credentials       []ProfileCredential `json:"credentials"`
	UnassignedPeriods []ProfileMembership `json:"unassigned_periods"`
	NextTransition    int64               `json:"next_transition"`
	// OrderSensitive marks a non-positive event start: the original First/Last
	// updates then depend on visit order, so the fast builder is not used.
	OrderSensitive bool         `json:"order_sensitive"`
	Buckets        []FactBucket `json:"buckets"`
	Windows        []FactWindow `json:"windows"`
}

// FactBucket groups events with one account, UTC day, model, raw key hash and
// card set. Cards are indices into Config.Profiles; len(Profiles) is unassigned.
type FactBucket struct {
	Account string        `json:"account"`
	Day     string        `json:"day"`
	Model   string        `json:"model"`
	Key     string        `json:"key"`
	Cards   []int         `json:"cards"`
	First   int64         `json:"first_ms"`
	Last    int64         `json:"last_ms"`
	Totals  ProfileTotals `json:"totals"`
}

type FactSelection struct {
	Key    string        `json:"key"`
	Model  string        `json:"model"`
	Cards  []int         `json:"cards"`
	Totals ProfileTotals `json:"totals"`
}

type FactCard struct {
	Index         int                   `json:"index"`
	Normalization *ProfileNormalization `json:"normalization"`
	CurrentAtPin  bool                  `json:"current_at_pin"`
}

// FactWindow is one derived 5h/weekly window in original visit order. Base holds
// the unfiltered QuotaUsageWindow without Selected; KeyAttribution is the result
// for any non-empty key without the gateway-only assumption.
type FactWindow struct {
	Account        string           `json:"account"`
	Base           QuotaUsageWindow `json:"base"`
	KeyAttribution QuotaAttribution `json:"key_attribution"`
	Latest         bool             `json:"latest"`
	TokensTotal    int64            `json:"tokens_total"`
	// Readings is the number of provider meter readings in the window (v2).
	Readings      int64           `json:"readings"`
	FirstObserved int64           `json:"first_observed_ms"`
	Selections    []FactSelection `json:"selections"`
	Cards         []FactCard      `json:"cards"`
}

func cardKey(cards []int) string {
	parts := make([]string, len(cards))
	for i, c := range cards {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, ",")
}

func hasCard(cards []int, i int) bool {
	for _, c := range cards {
		if c == i {
			return true
		}
	}
	return false
}

// BuildQuotaFacts mirrors buildProfileWorkspace (non-overview) for an unfiltered
// pin and records its filter-independent facts.
func (s *Service) BuildQuotaFacts(ctx context.Context, f ProfileWorkspaceFilter) (QuotaFacts, error) {
	out := QuotaFacts{Version: QuotaFactsVersion, Buckets: []FactBucket{}, Windows: []FactWindow{}, Credentials: []ProfileCredential{}, UnassignedPeriods: []ProfileMembership{}}
	if f.Profile != "" || f.Account != "" || f.Model != "" || f.APIKey != "" || f.From != 0 || f.To != 0 || f.Page != 0 {
		return out, errors.New("quota facts need an unfiltered pin")
	}
	if err := s.acquireHistory(ctx); err != nil {
		return out, err
	}
	defer s.releaseHistory()
	initialRevision := f.At > 0 && f.Revision == 0
	var err error
	f.ProfileFilter, err = s.pinProfileFilter(ctx, f.ProfileFilter)
	if err != nil {
		return out, err
	}
	revision := f.Revision
	if initialRevision {
		revision = -1
	}
	if out.Config, err = s.ProfileConfig(ctx, revision); err != nil {
		return out, err
	}
	f.Revision = out.Config.Revision
	if out.PriceBook, err = s.comparisonPriceBook(ctx, f.ValuationRule); err != nil {
		return out, err
	}
	f.ValuationRule = out.PriceBook.ID
	out.Filter = f
	if out.APIKeys, err = s.apiKeys(ctx, f.Through); err != nil {
		return out, err
	}
	accounts, err := s.Accounts(ctx)
	if err != nil {
		return out, err
	}
	for _, a := range accounts {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		events, obs, readErr := s.evidence(ctx, a.ID, f.Through, f.ObservationsThrough)
		if readErr != nil {
			return out, readErr
		}
		if err = s.enrichTokenEvidence(ctx, events, out.PriceBook, f.TokenEvidenceThrough); err != nil {
			return out, err
		}
		valueEvents(events, out.PriceBook)
		cred := ProfileCredential{Account: a, Status: "unknown", Requests: int64(len(events))}
		_ = s.db.QueryRowContext(ctx, `SELECT checked_ms,status FROM analytics_credential_inventory WHERE account=?`, a.ID).Scan(&cred.Checked, &cred.Status)
		if cred.Checked == 0 || f.At-cred.Checked > max(int64(s.PollInterval/time.Millisecond)*2, 900000) {
			cred.Status = "unknown"
		}
		if len(events) > 0 {
			cred.First, cred.Last = events[0].Started, events[len(events)-1].Started
		}
		part := AccountFactsFor(&out, a, events, obs)
		part.Credential = cred
		out.AddAccount(part)
	}
	return out, nil
}

// AccountFacts is one account's share of QuotaFacts. Accounts are independent,
// so a live build can recompute only accounts with new evidence and append the
// rest unchanged; appending parts in account order reproduces BuildQuotaFacts.
type AccountFacts struct {
	Credential     ProfileCredential `json:"credential"`
	Unassigned     bool              `json:"unassigned"`
	OrderSensitive bool              `json:"order_sensitive"`
	NextTransition int64             `json:"next_transition"`
	Buckets        []FactBucket      `json:"buckets"`
	Windows        []FactWindow      `json:"windows"`
}

// AddAccount appends one account's facts.
func (q *QuotaFacts) AddAccount(p AccountFacts) {
	q.Credentials = append(q.Credentials, p.Credential)
	if p.Unassigned {
		q.UnassignedPeriods = append(q.UnassignedPeriods, ProfileMembership{Account: p.Credential.Account.ID})
	}
	q.OrderSensitive = q.OrderSensitive || p.OrderSensitive
	if p.NextTransition != 0 && (q.NextTransition == 0 || p.NextTransition < q.NextTransition) {
		q.NextTransition = p.NextTransition
	}
	q.Buckets = append(q.Buckets, p.Buckets...)
	q.Windows = append(q.Windows, p.Windows...)
}

// AccountFactsFor is BuildQuotaFacts' per-account step: buckets and windows
// from one account's valued events (sorted by start) and meter readings, at
// frame.Filter.At with frame.Config and frame.PriceBook. The credential is the
// caller's.
func AccountFactsFor(frame *QuotaFacts, a Account, events []Event, obs []Observation) AccountFacts {
	f := frame.Filter
	part := AccountFacts{Credential: ProfileCredential{Account: a}, Buckets: []FactBucket{}, Windows: []FactWindow{}}
	out := frame
	profiles := frame.Config.Profiles
	unassigned := len(profiles)
	cardIDs := make([]string, 0, unassigned+1)
	for _, p := range profiles {
		cardIDs = append(cardIDs, p.ID)
	}
	cardIDs = append(cardIDs, "unassigned")
	buckets := map[string]*FactBucket{}
	bucketOrder := []string{}
	belongs := func(index int, at int64) bool {
		if index < unassigned {
			return quotaProfileContains(out.Config, profiles[index], a.ID, at)
		}
		for _, p := range profiles {
			if quotaProfileContains(out.Config, p, a.ID, at) {
				return false
			}
		}
		return true
	}
	cardsAt := func(at int64) []int {
		cards := []int{}
		for i := 0; i <= unassigned; i++ {
			if belongs(i, at) {
				cards = append(cards, i)
			}
		}
		return cards
	}
	eventCards := make([][]int, len(events))
	unassignedAccount := false
	for n, e := range events {
		cards := cardsAt(e.Started)
		eventCards[n] = cards
		if e.Started <= 0 {
			part.OrderSensitive = true
		}
		if hasCard(cards, unassigned) {
			unassignedAccount = true
		}
		key := strings.Join([]string{a.ID, time.UnixMilli(e.Started).UTC().Format("2006-01-02"), e.Model, e.APIKeyHash, cardKey(cards)}, "\x00")
		b := buckets[key]
		if b == nil {
			b = &FactBucket{Account: a.ID, Day: time.UnixMilli(e.Started).UTC().Format("2006-01-02"), Model: e.Model, Key: e.APIKeyHash, Cards: cards, First: e.Started, Last: e.Started}
			buckets[key] = b
			bucketOrder = append(bucketOrder, key)
		}
		b.First, b.Last = min(b.First, e.Started), max(b.Last, e.Started)
		b.Totals.add(e)
	}
	part.Unassigned = unassignedAccount

	derived := deriveValuationWindows(a, obs, events, f.At, out.PriceBook)
	latest := map[string]string{}
	latestAt := map[string]int64{}
	for _, w := range derived {
		key := w.Kind + "|" + w.Scope
		if w.Status == "current" && (latest[key] == "" || w.LastObserved > latestAt[key] || w.LastObserved == latestAt[key] && w.ID < latest[key]) {
			latest[key], latestAt[key] = w.ID, w.LastObserved
		}
	}
	for _, w := range derived {
		if !FactKind(w.Kind) {
			continue
		}
		for _, at := range []int64{w.Start, w.Reset} {
			if at > f.At && (part.NextTransition == 0 || at < part.NextTransition) {
				part.NextTransition = at
			}
		}
		windowEvents := []Event{}
		windowCards := [][]int{}
		decision := windowValuation(w, out.PriceBook)
		start := sort.Search(len(events), func(i int) bool { return events[i].Started >= w.Start })
		end := max(start, sort.Search(len(events), func(i int) bool { return events[i].Started >= w.AttributionEnd }))
		for n := start; n < end; n++ {
			e := events[n]
			if inWindow(e, w) {
				if out.PriceBook.Policy == profileValuePolicy {
					v := valueWindowEvent(e, out.PriceBook, decision)
					e.Value = &v
				}
				windowEvents = append(windowEvents, e)
				windowCards = append(windowCards, eventCards[n])
			}
		}
		base := QuotaUsageWindow{ID: w.ID, Account: w.Account, Provider: w.Provider, Label: w.Label, Kind: w.Kind, Scope: w.Scope, Start: w.Start, Reset: w.Reset, AttributionEnd: w.AttributionEnd, LastObserved: w.LastObserved, Used: w.Final, Status: w.Status, Stale: w.Status == "current" && f.At-w.LastObserved > 600000, Assigned: !strings.HasPrefix(w.Scope, "unknown"), AccountUsage: ProfileTotals{Tokens: w.Tokens, Requests: w.Requests, Failed: w.Failed, Unpriced: w.Unpriced, CostNano: w.CostNano}, Attribution: attributeKeyQuota(w, obs, windowEvents, "", false), Comparisons: []TierComparison{}, Notes: []string{"Quota is the credential's reported meter. Request totals follow the saved credential dates and selected API key. Historical quota is the last observed reading, not necessarily the final usage at reset."}}
		normalizedByPeriod := map[string]*ProfileNormalization{}
		normalization := func(profile string) *ProfileNormalization {
			periods := normalizationPeriods(out.Config, profile, a.ID, w, f.ProfileFilter)
			key := string(encode(periods))
			if prior := normalizedByPeriod[key]; prior != nil {
				return prior
			}
			n := normalizeProfileWindow(w, obs, events, out.PriceBook, f.ProfileFilter, periods)
			normalizedByPeriod[key] = &n
			return &n
		}
		base.Normalization = normalization("")
		if out.PriceBook.Policy == profileValuePolicy {
			base.Valuation = &decision
		}
		if w.BoundaryRevision {
			base.Notes = append(base.Notes, "The provider revised this reset; the earlier window remains in history.")
		}
		selections := map[string]*FactSelection{}
		selectionOrder := []string{}
		owned := make([]int64, unassigned+1)
		for n, e := range windowEvents {
			if e.Value != nil {
				base.AccountUsage.Value.add(*e.Value)
				base.AccountUsage.Valued = true
			}
			cards := windowCards[n]
			for _, c := range cards {
				owned[c]++
			}
			key := strings.Join([]string{e.APIKeyHash, e.Model, cardKey(cards)}, "\x00")
			sel := selections[key]
			if sel == nil {
				sel = &FactSelection{Key: e.APIKeyHash, Model: e.Model, Cards: cards}
				selections[key] = sel
				selectionOrder = append(selectionOrder, key)
			}
			sel.Totals.add(e)
		}
		fw := FactWindow{Account: a.ID, Base: base, KeyAttribution: attributeKeyQuota(w, obs, windowEvents, UnknownAPIKey, false), Latest: latest[w.Kind+"|"+w.Scope] == w.ID, TokensTotal: w.Tokens.Total, Readings: w.Observations, FirstObserved: w.FirstObserved, Selections: []FactSelection{}, Cards: []FactCard{}}
		for _, key := range selectionOrder {
			fw.Selections = append(fw.Selections, *selections[key])
		}
		for i, id := range cardIDs {
			qf := QuotaUsageFilter{ProfileFilter: ProfileFilter{Profile: id}}
			if i < unassigned && !quotaWindowSelected(out.Config, qf, w) {
				continue
			}
			if i == unassigned && owned[i] == 0 {
				continue
			}
			fw.Cards = append(fw.Cards, FactCard{Index: i, Normalization: normalization(id), CurrentAtPin: fw.Latest && belongs(i, f.At)})
		}
		part.Windows = append(part.Windows, fw)
	}
	for _, key := range bucketOrder {
		part.Buckets = append(part.Buckets, *buckets[key])
	}
	return part
}

// filteredNormalization is normalizeProfileWindow's result under a key or model
// filter: it returns before reading periods, observations or events.
func filteredNormalization(n *ProfileNormalization) *ProfileNormalization {
	if n == nil {
		return nil
	}
	if n.Status == "unavailable" && (n.Reason == reasonNoReference || n.Reason == reasonUnknownScope) {
		return n
	}
	return &ProfileNormalization{Valuation: n.Valuation, RuleID: n.RuleID, Status: "unavailable", Reason: reasonKeyFiltered, Reference: n.Reference, WindowIDs: []string{}, Notes: []string{}}
}

// FactsOptions selects the fast builder's date semantics. WholeWindows accepts
// UTC-midnight bounds: requests are selected exactly by start time, while each
// window overlapping the bounds keeps its whole-window normalization instead of
// the original builder's clipped interval. It is a Pacer view, not CPAMP parity.
type FactsOptions struct {
	WholeWindows bool
	SkipRequests bool
	// Exclude removes a profile's window from its average (rates) only; the
	// window stays listed. Used only with WholeWindows (a Pacer view).
	Exclude func(profileID, windowID string) bool
}

const dayMS = int64(86400000)

// FactsEligible reports whether the fast builder reproduces the original
// workspace for f (after the same pin resolution).
func (q *QuotaFacts) eligible(f ProfileWorkspaceFilter, revision int64, book string, o FactsOptions) bool {
	p := q.Filter
	if q.OrderSensitive || f.At != p.At || f.Through != p.Through || f.ObservationsThrough != p.ObservationsThrough || f.TokenEvidenceThrough != p.TokenEvidenceThrough || revision != p.Revision || book != p.ValuationRule {
		return false
	}
	if o.WholeWindows {
		return f.From%dayMS == 0 && f.To%dayMS == 0
	}
	return f.From == 0 && f.To == 0
}

// FactsWorkspace rebuilds buildProfileWorkspace(ctx, f, false) from facts. ok is
// false when the facts cannot reproduce this pin; the caller then uses the
// original builder.
func (s *Service) FactsWorkspace(ctx context.Context, q *QuotaFacts, f ProfileWorkspaceFilter, o FactsOptions) (ProfileWorkspaceReport, bool, error) {
	r := ProfileWorkspaceReport{}
	if q == nil || q.Version != QuotaFactsVersion {
		return r, false, nil
	}
	if !s.ready.Load() {
		return r, false, errors.New("usage history is initializing")
	}
	if f.Page < 0 || f.Page > 1000000 {
		return r, false, errors.New("invalid request page")
	}
	initialRevision := f.At > 0 && f.Revision == 0
	var err error
	f.ProfileFilter, err = s.pinProfileFilter(ctx, f.ProfileFilter)
	if err != nil {
		return r, false, err
	}
	revision := f.Revision
	if initialRevision {
		revision = -1
	}
	config, err := s.ProfileConfig(ctx, revision)
	if err != nil {
		return r, false, err
	}
	book, err := s.comparisonPriceBook(ctx, f.ValuationRule)
	if err != nil {
		return r, false, err
	}
	if !q.eligible(f, config.Revision, book.ID, o) {
		return r, false, nil
	}
	f.Revision = config.Revision
	f.ValuationRule = book.ID
	r, err = assembleFactsWorkspace(ctx, q, f, config, book, o)
	if err == nil && !o.SkipRequests {
		r.Requests, err = s.workspaceRequests(ctx, r, 0)
	}
	return r, true, err
}

// FactsView assembles the prepared pin's workspace from facts alone, without a
// database read. f must be the facts pin with only Profile, Account, Model,
// APIKey and UTC-midnight From/To changed. Requests are not paged.
func FactsView(ctx context.Context, q *QuotaFacts, profile, account, model, apiKey string, from, to int64, exclude func(profileID, windowID string) bool) (ProfileWorkspaceReport, error) {
	if q == nil || q.Version != QuotaFactsVersion || q.OrderSensitive {
		return ProfileWorkspaceReport{}, errors.New("quota facts unavailable")
	}
	if from < 0 || to < 0 || from%dayMS != 0 || to%dayMS != 0 || to > 0 && from >= to {
		return ProfileWorkspaceReport{}, errors.New("invalid report interval or checkpoint")
	}
	f := q.Filter
	f.Profile, f.Account, f.Model, f.APIKey, f.From, f.To, f.Page = profile, account, model, apiKey, from, to, 0
	return assembleFactsWorkspace(ctx, q, f, q.Config, q.PriceBook, FactsOptions{WholeWindows: true, SkipRequests: true, Exclude: exclude})
}

func assembleFactsWorkspace(ctx context.Context, q *QuotaFacts, f ProfileWorkspaceFilter, config ProfileConfig, book ComparisonPriceBook, o FactsOptions) (ProfileWorkspaceReport, error) {
	r := ProfileWorkspaceReport{Version: "profile-workspace-v1", Profiles: []SavedProfileUsage{}, Credentials: []ProfileCredential{}, Requests: []WorkspaceRequest{}, Windows: []QuotaUsageWindow{}, Current: []QuotaUsageWindow{}, PageSize: 25}
	r.Unassigned = SavedProfileUsage{Profile: Profile{ID: "unassigned", Name: "Unassigned history"}, Periods: []ProfileMembership{}, Windows: []QuotaUsageWindow{}, Current: []QuotaUsageWindow{}}
	var err error
	r.Config = config
	r.PriceBook = book
	found := f.Profile == "" || f.Profile == "all" || f.Profile == "unassigned"
	for _, p := range r.Config.Profiles {
		r.Profiles = append(r.Profiles, SavedProfileUsage{Profile: p, Periods: profileMemberships(r.Config, p), Windows: []QuotaUsageWindow{}, Current: []QuotaUsageWindow{}})
		if p.ID == f.Profile {
			found = true
		}
	}
	if !found {
		return r, errors.New("profile not found")
	}
	r.APIKeys = append([]APIKey{}, q.APIKeys...)
	r.Credentials = append(r.Credentials, q.Credentials...)
	r.Unassigned.Periods = append(r.Unassigned.Periods, q.UnassignedPeriods...)
	r.NextTransition = q.NextTransition
	cards := make([]*SavedProfileUsage, 0, len(r.Profiles)+1)
	for i := range r.Profiles {
		cards = append(cards, &r.Profiles[i])
	}
	cards = append(cards, &r.Unassigned)
	keys := []string(nil)
	if f.APIKey != "" {
		keys = apiKeyHashes(f.APIKey)
	}
	keyOK := func(hash string) bool { return keys == nil || contains(keys, hash) }
	dayOK := func(day string) bool {
		if f.From == 0 && f.To == 0 {
			return true
		}
		t, e := time.Parse("2006-01-02", day)
		if e != nil {
			return false
		}
		ms := t.UnixMilli()
		return (f.From == 0 || ms >= f.From) && (f.To == 0 || ms < f.To)
	}
	timeline, models := map[string]*ProfileTotals{}, map[string]*ProfileTotals{}
	for _, b := range q.Buckets {
		if err = ctx.Err(); err != nil {
			return r, err
		}
		if !keyOK(b.Key) {
			continue
		}
		selectedEvent := (f.Account == "" || f.Account == b.Account) && (f.Model == "" || f.Model == b.Model) && dayOK(b.Day)
		selected := false
		for _, i := range b.Cards {
			card := cards[i]
			card.Lifetime.addTotals(b.Totals)
			if card.First == 0 || b.First < card.First {
				card.First = b.First
			}
			card.Last = max(card.Last, b.Last)
			if !selectedEvent {
				continue
			}
			card.Selected.addTotals(b.Totals)
			if f.Profile == "" || f.Profile == card.ID || f.Profile == "all" && card.ID != "unassigned" {
				selected = true
			}
		}
		if !selected {
			continue
		}
		r.Totals.addTotals(b.Totals)
		for _, target := range []struct {
			m   map[string]*ProfileTotals
			key string
		}{{timeline, b.Day}, {models, b.Model}} {
			if target.m[target.key] == nil {
				target.m[target.key] = &ProfileTotals{}
			}
			target.m[target.key].addTotals(b.Totals)
		}
	}
	filtered := f.APIKey != "" || f.Model != ""
	for _, fw := range q.Windows {
		base := fw.Base
		base.Selected = ProfileTotals{}
		for _, sel := range fw.Selections {
			if keyOK(sel.Key) && (f.Model == "" || f.Model == sel.Model) {
				base.Selected.addTotals(sel.Totals)
			}
		}
		if f.APIKey != "" {
			base.Attribution = fw.KeyAttribution
		}
		if filtered && !o.WholeWindows {
			base.Normalization = filteredNormalization(base.Normalization)
		}
		inRange := (f.From == 0 || base.Reset > f.From) && (f.To == 0 || base.Start < f.To)
		if fw.Latest && (f.Account == "" || f.Account == fw.Account) {
			r.Current = append(r.Current, base)
		}
		if (f.Account == "" || f.Account == fw.Account) && inRange {
			r.Windows = append(r.Windows, base)
		}
		for _, fc := range fw.Cards {
			card := cards[fc.Index]
			totals := ProfileTotals{}
			for _, sel := range fw.Selections {
				if hasCard(sel.Cards, fc.Index) && keyOK(sel.Key) && (f.Model == "" || f.Model == sel.Model) {
					totals.addTotals(sel.Totals)
				}
			}
			qw := base
			qw.Selected = totals
			qw.Normalization = fc.Normalization
			if filtered && !o.WholeWindows {
				qw.Normalization = filteredNormalization(fc.Normalization)
			}
			if fw.TokensTotal > 0 {
				share := float64(totals.Tokens.Total) * 100 / float64(fw.TokensTotal)
				qw.TokenShare = &share
			}
			if fc.CurrentAtPin {
				card.Current = append(card.Current, qw)
			}
			if f.Account != "" && f.Account != fw.Account || !inRange {
				continue
			}
			card.Windows = append(card.Windows, qw)
		}
	}
	for i := range r.Profiles {
		counted := r.Profiles[i].Windows
		if o.WholeWindows && o.Exclude != nil {
			counted = []QuotaUsageWindow{}
			for _, w := range r.Profiles[i].Windows {
				if !o.Exclude(r.Profiles[i].ID, w.ID) {
					counted = append(counted, w)
				}
			}
		}
		r.Profiles[i].Rates = aggregateProfileRates(counted, r.PriceBook)
		sortWorkspaceWindows(r.Profiles[i].Windows)
		sortWorkspaceWindows(r.Profiles[i].Current)
	}
	r.Unassigned.Rates = aggregateProfileRates(r.Unassigned.Windows, r.PriceBook)
	sortWorkspaceWindows(r.Unassigned.Windows)
	sortWorkspaceWindows(r.Unassigned.Current)
	sortWorkspaceWindows(r.Windows)
	sortWorkspaceWindows(r.Current)
	r.Timeline, r.Models = buckets(timeline), buckets(models)
	sort.Slice(r.Models, func(i, j int) bool { return r.Models[i].Tokens.Total > r.Models[j].Tokens.Total })
	r.Filter = f
	return r, nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// addTotals adds a bucket exactly as ProfileTotals.add adds its events.
func (t *ProfileTotals) addTotals(b ProfileTotals) {
	if b.Valued {
		t.Value.add(b.Value)
		t.Valued = true
	}
	t.Requests += b.Requests
	addTokens(&t.Tokens, b.Tokens)
	t.Failed += b.Failed
	t.Unpriced += b.Unpriced
	t.CostNano += b.CostNano
}

// AddTotals adds another total exactly as ProfileTotals.add adds events.
func (t *ProfileTotals) AddTotals(b ProfileTotals) { t.addTotals(b) }

// KeyHashes expands a key selector ("", "unattributed", a hash or a "keys:"
// lineage) into the raw hashes it matches; nil means every key.
func KeyHashes(selector string) []string {
	if selector == "" {
		return nil
	}
	return apiKeyHashes(selector)
}

// WindowID is CPAMP's window identity for an observation (account, kind,
// scope, reset), used for native readings so windows keep one ID scheme.
func WindowID(o Observation) string { return windowID(o) }

// AccountEvidence reads one account's events (token evidence enriched and
// valued with the facts' book) and meter readings at the facts pin, exactly as
// BuildQuotaFacts does, for the live build's pre-H tail.
func (s *Service) AccountEvidence(ctx context.Context, q *QuotaFacts, account string) ([]Event, []Observation, error) {
	if err := s.acquireHistory(ctx); err != nil {
		return nil, nil, err
	}
	defer s.releaseHistory()
	f := q.Filter
	events, obs, err := s.evidence(ctx, account, f.Through, f.ObservationsThrough)
	if err != nil {
		return nil, nil, err
	}
	if err = s.enrichTokenEvidence(ctx, events, q.PriceBook, f.TokenEvidenceThrough); err != nil {
		return nil, nil, err
	}
	valueEvents(events, q.PriceBook)
	return events, obs, nil
}

// ValueEvents values events with a comparison book as BuildQuotaFacts does.
func ValueEvents(events []Event, book ComparisonPriceBook) { valueEvents(events, book) }
