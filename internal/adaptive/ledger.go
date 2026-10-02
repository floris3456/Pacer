package adaptive

import (
	"math"
)

type Ticket struct {
	ID        string             `json:"id"`
	WireID    string             `json:"wire_id"`
	Account   string             `json:"account"`
	Session   string             `json:"session"`
	Request   Request            `json:"request"`
	Started   int64              `json:"started"`
	Completed int64              `json:"completed"`
	Value     float64            `json:"value"`
	Estimated float64            `json:"estimated"`
	Actual    bool               `json:"actual"`
	Orphan    bool               `json:"orphan"`
	Executing bool               `json:"executing"`
	Succeeded bool               `json:"succeeded"`
	Uncertain bool               `json:"uncertain"`
	Usage     map[string]float64 `json:"usage,omitempty"`
	UsageAt   int64              `json:"usage_at"`
	Cycles    map[string]int64   `json:"cycles"`
	Fallback  map[string]float64 `json:"fallback"`
}

// Fanout can deliver several execution UUIDs for one host lifecycle. Keep the
// original envelope until terminal success and a quiet telemetry interval.
func (t *Ticket) Charge(now, lag int64) float64 {
	if !t.Actual || !t.Succeeded || t.Uncertain || t.Completed == 0 || now < t.Completed+lag || now < t.UsageAt+lag {
		return math.Max(t.Value, t.Estimated)
	}
	return t.Value
}

type State struct {
	Models  map[string]Model   `json:"models"`
	Tickets map[string]*Ticket `json:"tickets"`
}

func NewState() *State { return &State{Models: map[string]Model{}, Tickets: map[string]*Ticket{}} }

type Window struct {
	Key       string  `json:"key"`
	Scope     string  `json:"scope"`
	Seconds   int64   `json:"seconds"`
	Remaining float64 `json:"remaining"`
	Reset     int64   `json:"reset"`
	Cycle     int64   `json:"cycle"`
	Covered   int64   `json:"covered"`
}
type Session struct {
	ID          string
	Request     Request
	Auto        bool
	Summarizing bool
}
type Projection struct {
	Account      string  `json:"account"`
	AuthIndex    string  `json:"auth_index"`
	Window       string  `json:"window"`
	Seconds      int64   `json:"seconds"`
	Scope        string  `json:"scope"`
	Reset        int64   `json:"reset"`
	Remaining    float64 `json:"remaining"`
	Headroom     float64 `json:"headroom_points"`
	Compaction   float64 `json:"compaction_points"`
	Unfinished   float64 `json:"unfinished_points"`
	Unobserved   float64 `json:"unobserved_points"`
	RequestsLow  int     `json:"requests_low"`
	RequestsHigh int     `json:"requests_high"`
	Active       int     `json:"active_chats"`
	Mode         string  `json:"mode"`
	Quality      string  `json:"quality"`
	Generated    int64   `json:"generated"`
	Version      string  `json:"version"`
	Orphan       bool    `json:"orphan"`
}

func fallback(w Window) float64 {
	if w.Seconds == 18000 {
		return 10
	}
	return 5
}
func (s *State) Project(account string, w Window, sessions []Session, cfg Config, now int64) Projection {
	m := s.Models[account]
	rate, known := m.Rate(w.Scope, w.Seconds, w.Reset, now)
	p := Projection{Account: account, Window: w.Key, Seconds: w.Seconds, Scope: w.Scope, Reset: w.Reset, Remaining: w.Remaining, Generated: now, Mode: cfg.Mode, Quality: "calibrated", Version: m.Version}
	if !known {
		p.Quality = "fallback"
	}
	for _, t := range s.Tickets {
		if t.Account != account || t.Cycles[w.Key] != w.Cycle {
			continue
		}
		if t.Orphan {
			p.Orphan = true
		}
		if t.Completed > 0 && t.Completed <= w.Covered {
			continue
		}
		points := t.Fallback[w.Key]
		if value := t.Charge(now, cfg.LagSeconds); known && value > 0 {
			points = value / rate.Low
		}
		if points == 0 && !t.Actual {
			points = fallback(w)
		}
		if t.Completed == 0 {
			p.Unfinished += points
		} else {
			p.Unobserved += points
		}
	}
	var expected, high float64
	for _, session := range sessions {
		primary := Applies(session.Request.Model, w.Scope)
		summary := session.Auto && m.SummaryApplies(w.Scope) && !session.Summarizing
		if !primary && !summary {
			continue
		}
		if primary {
			p.Active++
		}
		estimate := m.Estimate(session.Request, session.Auto, now)
		if known && estimate.Known {
			if primary {
				expected += estimate.Expected / rate.Mean
				high += estimate.High / rate.Low
			}
			if summary {
				p.Compaction += estimate.Summary / rate.Low
			}
		} else {
			p.Quality = "fallback"
			if primary {
				expected += fallback(w)
				high += fallback(w)
			}
			if summary {
				p.Compaction += fallback(w)
			}
		}
	}
	p.Headroom = w.Remaining - cfg.Margin - p.Unfinished - p.Unobserved - p.Compaction
	if p.Active > 0 && high > 0 && expected > 0 {
		p.RequestsLow = int(math.Max(0, math.Floor(p.Headroom/(high/float64(p.Active)))))
		p.RequestsHigh = int(math.Max(0, math.Floor(p.Headroom/(expected/float64(p.Active)))))
	}
	if p.Orphan {
		p.Headroom = math.Min(0, p.Headroom)
		p.RequestsLow = 0
		p.RequestsHigh = 0
		p.Quality = "unresolved_request"
	}
	return p
}

func (s *State) Cost(account string, w Window, r Request, now int64) (float64, float64, bool) {
	m := s.Models[account]
	rate, ok := m.Rate(w.Scope, w.Seconds, w.Reset, now)
	e := m.Estimate(r, false, now)
	if !ok || !e.Known {
		return fallback(w), fallback(w), false
	}
	return e.High / rate.Low, e.Expected / rate.Mean, true
}

// PendingSavings is forecast-only. Admission always keeps the full reservation.
// Waiting for a running request to settle is preferable to initiating an early
// irreversible handoff solely because its cold-cache envelope is still held.
func (s *State) PendingSavings(account string, w Window, now int64) float64 {
	m := s.Models[account]
	rate, known := m.Rate(w.Scope, w.Seconds, w.Reset, now)
	if !known {
		return 0
	}
	var savings float64
	for _, t := range s.Tickets {
		if t.Account != account || t.Cycles[w.Key] != w.Cycle || (t.Completed != 0 && t.Completed <= w.Covered) || t.Orphan || t.Uncertain {
			continue
		}
		if t.Actual {
			if t.Completed == 0 || t.Succeeded {
				savings += math.Max(0, t.Estimated-t.Value) / rate.Low
			}
			continue
		}
		if t.Completed != 0 {
			continue
		}
		estimate := m.Estimate(t.Request, false, now)
		if estimate.Known {
			savings += math.Max(0, t.Value-estimate.Expected) / rate.Low
		}
	}
	return savings
}

// Trim only fully covered terminal records. Unfinished work never expires with
// a heartbeat. On restart it becomes an unresolved reservation until its cycles reset.
func (s *State) Trim(account string, windows []Window) {
	for id, t := range s.Tickets {
		if t.Account != account {
			continue
		}
		covered := true
		for _, w := range windows {
			if cycle, ok := t.Cycles[w.Key]; ok && cycle == w.Cycle && (t.Completed == 0 || t.Completed > w.Covered || t.Orphan) {
				covered = false
			}
		}
		if covered {
			delete(s.Tickets, id)
		}
	}
}
