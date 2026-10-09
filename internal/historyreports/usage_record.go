package historyreports

// The usage record (docs/usage-record.md): one window list per account across
// the imported history and Pacer's own record, derived by CPAMP's functions.
//
// Time is cut into fixed weekly segments. A segment of a window kind holds the
// windows whose reset falls inside it and is derived from a bounded evidence
// range around it (twice the window length before, one length after), so
// deriving it alone gives the same windows as deriving the whole history. Day
// totals are segmented the same way. A segment is sealed once its evidence
// range is complete; sealed segments are stored on disk and never derived
// again unless their inputs change (account links, the account's profile
// memberships, a price of a model they use, or this code's version). A build
// therefore derives only open segments with new evidence, whatever the age of
// the record.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/usageidentity"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

// RecordVersion is part of every sealed segment key: a new derivation or pool
// mapping re-derives everything once.
const RecordVersion = "pacer-usage-record-v1"

const (
	segmentMS      = 7 * dayMS
	segmentEpoch   = int64(345600000) // 1970-01-05T00:00Z, a Monday
	sealMargin     = 2 * dayMS        // evidence arriving later than this re-opens a segment
	boundaryMargin = int64(3600000)
)

// ErrRecordPending means the build ran out of time; it resumes next time.
var ErrRecordPending = errors.New("usage record: still deriving")

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func segmentOf(t int64) int64     { return floorDiv(t-segmentEpoch, segmentMS) }
func segmentStart(k int64) int64  { return segmentEpoch + k*segmentMS }
func bucketKind(kind string) bool { return kind == "" }

// evidenceRange is what a segment's derivation reads.
func evidenceRange(kind string, k int64) cpamp.Range {
	s0, s1 := segmentStart(k), segmentStart(k+1)
	if bucketKind(kind) {
		return cpamp.Range{EventsFrom: s0, EventsTo: s1}
	}
	d := kindDuration(kind)
	return cpamp.Range{EventsFrom: s0 - d - boundaryMargin, EventsTo: s1 + boundaryMargin, ObsFrom: s0 - 2*d - dayMS, ObsTo: s1 + d + dayMS, Kinds: []string{kind}}
}

func rangeEnd(r cpamp.Range) int64 { return max(r.EventsTo, r.ObsTo) }

// sealAt is when a segment's evidence range is complete.
func sealAt(kind string, k int64) int64 { return rangeEnd(evidenceRange(kind, k)) + sealMargin }

// AccountLink records that a credential is a later login of the same provider
// account (a re-authentication): their requests and meters are one account.
type AccountLink struct {
	Account string `json:"account"` // the later login
	SameAs  string `json:"same_as"` // the login it continues
	AtMS    int64  `json:"at_ms"`
	Note    string `json:"note,omitempty"`
}

// canonicalizer maps an account to the first login of its linked chain.
func canonicalizer(links []AccountLink) func(string) string {
	next := map[string]string{}
	for _, l := range links {
		if l.Account != "" && l.SameAs != "" && l.Account != l.SameAs {
			next[l.Account] = l.SameAs
		}
	}
	memo := map[string]string{}
	return func(a string) string {
		if c, ok := memo[a]; ok {
			return c
		}
		seen := map[string]bool{a: true}
		c := a
		for {
			n, ok := next[c]
			if !ok || seen[n] {
				break
			}
			seen[n] = true
			c = n
		}
		memo[a] = c
		return c
	}
}

// canonicalConfig rewrites every account in a profile configuration to its
// canonical account.
func canonicalConfig(c cpamp.ProfileConfig, canon func(string) string) cpamp.ProfileConfig {
	out := cpamp.ProfileConfig{Plans: c.Plans, Revision: c.Revision, Profiles: make([]cpamp.Profile, 0, len(c.Profiles)), Bindings: make([]cpamp.Binding, 0, len(c.Bindings))}
	for _, p := range c.Profiles {
		q := p
		if p.Memberships != nil {
			q.Memberships = make([]cpamp.ProfileMembership, len(p.Memberships))
			for i, m := range p.Memberships {
				m.Account = canon(m.Account)
				q.Memberships[i] = m
			}
		}
		if p.Accounts != nil {
			q.Accounts = []string{}
			seen := map[string]bool{}
			for _, a := range p.Accounts {
				if a = canon(a); !seen[a] {
					seen[a] = true
					q.Accounts = append(q.Accounts, a)
				}
			}
		}
		if q.ActiveAccount != "" {
			q.ActiveAccount = canon(q.ActiveAccount)
		}
		out.Profiles = append(out.Profiles, q)
	}
	for _, b := range c.Bindings {
		b.Account = canon(b.Account)
		out.Bindings = append(out.Bindings, b)
	}
	return out
}

// configDigest captures everything AccountFactsFor reads from the profile
// configuration for one account: per profile, its raw and effective intervals.
func configDigest(c cpamp.ProfileConfig, account string) string {
	type entry struct {
		ID    string                    `json:"id"`
		Raw   []cpamp.ProfileMembership `json:"raw"`
		Exact []cpamp.ProfileMembership `json:"exact"`
	}
	out := []entry{}
	for _, p := range c.Profiles {
		e := entry{ID: p.ID, Raw: []cpamp.ProfileMembership{}, Exact: []cpamp.ProfileMembership{}}
		for _, m := range cpamp.ProfileMemberships(c, p) {
			if m.Account == account {
				e.Raw = append(e.Raw, m)
			}
		}
		for _, m := range cpamp.ExactMemberships(c, p) {
			if m.Account == account {
				e.Exact = append(e.Exact, m)
			}
		}
		if len(e.Raw) > 0 || len(e.Exact) > 0 {
			out = append(out, e)
		}
	}
	return forwardDigest(out)
}

