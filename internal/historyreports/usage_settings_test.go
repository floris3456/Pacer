package historyreports

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

func TestAccountLinksAreRevisionedAndRefuseCycles(t *testing.T) {
	ctx := context.Background()
	s, _ := forwardLedger(t)
	known := func(a string) (string, bool) {
		switch a {
		case "old", "new", "newer":
			return "claude", true
		case "codex-1":
			return "codex", true
		}
		return "", false
	}
	at := time.UnixMilli(1790812800000)
	empty, err := s.AccountLinks(ctx)
	if err != nil || empty.Revision != 0 || len(empty.Links) != 0 {
		t.Fatal(empty, err)
	}
	one, err := s.SetAccountLink(ctx, 0, "new", "old", true, "re-login after expiry", at, known)
	if err != nil || one.Revision != 1 || len(one.Links) != 1 || one.Links[0].SameAs != "old" {
		t.Fatal(one, err)
	}
	if _, err = s.SetAccountLink(ctx, 0, "newer", "new", true, "", at, known); !errors.Is(err, ErrForwardConflict) {
		t.Fatal("stale revision accepted", err)
	}
	two, err := s.SetAccountLink(ctx, 1, "newer", "new", true, "", at, known)
	if err != nil || two.Revision != 2 || canonicalizer(two.Links)("newer") != "old" {
		t.Fatal(two, err)
	}
	for _, bad := range []struct{ account, sameAs string }{
		{"old", "newer"},   // cycle
		{"old", "old"},     // itself
		{"new", "codex-1"}, // another provider
		{"ghost", "old"},   // unknown
	} {
		if _, err = s.SetAccountLink(ctx, 2, bad.account, bad.sameAs, true, "", at, known); !errors.Is(err, ErrInput) {
			t.Fatal("accepted", bad, err)
		}
	}
	three, err := s.SetAccountLink(ctx, 2, "new", "", false, "", at, known)
	if err != nil || three.Revision != 3 || len(three.Links) != 1 || canonicalizer(three.Links)("new") != "new" {
		t.Fatal("unlink", three, err)
	}
}

const catalogFixture = `{
 "anthropic":{"models":{"claude-opus-5-5":{"id":"claude-opus-5-5","name":"Claude Opus 5.5","cost":{"input":4,"output":20,"cache_read":0.2,"cache_write":5},"limit":{"context":1000000,"output":128000},"modalities":{"input":["text","image","pdf"],"output":["text"]},"experimental":{"modes":{"fast":{"cost":{"input":8,"output":40,"cache_read":0.4,"cache_write":10}}}}},
  "claude-haiku-5-5":{"id":"claude-haiku-5-5","cost":{"input":0.1,"output":0.5,"cache_read":0.01,"cache_write":0.125,"tiers":[{"input":0.5,"output":2.5,"cache_read":0.05,"cache_write":0.625,"tier":{"type":"context","size":100000}}]}}}},
 "google":{"models":{"gemini-3.8-flash":{"id":"gemini-3.8-flash","cost":{"input":0.75,"output":3.75,"cache_read":0.075}}}},
 "opencode-go":{"models":{"deepseek-v4.1-flash":{"id":"deepseek-v4.1-flash","cost":{"input":0.15,"output":0.6,"cache_read":0.003}}}},
 "somebody-else":{"models":{"x":{"id":"x","cost":{"input":1,"output":1}}}}
}`

