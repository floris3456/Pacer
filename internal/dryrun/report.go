// Package dryrun builds Pacer's daily dry-run report from already persisted
// evidence. Build is pure: it performs no I/O, never calls the host or a
// provider, and never changes routing. See docs/dry-run-report.md.
package dryrun

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
)

const (
	Version = "pacer-dry-run-report-v1"
	// MoveLimit and FlipFlopLimit bound the item lists; totals stay exact.
	MoveLimit     = 200
	FlipFlopLimit = 200
	// FlipFlopWindow is PLAN §6: a chat should not move back within 24 h.
	FlipFlopWindow = 24 * time.Hour
	// FreshWithin is the routing freshness policy for a balance reading.
	FreshWithin = time.Hour
	// MaxLookback bounds how far before the day the simulation starts.
	MaxLookback = 8 * 24 * time.Hour

	chatRefDomain = "pacer-dry-run-chat-ref-v1\x00"
	chatRefMeta   = "hmac-sha256(installation secret, chat key), first 64 bits, hex"
)

type Interval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Decision is one persisted dry-run trace row. Chat is the private chat key and
// never leaves this package; reports carry only its keyed chat_ref.
type Decision struct {
	Seq             int64
	At              time.Time
	Chat            string
	RequestID       string
	Provider        string
	Model           string
	PreviousAccount string
	ActualAuth      string
	ActualAccount   string
	Proposed        routing.Decision
	Outcome         string
}

// Attempt is one priced usage record, joined to decisions by request and account.
type Attempt struct {
	RequestID string
	Account   string
	StartedAt time.Time
	EndedAt   time.Time
	NanoUSD   int64
	Exact     bool
}

type Reading struct {
	At          time.Time
	UsedPercent float64
}

// Ratio is the measured dollars per percent of exactly this cycle's window.
type Ratio struct {
	Known         bool
	USDPerPercent float64
	Reasons       []string
}

// Cycle is one provider quota cycle that may have ended in the report day.
type Cycle struct {
	Provider  string
	Account   string
	Scope     string
	Seconds   int64
	CycleID   string
	Scheduled time.Time // provider deadline carried by the cycle ID; zero if unknown
	Closed    time.Time // when a new cycle was first observed; zero if not yet
	Last      *Reading  // newest usable reading of this cycle at or before its end
	Ratio     Ratio
}

type Input struct {
	Day    time.Time // UTC midnight of the report day
	Now    time.Time
	Secret []byte
	// Since is the start of the supplied decision evidence (zero: unbounded).
	Since     time.Time
	Decisions []Decision // ascending by (At, Seq); may start before the day
	// Coverage holds, per routed provider, the intervals in which dry-run mode
	// was proven active by the evidence ledger.
	Coverage map[string][]Interval
	// ErrorSegments are ledger intervals in which a decision write failed.
	ErrorSegments []Interval
	Cycles        []Cycle
	Attempts      []Attempt
	// AttemptsUnavailable is a reason that applies to every projection.
	AttemptsUnavailable string
}

type Report struct {
	Meta         Meta        `json:"meta"`
	Placements   []Placement `json:"placements"`
	WouldBeMoves Moves       `json:"would_be_moves"`
	FlipFlops    FlipFlops   `json:"flip_flops"`
	Resets       []Reset     `json:"resets"`
}

type Meta struct {
	Version               string             `json:"version"`
	Day                   string             `json:"day"`
	From                  time.Time          `json:"from"`
	To                    time.Time          `json:"to"`
	GeneratedAt           time.Time          `json:"generated_at"`
	Complete              bool               `json:"complete"`
	Reasons               []string           `json:"reasons"`
	Decisions             int                `json:"decisions"`
	Chats                 int                `json:"chats"`
	UnattributedDecisions int                `json:"unattributed_decisions"`
	Coverage              []ProviderCoverage `json:"coverage"`
	ChatRef               string             `json:"chat_ref"`
}

