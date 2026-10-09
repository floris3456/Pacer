package historyreports

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

func openTestRecord(t *testing.T, r *Reader, dir string) (*Record, *RecordStore) {
	t.Helper()
	store, err := OpenRecordStore(context.Background(), filepath.Join(dir, "usage-record.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return NewRecord(r, store), store
}

// remapped reports whether the pool mapping renames a raw scope.
func remapped(provider, scope string) bool { return canonicalScope(provider, scope) != scope }

// factsDiff compares two quota facts as the usage views read them: the same
// windows (by account and ID), the same day totals per bucket key, the same
// credentials and unassigned accounts. Windows in pools the mapping renames
// are compared separately (skipped reports them).
func factsDiff(want, got *cpamp.QuotaFacts) (diffs []string, compared, skipped int) {
	gw := map[string]cpamp.FactWindow{}
	for _, w := range got.Windows {
		if _, dup := gw[w.Account+"|"+w.Base.ID]; dup {
			diffs = append(diffs, "duplicate window "+w.Base.ID)
		}
		gw[w.Account+"|"+w.Base.ID] = w
	}
	for _, w := range want.Windows {
		if remapped(w.Base.Provider, w.Base.Scope) {
			skipped++
			continue
		}
		g, ok := gw[w.Account+"|"+w.Base.ID]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("missing window %s %s %s %s reset %d", w.Account[:min(24, len(w.Account))], w.Base.Kind, w.Base.Scope, w.Base.ID, w.Base.Reset))
			continue
		}
		delete(gw, w.Account+"|"+w.Base.ID)
		x, _ := json.Marshal(w)
		y, _ := json.Marshal(g)
		if string(x) != string(y) {
			diffs = append(diffs, fmt.Sprintf("window %s %s %s differs:\n want %s\n got  %s", w.Base.Kind, w.Base.Scope, w.Base.ID, x, y))
		}
		compared++
	}
	for _, w := range gw {
		if remapped(w.Base.Provider, w.Base.Scope) || canonicalScope(w.Base.Provider, w.Base.Scope) != w.Base.Scope || w.Base.Scope == fableScope || w.Base.Scope == "codex-main" && w.Base.Provider == "codex" {
			continue
		}
		diffs = append(diffs, fmt.Sprintf("extra window %s %s %s reset %d", w.Base.Kind, w.Base.Scope, w.Base.ID, w.Base.Reset))
	}
	bucketKey := func(b cpamp.FactBucket) string {
		return strings.Join([]string{b.Account, b.Day, b.Model, b.Key, fmt.Sprint(b.Cards)}, "|")
	}
	sum := func(q *cpamp.QuotaFacts) map[string]string {
		m := map[string]*cpamp.FactBucket{}
		for _, b := range q.Buckets {
			k := bucketKey(b)
			if m[k] == nil {
				c := b
				c.Totals = cpamp.ProfileTotals{}
				m[k] = &c
			}
			m[k].Totals.AddTotals(b.Totals)
			m[k].First, m[k].Last = min(m[k].First, b.First), max(m[k].Last, b.Last)
		}
		out := map[string]string{}
		for k, b := range m {
			j, _ := json.Marshal(b)
			out[k] = string(j)
		}
		return out
	}
	wb, gb := sum(want), sum(got)
	for k, v := range wb {
		if gb[k] != v {
			diffs = append(diffs, "bucket "+k+" differs")
		}
	}
	if len(wb) != len(gb) {
		diffs = append(diffs, fmt.Sprintf("buckets %d vs %d", len(wb), len(gb)))
	}
	creds := map[string]cpamp.ProfileCredential{}
	for _, c := range got.Credentials {
		creds[c.Account.ID] = c
	}
	for _, c := range want.Credentials {
		g := creds[c.Account.ID]
		if c.Requests != g.Requests || c.First != g.First || c.Last != g.Last || c.Status != g.Status {
			diffs = append(diffs, fmt.Sprintf("credential %s: %d %d %d %s vs %d %d %d %s", c.Account.ID, c.Requests, c.First, c.Last, c.Status, g.Requests, g.First, g.Last, g.Status))
		}
	}
	un := func(q *cpamp.QuotaFacts) string {
		ids := []string{}
		for _, p := range q.UnassignedPeriods {
			ids = append(ids, p.Account)
		}
		sort.Strings(ids)
		return strings.Join(ids, ",")
	}
	if un(want) != un(got) {
		diffs = append(diffs, "unassigned "+un(want)+" vs "+un(got))
	}
	if want.NextTransition != got.NextTransition || want.OrderSensitive != got.OrderSensitive {
		diffs = append(diffs, fmt.Sprintf("next %d/%d order %v/%v", want.NextTransition, got.NextTransition, want.OrderSensitive, got.OrderSensitive))
	}
	return diffs, compared, skipped
}

