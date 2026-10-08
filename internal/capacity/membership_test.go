package capacity

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var epoch = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func tuple(id, ref string, binding uint64, state string) Tuple {
	return Tuple{Ref: TupleRef(id, "index-"+id), AccountRef: ref, BindingSequence: binding, State: state}
}

func checkpoint(incarnation string, seq uint64, tuples ...Tuple) Checkpoint {
	return Checkpoint{Coverage: "known", Incarnation: incarnation, Sequence: seq, At: epoch.Add(time.Duration(seq) * time.Minute), Tuples: tuples}
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

func TestMembershipAliasTuplesBothCounted(t *testing.T) {
	a, b := tuple("a", "ref-A", 3, "active"), tuple("b", "ref-A", 4, "active")
	other := tuple("c", "ref-B", 25, "active") // another account's change is not A's
	m := JudgeMembership("ref-A", checkpoint("inv", 10, a, b, tuple("c", "ref-B", 2, "active")), checkpoint("inv", 30, a, b, other))
	if !m.Holds || len(m.Reasons) != 0 {
		t.Fatal("alias membership refused", m)
	}
	want := []string{a.Ref, b.Ref}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	if !reflect.DeepEqual(m.Tuples, want) || !reflect.DeepEqual(m.Active, want) || m.AccountRef != "ref-A" {
		t.Fatal("alias tuple not counted", m)
	}
}

func TestMembershipRotationInsideSplitsInterval(t *testing.T) {
	c0 := checkpoint("inv", 10, tuple("a", "ref-A", 5, "active"))
	c1 := checkpoint("inv", 20, tuple("a", "ref-A", 15, "active")) // rotated at 15
	c2 := checkpoint("inv", 30, tuple("a", "ref-A", 15, "active"))
	if m := JudgeMembership("ref-A", c0, c1); m.Holds || !hasReason(m.Reasons, "binding_changed_inside") {
		t.Fatal("rotation inside kept membership", m)
	}
	if m := JudgeMembership("ref-A", c0, c2); m.Holds {
		t.Fatal("interval spanning rotation kept membership", m)
	}
	if m := JudgeMembership("ref-A", c1, c2); !m.Holds {
		t.Fatal("part after rotation not judged separately", m)
	}
	// An account_ref change (alias moved) is also a binding change.
	moved := checkpoint("inv", 20, tuple("a", "ref-A", 5, "active"), tuple("b", "ref-A", 18, "active"))
	if m := JudgeMembership("ref-A", checkpoint("inv", 10, tuple("a", "ref-A", 5, "active"), tuple("b", "ref-B", 4, "active")), moved); m.Holds {
		t.Fatal("account_ref change inside kept membership", m)
	}
}

func TestMembershipRemovalInsideSplitsInterval(t *testing.T) {
	a := tuple("a", "ref-A", 5, "active")
	c0 := checkpoint("inv", 10, a, tuple("b", "ref-A", 6, "active"))
	c1 := checkpoint("inv", 20, a, tuple("b", "ref-A", 15, "removed"))
	c2 := checkpoint("inv", 30, a, tuple("b", "ref-A", 15, "removed"))
	if m := JudgeMembership("ref-A", c0, c1); m.Holds || !hasReason(m.Reasons, "binding_changed_inside") {
		t.Fatal("removal inside kept membership", m)
	}
	m := JudgeMembership("ref-A", c1, c2)
	if !m.Holds || len(m.Tuples) != 2 || !reflect.DeepEqual(m.Active, []string{a.Ref}) {
		t.Fatal("part after removal not judged separately", m)
	}
	// A tuple that silently disappears from the listing is a change too.
	if m := JudgeMembership("ref-A", c0, checkpoint("inv", 20, a)); m.Holds {
		t.Fatal("vanished tuple kept membership", m)
	}
}

func TestMembershipUnknownAccountRefTupleIsIncomplete(t *testing.T) {
	a := tuple("a", "ref-A", 5, "active")
	unknown := tuple("x", "", 2, "active")
	for _, pair := range [][2]Checkpoint{
		{checkpoint("inv", 10, a, unknown), checkpoint("inv", 20, a)},
		{checkpoint("inv", 10, a), checkpoint("inv", 20, a, tuple("x", "", 2, "removed"))},
	} {
		if m := JudgeMembership("ref-A", pair[0], pair[1]); m.Holds || !hasReason(m.Reasons, "unknown_account_ref_tuple") {
			t.Fatal("unknown account_ref tuple accepted", m)
		}
	}
	if m := JudgeMembership("", checkpoint("inv", 10, a), checkpoint("inv", 20, a)); m.Holds {
		t.Fatal("empty account ref judged complete", m)
	}
}

func TestMembershipIncarnationChangeIsIncomplete(t *testing.T) {
	a := tuple("a", "ref-A", 5, "active")
	if m := JudgeMembership("ref-A", checkpoint("inv-1", 10, a), checkpoint("inv-2", 20, a)); m.Holds || !hasReason(m.Reasons, "inventory_incarnation_changed") {
		t.Fatal("restart between checkpoints kept membership", m)
	}
	if m := JudgeMembership("ref-A", checkpoint("inv", 20, a), checkpoint("inv", 10, a)); m.Holds {
		t.Fatal("reversed checkpoints accepted", m)
	}
}

func TestMembershipOverflowAndOldHostAreUnknown(t *testing.T) {
	big := pluginapi.HostAuthInventoryCheckpointResponse{Version: 1, Provider: "claude", Coverage: "known", ManagerIncarnationID: "inv", Sequence: 2000, At: epoch}
	for i := 0; i <= MaxInventoryTuples; i++ {
		big.Tuples = append(big.Tuples, pluginapi.HostAuthInventoryTuple{AuthID: fmt.Sprintf("auth-%d", i), AuthIndex: fmt.Sprintf("i%d", i), AccountRef: "ref-A", BindingSequence: 1, State: "active"})
	}
	raw, _ := json.Marshal(big)
	if c := ParseInventory(raw, nil).Checkpoint; c.Coverage != "unknown" || c.Reason != "inventory_overflow" || len(c.Tuples) != 0 {
		t.Fatal("overflow accepted", c.Coverage, c.Reason)
	}
	host := pluginapi.HostAuthInventoryCheckpointResponse{Version: 1, Provider: "claude", Coverage: "unknown", Reason: "overflow", ManagerIncarnationID: "inv", Sequence: 9, At: epoch}
	raw, _ = json.Marshal(host)
	c := ParseInventory(raw, nil).Checkpoint
	if c.Coverage != "unknown" {
		t.Fatal("host overflow became known", c)
	}
	a := tuple("a", "ref-A", 5, "active")
	if m := JudgeMembership("ref-A", c, checkpoint("inv", 20, a)); m.Holds || !hasReason(m.Reasons, "inventory_unknown") {
		t.Fatal("unknown inventory judged", m)
	}
	for name, in := range map[string]struct {
		raw []byte
		err error
	}{
		"unknown-method": {nil, errors.New("unsupported host method host.auth.inventory_checkpoint")},
		"empty-object":   {[]byte(`{}`), nil},
		"malformed":      {[]byte(`{`), nil},
		"bad-version":    {[]byte(`{"version":2,"provider":"claude","coverage":"known","manager_incarnation_id":"inv","sequence":1,"at":"2026-10-08T12:00:00Z","tuples":[]}`), nil},
		"bad-state":      {[]byte(`{"version":1,"provider":"claude","coverage":"known","manager_incarnation_id":"inv","sequence":3,"at":"2026-10-08T12:00:00Z","tuples":[{"auth_id":"a","auth_index":"i","binding_sequence":1,"state":"paused"}]}`), nil},
		"future-binding": {[]byte(`{"version":1,"provider":"claude","coverage":"known","manager_incarnation_id":"inv","sequence":3,"at":"2026-10-08T12:00:00Z","tuples":[{"auth_id":"a","auth_index":"i","binding_sequence":4,"state":"active"}]}`), nil},
		"duplicate":      {[]byte(`{"version":1,"provider":"claude","coverage":"known","manager_incarnation_id":"inv","sequence":3,"at":"2026-10-08T12:00:00Z","tuples":[{"auth_id":"a","auth_index":"i","binding_sequence":1,"state":"active"},{"auth_id":"a","auth_index":"i","binding_sequence":1,"state":"active"}]}`), nil},
		"other-provider": {[]byte(`{"version":1,"provider":"codex","coverage":"known","manager_incarnation_id":"inv","sequence":3,"at":"2026-10-08T12:00:00Z","tuples":[]}`), nil},
	} {
		if c := ParseInventory(in.raw, in.err).Checkpoint; c.Coverage == "known" || len(c.Tuples) != 0 {
			t.Fatal(name, "accepted as known inventory", c)
		}
	}
	good := []byte(`{"version":1,"provider":"claude","coverage":"known","manager_incarnation_id":"inv","sequence":3,"at":"2026-10-08T12:00:00Z","tuples":[{"auth_id":"user@example.com.json","auth_index":"i","account_ref":"ref-A","binding_sequence":2,"state":"active"}]}`)
	inv := ParseInventory(good, nil)
	if inv.Checkpoint.Coverage != "known" || len(inv.Checkpoint.Tuples) != 1 || inv.Checkpoint.Tuples[0].Ref != TupleRef("user@example.com.json", "i") || inv.Scopes[inv.Checkpoint.Tuples[0].Ref].AuthID != "user@example.com.json" {
		t.Fatal("valid inventory refused", inv)
	}
	stored, _ := json.Marshal(inv.Checkpoint)
	if strings.Contains(string(stored), "user@example.com") {
		t.Fatal("raw auth id would be stored")
	}
}

func TestMembershipPointDiscoveryAloneStaysIncomplete(t *testing.T) {
	// Today's discovery yields no inventory checkpoint at all.
	if m := JudgeMembership("ref-A", Checkpoint{}, Checkpoint{}); m.Holds || !hasReason(m.Reasons, "inventory_unknown") {
		t.Fatal("point discovery promoted to interval membership", m)
	}
	a := tuple("a", "ref-A", 5, "active")
	if m := JudgeMembership("ref-A", checkpoint("inv", 10, a), Checkpoint{}); m.Holds {
		t.Fatal("one-sided inventory accepted", m)
	}
}

func TestResolveAccountRefFromPacerTuples(t *testing.T) {
	a, b := tuple("a", "ref-A", 5, "active"), tuple("b", "ref-B", 5, "active")
	c := checkpoint("inv", 10, a, b, tuple("x", "", 1, "active"))
	if ref, reason := ResolveAccountRef(c, []string{a.Ref}); ref != "ref-A" || reason != "" {
		t.Fatal(ref, reason)
	}
	if _, reason := ResolveAccountRef(c, []string{a.Ref, b.Ref}); reason != "account_ref_ambiguous" {
		t.Fatal("two billing accounts merged", reason)
	}
	if _, reason := ResolveAccountRef(c, []string{TupleRef("x", "index-x")}); reason != "account_ref_unknown" {
		t.Fatal("unknown ref resolved", reason)
	}
	if _, reason := ResolveAccountRef(c, []string{TupleRef("missing", "m")}); reason != "pacer_tuple_not_in_inventory" {
		t.Fatal("missing tuple resolved", reason)
	}
	if _, reason := ResolveAccountRef(Checkpoint{Coverage: "unknown"}, []string{a.Ref}); reason != "inventory_unknown" {
		t.Fatal("unknown inventory resolved", reason)
	}
}
