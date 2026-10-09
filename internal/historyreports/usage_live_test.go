package historyreports

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

const liveSource = "pacer-src"

func liveEvent(n int, account, credential, model string, at time.Time, input, output int64) ForwardEvent {
	id := fmt.Sprintf("native-%s-%d", account, n)
	return ForwardEvent{ID: id, Origin: "native", SourceID: liveSource, StartedAt: at, EndedAt: at.Add(time.Second), Final: true,
		Attribution: measurement.Attribution{Provider: "claude", AccountID: account, CredentialRef: credential, Model: model},
		Event: cpamp.Event{Hash: id, Account: account, Provider: "claude", Model: model, Started: at.UnixMilli(), Completed: at.Add(time.Second).UnixMilli(), Recorded: at.Add(time.Second).UnixMilli(), AccountingVersion: 2, Quality: "complete",
			Tokens: cpamp.Tokens{Input: input, Output: output, Total: input + output}}}
}

func liveReading(id int64, account string, seconds int64, reset, at time.Time, used float64) NativeReading {
	remaining := 100 - used
	return NativeReading{ID: id, Account: account, Provider: "claude", Scope: "all", Seconds: seconds, Remaining: &remaining, Reset: reset, At: at, Source: "header"}
}

func windowsByKey(q *cpamp.QuotaFacts) map[string]cpamp.FactWindow {
	out := map[string]cpamp.FactWindow{}
	for _, w := range q.Windows {
		out[w.Account+"|"+w.Base.ID] = w
	}
	return out
}

func sameJSON(t *testing.T, a, b any, label string) {
	t.Helper()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Fatalf("%s differs:\n%s\n%s", label, x, y)
	}
}

