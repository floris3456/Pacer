package observation

import "testing"

func TestAntigravityGroupsServeTheirModelsIncludingAliases(t *testing.T) {
	for model, want := range map[string]string{
		"gemini-3.8-flash-high":    "gemini",
		"gemini-3.1-flash-lite":    "gemini",
		"claude-sonnet-5-5-high":   "claude_gpt",
		"gpt-oss-120b-medium":      "claude_gpt",
		"gemini-opus-4-6-thinking": "claude_gpt", // alias of claude-opus-4-6-thinking
		"gemini-oss-120b-medium":   "claude_gpt", // alias of gpt-oss-120b-medium
		"deepseek-v4.1-flash":      "",
	} {
		if got := AntigravityGroup(model); got != want {
			t.Fatalf("%s: group %q, want %q", model, got, want)
		}
		for _, scope := range []string{"gemini", "claude_gpt"} {
			if got := (Window{Scope: scope}).Applies(model); got != (scope == want) {
				t.Fatalf("%s: %s window applies = %v", model, scope, got)
			}
		}
	}
}