type ProviderCoverage struct {
	Provider       string     `json:"provider"`
	CoveredSeconds int64      `json:"covered_seconds"`
	DaySeconds     int64      `json:"day_seconds"`
	Complete       bool       `json:"complete"`
	Intervals      []Interval `json:"intervals"`
}

type Placement struct {
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	Decisions int             `json:"decisions"`
	Chats     int             `json:"chats"`
	Reasons   map[string]int  `json:"reasons"`
	Outcomes  map[string]int  `json:"outcomes"`
	Agree     int             `json:"agree"`
	Differ    int             `json:"differ"`
	Unhandled int             `json:"unhandled"`
	Accounts  []AccountServed `json:"accounts"`
}

// AccountServed compares where CPA served admitted requests (actual, fill-first)
// with Pacer's proposals and the pace simulation.
type AccountServed struct {
	Account   string `json:"account"`
	Actual    int    `json:"actual"`
	Proposed  int    `json:"proposed"`
	Simulated int    `json:"simulated"`
}

type Moves struct {
	Total     int    `json:"total"`
	Limit     int    `json:"limit"`
	Truncated bool   `json:"truncated"`
	Items     []Move `json:"items"`
}

type Move struct {
	At       time.Time `json:"at"`
	ChatRef  string    `json:"chat_ref"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	From     string    `json:"from_account"`
	To       string    `json:"to_account"`
	Actual   string    `json:"actual_account"`
	Reason   string    `json:"reason"`
	Forced   bool      `json:"forced"`
	Pace     float64   `json:"pace"`
}

type FlipFlops struct {
	WindowHours int        `json:"window_hours"`
	Total       int        `json:"total"`
	Limit       int        `json:"limit"`
	Truncated   bool       `json:"truncated"`
	Items       []FlipFlop `json:"items"`
}

type FlipFlop struct {
	ChatRef    string    `json:"chat_ref"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Account    string    `json:"account"`
	Via        string    `json:"via_account"`
	LeftAt     time.Time `json:"left_at"`
	ReturnedAt time.Time `json:"returned_at"`
	Reason     string    `json:"reason"`
}

type Reset struct {
	Provider      string     `json:"provider"`
	Account       string     `json:"account"`
	Scope         string     `json:"scope"`
	WindowSeconds int64      `json:"window_seconds"`
	CycleID       string     `json:"cycle_id"`
	CycleStart    *time.Time `json:"cycle_start"`
	ResetAt       time.Time  `json:"reset_at"`
	ResetBasis    string     `json:"reset_basis"`
	Baseline      Baseline   `json:"baseline"`
	Projection    Projection `json:"projection"`
}

// Baseline is the observed unused percent at the reset. Dry-run never changes
// CPA's selection, so the observation is the fill-first baseline.
type Baseline struct {
	Policy        string     `json:"policy"`
	Status        string     `json:"status"`
	UnusedPercent *float64   `json:"unused_percent"`
	ObservedAt    *time.Time `json:"observed_at"`
	GapSeconds    *int64     `json:"gap_seconds"`
}

type Projection struct {
	Policy           string   `json:"policy"`
	Known            bool     `json:"known"`
	UnusedPercent    *float64 `json:"unused_percent"`
	RawUnusedPercent *float64 `json:"raw_unused_percent"`
	PercentPerUSD    *float64 `json:"percent_per_usd"`
	ValueInUSD       float64  `json:"value_in_usd"`
	ValueOutUSD      float64  `json:"value_out_usd"`
	RequestsIn       int      `json:"requests_in"`
	RequestsOut      int      `json:"requests_out"`
	Assumptions      []string `json:"assumptions"`
	Reasons          []string `json:"reasons"`
}

// ChatRef is the stable keyed reference for a private chat key. The secret is
// per installation, so a ref cannot be recomputed from guessable chat inputs.
func ChatRef(secret []byte, chat string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(chatRefDomain))
	m.Write([]byte(chat))
	return hex.EncodeToString(m.Sum(nil)[:8])
}

func isMove(reason string) bool { return reason == "cold_move" || reason == "forced_move" }

