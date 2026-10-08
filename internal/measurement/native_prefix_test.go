package measurement

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// prefixHash returns a 64-character lowercase hex tag filled with one hex digit.
func prefixHash(c byte) string { return strings.Repeat(string(c), 64) }

func endpointWire(ordinal int, section, ttl, tag string) map[string]any {
	return map[string]any{"ordinal": ordinal, "section": section, "ttl": ttl, "tag": tag}
}

// prefixWire is a valid single-endpoint 1h snapshot in generic form, so each
// invalid case can change exactly one member.
func prefixWire() map[string]any {
	return map[string]any{
		"version":      1,
		"key_id":       "0123456789abcdef",
		"settings_tag": prefixHash('a'),
		"endpoints":    []map[string]any{endpointWire(0, "tools", "1h", prefixHash('b'))},
		"automatic":    false,
		"shape":        "text_single_pass",
	}
}

// nativeWithPrefix encodes the standard native fixture and injects prefix as
// CachePolicy.prefix. A nil prefix omits the member entirely.
func nativeWithPrefix(t *testing.T, prefix any) []byte {
	t.Helper()
	n, _ := nativeFixture()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(encodeNative(t, n), &obj); err != nil {
		t.Fatal(err)
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(obj["CachePolicy"], &policy); err != nil {
		t.Fatal(err)
	}
	if prefix != nil {
		policy["prefix"] = mustMarshal(t, prefix)
	}
	obj["CachePolicy"] = mustMarshal(t, policy)
	return mustMarshal(t, obj)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parseNative(t *testing.T, raw []byte) Record {
	t.Helper()
	_, c := nativeFixture()
	r, err := FromNative(raw, c, "host-installation")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// withoutPrefixDiagnostic returns a copy of r with PrefixSnapshot cleared and
// prefix_snapshot_invalid removed, so an invalid-snapshot record can be compared
// with the same record parsed without the member.
func withoutPrefixDiagnostic(r Record) Record {
	nm := *r.Native
	nm.PrefixSnapshot = nil
	var kept []string
	for _, d := range nm.Diagnostics {
		if d != "prefix_snapshot_invalid" {
			kept = append(kept, d)
		}
	}
	nm.Diagnostics = kept
	r.Native = &nm
	return r
}

func TestNativePrefixSnapshotValidRoundTrip(t *testing.T) {
	plain := parseNative(t, nativeWithPrefix(t, nil))
	key := "0123456789abcdef"

	single1h := prefixWire()
	three5m := prefixWire()
	three5m["endpoints"] = []map[string]any{
		endpointWire(0, "tools", "5m", prefixHash('c')),
		endpointWire(1, "system", "5m", prefixHash('d')),
		endpointWire(2, "messages", "5m", prefixHash('e')),
	}
	unsupported := prefixWire()
	unsupported["automatic"] = true
	unsupported["shape"] = "unsupported"
	unsupported["reasons"] = []string{"automatic_marker", "typed_tool"}
	empty := prefixWire()
	empty["endpoints"] = []map[string]any{}
	empty["shape"] = "unsupported"
	empty["reasons"] = []string{"no_endpoint"}

	cases := []struct {
		name string
		wire map[string]any
		want *PrefixSnapshot
	}{
		{"single 1h endpoint", single1h, &PrefixSnapshot{Version: 1, KeyID: key, SettingsTag: prefixHash('a'), Shape: "text_single_pass",
			Endpoints: []PrefixEndpoint{{Ordinal: 0, Section: "tools", TTL: "1h", Tag: prefixHash('b')}}}},
		{"three 5m endpoints in section order", three5m, &PrefixSnapshot{Version: 1, KeyID: key, SettingsTag: prefixHash('a'), Shape: "text_single_pass",
			Endpoints: []PrefixEndpoint{
				{Ordinal: 0, Section: "tools", TTL: "5m", Tag: prefixHash('c')},
				{Ordinal: 1, Section: "system", TTL: "5m", Tag: prefixHash('d')},
				{Ordinal: 2, Section: "messages", TTL: "5m", Tag: prefixHash('e')},
			}}},
		{"unsupported with sorted reasons", unsupported, &PrefixSnapshot{Version: 1, KeyID: key, SettingsTag: prefixHash('a'), Automatic: true, Shape: "unsupported",
			Reasons:   []string{"automatic_marker", "typed_tool"},
			Endpoints: []PrefixEndpoint{{Ordinal: 0, Section: "tools", TTL: "1h", Tag: prefixHash('b')}}}},
		{"empty endpoints with no_endpoint", empty, &PrefixSnapshot{Version: 1, KeyID: key, SettingsTag: prefixHash('a'), Shape: "unsupported",
			Reasons: []string{"no_endpoint"}, Endpoints: []PrefixEndpoint{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := parseNative(t, nativeWithPrefix(t, tc.wire))
			if !reflect.DeepEqual(r.Native.PrefixSnapshot, tc.want) {
				t.Fatalf("PrefixSnapshot = %+v, want %+v", r.Native.PrefixSnapshot, tc.want)
			}
			if has(r.Native.Diagnostics, "prefix_snapshot_invalid") {
				t.Fatal("valid snapshot produced prefix_snapshot_invalid")
			}
			if !bytes.Equal(mustMarshal(t, withoutPrefixDiagnostic(r)), mustMarshal(t, plain)) {
				t.Fatal("valid prefix snapshot altered the rest of the record")
			}
		})
	}
}

func TestNativePrefixSnapshotEachRuleRejectsOnlyTheSnapshot(t *testing.T) {
	plain := parseNative(t, nativeWithPrefix(t, nil))
	plainRaw := mustMarshal(t, plain)

	five := prefixWire()
	five["endpoints"] = []map[string]any{
		endpointWire(0, "tools", "1h", prefixHash('1')), endpointWire(1, "tools", "1h", prefixHash('2')),
		endpointWire(2, "tools", "1h", prefixHash('3')), endpointWire(3, "tools", "1h", prefixHash('4')),
		endpointWire(4, "tools", "1h", prefixHash('5')),
	}
	ordinalGap := prefixWire()
	ordinalGap["endpoints"] = []map[string]any{endpointWire(0, "tools", "1h", prefixHash('1')), endpointWire(2, "tools", "1h", prefixHash('2'))}
	sectionDown := prefixWire()
	sectionDown["endpoints"] = []map[string]any{endpointWire(0, "system", "1h", prefixHash('1')), endpointWire(1, "tools", "1h", prefixHash('2'))}
	unsortedReasons := prefixWire()
	unsortedReasons["automatic"], unsortedReasons["shape"] = true, "unsupported"
	unsortedReasons["reasons"] = []string{"typed_tool", "automatic_marker"}
	duplicateReasons := prefixWire()
	duplicateReasons["automatic"], duplicateReasons["shape"] = true, "unsupported"
	duplicateReasons["reasons"] = []string{"automatic_marker", "automatic_marker"}
	singlePassWithReason := prefixWire()
	singlePassWithReason["reasons"] = []string{"typed_tool"}
	unsupportedNoReason := prefixWire()
	unsupportedNoReason["shape"] = "unsupported"
	automaticWithoutMarker := prefixWire()
	automaticWithoutMarker["automatic"], automaticWithoutMarker["shape"] = true, "unsupported"
	automaticWithoutMarker["reasons"] = []string{"typed_tool"}
	markerWithoutAutomatic := prefixWire()
	markerWithoutAutomatic["shape"] = "unsupported"
	markerWithoutAutomatic["reasons"] = []string{"automatic_marker"}
	zeroEndpointsNoReason := prefixWire()
	zeroEndpointsNoReason["endpoints"] = []map[string]any{}
	noEndpointWithEndpoint := prefixWire()
	noEndpointWithEndpoint["shape"] = "unsupported"
	noEndpointWithEndpoint["reasons"] = []string{"no_endpoint"}

	cases := []struct {
		name string
		wire any
	}{
		{"version not 1", func() map[string]any { m := prefixWire(); m["version"] = 2; return m }()},
		{"key_id uppercase hex", func() map[string]any { m := prefixWire(); m["key_id"] = "0123456789ABCDEF"; return m }()},
		{"settings_tag not 64 hex", func() map[string]any { m := prefixWire(); m["settings_tag"] = strings.Repeat("a", 63); return m }()},
		{"endpoint tag not hex", func() map[string]any {
			m := prefixWire()
			m["endpoints"] = []map[string]any{endpointWire(0, "tools", "1h", strings.Repeat("g", 64))}
			return m
		}()},
		{"endpoints absent", func() map[string]any { m := prefixWire(); m["endpoints"] = nil; return m }()},
		{"more than four endpoints", five},
		{"ordinal gap", ordinalGap},
		{"section order decreases", sectionDown},
		{"unknown section", func() map[string]any {
			m := prefixWire()
			m["endpoints"] = []map[string]any{endpointWire(0, "thinking", "1h", prefixHash('1'))}
			return m
		}()},
		{"ttl not 5m or 1h", func() map[string]any {
			m := prefixWire()
			m["endpoints"] = []map[string]any{endpointWire(0, "tools", "10m", prefixHash('1'))}
			return m
		}()},
		{"unknown shape", func() map[string]any { m := prefixWire(); m["shape"] = "maybe"; return m }()},
		{"unknown reason", func() map[string]any {
			m := prefixWire()
			m["automatic"], m["shape"], m["reasons"] = true, "unsupported", []string{"automatic_marker", "bogus"}
			return m
		}()},
		{"reasons unsorted", unsortedReasons},
		{"reasons duplicated", duplicateReasons},
		{"single pass with reasons", singlePassWithReason},
		{"unsupported without reasons", unsupportedNoReason},
		{"automatic without marker", automaticWithoutMarker},
		{"marker without automatic", markerWithoutAutomatic},
		{"zero endpoints without no_endpoint", zeroEndpointsNoReason},
		{"no_endpoint with endpoints", noEndpointWithEndpoint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := parseNative(t, nativeWithPrefix(t, tc.wire))
			if r.Native.PrefixSnapshot != nil {
				t.Fatalf("invalid snapshot retained: %+v", r.Native.PrefixSnapshot)
			}
			if !has(r.Native.Diagnostics, "prefix_snapshot_invalid") {
				t.Fatalf("missing prefix_snapshot_invalid in %v", r.Native.Diagnostics)
			}
			if !bytes.Equal(mustMarshal(t, withoutPrefixDiagnostic(r)), plainRaw) {
				t.Fatal("invalid snapshot altered the rest of the record")
			}
		})
	}
}

func TestNativePrefixAbsentRecordMarshalsIdentically(t *testing.T) {
	without := parseNative(t, nativeWithPrefix(t, nil))
	raw := mustMarshal(t, without)
	if bytes.Contains(raw, []byte(`"prefix_snapshot"`)) || bytes.Contains(raw, []byte(`"prefix"`)) {
		t.Fatalf("snapshot-less record gained a prefix key: %s", raw)
	}
	if has(without.Native.Diagnostics, "prefix_snapshot_invalid") {
		t.Fatal("absent prefix produced a diagnostic")
	}

	// Parsing the same input with a valid snapshot must differ only by the new member.
	with := parseNative(t, nativeWithPrefix(t, prefixWire()))
	if with.Native.PrefixSnapshot == nil {
		t.Fatal("valid snapshot was not retained")
	}
	nm := *with.Native
	nm.PrefixSnapshot = nil
	stripped := with
	stripped.Native = &nm
	if !bytes.Equal(mustMarshal(t, stripped), raw) {
		t.Fatalf("snapshot-less encoding changed:\nwith-snapshot-stripped %s\nwithout %s", mustMarshal(t, stripped), raw)
	}
}

func TestNativePrefixUnknownMembersIgnoredButCannotValidate(t *testing.T) {
	plain := parseNative(t, nativeWithPrefix(t, nil))

	future := prefixWire()
	future["future_member"] = map[string]any{"anything": []int{1, 2}}
	future["endpoints"] = []map[string]any{{
		"ordinal": 0, "section": "tools", "ttl": "1h", "tag": prefixHash('b'), "future_endpoint_member": true,
	}}
	r := parseNative(t, nativeWithPrefix(t, future))
	if r.Native.PrefixSnapshot == nil {
		t.Fatal("unknown extra members made a valid snapshot invalid")
	}
	if len(r.Native.PrefixSnapshot.Endpoints) != 1 || r.Native.PrefixSnapshot.Endpoints[0].Tag != prefixHash('b') {
		t.Fatalf("unexpected endpoints: %+v", r.Native.PrefixSnapshot.Endpoints)
	}

	badWithFuture := prefixWire()
	badWithFuture["version"] = 2
	badWithFuture["future_member"] = true
	r = parseNative(t, nativeWithPrefix(t, badWithFuture))
	if r.Native.PrefixSnapshot != nil || !has(r.Native.Diagnostics, "prefix_snapshot_invalid") {
		t.Fatalf("unknown member rescued an invalid snapshot: %+v", r.Native.PrefixSnapshot)
	}
	if !bytes.Equal(mustMarshal(t, withoutPrefixDiagnostic(r)), mustMarshal(t, plain)) {
		t.Fatal("unknown-member invalid snapshot altered the rest of the record")
	}
}
