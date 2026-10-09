package historyreports

// models.dev price enrichment (https://models.dev/api.json). Pacer keeps the
// part of the catalog for providers it can serve and matches a model name the
// way CPAMP's sync did (same provider, then effort/thinking suffixes removed).
// Prices are USD per million tokens, converted to CPAMP's price shape: context
// tiers and the "fast" mode (service tier "priority") included.

import (
	"encoding/json"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/model"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/usageidentity"
)

// ModelsDevURL is the catalog Pacer syncs from.
const ModelsDevURL = "https://models.dev/api.json"

// modelsDevProviders are the catalog providers Pacer keeps, in search order.
var modelsDevProviders = []string{"anthropic", "openai", "google", "opencode-go", "opencode", "xai", "meta", "deepseek", "moonshotai", "zai", "minimax", "alibaba", "mistral"}

type ModelsDevCost struct {
	Input      *float64        `json:"input,omitempty"`
	Output     *float64        `json:"output,omitempty"`
	CacheRead  *float64        `json:"cache_read,omitempty"`
	CacheWrite *float64        `json:"cache_write,omitempty"`
	Tiers      []ModelsDevTier `json:"tiers,omitempty"`
}

type ModelsDevTier struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
	Tier       struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

// ModelsDevModel is the part of a models.dev model entry Pacer keeps.
type ModelsDevModel struct {
	Name  string         `json:"name,omitempty"`
	Cost  *ModelsDevCost `json:"cost,omitempty"`
	Fast  *ModelsDevCost `json:"fast,omitempty"` // experimental.modes.fast.cost
	Limit *struct {
		Context int64 `json:"context,omitempty"`
		Input   int64 `json:"input,omitempty"`
		Output  int64 `json:"output,omitempty"`
	} `json:"limit,omitempty"`
	Modalities *struct {
		Input  []string `json:"input,omitempty"`
		Output []string `json:"output,omitempty"`
	} `json:"modalities,omitempty"`
	Reasoning   bool   `json:"reasoning,omitempty"`
	ToolCall    bool   `json:"tool_call,omitempty"`
	OpenWeights bool   `json:"open_weights,omitempty"`
	ReleaseDate string `json:"release_date,omitempty"`
	Knowledge   string `json:"knowledge,omitempty"`
}

// ModelsDevCatalog maps provider -> model ID -> model.
type ModelsDevCatalog map[string]map[string]ModelsDevModel