// bookDigest captures the prices one segment's events use.
func bookDigest(book cpamp.ComparisonPriceBook, provider string, models []string) string {
	type entry struct {
		Policy, WriteRate, Historical string
		Reference                     cpamp.ReferenceUnit
		Prices                        map[string]*ForwardModelPrice
	}
	e := entry{Policy: book.Policy, WriteRate: book.WriteRateSource, Reference: book.References[provider], Prices: map[string]*ForwardModelPrice{}}
	if book.Historical != nil {
		e.Historical = book.Historical.ID
	}
	for _, m := range models {
		key := usageidentity.AnalyticsModel(m)
		if p, ok := book.Prices[key]; ok {
			p.UpdatedAtMS, p.SyncedAtMS = 0, nil
			e.Prices[key] = &p
		} else {
			e.Prices[key] = nil
		}
	}
	return forwardDigest(e)
}

// recordSegment is one derived segment (kind "" holds day totals).
type recordSegment struct {
	Kind           string             `json:"kind"`
	Segment        int64              `json:"segment"`
	Book           string             `json:"book"`
	Sealed         bool               `json:"sealed"`
	DerivedMS      int64              `json:"derived_ms"`
	NextMS         int64              `json:"next_ms"`
	Cards          []string           `json:"cards"` // profile IDs of card indices, then "unassigned"
	Models         []string           `json:"models"`
	Windows        []cpamp.FactWindow `json:"windows"`
	Idle           []bool             `json:"idle"`
	Latest         map[string]int64   `json:"latest"` // kind|scope -> newest reading read
	Buckets        []cpamp.FactBucket `json:"buckets"`
	Requests       int64              `json:"requests"`
	First          int64              `json:"first_ms"`
	Last           int64              `json:"last_ms"`
	Unassigned     bool               `json:"unassigned"`
	OrderSensitive bool               `json:"order_sensitive"`
	// dirty marks new evidence not yet derived (throttled; not stored).
	dirty bool
}

// refreshEvery is how often an open segment with new evidence is derived
// again. Weekly and monthly windows read weeks of evidence, so they follow new
// requests a little more slowly; resets and sealing are never delayed.
func refreshEvery(kind string) int64 {
	switch kind {
	case "weekly":
		return 2 * 60000
	case "monthly":
		return 10 * 60000
	}
	return 0
}

type segmentID struct {
	kind string
	k    int64
}

type recordAccount struct {
	id       string
	key      string
	provider string
	label    string
	members  []string // atomic accounts (imported IDs and native references)
	refs     []string // native references to read
	imported []string // imported members
	first    int64
	kinds    []string
	segs     map[segmentID]*recordSegment
	loaded   bool
}

// Record builds and keeps the usage record. Build is not concurrent.
type Record struct {
	mu            sync.Mutex
	reader        *Reader
	store         *RecordStore
	frozen        *cpamp.QuotaFacts
	art           string
	known         []cpamp.Account
	spans         map[string]cpamp.AccountSpan
	kinds         map[string][]string
	refs          map[string]*RefInfo
	refsAt        time.Time
	acc           map[string]*recordAccount
	pending       Changes
	built         *cpamp.QuotaFacts
	status        RecordStatus
	ident         identity
	wake          int64 // when a throttled segment is due
	ambiguousSeen int
	groups        map[string]*accountGroup
	labels        map[string]string
	links         []AccountLink
}

// identity maps atomic accounts to canonical ones for one build. A native
// account certified to exactly one imported account is that account (an
// implicit link): every request and reading of it continues the imported
// account, whatever credential carried it. Native accounts certified to
// several imported accounts are mapped per request by certificate.
type identity struct {
	canon    func(string) string
	implicit map[string]string                     // native reference -> imported account
	certs    map[string][]ForwardOriginCertificate // native reference -> its certificates
	// ambiguous counts native accounts certified to several imported accounts.
	ambiguous int
}

func (r *Record) identity(in RecordInput) identity {
	id := identity{implicit: map[string]string{}, certs: map[string][]ForwardOriginCertificate{}}
	for ref, info := range r.refs {
		legacy := ""
		ambiguous := false
		for _, c := range in.Origins.Certificates {
			if c.SourceID == info.Source && c.Provider == info.Provider && c.NativeAccount == info.Native {
				id.certs[ref] = append(id.certs[ref], c)
				if legacy != "" && legacy != c.LegacyAccount {
					ambiguous = true
				}
				legacy = c.LegacyAccount
			}
		}
		if legacy != "" && !ambiguous {
			id.implicit[ref] = legacy
		}
		if ambiguous {
			id.ambiguous++
		}
	}
	links := make([]AccountLink, 0, len(id.implicit)+len(in.Links))
	refs := make([]string, 0, len(id.implicit))
	for ref := range id.implicit {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		links = append(links, AccountLink{Account: ref, SameAs: id.implicit[ref]})
	}
	// The user's links come last, so they decide.
	id.canon = canonicalizer(append(links, in.Links...))
	return id
}