// The tail the live build reads reproduces the prepared windows at H exactly:
// continuing an account introduces no drift at the boundary.
func TestLiveTailReproducesPreparedWindowsAtH(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accounts, _ := r.service.Accounts(ctx)
	frame := cpamp.QuotaFacts{Version: frozen.Version, Filter: frozen.Filter, Config: frozen.Config, PriceBook: frozen.PriceBook, APIKeys: frozen.APIKeys}
	prepared := windowsByKey(frozen)
	checked := 0
	for _, a := range accounts {
		tail, err := r.tail(ctx, frozen, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		part := cpamp.AccountFactsFor(&frame, a, tail.events, tail.obs)
		for _, w := range part.Windows {
			if w.Base.Reset <= frozen.Filter.At {
				continue
			}
			want, ok := prepared[w.Account+"|"+w.Base.ID]
			if !ok {
				t.Fatal("tail derived a window the prepared facts lack", w.Base.ID)
			}
			sameJSON(t, w, want, "window "+w.Base.ID)
			checked++
		}
	}
	if checked < 5 {
		t.Fatal("too few windows open at H", checked)
	}
}

func TestBuildLiveContinuesAccountsAfterH(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := time.UnixMilli(frozen.Filter.At)
	prepared := windowsByKey(frozen)
	week := prepared["forecast|forecast-window"]
	if !week.Latest || week.Base.Used == nil {
		t.Fatal("fixture changed: forecast-window must be open at H", week.Base.ID)
	}
	final := *week.Base.Used
	reset := time.UnixMilli(week.Base.Reset)
	origins := ForwardOrigins{Certificates: []ForwardOriginCertificate{{LegacyAccount: "forecast", Provider: "claude", SourceID: liveSource, NativeAccount: "nf", NativeCredentialRef: "cred-f", StartMS: h.UnixMilli() - 3600000, EvidenceRef: "e", ProducerVersion: "p", ParserVersion: "p"},
		{LegacyAccount: "a", Provider: "claude", SourceID: liveSource, NativeAccount: "na", NativeCredentialRef: "cred-a", StartMS: h.UnixMilli()}}}
	events := []ForwardEvent{liveEvent(0, "nf", "cred-f", "claude-opus-5-5", h.Add(-time.Minute), 100, 10)} // before H: imported history owns it
	for i := 1; i <= 10; i++ {
		events = append(events, liveEvent(i, "nf", "cred-f", "claude-opus-5-5", h.Add(time.Duration(i)*10*time.Minute), 1000, 100))
	}
	events = append(events, liveEvent(1, "nx", "cred-x", "claude-opus-5-5", h.Add(time.Hour), 500, 50), liveEvent(2, "nx", "cred-x", "claude-opus-5-5", h.Add(2*time.Hour), 500, 50))
	fiveReset := h.Add(4 * time.Hour)
	readings := []NativeReading{
		liveReading(1, "nf", 7*86400, reset, h.Add(-time.Minute), final), // before H: skipped
		liveReading(2, "nf", 7*86400, reset, h.Add(5*time.Minute), final+1),
		liveReading(3, "nf", 7*86400, reset, h.Add(115*time.Minute), final+3),
		liveReading(4, "nx", 5*3600, fiveReset, h.Add(30*time.Minute), 10),
		liveReading(5, "nx", 5*3600, fiveReset, h.Add(150*time.Minute), 30),
		// Account a has windows that closed before H: re-deriving its tail must not duplicate them.
		liveReading(6, "na", 5*3600, time.UnixMilli(prepared["a|a-current"].Base.Reset), h.Add(time.Hour), 20),
	}
	now := h.Add(3 * time.Hour)
	live, stats, err := r.BuildLive(ctx, LiveInput{Now: now, Native: UsageNative{Events: events, Origins: origins}, Readings: readings, Source: liveSource, Labels: map[string]string{"nx": "x@example.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 12 || stats.RequestsBeforeH != 1 || stats.Readings != 5 || stats.ReadingsSkipped != 1 || stats.Accounts != 3 || stats.NewAccounts != 1 || stats.Ambiguous != 0 {
		t.Fatalf("%+v", stats)
	}
	// Everything that ended by H, and every account Pacer has not seen since,
	// stays exactly as imported.
	got := windowsByKey(live)
	if len(got) != len(live.Windows) || len(live.Windows) != len(frozen.Windows)+stats.Windows-2 {
		t.Fatal("windows duplicated or lost", len(got), len(live.Windows), len(frozen.Windows), stats.Windows)
	}
	for key, w := range prepared {
		if w.Base.Reset <= h.UnixMilli() || w.Account != "forecast" && w.Account != "a" {
			if !reflect.DeepEqual(got[key], w) {
				t.Fatal("imported window changed", key)
			}
		}
	}
	for i, b := range frozen.Buckets {
		if !reflect.DeepEqual(live.Buckets[i], b) {
			t.Fatal("imported bucket changed", i)
		}
	}
	added := int64(0)
	for _, b := range live.Buckets[len(frozen.Buckets):] {
		added += b.Totals.Requests
	}
	if added != 12 {
		t.Fatal("native requests in buckets", added)
	}
	// The window open at H continues: same ID, imported plus native evidence.
	cont := got["forecast|forecast-window"]
	if !cont.Latest || cont.Base.Status != "current" || cont.Readings != week.Readings+2 || cont.Base.AccountUsage.Requests != week.Base.AccountUsage.Requests+10 || *cont.Base.Used != final+3 {
		t.Fatalf("continued window: latest %v status %s readings %d requests %d used %v", cont.Latest, cont.Base.Status, cont.Readings, cont.Base.AccountUsage.Requests, *cont.Base.Used)
	}
	// An account without a certificate keeps its own reference and shows up
	// unassigned with its own 5-hour window.
	ref := ForwardAccountRef(liveSource, "claude", "nx")
	newWindow := false
	for _, w := range live.Windows {
		if w.Account == ref && w.Base.Kind == "5h" && w.Latest && w.Base.AccountUsage.Requests == 2 && w.Readings == 2 && w.Base.Label == "x@example.invalid" {
			newWindow = true
		}
	}
	unassigned := false
	for _, p := range live.UnassignedPeriods {
		unassigned = unassigned || p.Account == ref
	}
	if !newWindow || !unassigned {
		t.Fatal("uncertified native account", newWindow, unassigned)
	}
	// The usage page reads the live record: the profile's latest weekly window
	// is the continued one, with Pacer's requests in it.
	before, err := r.Compare(ctx, CompareQuery{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.UseLive(live)
	after, err := r.Compare(ctx, CompareQuery{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.FrozenAtMS != now.UnixMilli() || after.ImportedThroughMS != h.UnixMilli() || before.FrozenAtMS != h.UnixMilli() {
		t.Fatal("as-of times", after.FrozenAtMS, after.ImportedThroughMS)
	}
	requests := func(v CompareView) int64 {
		for _, p := range v.Profiles {
			if p.Name != "=Main subscription" {
				continue
			}
			total := int64(0)
			for _, pool := range p.Pools {
				for _, w := range []*CompareWindow{pool.Latest, pool.NewestUnmeasured} {
					if w != nil && w.ID == "forecast-window" {
						total += w.Selected.Requests
					}
				}
			}
			return total
		}
		return -1
	}
	if requests(after) != requests(before)+10 {
		t.Fatal("card does not show Pacer's requests", requests(before), requests(after))
	}
	// Accounts in no profile get one read-only card, so Pacer's unlinked
	// accounts are visible: Windows and Inspect work, exclusions do not.
	var card *CompareProfile
	for i := range after.Profiles {
		if after.Profiles[i].Reference == unassignedReference {
			card = &after.Profiles[i]
		}
	}
	if card == nil || card.Name != "Not in a profile" || len(card.Pools) == 0 || len(after.Profiles) != len(before.Profiles)+1 {
		t.Fatal("no card for accounts in no profile")
	}
	pool := card.Pools[0]
	for _, w := range []*CompareWindow{pool.Latest, pool.NewestUnmeasured} {
		if w != nil && w.Rate.Status != "available" && w.Rate.Reason != unassignedReason {
			t.Fatal("the unassigned card must say how to get a rate", w.Rate.Reason)
		}
	}
	list, err := r.CompareWindows(ctx, CompareWindowsQuery{Profile: unassignedReference, Kind: pool.Kind, Scope: pool.Scope}, nil)
	if err != nil || len(list.Windows) != pool.Windows {
		t.Fatal("unassigned windows", err)
	}
	inspect, err := r.Inspect(ctx, InspectQuery{Profile: unassignedReference, Kind: pool.Kind, Scope: pool.Scope, Window: list.Windows[0].ID}, nil)
	if err != nil || inspect.Totals.Requests != 2 || inspect.ProfileName != "Not in a profile" {
		t.Fatal("unassigned inspect", inspect.Totals.Requests, err)
	}
	r.UseLive(nil)
	if again, _ := r.Compare(ctx, CompareQuery{}, nil); again.FrozenAtMS != h.UnixMilli() {
		t.Fatal("UseLive(nil) must return to the prepared facts")
	}
	// Disagreeing certificates: the requests and readings are not guessed.
	origins.Certificates = append(origins.Certificates, ForwardOriginCertificate{LegacyAccount: "a", Provider: "claude", SourceID: liveSource, NativeAccount: "nf", NativeCredentialRef: "cred-f", StartMS: h.UnixMilli() - 3600000})
	_, stats, err = r.BuildLive(ctx, LiveInput{Now: now, Native: UsageNative{Events: events, Origins: origins}, Readings: readings, Source: liveSource})
	if err != nil || stats.Ambiguous != 12 {
		t.Fatalf("ambiguous %+v %v", stats, err)
	}
	if _, _, err = r.BuildLive(ctx, LiveInput{Now: h.Add(-time.Second)}); err == nil {
		t.Fatal("a live build before H")
	}
}
