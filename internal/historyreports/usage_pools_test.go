package historyreports

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func codexEvent(n int, account string, at time.Time) ForwardEvent {
	e := liveEvent(n, account, "cred-"+account, "gpt-5.6-sol", at, 1000, 100)
	e.Event.Provider = "codex"
	e.Attribution = measurement.Attribution{Provider: "codex", AccountID: account, CredentialRef: "cred-" + account, Model: "gpt-5.6-sol"}
	return e
}

// codexReading is a weekly Codex meter reading; start is reset - 7 days.
func codexReading(id int64, account string, start, at time.Time, used float64, source string) NativeReading {
	r := liveReading(id, account, 7*86400, start.Add(7*24*time.Hour), at, used)
	r.Provider, r.Scope, r.Source = "codex", "codex-main", source
	return r
}

// Every Codex cycle is one window with its real span: a twin recorded with
// boundaries a few seconds off joins its window, a window the provider ended
// early ends where the next cycle begins, and both count in the average.
func TestRecordCodexCycles(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := time.UnixMilli(frozen.Filter.At)
	book := frozen.PriceBook
	book.Prices = map[string]ForwardModelPrice{}
	for k, v := range frozen.PriceBook.Prices {
		book.Prices[k] = v
	}
	book.Prices["gpt-5.6-sol"] = ForwardModelPrice{Prompt: 4, Completion: 20, PromptConfigured: true, CompletionConfigured: true}

	// Account nc: cycle A (with a twin 3 s off), an idle forecast after A's
	// last reading, then cycle B starting inside A's week.
	aStart, bStart := h.Add(time.Hour), h.Add(5*time.Hour)
	twin := aStart.Add(3 * time.Second)
	readings := []NativeReading{
		codexReading(1, "nc", aStart, aStart.Add(20*time.Minute), 2, "api"),
		codexReading(2, "nc", aStart, h.Add(2*time.Hour), 10, "api"),
		codexReading(3, "nc", twin, h.Add(150*time.Minute), 15, "header"),
		codexReading(4, "nc", aStart, h.Add(3*time.Hour), 20, "api"),
		codexReading(5, "nc", twin, h.Add(210*time.Minute), 25, "header"),
		codexReading(6, "nc", aStart, h.Add(4*time.Hour), 30, "api"),
		codexReading(7, "nc", h.Add(270*time.Minute), h.Add(270*time.Minute), 0, "api"), // idle forecast
		codexReading(8, "nc", bStart, bStart.Add(5*time.Second), 0, "api"),              // a new cycle's first reading: looks like an idle forecast
		codexReading(9, "nc", bStart, h.Add(6*time.Hour), 10, "api"),
		codexReading(10, "nc", bStart, h.Add(7*time.Hour), 20, "api"),
	}
	// Historical headers called the very same allowance premium; API polls
	// and newer headers call it all. Both must contribute to the same cycles.
	for _, i := range []int{2, 4, 9} {
		readings[i].Scope = "limit:premium"
	}
	events := []ForwardEvent{}
	for at := aStart.Add(10 * time.Second); at.Before(h.Add(4 * time.Hour)); at = at.Add(10 * time.Minute) {
		events = append(events, codexEvent(len(events), "nc", at))
	}
	inA := len(events)
	for at := bStart.Add(10 * time.Second); at.Before(h.Add(7 * time.Hour)); at = at.Add(10 * time.Minute) {
		events = append(events, codexEvent(len(events), "nc", at))
	}
	ncTotal := len(events)
	// Account nd: two meters reported at once. Cycle C keeps reading after D
	// starts inside C's week, so nothing says C ended.
	cStart, dStart := h.Add(time.Hour), h.Add(3*time.Hour)
	readings = append(readings,
		codexReading(11, "nd", cStart, h.Add(70*time.Minute), 5, "api"),
		codexReading(12, "nd", cStart, h.Add(2*time.Hour), 10, "api"),
		codexReading(13, "nd", dStart, h.Add(190*time.Minute), 50, "header"),
		codexReading(14, "nd", dStart, h.Add(5*time.Hour), 60, "header"),
		codexReading(15, "nd", cStart, h.Add(6*time.Hour), 40, "api"),
	)
	for i, at := 0, cStart.Add(5*time.Minute); at.Before(h.Add(6 * time.Hour)); i, at = i+1, at.Add(20*time.Minute) {
		events = append(events, codexEvent(100+i, "nd", at))
	}

	// nc is in a profile (accounts in no profile have no average).
	ncRef := ForwardAccountRef(liveSource, "codex", "nc")
	config := frozen.Config
	config.Profiles = append(append([]cpamp.Profile{}, frozen.Config.Profiles...), cpamp.Profile{ID: "codex-cycles", Name: "Codex", Provider: "codex", Memberships: []cpamp.ProfileMembership{{Account: ncRef}}})

	rec, store := openTestRecord(t, r, t.TempDir())
	ch := putLive(t, store, events, readings)
	now := h.Add(8 * time.Hour)
	live, _, err := rec.Build(ctx, RecordInput{Now: now, Source: liveSource, Book: &book, Config: &config, Changes: ch})
	if err != nil {
		t.Fatal(err)
	}
	byAccount := map[string][]cpamp.FactWindow{}
	for _, w := range live.Windows {
		if w.Base.Provider == "codex" && w.Base.Kind == "weekly" {
			byAccount[w.Account] = append(byAccount[w.Account], w)
		}
	}
	nc := byAccount[ncRef]
	if len(nc) != 2 {
		for _, w := range nc {
			t.Logf("%s start %d readings %d status %s", w.Base.ID, w.Base.Start, w.Readings, w.Base.Status)
		}
		t.Fatalf("nc: %d weekly windows, want 2 (twin joined, idle forecast dropped)", len(nc))
	}
	a, b := nc[0], nc[1]
	if a.Base.Start > b.Base.Start {
		a, b = b, a
	}
	if a.Base.Start != aStart.UnixMilli() || b.Base.Start != bStart.UnixMilli() || a.Readings != 6 || b.Readings != 3 {
		t.Fatalf("windows: A start %d readings %d, B start %d readings %d", a.Base.Start, a.Readings, b.Base.Start, b.Readings)
	}
	if a.Base.Status != "closed" || a.Base.AttributionEnd != bStart.UnixMilli() || a.Base.Reset != aStart.Add(7*24*time.Hour).UnixMilli() {
		t.Fatalf("A must end where B begins: status %s end %d reset %d", a.Base.Status, a.Base.AttributionEnd, a.Base.Reset)
	}
	if a.Base.AccountUsage.Requests != int64(inA) || b.Base.AccountUsage.Requests != int64(ncTotal-inA) {
		t.Fatalf("requests: A %d (want %d), B %d", a.Base.AccountUsage.Requests, inA, b.Base.AccountUsage.Requests)
	}
	if n := a.Base.Normalization; n == nil || n.Status != "available" || n.Delta != 28 || n.From != aStart.Add(20*time.Minute).UnixMilli() {
		t.Fatalf("A's measurement must use every reading, twins included: %+v", n)
	}
	if b.Base.Status != "current" || !b.Latest || b.Base.Valuation == nil || b.Base.Valuation.Completion.Status != "uncertain" {
		t.Fatalf("B: status %s latest %v valuation %+v", b.Base.Status, b.Latest, b.Base.Valuation)
	}

	nd := byAccount[ForwardAccountRef(liveSource, "codex", "nd")]
	if len(nd) != 2 {
		t.Fatalf("nd: %d weekly windows, want 2", len(nd))
	}
	for _, w := range nd {
		if w.Base.Start == cStart.UnixMilli() && (w.Base.Status == "closed" || w.Base.AttributionEnd != w.Base.Reset) {
			t.Fatalf("C kept reading after D began, so it did not end: status %s end %d reset %d", w.Base.Status, w.Base.AttributionEnd, w.Base.Reset)
		}
	}

	// Both of nc's windows count in the average. CPAMP's rule would leave B
	// out (its cycle completion is uncertain: it began inside A's week).
	view, err := cpamp.FactsView(ctx, live, "", "", "", "", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	counted := map[string]bool{}
	profile := view.Profiles[len(view.Profiles)-1]
	if profile.ID != "codex-cycles" {
		t.Fatal("profile order", profile.ID)
	}
	for _, g := range profile.Rates {
		if g.Provider == "codex" && g.Kind == "weekly" {
			for _, id := range g.Normalization.WindowIDs {
				counted[id] = true
			}
		}
	}
	if !counted[a.Base.ID] || !counted[b.Base.ID] {
		for _, g := range profile.Rates {
			t.Logf("rate %s %+v", g.ID, g.Normalization)
		}
		t.Fatalf("average must count both cycles: A %v B %v", counted[a.Base.ID], counted[b.Base.ID])
	}
	if !cpamp.EndedEarlyOrOverlapping(b.Base, book) {
		t.Fatal("B is no longer a window CPAMP's rule leaves out; the test proves nothing")
	}
}

// Pacer's average counts windows CPAMP's v3 rule leaves out; each account's
// time still counts once and the rule ID and reference stay the book's.
func TestUsageRatesCountEndedEarly(t *testing.T) {
	book := cpamp.ComparisonPriceBook{ID: "book", Policy: "profile-api-value-v3", References: map[string]cpamp.ReferenceUnit{"codex": {Model: "m", InputPerMillion: 4}}}
	mk := func(id, status string, from, through int64, delta float64, completion string) cpamp.QuotaUsageWindow {
		n := &cpamp.ProfileNormalization{RuleID: "book", Status: "available", From: from, Through: through, Delta: delta, Contained: cpamp.APIValue{Low: int64(delta) * 1e9, High: int64(delta) * 1e9}, Possible: cpamp.APIValue{Low: int64(delta) * 1e9, High: int64(delta) * 1e9}, Samples: 1, WindowIDs: []string{id}}
		w := cpamp.QuotaUsageWindow{ID: id, Account: "acct", Provider: "codex", Kind: "weekly", Scope: "codex-main", Status: status, Normalization: n}
		w.Valuation = &cpamp.WindowValuation{Completion: cpamp.WindowCompletion{CycleClosure: cpamp.CycleClosure{Status: completion}}}
		return w
	}
	windows := []cpamp.QuotaUsageWindow{
		mk("closed", "closed", 0, 100, 10, "closed"),
		mk("ended-early", "superseded", 200, 300, 20, "uncertain"),
		mk("after-early-end", "closed", 400, 500, 30, "uncertain"),
		mk("same-time", "closed", 450, 480, 5, "closed"), // overlaps after-early-end on the same account
	}
	got := cpamp.UsageRates(windows, book)
	if len(got) != 1 {
		t.Fatal(got)
	}
	n := got[0].Normalization
	if n.Samples != 3 || n.Skipped != 1 || n.Delta != 60 || n.RuleID != "book" || n.Reference.Model != "m" || fmt.Sprint(n.WindowIDs) != "[after-early-end ended-early closed]" {
		t.Fatalf("%+v", n)
	}
}
