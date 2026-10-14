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
	Remaining float64   `json:"remaining"`
	Reset     time.Time `json:"reset,omitzero"`
	Known     bool      `json:"known"`
	Dormant   bool      `json:"dormant,omitempty"`
}

// Account contains only credentials CPA currently offers; cooling and disabled
// credentials must never be added by the caller. Aliases share Account identity.
type Account struct {
	Auth, Account string
	Budgets       []Budget
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
}

func Default() Config {
	return Config{Band: .9, ShortFloor: 2, LongFloor: 1, NewShort: 15, MinHorizon: time.Hour}
}

// Candidate records the inputs of one offered account in the decision trace.
type Candidate struct {
	Auth     string   `json:"auth"`
	Account  string   `json:"account"`
	Budgets  []Budget `json:"budgets"`
	Known    bool     `json:"known"`
	Pace     float64  `json:"pace"`
	Eligible bool     `json:"eligible"`
}

type Decision struct {
	Handled    bool        `json:"handled"`
	Auth       string      `json:"auth,omitempty"`
	Account    string      `json:"account,omitempty"`
	Reason     string      `json:"reason"`
	Forced     bool        `json:"forced"`
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
)

type score struct {
	Account
	known, hard bool
	pace, short float64
	running     bool      // a short window is running (its capacity expires at its reset)
	shortReset  time.Time // earliest reset of a running short window
}

func Pick(cfg Config, now time.Time, chat Chat, accounts []Account) Decision {
	scores := make([]score, 0, len(accounts))
	var current *score
	for _, a := range accounts {
		s := evaluate(cfg, now, a)
		scores = append(scores, s)
		if chat.Account != "" && a.Account == chat.Account && (current == nil || s.Auth < current.Auth) {
			c := s
			current = &c
		}
	}
	trace := make([]Candidate, 0, len(scores))
	eligible := []score{}
	anyKnown := false
	for _, s := range scores {
		anyKnown = anyKnown || s.known
		ok := s.known && !s.hard && (s.Account.Account == chat.Account || s.short >= cfg.NewShort)
		if ok {
			eligible = append(eligible, s)
		}
		trace = append(trace, Candidate{Auth: s.Auth, Account: s.Account.Account, Budgets: s.Budgets, Known: s.known, Pace: s.pace, Eligible: ok})
	}
	forced := chat.Account != "" && (current == nil || current.hard)
	if chat.Warm && current != nil && !forced {
		return chosen(*current, Warm, false, trace)
	}
	if len(eligible) == 0 {
		reason := NoEligible
		if !anyKnown {
			reason = NoUsableData
		}
		return Decision{Reason: reason, Forced: forced, Candidates: trace}
	}
	best := 0.0
	for _, s := range eligible {
		best = math.Max(best, s.pace)
	}
	band := eligible[:0]
	for _, s := range eligible {
		if s.pace >= best*cfg.Band {
			band = append(band, s)
		}
	}
	// Within the band: stay, then spend a running short window (the one that
	// expires first), then lowest auth. Dormant capacity does not expire.
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
	choice := band[0]
	switch {
	case forced:
		return chosen(choice, ForcedMove, true, trace)
	case chat.Account == "":
		return chosen(choice, NewChat, false, trace)
	case choice.Account.Account == chat.Account:
		return chosen(choice, ColdStay, false, trace)
	}
	return chosen(choice, ColdMove, false, trace)
}

// Pace is the account's 7-day pace in % per hour, if its meters are usable.
func Pace(cfg Config, now time.Time, a Account) (float64, bool) {
	s := evaluate(cfg, now, a)
	return s.pace, s.known
}

func chosen(s score, reason string, forced bool, trace []Candidate) Decision {
	return Decision{Handled: true, Auth: s.Auth, Account: s.Account.Account, Pace: s.pace, Reason: reason, Forced: forced, Candidates: trace}
}

// evaluate: pace = 7-day % left / max(MinHorizon, reset − now), the rate that
// spends the meter by its reset (a dormant 7-day window has its full length
// ahead). With several paced windows the slowest binds. Short windows only
// gate eligibility. An account is known when every window is usable and at
// least one is paced.
func evaluate(cfg Config, now time.Time, a Account) score {
	s := score{Account: a, known: true, pace: math.Inf(1), short: math.Inf(1)}
	paced := false
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
			horizon := time.Duration(b.Seconds) * time.Second
			if !b.Dormant {
				horizon = b.Reset.Sub(now)
			}
			s.pace = math.Min(s.pace, remaining/math.Max(cfg.MinHorizon.Hours(), horizon.Hours()))
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
	return s
}