// Deriving the imported history segment by segment gives the prepared facts.
func TestRecordReproducesPreparedFacts(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rec, _ := openTestRecord(t, r, dir)
	h := time.UnixMilli(frozen.Filter.At)
	got, st, err := rec.Build(ctx, RecordInput{Now: h})
	if err != nil {
		t.Fatal(err)
	}
	diffs, compared, skipped := factsDiff(frozen, got)
	for _, d := range diffs {
		t.Error(d)
	}
	if compared < 10 {
		t.Fatal("too few windows compared", compared, skipped)
	}
	t.Logf("compared %d windows (%d renamed pools), %+v", compared, skipped, st)
	// Nothing new: a second build derives nothing.
	if _, st2, err := rec.Build(ctx, RecordInput{Now: h}); err != nil || st2.Derived != 0 {
		t.Fatal("second build derived again", st2.Derived, err)
	}
	// A week later every imported segment is sealed and stored; a new process
	// derives only what is still open.
	later := h.Add(30 * 24 * time.Hour)
	if _, st3, err := rec.Build(ctx, RecordInput{Now: later}); err != nil || st3.Persisted == 0 {
		t.Fatal("nothing sealed a month later", st3, err)
	}
	again, _ := openTestRecord(t, r, dir)
	got2, st4, err := again.Build(ctx, RecordInput{Now: later})
	if err != nil {
		t.Fatal(err)
	}
	if st4.Derived > st4.Open {
		t.Fatal("sealed segments were derived again", st4)
	}
	if d, _, _ := factsDiff(rec.Facts(), got2); len(d) != 0 {
		t.Fatal("reloaded record differs", d[0])
	}
}

