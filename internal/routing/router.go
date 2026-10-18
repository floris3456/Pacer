// Package routing places chats by 7-day pace (docs/pace-routing.md). It does
// no I/O, uses provider meters only, and never treats an elapsed reset as
// replenishment.
package routing

import (
	"math"
	"sort"
	"time"
)

// LongSeconds separates the paced meters (7 days and longer) from the
// short windows that are simply used.
const LongSeconds = 7 * 24 * 3600

// Budget is one applicable provider window. Known means a usable reading: at
// most an hour old, with a future reset unless the window is dormant (reset,
// not yet started), which counts as 100 % left.
type Budget struct {
	Seconds   int64     `json:"seconds"`
	Scope     string    `json:"scope,omitempty"`
	Remaining float64   `json:"remaining"`
	Reset     time.Time `json:"reset,omitzero"`
	Known     bool      `json:"known"`
	Dormant   bool      `json:"dormant,omitempty"`
}

// Account is a credential CPA offers for this request; aliases share Account
// identity. Cooling and disabled credentials are never offered. The caller may
// add the chat's own account with NotOffered set: it is never chosen, but when
// its meters are usable the decision says so (Decision.Unoffered), because CPA
// left it out for another reason (a failed attempt of this request, a
// transient-error cooldown). Paused accounts take no new or moved chats; their
// warm chats stay. USDPerPercent is the measured value of 1 % of the account's
// shared ("all") 7-day meter, 0 when unmeasured.
type Account struct {
	Auth, Account string
	Budgets       []Budget
	USDPerPercent float64 `json:"-"`
	NotOffered    bool    `json:"-"`
	Paused        bool    `json:"-"`
}

// Chat is the chat being placed. Account is empty for a new chat. Warm means
// its prompt cache may be warm on Account: the chat stays unless that account
// cannot serve it.
type Chat struct {
	Account string
	Warm    bool
}

type Config struct {
	Band                  float64 // accounts with pace >= Band × best are equivalent
	ShortFloor, LongFloor float64 // % left at or below which an account cannot serve
	NewShort              float64 // minimum short-window % left for a new placement
	MinHorizon            time.Duration
	// Demand is the provider's measured spend in USD per hour, 0 when
	// unmeasured. An account is due when Demand needs about the time to its
	// 7-day reset, give or take Slack, to spend its 7-day balance.
	Demand float64
	Slack  time.Duration
}

func Default() Config {
	return Config{Band: .9, ShortFloor: 2, LongFloor: 1, NewShort: 15, MinHorizon: time.Hour, Slack: 48 * time.Hour}
}

// Candidate records the inputs of one offered account in the decision trace.
type Candidate struct {
	Auth     string   `json:"auth"`
	Account  string   `json:"account"`
	Budgets  []Budget `json:"budgets"`
	Known    bool     `json:"known"`
	Pace     float64  `json:"pace"`
	Eligible bool     `json:"eligible"`
	Paused   bool     `json:"paused,omitempty"`
	Offered  *bool    `json:"offered,omitempty"` // false only for the chat's own account CPA left out
	// The measured inputs: $ per 1 % of the 7-day meter, the hours the
	// measured demand needs to spend the 7-day balance, and whether that makes
	// the account due.
	USDPerPercent float64 `json:"usd_per_percent,omitempty"`
	NeedHours     float64 `json:"need_hours,omitempty"`
	Due           bool    `json:"due,omitempty"`
}

