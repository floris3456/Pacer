// Package adaptive holds the versioned CPAMP contract and quota reservation math.
// Values are API-equivalent input tokens, using CPAMP's frozen price rule, not dollars.
package adaptive

import (
	"encoding/json"
	"math"
	"strings"
)

const Protocol = 1

type Config struct {
	Mode         string  `yaml:"mode" json:"mode"`
	URL          string  `yaml:"cpamp_url" json:"-"`
	TokenEnv     string  `yaml:"token_env" json:"-"`
	TokenFile    string  `yaml:"token_file" json:"-"`
	LeaseSeconds int64   `yaml:"lease_seconds" json:"lease_seconds"`
	LagSeconds   int64   `yaml:"observation_lag_seconds" json:"observation_lag_seconds"`
	Margin       float64 `yaml:"margin_points" json:"margin_points"`
	Lead         float64 `yaml:"warning_requests" json:"warning_requests"`
}

func (c Config) Enabled() bool { return c.Mode == "shadow" || c.Mode == "enforce" }
func (c Config) Enforce() bool { return c.Mode == "enforce" }
func (c Config) Shadow() bool  { return c.Mode == "shadow" }
func (c Config) Valid() bool {
	return c.LeaseSeconds >= 30 && c.LagSeconds >= 1 && finite(c.Margin) && c.Margin >= .1 && c.Margin <= 10 && finite(c.Lead) && c.Lead >= 1
}
func (c *Config) Defaults() {
	if c.Mode == "" {
		c.Mode = "off"
	}
	if c.LeaseSeconds == 0 {
		c.LeaseSeconds = 120
	}
	if c.LagSeconds == 0 {
		c.LagSeconds = 5
	}
	if c.Margin == 0 {
		c.Margin = 1
	}
	if c.Lead == 0 {
		c.Lead = 4
	}
}

type Weights struct {
	Input  float64 `json:"input"`
	Read   float64 `json:"read"`
	Write  float64 `json:"write"`
	Output float64 `json:"output"`
}

func (p Weights) Valid() bool {
	return positive(p.Input) && positive(p.Output) && finite(p.Read) && p.Read >= 0 && finite(p.Write) && p.Write >= 0
}
func positive(x float64) bool { return finite(x) && x > 0 }
func finite(x float64) bool   { return !math.IsNaN(x) && !math.IsInf(x, 0) }

type Profile struct {
	Model         string  `json:"model"`
	Tier          string  `json:"tier"`
	Samples       int     `json:"samples"`
	Weights       Weights `json:"weights"`
	InputMean     float64 `json:"input_mean"`
	OutputMean    float64 `json:"output_mean"`
	OutputHigh    float64 `json:"output_high"`
	ReadFraction  float64 `json:"read_fraction"`
	WriteFraction float64 `json:"write_fraction"`
}
type Rate struct {
	Scope    string  `json:"scope"`
	Seconds  int64   `json:"seconds"`
	Reset    int64   `json:"reset"`
	Observed int64   `json:"observed"`
	Mean     float64 `json:"mean"`
	Low      float64 `json:"low"`
	Samples  int64   `json:"samples"`
}
type Model struct {
	Account             string    `json:"account"`
	AuthIndices         []string  `json:"auth_indices"`
	Version             string    `json:"version"`
	Rule                string    `json:"rule"`
	Generated           int64     `json:"generated"`
	Expires             int64     `json:"expires"`
	Through             int64     `json:"through_event_id"`
	ObservationsThrough int64     `json:"through_observation_row"`
	Rates               []Rate    `json:"rates"`
	Profiles            []Profile `json:"profiles"`
}
type Exchange struct {
	Protocol  int     `json:"protocol"`
	Generated int64   `json:"generated"`
	Models    []Model `json:"models"`
}

func (m Model) Valid(now int64) bool {
	if m.Account == "" || m.Version == "" || m.Rule != "api-equivalent-v1" || m.Generated > now+5 || m.Expires <= now || m.Expires > m.Generated+600 || len(m.Profiles) > 128 || len(m.Rates) > 16 {
		return false
	}
	for _, p := range m.Profiles {
		if p.Model == "" || !p.Weights.Valid() || p.Samples < 8 || !positive(p.InputMean) || !positive(p.OutputHigh) || !finite(p.OutputMean) || p.OutputMean < 0 || p.OutputMean > p.OutputHigh || !finite(p.ReadFraction) || !finite(p.WriteFraction) || p.ReadFraction < 0 || p.WriteFraction < 0 || p.ReadFraction+p.WriteFraction > 1.000001 {
			return false
		}
	}
	for _, r := range m.Rates {
		if !positive(r.Low) || !positive(r.Mean) || r.Low > r.Mean || r.Samples < 8 || r.Observed > now+5 || (r.Seconds != 18000 && r.Seconds != 604800) {
			return false
		}
	}
	return true
}
func (m Model) Rate(scope string, seconds, reset, now int64) (Rate, bool) {
	if !m.Valid(now) {
		return Rate{}, false
	}
	for _, r := range m.Rates {
		// Price conversion may survive a confirmed reset, but not an expired model.
		if r.Scope == scope && r.Seconds == seconds && r.Reset <= reset {
			return r, true
		}
	}
	return Rate{}, false
}
func (m Model) Profile(model, tier string) (Profile, bool) {
	for _, p := range m.Profiles {
		if p.Model == model && p.Tier == tier {
			return p, true
		}
	}
	// Providers often report the base tier as "default" although the request
	// omitted it. Other named tiers and model aliases require their own evidence.
	if tier == "" || tier == "default" {
		for _, p := range m.Profiles {
			if p.Model == model && (p.Tier == "" || p.Tier == "default") {
				return p, true
			}
		}
	}
	return Profile{}, false
}

