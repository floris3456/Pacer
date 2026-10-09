package historyreports

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// putLive stores forward requests and readings as the runtime does.
func putLive(t *testing.T, store *RecordStore, events []ForwardEvent, readings []NativeReading) Changes {
	t.Helper()
	ctx := context.Background()
	rec := make([]RecordEvent, len(events))
	for i, e := range events {
		rec[i] = RecordEventFromForward(e)
		rec[i].Revision = 1
	}
	ch, err := store.PutEvents(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]string, len(readings))
	for i, r := range readings {
		refs[i] = ForwardAccountRef(liveSource, r.Provider, r.Account)
	}
	more, err := store.PutReadings(ctx, readings, refs)
	if err != nil {
		t.Fatal(err)
	}
	ch.Merge(more)
	return ch
}

func TestRecordContinuesAccountsAfterH(t *testing.T) {
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
	// A later login of the same account (new credential, no certificate)
	// still continues the imported account.
	events = append(events, liveEvent(11, "nf", "cred-f2", "claude-opus-5-5", h.Add(115*time.Minute), 1000, 100))
	events = append(events, liveEvent(1, "nx", "cred-x", "claude-opus-5-5", h.Add(time.Hour), 500, 50), liveEvent(2, "nx", "cred-x", "claude-opus-5-5", h.Add(2*time.Hour), 500, 50))
	fiveReset := h.Add(4 * time.Hour)
	readings := []NativeReading{
		liveReading(1, "nf", 7*86400, reset, h.Add(-time.Minute), final), // before H: skipped
		liveReading(2, "nf", 7*86400, reset, h.Add(5*time.Minute), final+1),
		liveReading(3, "nf", 7*86400, reset, h.Add(115*time.Minute), final+3),
		liveReading(4, "nx", 5*3600, fiveReset, h.Add(30*time.Minute), 10),
		liveReading(5, "nx", 5*3600, fiveReset, h.Add(150*time.Minute), 30),
		// Account a has windows that closed before H: re-deriving it must not duplicate them.
		liveReading(6, "na", 5*3600, time.UnixMilli(prepared["a|a-current"].Base.Reset), h.Add(time.Hour), 20),
	}
	rec, store := openTestRecord(t, r, t.TempDir())
	ch := putLive(t, store, events, readings)
	now := h.Add(3 * time.Hour)
	in := RecordInput{Now: now, Source: liveSource, Origins: origins, Labels: map[string]string{"nx": "x@example.invalid"}, Changes: ch}
	live, st, err := rec.Build(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if st.Ambiguous != 0 {
		t.Fatalf("%+v", st)
	}
	got := windowsByKey(live)
	if len(got) != len(live.Windows) {
		t.Fatal("windows duplicated", len(got), len(live.Windows))
	}
	// Everything that ended by H, and every account Pacer has not seen since,
	// stays exactly as imported.
	for key, w := range prepared {
		if remapped(w.Base.Provider, w.Base.Scope) {
			continue // a pool the mapping renames (compared in TestRecordPools)
		}
		if w.Base.Reset <= h.UnixMilli() || w.Account != "forecast" && w.Account != "a" {
			g := got[key]
			// Staleness is decided now, not at H (the old live build froze it).
			if w.Base.Status == "current" && now.UnixMilli()-w.Base.LastObserved > 600000 {
				w.Base.Stale = true
			}
			if !reflect.DeepEqual(g, w) {
				t.Fatal("imported window changed", key)
			}
		}
	}
	native := int64(0)
	for _, c := range live.Credentials {
		native += c.Requests
	}
	for _, c := range frozen.Credentials {
		native -= c.Requests
	}
	if native != 13 {
		t.Fatal("native requests counted", native)
	}
	// The window open at H continues: same ID, imported plus native evidence,
	// including the later login's request.
	cont := got["forecast|forecast-window"]
	if !cont.Latest || cont.Base.Status != "current" || cont.Readings != week.Readings+2 || cont.Base.AccountUsage.Requests != week.Base.AccountUsage.Requests+11 || *cont.Base.Used != final+3 {
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
	var card *CompareProfile
	for i := range after.Profiles {
		if after.Profiles[i].Reference == unassignedReference {
			card = &after.Profiles[i]
		}
	}
	if card == nil || card.Name != "Not in a profile" || len(card.Pools) == 0 {
		t.Fatal("no card for accounts in no profile")
	}
	r.UseLive(nil)
	// Linking the unlinked login to the imported account merges them: one
	// account, one window list.
	in.Changes, in.Links = nil, []AccountLink{{Account: ref, SameAs: "forecast"}}
	linked, _, err := rec.Build(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range linked.Windows {
		if w.Account == ref {
			t.Fatal("a linked login kept its own windows")
		}
	}
	for _, c := range linked.Credentials {
		if c.Account.ID == ref {
			t.Fatal("a linked login kept its own credential")
		}
	}
	// Disagreeing certificates: the requests and readings are not guessed.
	in.Links = nil
	in.Origins.Certificates = append(in.Origins.Certificates, ForwardOriginCertificate{LegacyAccount: "a", Provider: "claude", SourceID: liveSource, NativeAccount: "nf", NativeCredentialRef: "cred-f", StartMS: h.UnixMilli() - 3600000})
	amb, st, err := rec.Build(ctx, in)
	if err != nil || st.Ambiguous != 1 {
		t.Fatalf("ambiguous %+v %v", st, err)
	}
	if w := windowsByKey(amb)["forecast|forecast-window"]; w.Base.AccountUsage.Requests != week.Base.AccountUsage.Requests {
		t.Fatal("ambiguous requests were counted", w.Base.AccountUsage.Requests)
	}
	if _, _, err = rec.Build(ctx, RecordInput{Now: h.Add(-time.Second)}); err == nil {
		t.Fatal("a build before H")
	}
}