type Decision struct {
	Handled bool   `json:"handled"`
	Auth    string `json:"auth,omitempty"`
	Account string `json:"account,omitempty"`
	Reason  string `json:"reason"`
	Forced  bool   `json:"forced"`
	// Unoffered: a warm chat's own account was not offered although its meters
	// can serve it. The caller may wait for it instead of forcing the move.
	Unoffered  bool        `json:"unoffered,omitempty"`
	Pace       float64     `json:"pace,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
}

// Reasons. Only cold_move is a discretionary move: the client keeps its
// context. forced_move leaves an account that cannot serve.
const (
	Warm         = "warm"
	NewChat      = "new_chat"
	ColdStay     = "cold_stay"
	ColdMove     = "cold_move"
	ForcedMove   = "forced_move"
	NoEligible   = "no_eligible"
	NoUsableData = "no_usable_meters"
	// WarmAuxiliary: a request on the chat's model besides its steps (a warming
	// ping) stays on the chat's account, where it reads and renews the cache.
	WarmAuxiliary = "warm_auxiliary"
	// KeepAccount: with routing off, a bound chat's request on its model stays
	// on the chat's account. CPA keeps its own chat map only in memory, so
	// after a CPA restart it would move the chat and its pings.
	KeepAccount = "keep_account"
)

type score struct {
	Account
	known, hard       bool
	pace, short, long float64
	running           bool      // a short window is running (its capacity expires at its reset)
	shortReset        time.Time // earliest reset of a running short window
	// need: hours the measured demand needs to spend the 7-day balance; spare:
	// hours to the 7-day reset minus need (negative when it cannot be spent).
	need, spare float64
	due         bool
}

func Pick(cfg Config, now time.Time, chat Chat, accounts []Account) Decision {
	scores := make([]score, 0, len(accounts))
	var current *score
	for _, a := range accounts {
		s := evaluate(cfg, now, a)
		scores = append(scores, s)
		// The chat's account: an offered alias first, then the lowest auth.
		if chat.Account != "" && a.Account == chat.Account && (current == nil || (current.NotOffered && !s.NotOffered) || (current.NotOffered == s.NotOffered && s.Auth < current.Auth)) {
			c := s
			current = &c
		}
	}
	trace := make([]Candidate, 0, len(scores))
	eligible := []score{}
	anyKnown := false
	for _, s := range scores {
		if !s.NotOffered {
			anyKnown = anyKnown || s.known
		}
		ok := s.known && !s.hard && !s.NotOffered && !s.Paused && (s.Account.Account == chat.Account || s.short >= cfg.NewShort)
		if ok {
			eligible = append(eligible, s)
		}
		c := Candidate{Auth: s.Auth, Account: s.Account.Account, Budgets: s.Budgets, Known: s.known, Pace: s.pace, Eligible: ok, Paused: s.Paused, USDPerPercent: s.USDPerPercent, NeedHours: s.need, Due: s.due}
		if s.NotOffered {
			c.Offered = new(bool)
		}
		trace = append(trace, c)
	}
	forced := chat.Account != "" && (current == nil || current.hard || current.NotOffered)
	if chat.Warm && current != nil && !forced {
		return chosen(*current, Warm, false, trace)
	}
	unoffered := chat.Warm && current != nil && current.NotOffered && current.known && !current.hard
	if len(eligible) == 0 {
		reason := NoEligible
		if !anyKnown {
			reason = NoUsableData
		}
		return Decision{Reason: reason, Forced: forced, Unoffered: unoffered, Candidates: trace}
	}
	choice := placement(cfg, chat, eligible)
	switch {
	case forced:
		d := chosen(choice, ForcedMove, true, trace)
		d.Unoffered = unoffered
		return d
	case chat.Account == "":
		return chosen(choice, NewChat, false, trace)
	case choice.Account.Account == chat.Account:
		return chosen(choice, ColdStay, false, trace)
	}
	return chosen(choice, ColdMove, false, trace)
}

// placement picks among eligible accounts. Due accounts come first: the chat's
// own account, then the least time to spare, then lowest auth. Without a due
// account, the best pace band: stay, then spend a running short window (the
// one that expires first), then lowest auth. Dormant capacity does not expire.
func placement(cfg Config, chat Chat, eligible []score) score {
	due := []score{}
	for _, s := range eligible {
		if s.due {
			due = append(due, s)
		}
	}
	if len(due) > 0 {
		sort.Slice(due, func(i, j int) bool {
			a, b := due[i], due[j]
			if stay := a.Account.Account == chat.Account; stay != (b.Account.Account == chat.Account) {
				return stay
			}
			if a.spare != b.spare {
				return a.spare < b.spare
			}
			return a.Auth < b.Auth
		})
		return due[0]
	}
	best := 0.0
	for _, s := range eligible {
		best = math.Max(best, s.pace)
	}
	band := []score{}
	for _, s := range eligible {
		if s.pace >= best*cfg.Band {
			band = append(band, s)
		}
	}
	sort.Slice(band, func(i, j int) bool {
		a, b := band[i], band[j]
		if stay := a.Account.Account == chat.Account; stay != (b.Account.Account == chat.Account) {
			return stay
		}
		if a.running != b.running {
			return a.running
		}
		if a.running && !a.shortReset.Equal(b.shortReset) {
			return a.shortReset.Before(b.shortReset)
		}
		return a.Auth < b.Auth
	})
	return band[0]
}

// Pace is the account's 7-day pace in % per hour, if its meters are usable.
func Pace(cfg Config, now time.Time, a Account) (float64, bool) {
	s := evaluate(cfg, now, a)
	return s.pace, s.known
}

// Spend is the hours the measured demand needs to spend the account's 7-day
// balance and whether that makes it due, when both are measured.
func Spend(cfg Config, now time.Time, a Account) (need float64, due, ok bool) {
	s := evaluate(cfg, now, a)
	return s.need, s.due, s.need > 0
}

// Preview tells, without a request, where the chat's next routing moments send
// it (docs/pace-routing.md, "Next and forced use"). next is the pick once the
// chat's cache is cold. force is the forced move off its account: now, when the
// account cannot serve (then force is next), or soon, when one of its meters is
// within soon points of its floor. An account CPA left out whose meters can
// serve still counts as serving: a warm chat waits for it. An unhandled
// decision has nothing to show.
func Preview(cfg Config, now time.Time, account string, accounts []Account, soon float64) (next, force Decision) {
	offered, waiting := false, false
	for _, a := range accounts {
		if a.Account != account {
			continue
		}
		s := evaluate(cfg, now, a)
		offered = offered || !a.NotOffered
		waiting = waiting || (a.NotOffered && s.known && !s.hard)
	}
	if !offered && waiting {
		accounts = withOffer(accounts, account, false)
	}
	next = Pick(cfg, now, Chat{Account: account}, accounts)
	if next.Forced {
		return next, next
	}
	near := false
	for _, a := range accounts {
		if s := evaluate(cfg, now, a); a.Account == account && !a.NotOffered && s.known {
			near = near || s.short <= cfg.ShortFloor+soon || s.long <= cfg.LongFloor+soon
		}
	}
	if !near {
		return next, Decision{}
	}
	return next, Pick(cfg, now, Chat{Account: account}, withOffer(accounts, account, true))
}

// withOffer copies accounts with the given account's NotOffered set.
func withOffer(accounts []Account, account string, notOffered bool) []Account {
	out := append([]Account(nil), accounts...)
	for i := range out {
		if out[i].Account == account {
			out[i].NotOffered = notOffered
		}
	}
	return out
}

func chosen(s score, reason string, forced bool, trace []Candidate) Decision {
	return Decision{Handled: true, Auth: s.Auth, Account: s.Account.Account, Pace: s.pace, Reason: reason, Forced: forced, Candidates: trace}
}

// evaluate: pace = 7-day % left / max(MinHorizon, reset − now), the rate that
// spends the meter by its reset (a dormant 7-day window has its full length
// ahead). With several paced windows the slowest binds. Short windows only
// gate eligibility. An account is known when every window is usable and at
// least one is paced. With the account's measured $ per 1 % and the measured
// demand, need is the hours that demand takes to spend the shared 7-day
// balance; the account is due when need is within Slack of the time to that
// reset. An account with more than the demand can spend is not due, nor one
// whose reset is still far off.
func evaluate(cfg Config, now time.Time, a Account) score {
	s := score{Account: a, known: true, pace: math.Inf(1), short: math.Inf(1), long: math.Inf(1)}
	paced := false
	week, weekLeft, weekAhead := false, 0.0, time.Duration(0)
	for _, b := range a.Budgets {
		remaining := b.Remaining
		if b.Dormant {
			remaining = 100
		}
		valid := b.Known && b.Seconds > 0 && !math.IsNaN(remaining) && !math.IsInf(remaining, 0) && remaining >= 0 && remaining <= 100 && (b.Dormant || b.Reset.After(now))
		if !valid {
			s.known = false
			continue
		}
		if b.Seconds >= LongSeconds {
			paced = true
			s.hard = s.hard || remaining <= cfg.LongFloor
			s.long = math.Min(s.long, remaining)
			horizon := time.Duration(b.Seconds) * time.Second
			if !b.Dormant {
				horizon = b.Reset.Sub(now)
			}
			s.pace = math.Min(s.pace, remaining/math.Max(cfg.MinHorizon.Hours(), horizon.Hours()))
			if b.Seconds == LongSeconds && b.Scope == "all" {
				week, weekLeft, weekAhead = true, remaining, horizon
			}
			continue
		}
		s.hard = s.hard || remaining <= cfg.ShortFloor
		s.short = math.Min(s.short, remaining)
		if !b.Dormant {
			if !s.running || b.Reset.Before(s.shortReset) {
				s.shortReset = b.Reset
			}
			s.running = true
		}
	}
	if !paced {
		s.known = false
	}
	if !s.known {
		s.pace = 0
	}
	if s.known && week && a.USDPerPercent > 0 && cfg.Demand > 0 && cfg.Slack > 0 {
		s.need = weekLeft * a.USDPerPercent / cfg.Demand
		s.spare = weekAhead.Hours() - s.need
		s.due = !s.hard && math.Abs(s.spare) <= cfg.Slack.Hours()
	}
	return s
}