func Applies(model, scope string) bool {
	return scope == "all" || scope == "oauth" || strings.Contains(strings.ToLower(model), strings.ToLower(scope))
}

func (m Model) SummaryApplies(scope string) bool {
	for _, p := range m.Profiles {
		if Applies(p.Model, scope) {
			return true
		}
	}
	return scope == "all" || scope == "oauth"
}

type Request struct {
	TraceID   string  `json:"trace_id,omitempty"`
	ID        string  `json:"id"`
	Model     string  `json:"model"`
	Tier      string  `json:"tier"`
	Input     float64 `json:"input"`
	ByteInput bool    `json:"byte_input,omitempty"`
	MaxOutput float64 `json:"max_output"`
	Opaque    bool    `json:"opaque"`
	Kind      string  `json:"kind"`
}

// Describe uses a text byte upper envelope, including tool schemas. It never
// retains prompts. Opaque continuations/media need the conservative fallback.
func Describe(id, model string, body []byte) Request {
	r := Request{ID: id, Model: model, Input: float64(len(body)), ByteInput: true, Kind: "primary"}
	var v map[string]json.RawMessage
	if json.Unmarshal(body, &v) != nil {
		r.Opaque = true
		return r
	}
	_ = json.Unmarshal(v["service_tier"], &r.Tier)
	for _, k := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
		var n float64
		if json.Unmarshal(v[k], &n) == nil && positive(n) {
			r.MaxOutput = n
		}
	}
	s := string(body)
	for _, key := range []string{"previous_response_id", "image_url", "input_audio", "file_id", "image", "compaction"} {
		if strings.Contains(s, `"`+key+`"`) {
			r.Opaque = true
		}
	}
	if r.Input <= 0 {
		r.Opaque = true
	}
	return r
}

type Estimate struct {
	Expected float64 `json:"expected"`
	High     float64 `json:"high"`
	Summary  float64 `json:"summary"`
	Known    bool    `json:"known"`
}

func (m Model) Estimate(r Request, auto bool, now int64) Estimate {
	p, ok := m.Profile(r.Model, r.Tier)
	if !ok || !m.Valid(now) || r.Opaque || !positive(r.Input) {
		return Estimate{}
	}
	output := p.OutputHigh * 1.5
	if r.Kind == "compaction" {
		output = math.Max(2048, output)
	}
	if r.MaxOutput > 0 {
		output = math.Min(output, r.MaxOutput)
	}
	cold := math.Max(p.Weights.Input, math.Max(p.Weights.Read, p.Weights.Write))
	mean := p.Weights.Input*(1-p.ReadFraction-p.WriteFraction) + p.Weights.Read*p.ReadFraction + p.Weights.Write*p.WriteFraction
	e := Estimate{Known: true, Expected: r.Input*mean + math.Min(p.OutputMean, output)*p.Weights.Output, High: r.Input*cold + output*p.Weights.Output}
	if auto {
		// The current OCP protocol does not promise which model will summarize.
		// Reserve the most expensive supported cold-cache summary, and include
		// growth from this request's output and a bounded tool-result allowance.
		growth := output + 4096
		if r.ByteInput {
			// A byte-derived input envelope must also budget serialized output,
			// notice text and the summarizer's replacement system instructions.
			// Installed OCP fixtures showed up to 37% request-body growth before
			// the first summary. Actual summary admission still rechecks its body.
			growth = math.Max(output*4+4096, r.Input*.4)
		}
		for _, summary := range m.Profiles {
			input := math.Max(summary.Weights.Input, math.Max(summary.Weights.Read, summary.Weights.Write))
			cost := (r.Input+growth)*input + math.Max(2048, summary.OutputHigh*1.5)*summary.Weights.Output
			e.Summary = math.Max(e.Summary, cost)
		}
	}
	return e
}

// ActualInput is normalized total input; output already includes reasoning.
func (m Model) Actual(model, tier string, input, read, write, output float64) (float64, bool) {
	p, ok := m.Profile(model, tier)
	if !ok || input < read+write || input < 0 || read < 0 || write < 0 || output < 0 {
		return 0, false
	}
	v := (input-read-write)*p.Weights.Input + read*p.Weights.Read + write*p.Weights.Write + output*p.Weights.Output
	return v, finite(v)
}
