package handoff

import (
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"strings"
	"testing"
)

func TestShadowUsesHostSelectionAndOnlyManualNotices(t *testing.T) {
	for i := 0; i < 3; i++ {
		f := fakeFleetFor(t, "shadow", false)
		q, cap := fakeClaim(i)
		q.Portable = false // Existing opaque chats do not have to compact to enroll.
		r := adaptive.Request{ID: q.Request, Model: "light", Input: 1000, Kind: "primary"}
		flight, err := f.e.Pick("caller", "claude", "light", []string{"small"}, q, r)
		if err != nil || flight.Auth != "small" {
			t.Fatalf("host choice lost: %+v %v", flight, err)
		}
		f.e.Complete(flight.Admission)
		f.accounts["small"].spent = 600000 * .81
		f.now += 10
		f.refresh(t)
		s, err := f.e.Snapshot(cap, 0, q.Route)
		if err != nil || s.Mode != "shadow" || s.Intent != "" {
			t.Fatalf("shadow triggered action: %+v %v", s, err)
		}
		if len(s.Events) == 0 || !strings.Contains(s.Events[0].Text, "next reminder") {
			t.Fatalf("early notice missing: %+v", s)
		}
		f.accounts["small"].spent = 600000 * .95
		f.now += 10
		f.refresh(t)
		s, _ = f.e.Snapshot(cap, s.Cursor, q.Route)
		if s.Intent != "" || len(s.Events) == 0 || !strings.Contains(s.Events[0].Text, "new chat now") {
			t.Fatalf("manual reminder missing: %+v", s)
		}
		q.Request += "again"
		r.ID = q.Request
		flight, err = f.e.Pick("caller", "claude", "light", []string{"small"}, q, r)
		if err != nil || flight.Auth != "small" {
			t.Fatalf("shadow refused depleted host choice: %+v %v", flight, err)
		}
		warnings := f.e.LegacyWarnings("small", "light", r)
		if len(warnings) == 0 || !strings.Contains(warnings[0].Text, "/compact") {
			t.Fatal(warnings)
		}
		f.e.Complete(flight.Admission)
		f.accounts["small"].spent = 0
	}
}