// eventAccount is the atomic account of a native request.
func (id identity) eventAccount(in RecordInput, n RecordEvent) (string, bool) {
	if _, ok := id.implicit[n.Ref]; ok || len(id.certs[n.Ref]) == 0 {
		return n.Ref, true
	}
	fe := ForwardEvent{SourceID: n.Source, StartedAt: time.UnixMilli(n.Started), Attribution: attributionOf(n), Event: cpamp.Event{Account: n.Native, Provider: n.Provider}}
	cert, err := originForNative(ForwardOrigins{Certificates: id.certs[n.Ref]}, fe)
	if err != nil {
		return "", false
	}
	if cert != nil {
		return cert.LegacyAccount, true
	}
	return n.Ref, true
}

// readingAccount is the atomic account of a native reading.
func (id identity) readingAccount(in RecordInput, ref string, rd NativeReading) (string, bool) {
	if _, ok := id.implicit[ref]; ok || len(id.certs[ref]) == 0 {
		return ref, true
	}
	return readingAccount(ForwardOrigins{Certificates: id.certs[ref]}, in.Source, rd)
}

// RecordStatus describes the last build for the page's technical details.
type RecordStatus struct {
	BuiltAtMS       int64            `json:"built_at_ms,omitempty"`
	ElapsedMS       int64            `json:"elapsed_ms"`
	Accounts        int              `json:"accounts"`
	Derived         int              `json:"segments_derived"`
	Persisted       int              `json:"segments_persisted"`
	Sealed          int              `json:"segments_sealed"`
	Open            int              `json:"segments_open"`
	Windows         int              `json:"windows"`
	PendingAccounts int              `json:"pending_accounts,omitempty"`
	Store           RecordStoreStats `json:"store"`
	Ambiguous       int              `json:"ambiguous_evidence"`
	// WakeMS is when throttled new evidence is due to be derived.
	WakeMS int64 `json:"wake_ms,omitempty"`
}

func NewRecord(reader *Reader, store *RecordStore) *Record {
	return &Record{reader: reader, store: store, pending: Changes{}}
}

// RecordInput is one build's settings.
type RecordInput struct {
	Now     time.Time
	Source  string // Pacer's producer source ID (native account references)
	Origins ForwardOrigins
	// Config is the effective profile configuration (nil: the imported one).
	Config *cpamp.ProfileConfig
	Links  []AccountLink
	// Book is the merged price book (nil: the imported one).
	Book *cpamp.ComparisonPriceBook
	// Labels names Pacer's native accounts (native account ID -> name).
	Labels  map[string]string
	Changes Changes
	// Budget bounds the derivation time of one build (0: unbounded).
	Budget time.Duration
}

func (r *Record) Status() RecordStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Facts returns the last complete build (nil before the first).
func (r *Record) Facts() *cpamp.QuotaFacts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.built
}

// Build derives what changed and assembles the record. It returns
// ErrRecordPending when Budget ran out; derived segments are kept, so the next
// call continues where this one stopped.
func (r *Record) Build(ctx context.Context, in RecordInput) (*cpamp.QuotaFacts, RecordStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	started := time.Now()
	st := RecordStatus{}
	frozen, err := r.reader.quotaFacts(ctx)
	if err != nil {
		return nil, st, err
	}
	now := in.Now.UnixMilli()
	if now < frozen.Filter.At {
		return nil, st, ErrInput
	}
	if r.frozen != frozen {
		if err = r.reset(ctx, frozen); err != nil {
			return nil, st, err
		}
	}
	r.pending.Merge(in.Changes)
	r.wake = 0
	config := frozen.Config
	if in.Config != nil {
		config = *in.Config
	}
	book := frozen.PriceBook
	if in.Book != nil {
		book = *in.Book
	}
	if err = r.refreshRefs(ctx, now); err != nil {
		return nil, st, err
	}
	r.ident = r.identity(in)
	st.Ambiguous = r.ident.ambiguous
	cconfig := canonicalConfig(config, r.ident.canon)
	groups := r.group(frozen, in)
	r.groups, r.labels, r.links = groups, in.Labels, in.Links
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	r.orderAccounts(ids)
	deadline := time.Time{}
	if in.Budget > 0 {
		deadline = started.Add(in.Budget)
	}
	pending := 0
	keep := map[string]bool{}
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return nil, st, err
		}
		g := groups[id]
		keep[id] = true
		key := forwardDigest([]any{RecordVersion, PoolsVersion, r.art, g.id, g.provider, g.members, g.certs, configDigest(cconfig, g.id)})
		a := r.acc[id]
		if a == nil || a.key != key {
			if a != nil {
				if err = r.store.DropSegments(ctx, id); err != nil {
					return nil, st, err
				}
			}
			a = &recordAccount{id: id, key: key, segs: map[segmentID]*recordSegment{}}
			r.acc[id] = a
		}
		a.provider, a.label, a.members, a.refs, a.imported, a.first, a.kinds = g.provider, g.label, g.members, g.refs, g.imported, g.first, g.kinds
		if !a.loaded {
			if err = r.load(ctx, a); err != nil {
				return nil, st, err
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			pending++
			continue
		}
		n, persisted, err := r.derive(ctx, frozen, a, cconfig, book, in, now, deadline)
		st.Derived += n
		st.Persisted += persisted
		if errors.Is(err, ErrRecordPending) {
			pending++
			continue
		}
		if err != nil {
			return nil, st, err
		}
	}
	for id := range r.acc {
		if !keep[id] {
			delete(r.acc, id)
		}
	}
	if pending > 0 {
		st.PendingAccounts = pending
		st.ElapsedMS = time.Since(started).Milliseconds()
		r.status = st
		return nil, st, ErrRecordPending
	}
	r.pending = Changes{}
	facts := r.assemble(frozen, ids, cconfig, book, now, &st)
	if r.wake != 0 && (facts.NextTransition == 0 || r.wake < facts.NextTransition) {
		st.WakeMS = r.wake
	}
	st.Accounts = len(ids)
	st.BuiltAtMS = now
	st.ElapsedMS = time.Since(started).Milliseconds()
	if s, e := r.store.Stats(ctx); e == nil {
		st.Store = s
	}
	r.built, r.status = facts, st
	r.ambiguousSeen = st.Ambiguous
	return facts, st, nil
}

