package historyreports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// quotaFactFilters returns every profile/key/model/account selector the default
// report exposes, crossed pairwise with the profile selector, plus full crosses
// of the first few values.
func quotaFactFilters(t *testing.T, r *Reader, base Pin) []Pin {
	t.Helper()
	ctx := context.Background()
	w, e := r.service.WorkspaceOriginal(ctx, base)
	if e != nil {
		t.Fatal(e)
	}
	profiles := []string{"", "all", "unassigned"}
	for _, p := range w.Config.Profiles {
		profiles = append(profiles, p.ID)
	}
	keys := []string{"", cpamp.UnknownAPIKey}
	for _, k := range w.APIKeys {
		if k.ID == cpamp.UnknownAPIKey {
			continue
		}
		keys = append(keys, k.ID)
		if strings.HasPrefix(k.ID, "keys:") {
			keys = append(keys, strings.Split(strings.TrimPrefix(k.ID, "keys:"), ",")...)
		}
	}
	models := []string{""}
	for _, m := range w.Models {
		models = append(models, m.Key)
	}
	accounts := []string{""}
	for _, c := range w.Credentials {
		accounts = append(accounts, c.ID)
	}
	var out []Pin
	add := func(profile, key, model, account string) {
		f := base
		f.Profile, f.APIKey, f.Model, f.Account = profile, key, model, account
		out = append(out, f)
	}
	for _, p := range profiles {
		for _, k := range keys {
			add(p, k, "", "")
		}
		for _, m := range models[1:] {
			add(p, "", m, "")
		}
		for _, a := range accounts[1:] {
			add(p, "", "", a)
		}
	}
	for _, p := range profiles[:min(4, len(profiles))] {
		for _, k := range keys[:min(3, len(keys))] {
			for _, m := range models[:min(3, len(models))] {
				for _, a := range accounts[:min(3, len(accounts))] {
					add(p, k, m, a)
				}
			}
		}
	}
	return out
}

func verifyQuotaFactsParity(t *testing.T, r *Reader) int {
	t.Helper()
	ctx := context.Background()
	q, e := r.quotaFacts(ctx)
	if e != nil {
		t.Fatal(e)
	}
	n := 0
	for _, f := range quotaFactFilters(t, r, r.meta.Pin) {
		want, we := r.service.WorkspaceOriginal(ctx, f)
		got, ok, ge := r.service.FactsWorkspace(ctx, q, f, cpamp.FactsOptions{})
		if !ok {
			t.Fatalf("filter %+v not eligible", f.ProfileFilter)
		}
		if (we == nil) != (ge == nil) || we != nil && we.Error() != ge.Error() {
			t.Fatalf("filter %+v: errors differ: %v / %v", f.ProfileFilter, we, ge)
		}
		if we != nil {
			continue
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if !bytes.Equal(a, b) {
			at := 0
			for at < len(a) && at < len(b) && a[at] == b[at] {
				at++
			}
			lo := max(at-160, 0)
			t.Fatalf("filter %+v differs at byte %d:\nfast=%s\norig=%s", f.ProfileFilter, at, a[lo:min(len(a), at+200)], b[lo:min(len(b), at+200)])
		}
		n++
	}
	return n
}

func TestQuotaFactsWorkspaceParity(t *testing.T) {
	for _, c := range []struct {
		name   string
		reader func(*testing.T) *Reader
	}{{"workspace-fixture", fixtureReader}, {"historical-fixture", historicalFixture}} {
		t.Run(c.name, func(t *testing.T) {
			r := c.reader(t)
			if !r.meta.QuotaFacts {
				t.Fatal("preparation did not record quota facts")
			}
			n := verifyQuotaFactsParity(t, r)
			if n < 50 {
				t.Fatalf("only %d filters compared", n)
			}
			t.Logf("%d filters byte-identical", n)
		})
	}
}

func TestQuotaFactsEligibilityAndFallback(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	q, e := r.quotaFacts(ctx)
	if e != nil {
		t.Fatal(e)
	}
	base := r.meta.Pin
	for name, f := range map[string]Pin{
		"from":        func() Pin { p := base; p.From = 86400000; return p }(),
		"to":          func() Pin { p := base; p.To = base.At; return p }(),
		"through":     func() Pin { p := base; p.Through--; return p }(),
		"observation": func() Pin { p := base; p.ObservationsThrough--; return p }(),
		"at":          func() Pin { p := base; p.At--; return p }(),
	} {
		if _, ok, e := r.service.FactsWorkspace(ctx, q, f, cpamp.FactsOptions{}); ok || e != nil {
			t.Fatalf("%s: fast builder must decline (ok=%v err=%v)", name, ok, e)
		}
	}
	// WholeWindows accepts only UTC-midnight bounds.
	p := base
	p.From = 86400000 + 1
	if _, ok, _ := r.service.FactsWorkspace(ctx, q, p, cpamp.FactsOptions{WholeWindows: true}); ok {
		t.Fatal("unaligned bound accepted")
	}
	// Report on a date-bounded pin still equals the original builder.
	p = base
	p.To = (base.At/86400000 - 1) * 86400000
	want, e := r.service.WorkspaceOriginal(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	got, e := r.Report(ctx, Query{To: p.To})
	if e != nil {
		t.Fatal(e)
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		t.Fatal("date-bounded report differs from the original builder")
	}
	// An artifact without facts keeps working and reports the missing capability.
	r.meta.QuotaFacts = false
	r.facts = nil
	if _, e = r.quotaFacts(ctx); !errors.Is(e, ErrNotPrepared) {
		t.Fatal(e)
	}
}