// ParseModelsDev keeps the providers Pacer serves from a models.dev catalog.
func ParseModelsDev(body []byte) (ModelsDevCatalog, error) {
	var raw map[string]struct {
		Models map[string]struct {
			ModelsDevModel
			Experimental *struct {
				Modes map[string]struct {
					Cost *ModelsDevCost `json:"cost"`
				} `json:"modes"`
			} `json:"experimental"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := ModelsDevCatalog{}
	for _, p := range modelsDevProviders {
		entry, ok := raw[p]
		if !ok {
			continue
		}
		models := map[string]ModelsDevModel{}
		for id, m := range entry.Models {
			if len(id) > 200 {
				continue
			}
			v := m.ModelsDevModel
			if m.Experimental != nil {
				if fast, ok := m.Experimental.Modes["fast"]; ok {
					v.Fast = fast.Cost
				}
			}
			models[strings.ToLower(id)] = v
		}
		out[p] = models
	}
	if len(out) == 0 {
		return nil, ErrSource
	}
	return out, nil
}

var effortSuffixes = []string{"-extra-low", "-xhigh", "-high", "-medium", "-low", "-minimal", "-max"}

// nameVariants are the names a model may have in the catalog, in order.
func nameVariants(name string) []string {
	out := []string{name}
	add := func(v string) {
		for _, x := range out {
			if x == v {
				return
			}
		}
		out = append(out, v)
	}
	base := strings.TrimSuffix(name, "-thinking")
	add(base)
	for _, s := range effortSuffixes {
		if strings.HasSuffix(base, s) {
			add(strings.TrimSuffix(base, s))
		}
	}
	if strings.HasSuffix(base, "-free") {
		add(strings.TrimSuffix(base, "-free"))
	}
	return out
}

// providerOrder lists catalog providers to search for a CPA provider and model.
func providerOrder(provider, name string) []string {
	order := []string{}
	add := func(ps ...string) {
		for _, p := range ps {
			if !contains(order, p) {
				order = append(order, p)
			}
		}
	}
	switch provider {
	case "claude":
		add("anthropic")
	case "codex":
		add("openai")
	case "opencode-go":
		add("opencode-go", "opencode")
	case "xai", "grok":
		add("xai")
	case "meta":
		add("meta")
	}
	for prefix, p := range map[string]string{"claude": "anthropic", "gpt": "openai", "o1": "openai", "o3": "openai", "o4": "openai", "codex": "openai", "gemini": "google", "grok": "xai", "muse": "meta", "deepseek": "deepseek", "glm": "zai", "kimi": "moonshotai", "qwen": "alibaba", "minimax": "minimax", "mistral": "mistral", "devstral": "mistral"} {
		if strings.HasPrefix(name, prefix) {
			add(p)
		}
	}
	add(modelsDevProviders...)
	return order
}

// Match finds a model in the catalog: catalog ID and entry.
func (c ModelsDevCatalog) Match(provider, model string) (string, ModelsDevModel, bool) {
	name := strings.ToLower(usageidentity.AnalyticsModel(strings.TrimSpace(model)))
	if name == "" || len(c) == 0 {
		return "", ModelsDevModel{}, false
	}
	if prefix, rest, ok := strings.Cut(name, "/"); ok {
		if prefix == "zen" {
			prefix = "opencode"
		}
		if m, ok := c[prefix][rest]; ok && m.Cost != nil {
			return prefix + "/" + rest, m, true
		}
		if provider == "" {
			provider = prefix
		}
		name = rest
	}
	for _, v := range nameVariants(name) {
		for _, p := range providerOrder(provider, v) {
			if m, ok := c[p][v]; ok && m.Cost != nil {
				return p + "/" + v, m, true
			}
		}
	}
	return "", ModelsDevModel{}, false
}

func priceField(dst *float64, configured *bool, v *float64) {
	if v != nil {
		*dst, *configured = *v, true
	}
}

// Price converts a matched model into CPAMP's price shape.
func (c ModelsDevCatalog) Price(provider, modelName string, syncedAt int64) (ForwardModelPrice, bool) {
	id, m, ok := c.Match(provider, modelName)
	if !ok || m.Cost == nil || m.Cost.Input == nil || m.Cost.Output == nil {
		return ForwardModelPrice{}, false
	}
	p := model.ModelPrice{Source: PriceModelsDev, SourceModelID: id, UpdatedAtMS: syncedAt}
	if syncedAt > 0 {
		at := syncedAt
		p.SyncedAtMS = &at
	}
	priceField(&p.Prompt, &p.PromptConfigured, m.Cost.Input)
	priceField(&p.Completion, &p.CompletionConfigured, m.Cost.Output)
	priceField(&p.CacheRead, &p.CacheReadConfigured, m.Cost.CacheRead)
	p.Cache = p.CacheRead
	priceField(&p.CacheCreation, &p.CacheCreationConfigured, m.Cost.CacheWrite)
	for _, t := range m.Cost.Tiers {
		if t.Tier.Type != "context" || t.Tier.Size <= 0 {
			continue
		}
		tier := model.ModelPriceContextTier{ThresholdTokens: t.Tier.Size}
		priceField(&tier.Prompt, &tier.PromptConfigured, t.Input)
		priceField(&tier.Completion, &tier.CompletionConfigured, t.Output)
		priceField(&tier.CacheRead, &tier.CacheReadConfigured, t.CacheRead)
		priceField(&tier.Cache, &tier.CacheConfigured, t.CacheRead)
		priceField(&tier.CacheCreation, &tier.CacheCreationConfigured, t.CacheWrite)
		p.ContextTiers = append(p.ContextTiers, tier)
	}
	if f := m.Fast; f != nil {
		tier := model.ModelPriceServiceTier{Mode: "fast", ServiceTier: "priority"}
		priceField(&tier.Prompt, &tier.PromptConfigured, f.Input)
		priceField(&tier.Completion, &tier.CompletionConfigured, f.Output)
		priceField(&tier.CacheRead, &tier.CacheReadConfigured, f.CacheRead)
		priceField(&tier.Cache, &tier.CacheConfigured, f.CacheRead)
		priceField(&tier.CacheCreation, &tier.CacheCreationConfigured, f.CacheWrite)
		p.ServiceTiers = append(p.ServiceTiers, tier)
	}
	return p, true
}
