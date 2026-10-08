// Package routing places chats using source-backed budgets. It does no I/O and
// never treats an elapsed reset as replenishment.
package routing

import (
	"math"
	"sort"
	"time"
)

type Budget struct {
	Seconds   int64
	Remaining float64
	Reset     time.Time
	Known     bool
}

// Account contains only credentials CPA currently offers; cooling and disabled
// credentials must never be added by the caller. Aliases share Account identity.
type Account struct {
	Auth, Account    string
	Budgets          []Budget
	Size, ShortValue float64 // dollars per percent; zero means unmeasured
	WarmBurn         float64 // dollars/hour from compatible, observed warm stretches
	RunwayUnknown    bool    // measured capacity exists, but outstanding warm load is unbounded
	KeyWarm          bool
}

type Chat struct {
	Account        string
	ProtectedUntil time.Time
	UnknownWarmth  bool
	InFlight       bool
	ExpectedValue  float64
}

type Config struct {
	MoveMargin            float64
	MinHorizon            time.Duration
	ShortFloor, LongFloor float64
	NewLong, NewShort     float64
}

func Default() Config {
	return Config{MoveMargin: .25, MinHorizon: time.Hour, ShortFloor: 2, LongFloor: 1, NewLong: 3, NewShort: 15}
}

type Decision struct {
	Handled bool    `json:"handled"`
	Auth    string  `json:"auth,omitempty"`
	Account string  `json:"account,omitempty"`
	Reason  string  `json:"reason"`
	Forced  bool    `json:"forced"`
	Pace    float64 `json:"pace,omitempty"`
}

type score struct {
	Account
	pace, room               float64
	known, hard, fit, runway bool
}

func Pick(cfg Config, now time.Time, chat Chat, accounts []Account) Decision {
	scores := make([]score, 0, len(accounts))
	var current *score
	for _, account := range accounts {
		s := evaluate(cfg, now, chat.ExpectedValue, account)
		scores = append(scores, s)
		if account.Account == chat.Account && (current == nil || s.Auth < current.Auth) {
			copy := s
			current = &copy
		}
	}
	forced := chat.Account != "" && (current == nil || current.hard)
	// Missing telemetry never evicts a healthy warm/protected binding. Unknown
	// legacy TTL cannot authorize a cold move at five minutes.
	if current != nil && !current.hard && (chat.InFlight || chat.UnknownWarmth || now.Before(chat.ProtectedUntil)) {
		return chosen(*current, "protected", false)
	}
	eligible := make([]score, 0, len(scores))
	runwayBlocked := false
	for _, s := range scores {
		runwayBlocked = runwayBlocked || (s.known && !s.hard && !s.runway)
		if s.known && !s.hard && (forced || s.runway) {
			eligible = append(eligible, s)
		}
	}
	if len(eligible) == 0 {
		if runwayBlocked {
			return Decision{Reason: "placement_runway_unknown_or_exhausting", Forced: forced}
		}
		return Decision{Reason: "no_known_budget", Forced: forced}
	}
	// Compare tie candidates to a fixed maximum (not pairwise tolerance, which
	// violates sort transitivity). A tie prefers shared key warmth, then room.
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].pace != eligible[j].pace {
			return eligible[i].pace > eligible[j].pace
		}
		return eligible[i].Auth < eligible[j].Auth
	})
	fitting := make([]score, 0, len(eligible))
	for _, s := range eligible {
		if s.fit {
			fitting = append(fitting, s)
		}
	}
	best := eligible[0]
	if len(fitting) == 0 {
		for _, s := range eligible {
			if s.room > best.room || (s.room == best.room && s.Auth < best.Auth) {
				best = s
			}
		}
	} else {
		best = fitting[0]
		top := best.pace
		for _, s := range fitting[1:] {
			if s.pace < top*.95 {
				break
			}
			if (s.KeyWarm && !best.KeyWarm) || (s.KeyWarm == best.KeyWarm && (s.room > best.room || (s.room == best.room && s.Auth < best.Auth))) {
				best = s
			}
		}
	}
	if forced {
		return chosen(best, "forced_move", true)
	}
	if current == nil {
		return chosen(best, "new_chat", false)
	}
	if best.Account.Account == current.Account.Account {
		return chosen(*current, "current_best", false)
	}
	// The emergency most-room fallback is not permission for a discretionary
	// cold move onto a runway-blocked or undersized account.
	if !best.fit || !current.known || best.pace < current.pace*(1+cfg.MoveMargin) {
		return chosen(*current, "cold_margin", false)
	}
	return chosen(best, "cold_move", false)
}

func chosen(s score, reason string, forced bool) Decision {
	return Decision{Handled: true, Auth: s.Auth, Account: s.Account.Account, Pace: s.pace, Reason: reason, Forced: forced}
}

func evaluate(cfg Config, now time.Time, expected float64, a Account) score {
	s := score{Account: a, known: len(a.Budgets) > 0, fit: true, runway: !a.RunwayUnknown, room: math.Inf(1), pace: math.Inf(1)}
	shortValue := a.ShortValue
	// Invalid measured load/capacity is not a verified zero load. Keep existing
	// warm chats, but exclude this account from discretionary placement/fallback.
	if shortValue < 0 || math.IsNaN(shortValue) || math.IsInf(shortValue, 0) {
		shortValue, s.runway = 0, false
	}
	if a.WarmBurn < 0 || math.IsNaN(a.WarmBurn) || math.IsInf(a.WarmBurn, 0) {
		s.runway = false
	}
	longest := int64(0)
	for _, b := range a.Budgets {
		if b.Seconds > longest {
			longest = b.Seconds
		}
	}
	size := a.Size
	if size <= 0 || math.IsNaN(size) || math.IsInf(size, 0) {
		size = 1
	}
	for _, b := range a.Budgets {
		valid := b.Known && b.Seconds > 0 && !math.IsNaN(b.Remaining) && !math.IsInf(b.Remaining, 0) && b.Remaining >= 0 && b.Remaining <= 100 && b.Reset.After(now)
		if !valid {
			s.known = false
			continue
		}
		floor := cfg.ShortFloor
		if b.Seconds == longest {
			floor = cfg.LongFloor
		}
		if b.Remaining <= floor {
			s.hard = true
		}
		if b.Seconds == longest {
			horizon := b.Reset.Sub(now)
			if horizon < cfg.MinHorizon {
				horizon = cfg.MinHorizon
			}
			if horizon <= 0 {
				s.known = false
				continue
			}
			s.pace = math.Min(s.pace, size*b.Remaining/horizon.Hours())
			if b.Remaining < cfg.NewLong {
				s.fit = false
			}
			continue
		}
		room := b.Remaining
		if shortValue > 0 {
			room *= shortValue
		}
		s.room = math.Min(s.room, room)
		if shortValue > 0 && expected > 0 {
			if room < expected {
				s.fit = false
			}
		} else if b.Remaining < cfg.NewShort {
			s.fit = false
		}
		if shortValue > 0 && a.WarmBurn > 0 && a.WarmBurn*b.Reset.Sub(now).Hours() > room {
			s.fit = false
			s.runway = false
		}
	}
	if math.IsInf(s.room, 1) {
		// A provider may genuinely have only one budget. Use its remaining
		// capacity for fallback; never manufacture a five-hour window.
		s.room = 0
		for _, b := range a.Budgets {
			if b.Known {
				s.room = b.Remaining
			}
		}
	}
	if math.IsInf(s.pace, 1) {
		s.known = false
		s.pace = 0
	}
	return s
}