// Build computes the report for one UTC day.
func Build(in Input) (Report, error) {
	if len(in.Secret) == 0 {
		return Report{}, errors.New("dryrun: installation secret required")
	}
	from := in.Day.UTC()
	if from.Hour() != 0 || from.Minute() != 0 || from.Second() != 0 || from.Nanosecond() != 0 {
		return Report{}, errors.New("dryrun: day must be a UTC midnight")
	}
	if in.Now.Before(from) {
		return Report{}, errors.New("dryrun: day has not started")
	}
	to := from.Add(24 * time.Hour)
	ds := in.Decisions
	if !sort.SliceIsSorted(ds, func(i, j int) bool { return before(ds[i], ds[j]) }) {
		ds = append([]Decision(nil), ds...)
		sort.SliceStable(ds, func(i, j int) bool { return before(ds[i], ds[j]) })
	}
	ds = ds[:sort.Search(len(ds), func(i int) bool { return !ds[i].At.Before(to) })]
	b := builder{in: in, ds: ds, from: from, to: to, cov: newCoverage(in.Coverage)}
	b.simulate()
	r := Report{
		Placements:   b.placements(),
		WouldBeMoves: b.moveItems(),
		FlipFlops:    b.flipItems(),
		Resets:       b.resets(),
	}
	r.Meta = b.meta()
	return r, nil
}

func before(a, b Decision) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	return a.Seq < b.Seq
}

type moveEvent struct {
	index int
	from  string
}

type flipEvent struct {
	index         int
	account, via  string
	left, arrived time.Time
}

type departure struct {
	account string
	at      time.Time
}

type chatState struct {
	account  string
	interval int
	left     []departure
}

type builder struct {
	in       Input
	ds       []Decision
	from, to time.Time
	cov      coverageIndex
	served   []string
	moves    []moveEvent
	flips    []flipEvent
	groups   map[string]group
	attempts map[string]*attemptSum
	refs     map[string]string
	chats    int
	unattrib int
}

// simulate replays Pacer's proposals per chat. A placement or move proposal
// puts the chat on the proposed account; a proposal to stay keeps the
// simulated account; an unhandled request follows CPA's own pick. A chat's
// state restarts from CPA's binding whenever dry-run proof is interrupted.
func (b *builder) simulate() {
	b.served = make([]string, len(b.ds))
	states := map[string]*chatState{}
	for i, d := range b.ds {
		key := d.Provider + "\x00" + d.Chat
		index := b.cov.index(d.Provider, d.At)
		st := states[key]
		if st == nil {
			st = &chatState{interval: -1}
			states[key] = st
		}
		if index < 0 || st.interval != index {
			st.account, st.interval, st.left = d.PreviousAccount, index, st.left[:0]
		}
		if d.Outcome != "admitted" {
			continue
		}
		p := d.Proposed
		prior := st.account
		switch {
		case !p.Handled || p.Account == "":
			st.account = d.ActualAccount
		case isMove(p.Reason) || p.Reason == "new_chat":
			st.account = p.Account
		case st.account == "":
			st.account = p.Account
		}
		b.served[i] = st.account
		if !p.Handled || !isMove(p.Reason) || prior == "" || prior == st.account {
			continue
		}
		for _, l := range st.left {
			if l.account == st.account && d.At.Sub(l.at) <= FlipFlopWindow {
				b.flips = append(b.flips, flipEvent{index: i, account: st.account, via: prior, left: l.at, arrived: d.At})
				break
			}
		}
		replaced := false
		for j := range st.left {
			if st.left[j].account == prior {
				st.left[j].at, replaced = d.At, true
			}
		}
		if !replaced {
			st.left = append(st.left, departure{prior, d.At})
		}
		b.moves = append(b.moves, moveEvent{index: i, from: prior})
	}
}

func (b *builder) inDay(t time.Time) bool { return !t.Before(b.from) && t.Before(b.to) }