func TestModelsDevPricesMatchCPAMPsShape(t *testing.T) {
	c, err := ParseModelsDev([]byte(catalogFixture))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c["somebody-else"]; ok {
		t.Fatal("kept a provider Pacer does not serve")
	}
	// The same shape CPAMP stored for claude-opus-5-5 in the imported book.
	p, ok := c.Price("claude", "claude-opus-5-5", 0)
	want := `{"prompt":4,"completion":20,"cache":0.2,"cacheRead":0.2,"cacheCreation":5,"promptConfigured":true,"completionConfigured":true,"cacheReadConfigured":true,"cacheCreationConfigured":true,"source":"models.dev","sourceModelId":"anthropic/claude-opus-5-5","serviceTiers":[{"mode":"fast","serviceTier":"priority","prompt":8,"completion":40,"cache":0.4,"cacheRead":0.4,"cacheCreation":10,"promptConfigured":true,"completionConfigured":true,"cacheConfigured":true,"cacheReadConfigured":true,"cacheCreationConfigured":true}]}`
	if got, _ := json.Marshal(p); !ok || string(got) != want {
		t.Fatalf("%s", got)
	}
	for _, tc := range []struct{ provider, model, id string }{
		{"antigravity", "gemini-3.8-flash-high", "google/gemini-3.8-flash"},
		{"opencode-go", "opencode-go/deepseek-v4.1-flash", "opencode-go/deepseek-v4.1-flash"},
		{"claude", "claude-opus-5-5(high)", "anthropic/claude-opus-5-5"},
		{"claude", "claude-haiku-5-5", "anthropic/claude-haiku-5-5"},
	} {
		if id, _, ok := c.Match(tc.provider, tc.model); !ok || id != tc.id {
			t.Fatal(tc, id)
		}
	}
	if h, _ := c.Price("claude", "claude-haiku-5-5", 0); len(h.ContextTiers) != 1 || h.ContextTiers[0].ThresholdTokens != 100000 || h.ContextTiers[0].Prompt != 0.5 {
		t.Fatal("context tier", h.ContextTiers)
	}
	if _, _, ok := c.Match("claude", "claude-unknown-9"); ok {
		t.Fatal("matched an unknown model")
	}
}

func TestMergedBook(t *testing.T) {
	c, _ := ParseModelsDev([]byte(catalogFixture))
	frozen := cpamp.ComparisonPriceBook{ID: "book", Policy: "profile-api-value-v3", Prices: map[string]ForwardModelPrice{"claude-opus-5-5": {Prompt: 3, Completion: 15, PromptConfigured: true, CompletionConfigured: true}}, References: map[string]cpamp.ReferenceUnit{"claude": {Model: "claude-opus-5-5", InputPerMillion: 4}}}
	prices := ModelPrices{Catalog: c, Manual: map[string]ForwardModelPrice{}}
	same := MergedBook(frozen, prices, map[string]string{"claude-opus-5-5": "claude"})
	if same.ID != "book" || !reflect.DeepEqual(same.Prices, frozen.Prices) {
		t.Fatal("an imported price was replaced by models.dev", same.ID)
	}
	merged := MergedBook(frozen, prices, map[string]string{"claude-opus-5-5": "claude", "opencode-go/deepseek-v4.1-flash": "opencode-go", "claude-haiku-5-5": "claude"})
	if merged.ID == "book" || merged.Prices["claude-opus-5-5"].Prompt != 3 || merged.Prices["opencode-go/deepseek-v4.1-flash"].Prompt != 0.15 || merged.Prices["claude-haiku-5-5"].Completion != 0.5 {
		t.Fatal("models.dev did not fill missing prices", merged.Prices)
	}
	if ref := merged.References["opencode-go"]; ref.InputPerMillion != 0.15 || ref.Model != "opencode-go/deepseek-v4.1-flash" {
		t.Fatal("OpenCode Go has no reference unit", ref)
	}
	prices.Manual["claude-opus-5-5"] = ForwardModelPrice{Prompt: 5, Completion: 25, PromptConfigured: true, CompletionConfigured: true}
	if m := MergedBook(frozen, prices, nil); m.Prices["claude-opus-5-5"].Prompt != 5 {
		t.Fatal("a manual price must replace the imported one")
	}
	if _, src, _ := ModelPriceFor(frozen, prices, "claude", "claude-opus-5-5"); src != PriceManual {
		t.Fatal(src)
	}
	if frozen.Prices["claude-opus-5-5"].Prompt != 3 {
		t.Fatal("the imported book was changed in place")
	}
}

