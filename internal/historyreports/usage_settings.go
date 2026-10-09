package historyreports

// Settings of the usage record, kept as small append-only revisions in the
// forward store (like window exclusions): account links (re-logins of one
// provider account), the model price list (models.dev sync and manual prices)
// and profile edits (the forward ledger's own profile revisions). Nothing is
// deleted; every change is a new revision under optimistic revision control.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/usageidentity"
)

const (
	linksKind  = "account-links"
	pricesKind = "model-prices"
)

func latestObject(ctx context.Context, q rowQueryer, kind string, v any) (bool, error) {
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT body FROM forward_objects WHERE kind=? ORDER BY name DESC LIMIT 1`, kind).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if json.Unmarshal(raw, v) != nil {
		return false, ErrSource
	}
	return true, nil
}

// ---- Account links ----

type AccountLinks struct {
	Revision int64         `json:"revision"`
	Links    []AccountLink `json:"links"`
}

func (s *ForwardStore) AccountLinks(ctx context.Context) (AccountLinks, error) {
	out := AccountLinks{Links: []AccountLink{}}
	_, err := latestObject(ctx, s.reader, linksKind, &out)
	if out.Links == nil {
		out.Links = []AccountLink{}
	}
	return out, err
}

// AccountKnown reports an account's provider when the usage record knows it.
type AccountKnown func(account string) (provider string, ok bool)

// SetAccountLink links account to sameAs (a later login of the same provider
// account) or removes account's link. Both must be known accounts of one
// provider; a link never forms a cycle.
func (s *ForwardStore) SetAccountLink(ctx context.Context, expected int64, account, sameAs string, linked bool, note string, at time.Time, known AccountKnown) (AccountLinks, error) {
	if expected < 0 || account == "" || len(account) > 256 || len(sameAs) > 256 || !validExclusionNote(note) || linked && (sameAs == "" || sameAs == account) || !linked && (sameAs != "" || note != "") {
		return AccountLinks{}, ErrInput
	}
	pa, ok := known(account)
	if !ok {
		return AccountLinks{}, ErrInput
	}
	if linked {
		if pb, ok := known(sameAs); !ok || pb != pa {
			return AccountLinks{}, ErrInput
		}
	}
	var out AccountLinks
	err := s.write(ctx, func(tx *sql.Tx) error {
		current := AccountLinks{Links: []AccountLink{}}
		if _, err := latestObject(ctx, tx, linksKind, &current); err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrForwardConflict
		}
		next := []AccountLink{}
		for _, l := range current.Links {
			if l.Account != account {
				next = append(next, l)
			}
		}
		if linked {
			canon := canonicalizer(next)
			if canon(sameAs) == account || canonicalizer(append(next, AccountLink{Account: account, SameAs: sameAs}))(account) != canon(sameAs) {
				return ErrInput // a cycle
			}
			next = append(next, AccountLink{Account: account, SameAs: sameAs, AtMS: at.UnixMilli(), Note: note})
		}
		if len(next) > 10000 {
			return ErrLimit
		}
		sort.Slice(next, func(i, j int) bool { return next[i].Account < next[j].Account })
		out = AccountLinks{Revision: current.Revision + 1, Links: next}
		_, err := forwardObject(tx, ctx, linksKind, exclusionName(out.Revision), out)
		return err
	})
	return out, err
}

// ---- Model prices ----

// ModelPrices is Pacer's price list: prices set by hand (append-only
// revisions in the forward store) and the last models.dev sync. Prices are
// USD per million tokens, as CPAMP's price book.
type ModelPrices struct {
	Revision   int64                        `json:"revision"`
	Manual     map[string]ForwardModelPrice `json:"manual"`
	ManualAtMS map[string]int64             `json:"manual_at_ms,omitempty"`
	// Catalog is the last models.dev sync, kept in the record file (it is
	// fetched again when lost); the caller fills it in.
	Catalog    ModelsDevCatalog `json:"-"`
	SyncedAtMS int64            `json:"-"`
}

func (s *ForwardStore) ModelPrices(ctx context.Context) (ModelPrices, error) {
	out := ModelPrices{Manual: map[string]ForwardModelPrice{}}
	_, err := latestObject(ctx, s.reader, pricesKind, &out)
	if out.Manual == nil {
		out.Manual = map[string]ForwardModelPrice{}
	}
	return out, err
}

// UpdateModelPrices applies change to the current price list as a new
// revision. expected < 0 skips the revision check (background sync).
func (s *ForwardStore) UpdateModelPrices(ctx context.Context, expected int64, change func(*ModelPrices) error) (ModelPrices, error) {
	var out ModelPrices
	err := s.write(ctx, func(tx *sql.Tx) error {
		current := ModelPrices{Manual: map[string]ForwardModelPrice{}}
		if _, err := latestObject(ctx, tx, pricesKind, &current); err != nil {
			return err
		}
		if current.Manual == nil {
			current.Manual = map[string]ForwardModelPrice{}
		}
		if expected >= 0 && current.Revision != expected {
			return ErrForwardConflict
		}
		if err := change(&current); err != nil {
			return err
		}
		if len(current.Manual) > 5000 {
			return ErrLimit
		}
		current.Revision++
		out = current
		_, err := forwardObject(tx, ctx, pricesKind, exclusionName(current.Revision), current)
		return err
	})
	return out, err
}

// ValidManualPrice checks a hand-entered price (USD per million tokens).
func ValidManualPrice(p ForwardModelPrice) bool {
	for _, v := range []float64{p.Prompt, p.Completion, p.CacheRead, p.CacheCreation} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 10000 {
			return false
		}
	}
	return p.PromptConfigured && p.CompletionConfigured && len(p.ContextTiers) == 0 && len(p.ServiceTiers) == 0
}

// PriceSource names where a model's usage price comes from.
const (
	PriceImported  = "imported"
	PriceModelsDev = "models.dev"
	PriceManual    = "manual"
)

// defaultReferences are reference units for providers the imported book has
// none for: a provider needs one before its $ per 1% can be shown.
var defaultReferences = map[string]struct{ model, name string }{
	"opencode-go": {"opencode-go/deepseek-v4.1-flash", "DeepSeek V4.1 Flash"},
}

// ModelPrice is the price the usage record uses for a model, and its source.
// Order: a manual price, then the imported book, then models.dev.
func ModelPriceFor(frozen cpamp.ComparisonPriceBook, prices ModelPrices, provider, model string) (ForwardModelPrice, string, bool) {
	key := usageidentity.AnalyticsModel(model)
	if p, ok := prices.Manual[key]; ok {
		return p, PriceManual, true
	}
	if p, ok := frozen.Prices[key]; ok {
		return p, PriceImported, true
	}
	if p, ok := prices.Catalog.Price(provider, key, prices.SyncedAtMS); ok {
		return p, PriceModelsDev, true
	}
	return ForwardModelPrice{}, "", false
}

// MergedBook is the imported price book with Pacer's prices: manual prices
// replace imported ones, models.dev fills only models the import has no price
// for. models maps every model the record holds to its provider. The imported
// book is returned unchanged (same ID) when nothing is added or replaced.
func MergedBook(frozen cpamp.ComparisonPriceBook, prices ModelPrices, models map[string]string) cpamp.ComparisonPriceBook {
	out := frozen
	out.Prices = make(map[string]ForwardModelPrice, len(frozen.Prices)+len(models))
	for k, v := range frozen.Prices {
		out.Prices[k] = v
	}
	out.References = make(map[string]cpamp.ReferenceUnit, len(frozen.References)+1)
	for k, v := range frozen.References {
		out.References[k] = v
	}
	added := map[string]ForwardModelPrice{}
	for model, provider := range models {
		key := usageidentity.AnalyticsModel(model)
		p, source, ok := ModelPriceFor(frozen, prices, provider, key)
		if ok && source != PriceImported {
			p.UpdatedAtMS, p.SyncedAtMS = 0, nil
			out.Prices[key], added[key] = p, p
		}
	}
	for key, p := range prices.Manual {
		if _, ok := added[key]; !ok {
			out.Prices[key], added[key] = p, p
		}
	}
	for provider, ref := range defaultReferences {
		if _, ok := out.References[provider]; ok {
			continue
		}
		if p, ok := out.Prices[usageidentity.AnalyticsModel(ref.model)]; ok && p.Prompt > 0 {
			out.References[provider] = cpamp.ReferenceUnit{Model: ref.model, Name: ref.name, InputPerMillion: p.Prompt, Source: "https://models.dev"}
			added["reference:"+provider] = p
		}
	}
	if len(added) > 0 {
		out.ID = frozen.ID + "+pacer:" + forwardDigest(added)[:16]
	}
	return out
}

// ---- Profiles ----

// EffectiveProfiles is the profile configuration of the usage record: the
// imported profiles, each replaced by its forward revision when the forward
// ledger has one with the same ID, then the forward ledger's own profiles.
// Bindings of a replaced profile no longer apply.
func EffectiveProfiles(frozen cpamp.ProfileConfig, forward ForwardProfileConfig) cpamp.ProfileConfig {
	out := cpamp.ProfileConfig{Plans: frozen.Plans, Revision: forward.Revision, Profiles: []cpamp.Profile{}, Bindings: []cpamp.Binding{}}
	replaced := map[string]bool{}
	byID := map[string]cpamp.Profile{}
	for _, p := range forward.Profiles {
		byID[p.ID] = p
	}
	seen := map[string]bool{}
	for _, p := range frozen.Profiles {
		if f, ok := byID[p.ID]; ok && !sameProfile(p, f) {
			p, replaced[p.ID] = f, true
		}
		out.Profiles = append(out.Profiles, p)
		seen[p.ID] = true
	}
	for _, p := range forward.Profiles {
		if !seen[p.ID] {
			out.Profiles = append(out.Profiles, p)
			seen[p.ID] = true
			replaced[p.ID] = true
		}
	}
	bindings := map[string]bool{}
	for _, list := range [][]cpamp.Binding{frozen.Bindings, forward.Bindings} {
		for _, b := range list {
			key := forwardDigest(b)
			if replaced[b.Profile] || bindings[key] {
				continue
			}
			bindings[key] = true
			out.Bindings = append(out.Bindings, b)
		}
	}
	return out
}

func sameProfile(a, b cpamp.Profile) bool { return forwardDigest(a) == forwardDigest(b) }

// LatestProfiles reads the forward ledger's current profile revision (an empty
// configuration before the first one).
func (s *ForwardStore) LatestProfiles(ctx context.Context) (ForwardProfileConfig, error) {
	var revision int64
	if err := s.reader.QueryRowContext(ctx, `SELECT profile_revision FROM forward_meta WHERE id=1`).Scan(&revision); err != nil {
		return ForwardProfileConfig{}, err
	}
	c, _, err := s.ProfilesAt(ctx, revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ForwardProfileConfig{Revision: revision, Profiles: []cpamp.Profile{}, Bindings: []cpamp.Binding{}}, nil
	}
	return c, err
}

// ProfileEdit is one change to a profile from the usage page.
type ProfileEdit struct {
	Op      string `json:"op"` // add, end, remove, create, rename, archive, restore
	Profile string `json:"profile,omitempty"`
	Name    string `json:"name,omitempty"`
	// Provider of a new profile.
	Provider string `json:"provider,omitempty"`
	Account  string `json:"account,omitempty"`
	// FromMS and AtMS: add counts the account from FromMS (0: its first
	// use); end stops counting it at AtMS.
	FromMS int64 `json:"from_ms,omitempty"`
	AtMS   int64 `json:"at_ms,omitempty"`
}

func validProfileName(n string) bool {
	n = strings.TrimSpace(n)
	return n != "" && utf8.ValidString(n) && utf8.RuneCountInString(n) <= 80 && validExclusionNote(n)
}

// ApplyProfileEdit turns a usage-page edit into the profile the forward ledger
// stores (always explicit memberships). members lists the atomic accounts of
// the edited (canonical) account, so every login of it is counted.
func ApplyProfileEdit(effective cpamp.ProfileConfig, e ProfileEdit, members []string, newID string) (cpamp.Profile, error) {
	var p cpamp.Profile
	found := false
	for _, x := range effective.Profiles {
		if x.ID == e.Profile {
			p, found = x, true
		}
	}
	switch e.Op {
	case "create":
		if !validProfileName(e.Name) || e.Provider == "" || len(e.Provider) > 64 || newID == "" {
			return p, ErrInput
		}
		return cpamp.Profile{ID: newID, Name: strings.TrimSpace(e.Name), Provider: e.Provider, Memberships: []cpamp.ProfileMembership{}}, nil
	}
	if !found {
		return p, ErrInput
	}
	q := p
	q.Memberships = cpamp.ExactMemberships(effective, p)
	q.Selection, q.Accounts, q.ActiveAccount = false, nil, ""
	switch e.Op {
	case "rename":
		if !validProfileName(e.Name) {
			return p, ErrInput
		}
		q.Name = strings.TrimSpace(e.Name)
	case "restore":
		q.Archived = false
	case "archive":
		q.Archived = true
	case "add":
		if len(members) == 0 || e.FromMS < 0 {
			return p, ErrInput
		}
		for _, m := range members {
			for _, x := range q.Memberships {
				if x.Account == m && (x.End == 0 || e.FromMS < x.End) {
					return p, ErrInput // already counted from then on
				}
			}
			q.Memberships = append(q.Memberships, cpamp.ProfileMembership{Account: m, Start: e.FromMS})
		}
	case "end":
		if len(members) == 0 || e.AtMS <= 0 {
			return p, ErrInput
		}
		changed := false
		for i, x := range q.Memberships {
			if contains(members, x.Account) && x.Start < e.AtMS && (x.End == 0 || x.End > e.AtMS) {
				q.Memberships[i].End, changed = e.AtMS, true
			}
		}
		if !changed {
			return p, ErrInput
		}
	case "remove":
		kept := []cpamp.ProfileMembership{}
		for _, x := range q.Memberships {
			if !contains(members, x.Account) {
				kept = append(kept, x)
			}
		}
		if len(kept) == len(q.Memberships) {
			return p, ErrInput
		}
		q.Memberships = kept
	default:
		return p, ErrInput
	}
	// Overlapping memberships of one account are merged (EditProfiles refuses them).
	q.Memberships = mergeMemberships(q.Memberships)
	return q, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func mergeMemberships(in []cpamp.ProfileMembership) []cpamp.ProfileMembership {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Account != in[j].Account {
			return in[i].Account < in[j].Account
		}
		return in[i].Start < in[j].Start
	})
	out := []cpamp.ProfileMembership{}
	for _, m := range in {
		if n := len(out); n > 0 && out[n-1].Account == m.Account && (out[n-1].End == 0 || m.Start <= out[n-1].End) {
			if out[n-1].End != 0 && (m.End == 0 || m.End > out[n-1].End) {
				out[n-1].End = m.End
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

// EditProfilesKnown is EditProfiles for the usage page: a membership's account
// may also be an account the usage record knows (imported accounts have no
// forward requests in a native-only ledger).
func (s *ForwardStore) EditProfilesKnown(ctx context.Context, expected int64, edit ForwardProfileEdit, known AccountKnown) (int64, error) {
	if edit.Profile.ID != "" {
		for _, m := range edit.Profile.Memberships {
			if provider, ok := known(m.Account); !ok || provider != edit.Profile.Provider {
				return 0, ErrInput
			}
		}
	}
	return s.editProfiles(ctx, expected, edit, true)
}

// Hash is the sha256 hex digest used for references.
func Hash(s string) string { return hash(s) }

// ExactProfileMemberships are a profile's effective intervals per account.
func ExactProfileMemberships(c ForwardProfileConfig, p ForwardProfile) []ForwardMembership {
	return cpamp.ExactMemberships(c, p)
}