func (r *Record) reset(ctx context.Context, frozen *cpamp.QuotaFacts) error {
	known, err := r.reader.service.Accounts(ctx)
	if err != nil {
		return domainError(ctx, err)
	}
	spans, err := r.reader.service.AccountSpans(ctx, frozen)
	if err != nil {
		return domainError(ctx, err)
	}
	kinds, err := r.reader.service.AccountKinds(ctx, frozen)
	if err != nil {
		return domainError(ctx, err)
	}
	r.frozen, r.known, r.spans, r.kinds = frozen, known, spans, kinds
	r.art = forwardDigest([]any{frozen.Version, frozen.Filter, frozen.PriceBook.ID})
	r.acc = map[string]*recordAccount{}
	r.refs, r.refsAt = nil, time.Time{}
	return nil
}

// refreshRefs reloads the native references when evidence arrived for one not
// yet known, and at least every ten minutes.
func (r *Record) refreshRefs(ctx context.Context, now int64) error {
	stale := r.refs == nil || time.Since(r.refsAt) > 10*time.Minute
	for ref, at := range r.pending {
		if info := r.refs[ref]; info == nil {
			stale = true
		} else {
			if at < info.First {
				info.First = at
			}
		}
	}
	if !stale {
		return nil
	}
	refs, err := r.store.Refs(ctx)
	if err != nil {
		return err
	}
	r.refs, r.refsAt = refs, time.Now()
	return nil
}

type accountGroup struct {
	id, provider, label string
	members             []string
	refs                []string
	imported            []string
	certs               []ForwardOriginCertificate
	first               int64
	kinds               []string
}

// group collects every atomic account (imported accounts and native
// references) under its canonical account.
func (r *Record) group(frozen *cpamp.QuotaFacts, in RecordInput) map[string]*accountGroup {
	canon := r.ident.canon
	out := map[string]*accountGroup{}
	get := func(id string) *accountGroup {
		if out[id] == nil {
			out[id] = &accountGroup{id: id}
		}
		return out[id]
	}
	imported := map[string]cpamp.Account{}
	for _, a := range r.known {
		imported[a.ID] = a
	}
	addMember := func(g *accountGroup, m string) {
		for _, x := range g.members {
			if x == m {
				return
			}
		}
		g.members = append(g.members, m)
	}
	for _, a := range r.known {
		g := get(canon(a.ID))
		addMember(g, a.ID)
		g.imported = append(g.imported, a.ID)
		if sp, ok := r.spans[a.ID]; ok && (g.first == 0 || sp.First < g.first) {
			g.first = sp.First
		}
		g.kinds = append(g.kinds, r.kinds[a.ID]...)
	}
	for ref, info := range r.refs {
		targets := map[string]bool{canon(ref): true}
		if _, ok := r.ident.implicit[ref]; !ok {
			for _, c := range r.ident.certs[ref] {
				targets[canon(c.LegacyAccount)] = true
				get(canon(c.LegacyAccount)).certs = append(get(canon(c.LegacyAccount)).certs, c)
			}
		}
		for t := range targets {
			g := get(t)
			addMember(g, ref)
			g.refs = append(g.refs, ref)
			first := max(info.First, frozen.Filter.At)
			if g.first == 0 || first < g.first {
				g.first = first
			}
			g.kinds = append(g.kinds, info.Kinds...)
			if g.provider == "" {
				g.provider = info.Provider
			}
		}
	}
	for id, g := range out {
		sort.Strings(g.members)
		sort.Strings(g.refs)
		sort.Strings(g.imported)
		sort.Slice(g.certs, func(i, j int) bool { return forwardDigest(g.certs[i]) < forwardDigest(g.certs[j]) })
		g.kinds = uniqueKinds(g.kinds)
		if a, ok := imported[id]; ok {
			g.provider, g.label = a.Provider, a.Label
		}
		for _, m := range g.members {
			if a, ok := imported[m]; ok && g.provider == "" {
				g.provider = a.Provider
			}
			if a, ok := imported[m]; ok && g.label == "" {
				g.label = a.Label
			}
		}
		if g.label == "" {
			for _, ref := range g.refs {
				if info := r.refs[ref]; info != nil && in.Labels[info.Native] != "" {
					g.label = in.Labels[info.Native]
					break
				}
			}
		}
		if g.label == "" {
			g.label = id
		}
	}
	return out
}