func (b *builder) ref(chat string) string {
	if b.refs == nil {
		b.refs = map[string]string{}
	}
	if r, ok := b.refs[chat]; ok {
		return r
	}
	r := ChatRef(b.in.Secret, chat)
	b.refs[chat] = r
	return r
}

type placementAgg struct {
	Placement
	chats    map[string]struct{}
	accounts map[string]*AccountServed
}

func (b *builder) placements() []Placement {
	aggs := map[[2]string]*placementAgg{}
	chats := map[string]struct{}{}
	first := sort.Search(len(b.ds), func(i int) bool { return !b.ds[i].At.Before(b.from) })
	for i := first; i < len(b.ds); i++ {
		d := b.ds[i]
		chats[d.Provider+"\x00"+d.Chat] = struct{}{}
		k := [2]string{d.Provider, d.Model}
		a := aggs[k]
		if a == nil {
			a = &placementAgg{Placement: Placement{Provider: d.Provider, Model: d.Model, Reasons: map[string]int{}, Outcomes: map[string]int{}}, chats: map[string]struct{}{}, accounts: map[string]*AccountServed{}}
			aggs[k] = a
		}
		account := func(id string) *AccountServed {
			s := a.accounts[id]
			if s == nil {
				s = &AccountServed{Account: id}
				a.accounts[id] = s
			}
			return s
		}
		a.Decisions++
		a.chats[d.Chat] = struct{}{}
		a.Reasons[label(d.Proposed.Reason)]++
		a.Outcomes[label(d.Outcome)]++
		p := d.Proposed
		switch {
		case !p.Handled || p.Account == "":
			a.Unhandled++
		case d.ActualAccount != "" && p.Account == d.ActualAccount || d.ActualAccount == "" && p.Auth == d.ActualAuth:
			a.Agree++
		default:
			a.Differ++
		}
		if p.Handled && p.Account != "" {
			account(p.Account).Proposed++
		}
		if d.ActualAccount == "" {
			b.unattrib++
		}
		if d.Outcome == "admitted" {
			if d.ActualAccount != "" {
				account(d.ActualAccount).Actual++
			}
			if b.served[i] != "" {
				account(b.served[i]).Simulated++
			}
		}
	}
	b.chats = len(chats)
	out := make([]Placement, 0, len(aggs))
	for _, a := range aggs {
		a.Chats = len(a.chats)
		a.Accounts = make([]AccountServed, 0, len(a.accounts))
		for _, s := range a.accounts {
			a.Accounts = append(a.Accounts, *s)
		}
		sort.Slice(a.Accounts, func(i, j int) bool { return a.Accounts[i].Account < a.Accounts[j].Account })
		out = append(out, a.Placement)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func label(s string) string {
	if s == "" {
		return "unspecified"
	}
	return s
}

func (b *builder) moveItems() Moves {
	m := Moves{Limit: MoveLimit, Items: []Move{}}
	for _, e := range b.moves {
		d := b.ds[e.index]
		if !b.inDay(d.At) {
			continue
		}
		m.Total++
		if len(m.Items) == MoveLimit {
			m.Truncated = true
			continue
		}
		m.Items = append(m.Items, Move{At: d.At, ChatRef: b.ref(d.Chat), Provider: d.Provider, Model: d.Model, From: e.from, To: b.served[e.index], Actual: d.ActualAccount, Reason: d.Proposed.Reason, Forced: d.Proposed.Forced, Pace: finiteOrZero(d.Proposed.Pace)})
	}
	return m
}

func (b *builder) flipItems() FlipFlops {
	f := FlipFlops{WindowHours: int(FlipFlopWindow / time.Hour), Limit: FlipFlopLimit, Items: []FlipFlop{}}
	for _, e := range b.flips {
		d := b.ds[e.index]
		if !b.inDay(e.arrived) {
			continue
		}
		f.Total++
		if len(f.Items) == FlipFlopLimit {
			f.Truncated = true
			continue
		}
		f.Items = append(f.Items, FlipFlop{ChatRef: b.ref(d.Chat), Provider: d.Provider, Model: d.Model, Account: e.account, Via: e.via, LeftAt: e.left, ReturnedAt: e.arrived, Reason: d.Proposed.Reason})
	}
	return f
}

func finiteOrZero(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func (b *builder) meta() Meta {
	m := Meta{Version: Version, Day: b.from.Format("2006-01-02"), From: b.from, To: b.to, GeneratedAt: b.in.Now, Reasons: []string{}, Coverage: []ProviderCoverage{}, ChatRef: chatRefMeta, Chats: b.chats, UnattributedDecisions: b.unattrib}
	providers := map[string]bool{}
	for provider, intervals := range b.cov {
		for _, iv := range intervals {
			if iv.From.Before(b.to) && iv.To.After(b.from) {
				providers[provider] = true
			}
		}
	}
	first := sort.Search(len(b.ds), func(i int) bool { return !b.ds[i].At.Before(b.from) })
	m.Decisions = len(b.ds) - first
	for _, d := range b.ds[first:] {
		providers[d.Provider] = true
	}
	names := make([]string, 0, len(providers))
	for p := range providers {
		names = append(names, p)
	}
	sort.Strings(names)
	end := b.to
	if b.in.Now.Before(end) {
		end = b.in.Now
	}
	total, partial := int64(0), false
	for _, p := range names {
		c := ProviderCoverage{Provider: p, DaySeconds: int64(b.to.Sub(b.from) / time.Second), Intervals: []Interval{}}
		for _, iv := range b.cov[p] {
			lo, hi := later(iv.From, b.from), earlier(iv.To, b.to)
			if lo.Before(hi) {
				c.Intervals = append(c.Intervals, Interval{lo, hi})
				c.CoveredSeconds += int64(hi.Sub(lo) / time.Second)
			}
		}
		c.Complete = b.cov.contains(p, b.from, b.to)
		partial = partial || !b.cov.contains(p, b.from, end)
		total += c.CoveredSeconds
		m.Coverage = append(m.Coverage, c)
	}
	if b.in.Now.Before(b.to) {
		m.Reasons = append(m.Reasons, "day_in_progress")
	}
	switch {
	case total == 0:
		m.Reasons = append(m.Reasons, "dry_run_not_active")
	case partial:
		m.Reasons = append(m.Reasons, "dry_run_coverage_partial")
	}
	if overlaps(b.in.ErrorSegments, b.from, b.to) {
		m.Reasons = append(m.Reasons, "decision_write_errors")
	}
	if m.UnattributedDecisions > 0 {
		m.Reasons = append(m.Reasons, "decisions_unattributed")
	}
	m.Complete = len(m.Reasons) == 0
	return m
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func overlaps(segments []Interval, from, to time.Time) bool {
	for _, s := range segments {
		if s.From.Before(to) && s.To.After(from) {
			return true
		}
	}
	return false
}

// coverageIndex holds merged, sorted half-open intervals per provider.
type coverageIndex map[string][]Interval

func newCoverage(in map[string][]Interval) coverageIndex {
	c := coverageIndex{}
	for provider, intervals := range in {
		list := make([]Interval, 0, len(intervals))
		for _, iv := range intervals {
			if iv.From.Before(iv.To) {
				list = append(list, iv)
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].From.Before(list[j].From) })
		merged := []Interval{}
		for _, iv := range list {
			if n := len(merged); n > 0 && !iv.From.After(merged[n-1].To) {
				merged[n-1].To = later(merged[n-1].To, iv.To)
				continue
			}
			merged = append(merged, iv)
		}
		c[provider] = merged
	}
	return c
}

func (c coverageIndex) index(provider string, t time.Time) int {
	list := c[provider]
	i := sort.Search(len(list), func(i int) bool { return list[i].To.After(t) })
	if i < len(list) && !t.Before(list[i].From) {
		return i
	}
	return -1
}

func (c coverageIndex) contains(provider string, from, to time.Time) bool {
	if !from.Before(to) {
		return true
	}
	i := c.index(provider, from)
	return i >= 0 && !c[provider][i].To.Before(to)
}