func TestProfileEditsKeepContentsExact(t *testing.T) {
	// A binding-based profile converted to memberships keeps exactly what it
	// contained: the first binding owns an account at any time.
	c := cpamp.ProfileConfig{Profiles: []cpamp.Profile{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}}, Bindings: []cpamp.Binding{
		{Profile: "a", Account: "x", Start: 100, End: 500},
		{Profile: "b", Account: "x", Start: 0},
	}}
	got := cpamp.ExactMemberships(c, c.Profiles[1])
	want := []cpamp.ProfileMembership{{Account: "x", Start: 0, End: 100}, {Account: "x", Start: 500}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	p, err := ApplyProfileEdit(c, ProfileEdit{Op: "add", Profile: "b", Account: "y"}, []string{"y", "y-relogin"}, "")
	if err != nil || len(p.Memberships) != 4 || p.Memberships != nil && p.Selection {
		t.Fatal(p, err)
	}
	if _, err = ApplyProfileEdit(c, ProfileEdit{Op: "add", Profile: "b"}, []string{"x"}, ""); !errors.Is(err, ErrInput) {
		t.Fatal("added an account counted already", err)
	}
	ended, err := ApplyProfileEdit(c, ProfileEdit{Op: "end", Profile: "b", AtMS: 700}, []string{"x"}, "")
	if err != nil || ended.Memberships[1].End != 700 {
		t.Fatal(ended, err)
	}
	removed, err := ApplyProfileEdit(c, ProfileEdit{Op: "remove", Profile: "b"}, []string{"x"}, "")
	if err != nil || len(removed.Memberships) != 0 || removed.Memberships == nil {
		t.Fatal(removed, err)
	}
	created, err := ApplyProfileEdit(c, ProfileEdit{Op: "create", Name: "Claude Max 5x", Provider: "claude"}, nil, "new-id")
	if err != nil || created.ID != "new-id" || created.Memberships == nil {
		t.Fatal(created, err)
	}
	// The effective configuration: the forward revision replaces the imported
	// profile and its bindings stop applying.
	eff := EffectiveProfiles(c, ForwardProfileConfig{Revision: 3, Profiles: []cpamp.Profile{ended, created}})
	if len(eff.Profiles) != 3 || eff.Profiles[1].Memberships == nil || len(eff.Bindings) != 1 || eff.Bindings[0].Profile != "a" {
		t.Fatal(eff)
	}
	if !cpamp.CardContains(eff, 1, "x", 50) || cpamp.CardContains(eff, 1, "x", 800) {
		t.Fatal("edited membership")
	}
}

// TestRealModelsDevMatchesImportedBook (opt-in): the prices the import took
// from models.dev are what Pacer's sync derives from the same catalog.
// PACER_MODELSDEV=api.json PACER_REAL_FACTS_JSON=facts.json.
func TestRealModelsDevMatchesImportedBook(t *testing.T) {
	catalog, facts := os.Getenv("PACER_MODELSDEV"), os.Getenv("PACER_REAL_FACTS_JSON")
	if catalog == "" || facts == "" {
		t.Skip("PACER_MODELSDEV and PACER_REAL_FACTS_JSON required")
	}
	body, err := os.ReadFile(catalog)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseModelsDev(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(facts)
	if err != nil {
		t.Fatal(err)
	}
	var q cpamp.QuotaFacts
	if err = json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	same, changed, missing := 0, 0, 0
	for name, want := range q.PriceBook.Prices {
		if want.Source != "models.dev" {
			continue
		}
		got, ok := c.Price("", name, 0)
		if !ok {
			missing++
			t.Logf("not matched: %s (imported %s)", name, want.SourceModelID)
			continue
		}
		if got.SourceModelID != want.SourceModelID {
			t.Errorf("%s matched %s, imported %s", name, got.SourceModelID, want.SourceModelID)
			continue
		}
		want.UpdatedAtMS, want.SyncedAtMS, want.RawJSON = 0, nil, ""
		x, _ := json.Marshal(want)
		y, _ := json.Marshal(got)
		if string(x) != string(y) {
			changed++
			t.Logf("%s price differs (the catalog changed since the import?):\n %s\n %s", name, x, y)
			continue
		}
		same++
	}
	t.Logf("%d identical, %d changed, %d unmatched", same, changed, missing)
	if same == 0 || missing > 0 {
		t.Fatal("models.dev matching does not reproduce the imported book")
	}
}