func uniqueKinds(kinds []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, k := range []string{"monthly", "weekly", "5h"} {
		for _, x := range kinds {
			if x == k && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// orderAccounts keeps the imported account order, then new accounts by ID.
func (r *Record) orderAccounts(ids []string) {
	pos := map[string]int{}
	for i, a := range r.known {
		pos[a.ID] = i
	}
	sort.Slice(ids, func(i, j int) bool {
		pi, iok := pos[ids[i]]
		pj, jok := pos[ids[j]]
		if iok != jok {
			return iok
		}
		if iok {
			return pi < pj
		}
		return ids[i] < ids[j]
	})
}

// load reads an account's sealed segments from disk.
func (r *Record) load(ctx context.Context, a *recordAccount) error {
	stored, err := r.store.Segments(ctx, a.id)
	if err != nil {
		return err
	}
	for _, g := range stored {
		if !strings.HasPrefix(g.Key, a.key+"|") {
			continue
		}
		var seg recordSegment
		if json.Unmarshal(g.Body, &seg) != nil || !seg.Sealed {
			continue
		}
		a.segs[segmentID{g.Kind, g.Segment}] = &seg
	}
	a.loaded = true
	return nil
}

// derive brings an account's segments up to date.
func (r *Record) derive(ctx context.Context, frozen *cpamp.QuotaFacts, a *recordAccount, config cpamp.ProfileConfig, book cpamp.ComparisonPriceBook, in RecordInput, now int64, deadline time.Time) (derived, sealed int, err error) {
	if a.first == 0 {
		return 0, 0, nil
	}
	dirty := int64(0)
	for _, ref := range a.refs {
		if at, ok := r.pending[ref]; ok && (dirty == 0 || at < dirty) {
			dirty = at
		}
	}
	first, last := segmentOf(a.first), segmentOf(now)
	toSeal := []StoredSegment{}
	defer func() {
		if e := r.store.PutSegments(ctx, a.id, toSeal); e != nil && err == nil {
			err = e
		}
	}()
	for _, kind := range append([]string{""}, a.kinds...) {
		// A window resets after its first reading, at most one window length
		// after now.
		hi := last
		if !bucketKind(kind) {
			hi = segmentOf(now + kindDuration(kind))
		}
		for k := first; k <= hi; k++ {
			id := segmentID{kind, k}
			seg := a.segs[id]
			rg := evidenceRange(kind, k)
			if seg != nil && dirty != 0 && rangeEnd(rg) > dirty {
				seg.dirty = true
			}
			need := seg == nil || seg.Book != bookDigest(book, a.provider, seg.Models) ||
				seg.dirty && (seg.Sealed || now-seg.DerivedMS >= refreshEvery(kind)) ||
				!seg.Sealed && seg.NextMS != 0 && now >= seg.NextMS ||
				!seg.Sealed && now >= sealAt(kind, k)
			if !need {
				if seg.dirty && (r.wake == 0 || seg.DerivedMS+refreshEvery(kind) < r.wake) {
					r.wake = seg.DerivedMS + refreshEvery(kind)
				}
				continue
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				return derived, sealed, ErrRecordPending
			}
			if err = ctx.Err(); err != nil {
				return derived, sealed, err
			}
			seg, err = r.deriveSegment(ctx, frozen, a, config, book, in, now, kind, k)
			if err != nil {
				return derived, sealed, err
			}
			derived++
			a.segs[id] = seg
			if seg.Sealed {
				body, e := json.Marshal(seg)
				if e != nil {
					return derived, sealed, e
				}
				toSeal = append(toSeal, StoredSegment{Kind: kind, Segment: k, Key: a.key + "|" + seg.Book, Body: body})
				sealed++
			}
		}
	}
	return derived, sealed, nil
}

// observations reads one account's meter readings in a range: imported
// readings of its imported members and native readings of its references from
// H on, mapped to the canonical account, sorted by time.
func (r *Record) observations(ctx context.Context, frozen *cpamp.QuotaFacts, a *recordAccount, in RecordInput, rg cpamp.Range) ([]cpamp.Observation, error) {
	h := frozen.Filter.At
	obs := []cpamp.Observation{}
	if rg.ObsTo <= rg.ObsFrom {
		return obs, nil
	}
	if rg.ObsFrom <= h {
		for _, m := range a.imported {
			_, o, err := r.reader.service.AccountEvidenceRange(ctx, frozen, m, cpamp.Range{ObsFrom: rg.ObsFrom, ObsTo: rg.ObsTo, Kinds: rg.Kinds})
			if err != nil {
				return nil, domainError(ctx, err)
			}
			for i := range o {
				o[i].Account = a.id
				obs = append(obs, o[i])
			}
		}
	}
	if len(a.refs) > 0 && rg.ObsTo > h {
		for _, ref := range a.refs {
			readings, err := r.store.Readings(ctx, []string{ref}, max(rg.ObsFrom, h), rg.ObsTo, rg.Kinds)
			if err != nil {
				return nil, err
			}
			for _, rd := range readings {
				atomic, ok := r.ident.readingAccount(in, ref, rd)
				if !ok || r.ident.canon(atomic) != a.id {
					continue
				}
				if o, ok := nativeObservation(rd, a.id); ok {
					obs = append(obs, o)
				}
			}
		}
	}
	sort.SliceStable(obs, func(i, j int) bool {
		if obs[i].Observed != obs[j].Observed {
			return obs[i].Observed < obs[j].Observed
		}
		return obs[i].ID < obs[j].ID
	})
	return obs, nil
}

// events reads one account's requests started in a range, mapped to the
// canonical account, sorted by start and valued with book.
func (r *Record) events(ctx context.Context, frozen *cpamp.QuotaFacts, a *recordAccount, book cpamp.ComparisonPriceBook, in RecordInput, rg cpamp.Range) ([]cpamp.Event, error) {
	h := frozen.Filter.At
	events := []cpamp.Event{}
	if rg.EventsTo <= rg.EventsFrom {
		return events, nil
	}
	if rg.EventsFrom <= h {
		for _, m := range a.imported {
			e, _, err := r.reader.service.AccountEvidenceRange(ctx, frozen, m, cpamp.Range{EventsFrom: rg.EventsFrom, EventsTo: rg.EventsTo})
			if err != nil {
				return nil, domainError(ctx, err)
			}
			for i := range e {
				e[i].Account = a.id
			}
			events = append(events, e...)
		}
	}
	idBase := frozen.Filter.Through
	if hv := frozen.PriceBook.Historical; hv != nil {
		idBase = max(idBase, hv.EventsThrough)
	}
	if len(a.refs) > 0 && rg.EventsTo > h {
		native, err := r.store.Events(ctx, a.refs, max(rg.EventsFrom, h), rg.EventsTo)
		if err != nil {
			return nil, err
		}
		for _, n := range native {
			atomic, ok := r.ident.eventAccount(in, n)
			if !ok || r.ident.canon(atomic) != a.id {
				continue
			}
			ev := n.Event
			ev.Account = a.id
			ev.ID = idBase + n.Seq
			events = append(events, ev)
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Started != events[j].Started {
			return events[i].Started < events[j].Started
		}
		return events[i].ID < events[j].ID
	})
	cpamp.ValueEvents(events, book)
	return events, nil
}

func (r *Record) deriveSegment(ctx context.Context, frozen *cpamp.QuotaFacts, a *recordAccount, config cpamp.ProfileConfig, book cpamp.ComparisonPriceBook, in RecordInput, now int64, kind string, k int64) (*recordSegment, error) {
	rg := evidenceRange(kind, k)
	s0, s1 := segmentStart(k), segmentStart(k+1)
	seg := &recordSegment{Kind: kind, Segment: k, DerivedMS: now, Sealed: now >= sealAt(kind, k), Windows: []cpamp.FactWindow{}, Idle: []bool{}, Latest: map[string]int64{}, Buckets: []cpamp.FactBucket{}, Models: []string{}}
	obs, err := r.observations(ctx, frozen, a, in, rg)
	if err != nil {
		return nil, err
	}
	resets := false
	for _, o := range obs {
		key := o.Kind + "|" + canonicalScope(o.Provider, o.Scope)
		if o.Observed > seg.Latest[key] {
			seg.Latest[key] = o.Observed
		}
		if o.Reset >= s0-2001 && o.Reset < s1+2001 {
			resets = true
		}
	}
	if !bucketKind(kind) && !resets {
		seg.Book = bookDigest(book, a.provider, nil)
		return seg, nil // no window resets in this segment: nothing to derive
	}
	events, err := r.events(ctx, frozen, a, book, in, rg)
	if err != nil {
		return nil, err
	}
	canonicalObservations(obs, events)
	seen := map[string]bool{}
	for _, e := range events {
		if !seen[e.Model] {
			seen[e.Model] = true
			seg.Models = append(seg.Models, e.Model)
		}
	}
	sort.Strings(seg.Models)
	seg.Book = bookDigest(book, a.provider, seg.Models)
	for _, p := range config.Profiles {
		seg.Cards = append(seg.Cards, p.ID)
	}
	seg.Cards = append(seg.Cards, "unassigned")
	frame := cpamp.QuotaFacts{Version: frozen.Version, Filter: frozen.Filter, Config: config, PriceBook: book, APIKeys: frozen.APIKeys}
	frame.Filter.At = now
	account := cpamp.Account{ID: a.id, Provider: a.provider, Label: a.label}
	if bucketKind(kind) {
		part := cpamp.AccountFactsFor(&frame, account, events, nil)
		seg.Buckets, seg.Unassigned, seg.OrderSensitive = part.Buckets, part.Unassigned, part.OrderSensitive
		seg.Requests = int64(len(events))
		if len(events) > 0 {
			seg.First, seg.Last = events[0].Started, events[len(events)-1].Started
		}
		return seg, nil
	}
	part := cpamp.AccountFactsFor(&frame, account, events, obs)
	for _, w := range part.Windows {
		b := w.Base
		if b.Kind != kind || b.Reset < s0 || b.Reset >= s1 {
			continue
		}
		seg.Windows = append(seg.Windows, w)
		seg.Idle = append(seg.Idle, idleWindow(b, obs))
		for _, at := range []int64{b.Start, b.Reset} {
			if at > now && (seg.NextMS == 0 || at < seg.NextMS) {
				seg.NextMS = at
			}
		}
	}
	return seg, nil
}

// idleWindow is Derive's idle rule: every reading of the window says 0% used
// and the first one forecasts its start at its own time.
func idleWindow(w cpamp.QuotaUsageWindow, obs []cpamp.Observation) bool {
	var first *cpamp.Observation
	for i := range obs {
		o := &obs[i]
		if o.Kind != w.Kind || o.Scope != w.Scope || abs64(o.Reset-w.Reset) > 2000 || abs64(o.Start-w.Start) > 2000 {
			continue
		}
		if o.Used == nil || *o.Used != 0 {
			return false
		}
		if first == nil {
			first = o
		}
	}
	return first != nil && abs64(first.Start-first.Observed) <= 10000
}

// attributionOf is the part of a forward attribution certificates match on.
func attributionOf(e RecordEvent) measurement.Attribution {
	return measurement.Attribution{Provider: e.Provider, AccountID: e.Native, CredentialRef: e.Credential}
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// assemble concatenates every account's segments into quota facts at now.
func (r *Record) assemble(frozen *cpamp.QuotaFacts, ids []string, config cpamp.ProfileConfig, book cpamp.ComparisonPriceBook, now int64, st *RecordStatus) *cpamp.QuotaFacts {
	out := &cpamp.QuotaFacts{Version: frozen.Version, Filter: frozen.Filter, Config: config, PriceBook: book, APIKeys: append([]cpamp.APIKey{}, frozen.APIKeys...), Credentials: []cpamp.ProfileCredential{}, UnassignedPeriods: []cpamp.ProfileMembership{}, Buckets: []cpamp.FactBucket{}, Windows: []cpamp.FactWindow{}}
	out.Filter.At = now
	cards := map[string]int{}
	for i, p := range config.Profiles {
		cards[p.ID] = i
	}
	cards["unassigned"] = len(config.Profiles)
	frozenCreds := map[string]cpamp.ProfileCredential{}
	for _, c := range frozen.Credentials {
		frozenCreds[c.Account.ID] = c
	}
	for _, id := range ids {
		a := r.acc[id]
		if a == nil {
			continue
		}
		cred := cpamp.ProfileCredential{Account: cpamp.Account{ID: id, Provider: a.provider, Label: a.label}, Status: "unknown"}
		if c, ok := frozenCreds[id]; ok {
			cred = c
			cred.Requests, cred.First, cred.Last = 0, 0, 0
			cred.Label = a.label
		}
		keys := make([]segmentID, 0, len(a.segs))
		for sid := range a.segs {
			keys = append(keys, sid)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].kind != keys[j].kind {
				return kindOrder(keys[i].kind) < kindOrder(keys[j].kind)
			}
			return keys[i].k < keys[j].k
		})
		latestObs := map[string]int64{}
		for _, sid := range keys {
			for key, at := range a.segs[sid].Latest {
				latestObs[key] = max(latestObs[key], at)
			}
		}
		unassigned := false
		windows := []cpamp.FactWindow{}
		for _, sid := range keys {
			seg := a.segs[sid]
			if seg.Sealed {
				st.Sealed++
			} else {
				st.Open++
			}
			remap := cardMap(seg.Cards, cards)
			if bucketKind(sid.kind) {
				cred.Requests += seg.Requests
				if seg.Requests > 0 {
					if cred.First == 0 || seg.First < cred.First {
						cred.First = seg.First
					}
					cred.Last = max(cred.Last, seg.Last)
				}
				unassigned = unassigned || seg.Unassigned
				out.OrderSensitive = out.OrderSensitive || seg.OrderSensitive
				for _, b := range seg.Buckets {
					if remap != nil {
						b.Cards = remapCards(b.Cards, remap)
					}
					out.Buckets = append(out.Buckets, b)
				}
				if !seg.Sealed && seg.NextMS > now && (out.NextTransition == 0 || seg.NextMS < out.NextTransition) {
					out.NextTransition = seg.NextMS
				}
				continue
			}
			for i, w := range seg.Windows {
				if seg.Idle[i] && w.Base.LastObserved < latestObs[w.Base.Kind+"|"+w.Base.Scope] {
					continue // a later reading superseded this idle forecast
				}
				w.Account = id
				w.Base.Label = a.label
				if remap != nil {
					w.Selections = append([]cpamp.FactSelection{}, w.Selections...)
					for j := range w.Selections {
						w.Selections[j].Cards = remapCards(w.Selections[j].Cards, remap)
					}
					w.Cards = append([]cpamp.FactCard{}, w.Cards...)
					for j := range w.Cards {
						w.Cards[j].Index = remap[w.Cards[j].Index]
					}
				}
				windows = append(windows, w)
				for _, at := range []int64{w.Base.Start, w.Base.Reset} {
					if at > now && (out.NextTransition == 0 || at < out.NextTransition) {
						out.NextTransition = at
					}
				}
			}
		}
		// Latest and current-at-now are decided over the whole record.
		latest := map[string]string{}
		latestAt := map[string]int64{}
		for _, w := range windows {
			key := w.Base.Kind + "|" + w.Base.Scope
			if w.Base.Status == "current" && (latest[key] == "" || w.Base.LastObserved > latestAt[key] || w.Base.LastObserved == latestAt[key] && w.Base.ID < latest[key]) {
				latest[key], latestAt[key] = w.Base.ID, w.Base.LastObserved
			}
		}
		for i := range windows {
			w := &windows[i]
			w.Latest = latest[w.Base.Kind+"|"+w.Base.Scope] == w.Base.ID
			w.Base.Stale = w.Base.Status == "current" && now-w.Base.LastObserved > 600000
			w.Cards = append([]cpamp.FactCard{}, w.Cards...)
			for j := range w.Cards {
				w.Cards[j].CurrentAtPin = w.Latest && cpamp.CardContains(config, w.Cards[j].Index, id, now)
			}
		}
		if len(a.imported) == 0 && cred.Requests == 0 && len(windows) == 0 {
			continue // a native reference whose evidence all belongs to other accounts
		}
		out.Windows = append(out.Windows, windows...)
		st.Windows += len(windows)
		if unassigned {
			out.UnassignedPeriods = append(out.UnassignedPeriods, cpamp.ProfileMembership{Account: id})
		}
		out.Credentials = append(out.Credentials, cred)
	}
	return out
}

// cardMap maps a segment's card indices to the current configuration's, or nil
// when they are the same.
func cardMap(segCards []string, current map[string]int) map[int]int {
	same := len(segCards) == len(current)
	m := make(map[int]int, len(segCards))
	for i, id := range segCards {
		j, ok := current[id]
		if !ok {
			j = current["unassigned"]
		}
		m[i] = j
		same = same && i == j
	}
	if same {
		return nil
	}
	return m
}

func remapCards(in []int, m map[int]int) []int {
	out := make([]int, len(in))
	for i, c := range in {
		out[i] = m[c]
	}
	sort.Ints(out)
	return out
}

// AccountRef is the page's opaque reference for an account or login (imported
// account IDs encode credential file names).
func AccountRef(id string) string { return "acct:" + hash(id)[:24] }

// UsageLogin is one login (credential) of an account.
type UsageLogin struct {
	ID      string `json:"-"`
	Ref     string `json:"ref"`
	Label   string `json:"label"`
	Source  string `json:"source"` // imported or pacer
	FirstMS int64  `json:"first_ms,omitempty"`
	LastMS  int64  `json:"last_ms,omitempty"`
	// SameAs is the login the user linked this one to.
	SameAs string `json:"same_as,omitempty"`
	// Certified: Pacer's cutover certified this login as the imported account.
	Certified bool `json:"certified,omitempty"`
}

// UsageAccount is one account of the record: every login linked to it.
type UsageAccount struct {
	ID       string       `json:"-"`
	Ref      string       `json:"ref"`
	Provider string       `json:"provider"`
	Label    string       `json:"label"`
	Requests int64        `json:"requests"`
	FirstMS  int64        `json:"first_ms,omitempty"`
	LastMS   int64        `json:"last_ms,omitempty"`
	Logins   []UsageLogin `json:"logins"`
}

// Accounts lists the record's accounts as of the last build.
func (r *Record) Accounts() []UsageAccount {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []UsageAccount{}
	if r.groups == nil || r.built == nil {
		return out
	}
	creds := map[string]cpamp.ProfileCredential{}
	for _, c := range r.built.Credentials {
		creds[c.Account.ID] = c
	}
	known := map[string]cpamp.Account{}
	for _, a := range r.known {
		known[a.ID] = a
	}
	user := map[string]string{}
	for _, l := range r.links {
		user[l.Account] = l.SameAs
	}
	ids := make([]string, 0, len(r.groups))
	for id := range r.groups {
		if _, ok := creds[id]; ok {
			ids = append(ids, id)
		}
	}
	r.orderAccounts(ids)
	for _, id := range ids {
		g := r.groups[id]
		c := creds[id]
		a := UsageAccount{ID: id, Ref: AccountRef(id), Provider: g.provider, Label: g.label, Requests: c.Requests, FirstMS: c.First, LastMS: c.Last, Logins: []UsageLogin{}}
		for _, m := range g.members {
			l := UsageLogin{ID: m, Ref: AccountRef(m), Source: "pacer"}
			if k, ok := known[m]; ok {
				l.Label, l.Source = k.Label, "imported"
				if sp, ok := r.spans[m]; ok {
					l.FirstMS, l.LastMS = sp.First, sp.Last
				}
			} else if info := r.refs[m]; info != nil {
				l.Label, l.FirstMS, l.LastMS = r.labels[info.Native], info.First, info.Last
				_, l.Certified = r.ident.implicit[m]
				if l.Label == "" {
					l.Label = info.Provider + " login " + hash(m)[:8]
				}
			}
			if to, ok := user[m]; ok {
				l.SameAs = AccountRef(to)
			}
			a.Logins = append(a.Logins, l)
		}
		out = append(out, a)
	}
	return out
}

// Verification is a proven capacity interval on a record account.
type Verification struct {
	Account            string
	Kind, Scope        string
	FromMS, ToMS       int64
	LowerUSDPerPercent float64
}

// Verifications maps proven capacity intervals of native accounts to the
// record's accounts and pools.
func (r *Record) Verifications(source string, in []CapacityInterval) []Verification {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Verification{}
	if r.ident.canon == nil {
		return out
	}
	for _, c := range in {
		scope, secondsRaw, ok := strings.Cut(c.Window, ":")
		if !ok {
			continue
		}
		var seconds int64
		if json.Unmarshal([]byte(secondsRaw), &seconds) != nil {
			continue
		}
		kind := nativeKind(c.Provider, seconds)
		if !validKind(kind) {
			continue
		}
		ref := ForwardAccountRef(source, c.Provider, c.Native)
		if _, ok := r.ident.implicit[ref]; !ok && len(r.ident.certs[ref]) > 0 {
			continue // certified to several accounts: not attributable
		}
		out = append(out, Verification{Account: r.ident.canon(ref), Kind: kind, Scope: canonicalScope(c.Provider, scope), FromMS: c.FromMS, ToMS: c.ToMS, LowerUSDPerPercent: c.LowerUSDPerPercent})
	}
	return out
}