// TestRealRecordProbe runs the record over a real prepared artifact (opt-in):
// PACER_REAL_ARTIFACT=/path/reports.sqlite.
func TestRealRecordProbe(t *testing.T) {
	artifact := os.Getenv("PACER_REAL_ARTIFACT")
	if artifact == "" {
		t.Skip("PACER_REAL_ARTIFACT required")
	}
	ctx := context.Background()
	r, err := Open(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := time.UnixMilli(frozen.Filter.At)
	// With CPAMP's own pool names the record must equal the prepared facts.
	poolMapping = false
	plain, _ := openTestRecord(t, r, t.TempDir())
	start := time.Now()
	same, st, err := plain.Build(ctx, RecordInput{Now: h})
	poolMapping = true
	if err != nil {
		t.Fatal(err)
	}
	strict, compared, _ := factsDiff(frozen, same)
	t.Logf("CPAMP pool names: %.1fs, %d windows compared, %d differences", time.Since(start).Seconds(), compared, len(strict))
	for i, d := range strict {
		if i < 10 {
			t.Log(d)
		}
	}
	if len(strict) > 0 {
		t.Fatal("segmented derivation differs from the prepared facts")
	}
	dir := t.TempDir()
	rec, store := openTestRecord(t, r, dir)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start = time.Now()
	got, st, err := rec.Build(ctx, RecordInput{Now: h})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	t.Logf("first build %.1fs, %.0f MB allocated: %+v", time.Since(start).Seconds(), float64(after.TotalAlloc-before.TotalAlloc)/1e6, st)
	diffs, compared, skipped := factsDiff(frozen, got)
	for i, d := range diffs {
		if i < 30 {
			t.Log(d)
		}
	}
	t.Logf("pool mapping: compared %d windows, %d in renamed pools, %d differences (expected only in Codex pools: merged pool, joined twins, early ends)", compared, skipped, len(diffs))
	for _, d := range diffs {
		if !strings.Contains(d, "codex-main") && !strings.Contains(d, sparkScope) {
			t.Error("difference outside the Codex pools:", d[:min(200, len(d))])
		}
	}
	// Sealing: a month on, then a fresh process.
	later := h.Add(30 * 24 * time.Hour)
	start = time.Now()
	if _, st, err = rec.Build(ctx, RecordInput{Now: later}); err != nil {
		t.Fatal(err)
	}
	t.Logf("month later %.1fs: %+v", time.Since(start).Seconds(), st)
	again := NewRecord(r, store)
	runtime.GC()
	runtime.ReadMemStats(&before)
	start = time.Now()
	if _, st, err = again.Build(ctx, RecordInput{Now: later}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	t.Logf("fresh process %.1fs, %.0f MB allocated: %+v", time.Since(start).Seconds(), float64(after.TotalAlloc-before.TotalAlloc)/1e6, st)
}

// TestRealRecordTickProbe measures one live update after a week of simulated
// native traffic (each account's last imported week replayed after H).
func TestRealRecordTickProbe(t *testing.T) {
	artifact := os.Getenv("PACER_REAL_ARTIFACT")
	if artifact == "" {
		t.Skip("PACER_REAL_ARTIFACT required")
	}
	ctx := context.Background()
	r, err := Open(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	frozen, _ := r.quotaFacts(ctx)
	h := frozen.Filter.At
	// Replay the last three imported weeks five weeks after H, so open
	// segments read only native evidence (the steady state).
	shift, span := 35*dayMS, 21*dayMS
	if os.Getenv("PACER_TICK_EARLY") != "" {
		shift, span = 7*dayMS, 7*dayMS
	}
	rec, store := openTestRecord(t, r, t.TempDir())
	origins := ForwardOrigins{}
	var events []RecordEvent
	var readings []NativeReading
	var refs []string
	for _, a := range r.known(t, ctx) {
		native := "native-" + fmt.Sprint(len(origins.Certificates))
		origins.Certificates = append(origins.Certificates, ForwardOriginCertificate{LegacyAccount: a.ID, Provider: a.Provider, SourceID: liveSource, NativeAccount: native, NativeCredentialRef: "cred", StartMS: h})
		ref := ForwardAccountRef(liveSource, a.Provider, native)
		ev, obs, err := r.service.AccountEvidenceRange(ctx, frozen, a.ID, cpamp.Range{EventsFrom: h - span, EventsTo: h + 1, ObsFrom: h - span, ObsTo: h + 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ev {
			e.ID, e.Value, e.Account = 0, nil, native
			e.Started += shift
			e.Completed += shift
			e.Recorded += shift
			events = append(events, RecordEvent{ID: fmt.Sprintf("probe-%d", len(events)), Revision: 1, Ref: ref, Native: native, Credential: "cred", Source: liveSource, Provider: a.Provider, Started: e.Started, Origin: "forward", Event: e})
		}
		for _, o := range obs {
			if o.Used == nil || o.Duration <= 0 {
				continue
			}
			remaining := 100 - *o.Used
			source := "api"
			if o.Source == "response_header" {
				source = "header"
			}
			readings = append(readings, NativeReading{ID: int64(len(readings) + 1), Account: native, Provider: a.Provider, Scope: o.Scope, Seconds: o.Duration / 1000, Remaining: &remaining, Reset: time.UnixMilli(o.Reset + shift), At: time.UnixMilli(o.Observed + shift), Source: source})
			refs = append(refs, ref)
		}
	}
	changes, err := store.PutEvents(ctx, events)
	if err != nil {
		t.Fatal(err)
	}
	more, err := store.PutReadings(ctx, readings, refs)
	if err != nil {
		t.Fatal(err)
	}
	changes.Merge(more)
	now := time.UnixMilli(h + shift)
	start := time.Now()
	_, st, err := rec.Build(ctx, RecordInput{Now: now, Source: liveSource, Origins: origins, Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d native requests, %d readings; first build %.1fs: %+v", len(events), len(readings), time.Since(start).Seconds(), st)
	// One more request and reading on the busiest account, 30 s later.
	last := events[len(events)-1]
	busiest := map[string]int{}
	for _, e := range events {
		busiest[e.Ref]++
	}
	for _, e := range events {
		if e.Provider == "claude" && (last.Provider != "claude" || busiest[e.Ref] > busiest[last.Ref]) {
			last = e
		}
	}
	var reading NativeReading
	for _, rd := range readings {
		if ForwardAccountRef(liveSource, rd.Provider, rd.Account) == last.Ref && rd.Seconds == 7*86400 && rd.Scope == "all" {
			reading = rd
		}
	}
	tick := func(n int) {
		at := now.Add(time.Duration(n) * 30 * time.Second)
		e := last
		e.ID, e.Started = fmt.Sprintf("tick-%d", n), at.UnixMilli()
		e.Event.Started, e.Event.Completed = at.UnixMilli(), at.UnixMilli()+1000
		ch, err := store.PutEvents(ctx, []RecordEvent{e})
		if err != nil {
			t.Fatal(err)
		}
		rd := reading
		rd.ID, rd.At = int64(len(readings)+n), at
		more, err := store.PutReadings(ctx, []NativeReading{rd}, []string{last.Ref})
		if err != nil {
			t.Fatal(err)
		}
		ch.Merge(more)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		start := time.Now()
		_, st, err := rec.Build(ctx, RecordInput{Now: at, Source: liveSource, Origins: origins, Changes: ch})
		if err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		t.Logf("tick %d: %.0f ms, %.0f MB allocated, %d segments derived", n, float64(time.Since(start).Milliseconds()), float64(after.TotalAlloc-before.TotalAlloc)/1e6, st.Derived)
	}
	if path := os.Getenv("PACER_TICK_PROFILE"); path != "" {
		f, _ := os.Create(path + ".cpu")
		pprof.StartCPUProfile(f)
		runtime.MemProfileRate = 64 * 1024
		for n := 1; n <= 3; n++ {
			tick(n)
		}
		pprof.StopCPUProfile()
		f.Close()
		m, _ := os.Create(path + ".mem")
		pprof.Lookup("allocs").WriteTo(m, 0)
		m.Close()
	}
	for n := 4; n <= 6; n++ {
		tick(n)
	}
	for id, a := range rec.acc {
		for _, ref := range a.refs {
			if ref == last.Ref {
				t.Logf("busiest %s %s kinds %v refs %d first %d", id[:30], a.provider, a.kinds, len(a.refs), a.first)
			}
		}
	}
}

func (r *Reader) known(t *testing.T, ctx context.Context) []cpamp.Account {
	a, err := r.service.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
