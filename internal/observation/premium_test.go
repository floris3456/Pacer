package observation

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexPremiumHeadersAndAPIMerge(t *testing.T) {
	s, c, _ := testStore(t)
	at := c.Now()
	windows, err := API("codex", []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":67,"reset_after_seconds":604800}}}`), at)
	if err != nil {
		t.Fatal(err)
	}
	observe(t, s, "account", "codex", windows...)
	c.Add(3 * time.Minute)
	windows = Headers("codex", http.Header{
		"X-Codex-Active-Limit":                {"premium"},
		"X-Codex-Primary-Window-Minutes":      {"10080"},
		"X-Codex-Primary-Used-Percent":        {"69"},
		"X-Codex-Primary-Reset-After-Seconds": {"604623"},
	}, c.Now())
	observe(t, s, "account", "codex", windows...)
	got := s.Windows("account")
	if len(got) != 1 || got[0].Scope != "all" || *got[0].Remaining != 31 || got[0].Usage.Source != HeaderSource {
		t.Fatalf("duplicate or stale weekly allowance: %#v", got)
	}
	for _, name := range []string{"premium", "spark", "custom-gpu"} {
		data := []byte(`{"metered_limit_name":"` + name + `","rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":69}}}`)
		got, err := API("codex", data, at)
		want := map[string]string{"premium": "all", "spark": "spark", "custom-gpu": "limit:custom-gpu"}[name]
		if err != nil || len(got) != 1 || got[0].Scope != want {
			t.Fatalf("%s: %#v, %v", name, got, err)
		}
	}
}

func TestMigrateCodexPremium(t *testing.T) {
	for _, mode := range []string{"newer-header", "newer-api", "alias-only", "retired"} {
		t.Run(mode, func(t *testing.T) {
			c := newClock()
			s, db := openTest(t, filepath.Join(t.TempDir(), "state.sqlite"), c)
			at := c.Now()
			api := sample(at, at.Add(7*24*time.Hour), 33)
			alias := sample(at.Add(3*time.Minute), api.Reset.Add(3*time.Second), 31)
			alias.Scope = "limit:premium"
			want := 31.0
			if mode == "newer-api" {
				api.Usage.At = at.Add(4 * time.Minute)
				api.ResetEvidence.At = api.Usage.At
				want = 33
			}
			if mode != "alias-only" {
				observe(t, s, "account", "codex", api)
			}
			raw, _ := json.Marshal(alias)
			if _, err := db.Exec("INSERT INTO observation_windows(account,key,provider,body) VALUES(?,?,'codex',?)", "account", alias.Key(), string(raw)); err != nil {
				t.Fatal(err)
			}
			if mode == "retired" {
				want = 33
				if _, err := db.Exec("INSERT INTO observation_retired(account,key,reset) VALUES(?,?,?)", "account", api.Key(), alias.Reset.UnixNano()); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				migrated, err := Open(db, c.Now)
				if err != nil {
					t.Fatal(err)
				}
				got := migrated.Windows("account")
				if len(got) != 1 || got[0].Scope != "all" || *got[0].Remaining != want {
					t.Fatalf("migration %d: %#v", i, got)
				}
			}
		})
	}
}
