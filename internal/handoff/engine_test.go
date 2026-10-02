package handoff

import (
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type lab struct {
	e   *Engine
	now int64
	cfg Config
	n   int
}

func setup(t *testing.T) *lab {
	t.Helper()
	l := &lab{now: 100000, cfg: Config{StatePath: filepath.Join(t.TempDir(), "quota.sqlite"), Credentials: []Credential{
		{ID: "small", Index: "1", Provider: "claude", Account: "same-small-account", Alias: "Small", Rank: 1},
		{ID: "small-copy", Index: "2", Provider: "claude", Account: "same-small-account", Alias: "Small copy", Rank: 1},
		{ID: "large", Index: "3", Provider: "claude", Account: "large", Alias: "Large", Rank: 2},
	}}}
	var err error
	l.e, err = Open(l.cfg, func() time.Time { return time.Unix(l.now, 0) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.e.Close() })
	l.observe(t, "small", 100, 100, 118000)
	l.observe(t, "large", 100, 100, 118000)
	return l
}
func (l *lab) observe(t *testing.T, id string, hour, week float64, reset int64) {
	t.Helper()
	err := l.e.Observe(id, []quota.Window{{Scope: "all", Seconds: 18000, Remaining: hour, Reset: reset, Observed: l.now}, {Scope: "all", Seconds: 604800, Remaining: week, Reset: 704800, Observed: l.now}})
	if err != nil {
		t.Fatal(err)
	}
}
func claim(session string) Claim {
	return Claim{Protocol: 1, Installation: "test", Session: session, Route: "model", Request: "r-" + session, Kind: "primary", Capability: Digest(strings.Repeat(session, 48)), Auto: true, Fresh: true, Portable: true}
}
func (l *lab) pick(q Claim) (Flight, error) {
	return l.e.Pick("caller", "claude", "claude-sonnet", []string{"small", "small-copy", "large"}, &q)
}
func (l *lab) snapshot(t *testing.T, session string) Snapshot {
	t.Helper()
	s, err := l.e.Snapshot(strings.Repeat(session, 48), 0, "model")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWarningsOnlyBoundChatsAndCoalescedHandoff(t *testing.T) {
	l := setup(t)
	q := claim("a")
	f, err := l.pick(q)
	if err != nil || f.Auth != "small" {
		t.Fatal(f, err)
	}
	l.complete(q)
	q2 := claim("b")
	q2.Request = "b"
	f, err = l.e.Pick("caller", "claude", "claude-sonnet", []string{"large"}, &q2)
	if err != nil {
		t.Fatal(err)
	}
	l.complete(q2)
	l.observe(t, "small-copy", 19, 14, 118000)
	if s := l.snapshot(t, "a"); len(s.Events) != 1 || s.Events[0].Kind != "warning" {
		t.Fatal(s)
	}
	if len(l.snapshot(t, "b").Events) != 0 {
		t.Fatal("unrelated chat warned")
	}
	l.observe(t, "small", 9, 4, 118000)
	s := l.snapshot(t, "a")
	if s.Intent == "" || len(s.Events) != 2 {
		t.Fatal(s)
	}
	l.observe(t, "small", 11, 6, 118000)
	if len(l.snapshot(t, "a").Events) != 2 {
		t.Fatal("duplicate event from jitter")
	}
	q.Generation = 1
	q.Fresh = false
	q.Request = "next"
	if _, err = l.pick(q); err == nil || !strings.Contains(err.Error(), "quota_checkpoint_required") {
		t.Fatal(err)
	}
	q.Intent = s.Intent
	q.Checkpoint = "persisted-checkpoint"
	f, err = l.pick(q)
	if err != nil || f.Auth != "large" || f.Generation != 2 {
		t.Fatal(f, err)
	}
	if f2, err := l.pick(q); err != nil || f2 != f {
		t.Fatal("retry advanced binding twice", f2, err)
	}
}
func TestAutoOffAndOpaqueContext(t *testing.T) {
	l := setup(t)
	q := claim("a")
	_, _ = l.pick(q)
	l.complete(q)
	l.observe(t, "small", 9, 100, 118000)
	s := l.snapshot(t, "a")
	q.Generation = 1
	q.Intent = s.Intent
	q.Fresh = false
	q.Auto = false
	q.Request = "off"
	q.Portable = false
	if _, err := l.pick(q); err == nil {
		t.Fatal("opaque context admitted")
	}
	q.Portable = true
	f, err := l.pick(q)
	if err != nil || f.Auth != "large" {
		t.Fatal(f, err)
	}
}
func TestResetDoesNotMoveAnOngoingChatOrReuseCheckpoint(t *testing.T) {
	l := setup(t)
	q := claim("a")
	_, _ = l.pick(q)
	l.complete(q)
	l.observe(t, "small", 9, 100, 118000)
	s := l.snapshot(t, "a")
	q.Generation = 1
	q.Intent = s.Intent
	q.Checkpoint = "cp1"
	q.Fresh = false
	q.Request = "handoff"
	_, err := l.pick(q)
	if err != nil {
		t.Fatal(err)
	}
	l.complete(q)
	l.now = 118001
	l.observe(t, "small", 100, 100, 136000)
	l.observe(t, "large", 100, 100, 136000)
	q.Generation = 2
	q.Intent = ""
	q.Request = "ongoing"
	f, err := l.pick(q)
	if err != nil || f.Auth != "large" {
		t.Fatal("old checkpoint reused", f, err)
	}
	l.complete(q)
	newChat := claim("new")
	f, err = l.pick(newChat)
	if err != nil || f.Auth != "small" {
		t.Fatal(f, err)
	}
	l.complete(newChat)
	q.Request = "new-checkpoint"
	q.Checkpoint = "cp2"
	f, err = l.pick(q)
	if err != nil || f.Auth != "small" {
		t.Fatal(f, err)
	}
}
func TestWeeklyHoldSurvivesFiveHourResetAndRestart(t *testing.T) {
	l := setup(t)
	q := claim("a")
	_, _ = l.pick(q)
	l.complete(q)
	l.observe(t, "small", 9, 4, 118000)
	_ = l.e.Close()
	var err error
	l.e, err = Open(l.cfg, func() time.Time { return time.Unix(l.now, 0) })
	if err != nil {
		t.Fatal(err)
	}
	l.now = 118001
	l.observe(t, "small-copy", 100, 4, 136000)
	l.observe(t, "large", 100, 100, 136000)
	f, err := l.pick(claim("new"))
	if err != nil || f.Auth != "large" {
		t.Fatal(f, err)
	}
	if s := l.snapshot(t, "a"); s.Intent == "" || len(s.Events) != 1 {
		t.Fatal(s)
	}
}
func TestNoCapacityAndPersistenceFailureRefuse(t *testing.T) {
	l := setup(t)
	l.observe(t, "small", 9, 100, 118000)
	l.observe(t, "large", 9, 100, 118000)
	if _, err := l.pick(claim("a")); err == nil {
		t.Fatal("no-capacity delegated")
	}
	if _, err := l.e.Snapshot("wrong-capability", 0, "model"); err == nil {
		t.Fatal("capability isolation failed")
	}
	_ = l.e.db.Close()
	if err := l.e.Observe("small", nil); err == nil {
		t.Fatal("store failure ignored")
	}
	if _, err := l.pick(claim("a")); err == nil {
		t.Fatal("uncommitted routing admitted")
	}
}
func TestHandoffWaitsForInflightAndCompactionIsBounded(t *testing.T) {
	l := setup(t)
	q := claim("a")
	_, _ = l.pick(q)
	l.observe(t, "small", 9, 100, 118000)
	s := l.snapshot(t, "a")
	next := q
	next.Request = "next"
	next.Generation = 1
	next.Fresh = false
	next.Intent = s.Intent
	next.Checkpoint = "cp"
	if _, err := l.pick(next); err == nil || !strings.Contains(err.Error(), "quota_request_active") {
		t.Fatal(err)
	}
	next.Kind = "compaction"
	next.Checkpoint = ""
	next.Request = "summary"
	f, err := l.pick(next)
	if err != nil || f.Auth != "small" {
		t.Fatal(f, err)
	}
	l.complete(next)
	for i := 0; i < 3; i++ {
		_, err = l.pick(next)
		if err != nil {
			t.Fatal(err)
		}
		l.complete(next)
	}
	if _, err = l.pick(next); err == nil {
		t.Fatal("unbounded reserve compactions")
	}
	l.complete(q)
}
func TestCallerAndCapabilityIsolation(t *testing.T) {
	l := setup(t)
	q := claim("a")
	_, _ = l.pick(q)
	l.complete(q)
	q.Generation = 1
	q.Request = "tamper"
	q.Capability = Digest("other")
	if _, err := l.pick(q); err == nil {
		t.Fatal("session capability was replaced")
	}
	q.Generation = 0
	if _, err := l.e.Pick("other-caller", "claude", "claude-sonnet", []string{"large"}, &q); err != nil {
		t.Fatal("independent authenticated caller blocked", err)
	}
}

func (l *lab) complete(q Claim) {
	l.e.Complete(Digest("caller\x00"+q.Installation+"\x00"+q.Session+"\x00claude\x00"+q.Route) + ":" + q.Request)
}

func TestHeldOnlyCredentialResumesAfterConfirmedReset(t *testing.T) {
	l := setup(t)
	q := claim("a")
	_, err := l.e.Pick("caller", "claude", "claude-sonnet", []string{"small"}, &q)
	if err != nil {
		t.Fatal(err)
	}
	l.complete(q)
	l.observe(t, "small", 9, 4, 118000)
	l.observe(t, "large", 0, 0, 118000)
	if s := l.snapshot(t, "a"); s.Available || s.Intent == "" {
		t.Fatal(s)
	}
	l.now = 118001
	l.observe(t, "small", 100, 4, 136000)
	if s := l.snapshot(t, "a"); s.Intent == "" {
		t.Fatal("five-hour reset erased weekly hold", s)
	}
	l.now = 704801
	err = l.e.Observe("small", []quota.Window{
		{Scope: "all", Seconds: 18000, Remaining: 100, Reset: 722800, Observed: l.now},
		{Scope: "all", Seconds: 604800, Remaining: 100, Reset: 1309600, Observed: l.now},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := l.snapshot(t, "a")
	if s.Intent != "" || s.Generation != 1 || s.Events[len(s.Events)-1].Kind != "recovered" {
		t.Fatal("confirmed reset did not resume original binding", s)
	}
	q.Generation, q.Fresh, q.Request = 1, false, "resume"
	f, err := l.e.Pick("caller", "claude", "claude-sonnet", []string{"small"}, &q)
	if err != nil || f.Auth != "small" || f.Generation != 1 {
		t.Fatal(f, err)
	}
}

func TestAuxiliaryBindingDoesNotEnrollOpaquePrimary(t *testing.T) {
	l := setup(t)
	q := claim("a")
	q.Kind, q.Fresh = "title", false
	if _, err := l.pick(q); err != nil {
		t.Fatal(err)
	}
	l.complete(q)
	if l.snapshot(t, "a").PrimaryUsed {
		t.Fatal("title registered primary enrollment")
	}
	q.Kind, q.Portable, q.Generation, q.Request = "primary", false, 1, "primary"
	if _, err := l.pick(q); err == nil || !strings.Contains(err.Error(), "quota_checkpoint_required") {
		t.Fatal("opaque enrollment accepted", err)
	}
	q.Portable, q.Checkpoint = true, "local-checkpoint"
	if _, err := l.pick(q); err != nil {
		t.Fatal(err)
	}
	if !l.snapshot(t, "a").PrimaryUsed {
		t.Fatal("portable enrollment was not persisted")
	}
}

func TestRecoveryCompactionsMayShareReplacementAccount(t *testing.T) {
	l := setup(t)
	for _, session := range []string{"a", "b", "c"} {
		q := claim(session)
		if _, err := l.pick(q); err != nil {
			t.Fatal(err)
		}
		l.complete(q)
	}
	l.observe(t, "small", 0, 100, 118000)
	for _, session := range []string{"a", "b", "c"} {
		q := claim(session)
		q.Kind, q.Generation, q.Request = "compaction", 1, "summary"
		f, err := l.pick(q)
		if err != nil || f.Auth != "large" {
			t.Fatal(f, err)
		}
		if s := l.snapshot(t, session); s.Alias != "Small" || s.Generation != 1 || s.Intent == "" {
			t.Fatal("recovery summary changed the primary binding", s)
		}
	}
}

func TestStaleQuotaRecoveryResumesOriginalBinding(t *testing.T) {
	l := setup(t)
	q := claim("a")
	if _, err := l.e.Pick("caller", "claude", "claude-sonnet", []string{"small"}, &q); err != nil {
		t.Fatal(err)
	}
	l.complete(q)
	l.now += 91
	q.Generation, q.Fresh, q.Request = 1, false, "stale"
	if _, err := l.e.Pick("caller", "claude", "claude-sonnet", []string{"small"}, &q); err == nil {
		t.Fatal("stale quota was admitted")
	}
	if l.snapshot(t, "a").Intent == "" {
		t.Fatal("stale binding not paused")
	}
	if err := l.e.Observe("small", nil); err != nil {
		t.Fatal(err)
	}
	if l.snapshot(t, "a").Intent == "" {
		t.Fatal("empty sample fabricated recovery")
	}
	l.observe(t, "small", 90, 90, 118000)
	if l.snapshot(t, "a").Intent != "" {
		t.Fatal("fresh evidence did not release temporary pause")
	}
	q.Request = "fresh-evidence"
	f, err := l.e.Pick("caller", "claude", "claude-sonnet", []string{"small"}, &q)
	if err != nil || f.Auth != "small" || f.Generation != 1 {
		t.Fatal(f, err)
	}
}
