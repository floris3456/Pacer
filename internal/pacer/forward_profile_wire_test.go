package pacer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Check the actual Plugin.Handle wire, including raw [] versus omitted, every
// retained profile/config field, quoted revisions, and the durable metadata digest.
func forwardProfileWireRead(t *testing.T, p *Plugin, revision int64) historyreports.ForwardProfileConfig {
	t.Helper()
	var want historyreports.ForwardProfileConfig
	var digest string
	if err := p.WithForwardStore(context.Background(), func(ctx context.Context, s *historyreports.ForwardStore) error {
		var err error
		want, digest, err = s.ProfilesAt(ctx, revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := forwardWiringCall(t, p, pluginapi.ManagementRequest{Method: "GET", Path: forwardHTTPRoot + "profiles", Query: url.Values{"revision": {strconv.FormatInt(revision, 10)}}}, 200)
	var response struct {
		Revision string          `json:"revision"`
		Digest   string          `json:"digest"`
		Config   json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(r.Body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Revision != strconv.FormatInt(revision, 10) || response.Digest != digest {
		t.Fatal("branch revision or stored metadata digest changed")
	}
	var rawConfig struct {
		Revision string            `json:"revision"`
		Profiles []json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(response.Config, &rawConfig); err != nil {
		t.Fatal(err)
	}
	if rawConfig.Revision != strconv.FormatInt(want.Revision, 10) || len(rawConfig.Profiles) != len(want.Profiles) {
		t.Fatal("retained configuration revision or profile count changed")
	}
	for i, profile := range want.Profiles {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawConfig.Profiles[i], &fields); err != nil {
			t.Fatal(err)
		}
		memberships, present := fields["memberships"]
		switch {
		case profile.Memberships == nil:
			if present {
				t.Fatalf("legacy nil memberships became explicit: revision=%d profile=%s wire=%s", revision, profile.ID, memberships)
			}
		case len(profile.Memberships) == 0:
			if !present || string(memberships) != "[]" {
				t.Fatalf("explicit empty memberships must be literal []: revision=%d profile=%s present=%v wire=%s", revision, profile.ID, present, memberships)
			}
		default:
			if !present {
				t.Fatalf("nonempty memberships omitted: %s", profile.ID)
			}
		}
	}
	var decoded struct {
		historyreports.ForwardProfileConfig
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(response.Config, &decoded); err != nil {
		t.Fatal(err)
	}
	// The string override is asserted above; restore only its numeric Go field
	// for the full typed comparison (including legacy accounts/bindings/plans).
	decoded.ForwardProfileConfig.Revision = want.Revision
	if !reflect.DeepEqual(decoded.ForwardProfileConfig, want) {
		t.Fatalf("profile/config field changed: got=%+v want=%+v", decoded.ForwardProfileConfig, want)
	}
	if err := p.WithForwardStore(context.Background(), func(ctx context.Context, s *historyreports.ForwardStore) error {
		after, afterDigest, err := s.ProfilesAt(ctx, revision)
		if err == nil && (afterDigest != digest || !reflect.DeepEqual(after, want)) {
			t.Error("readback changed durable metadata")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return decoded.ForwardProfileConfig
}

func TestForwardProfileWireHandlePatchEmptyArchiveAndPinnedReplay(t *testing.T) {
	f := preparedRuntime(t)
	p := forwardWiringOpen(t, f, nil, nil)
	forwardWiringRegister(t, p, hookRaw(t, forwardWiringRegistration()))
	initial := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	forwardProfileWireRead(t, p, 0)
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "profile-wire-logical", c.ID)
	f.h.clock.Add(int64(3 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "profile-wire-logical", "profile-wire-physical", hookEpoch.Add(time.Nanosecond)))
	head, err := p.measurement.ReportFeedHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ready := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Projected == head.Through && v.Latest != initial.Latest
	})
	pinResponse := forwardWiringCall(t, p, forwardWiringRequest(t, "pin", map[string]any{"generation": ready.Latest}), 200)
	var oldPin historyreports.ForwardPin
	if err = json.Unmarshal(pinResponse.Body, &oldPin); err != nil {
		t.Fatal(err)
	}
	exportRequest := forwardWiringRequest(t, "export-json", map[string]any{"pin": oldPin})
	saved := forwardWiringCall(t, p, exportRequest, 200)
	account := historyreports.ForwardAccountRef(f.source, c.Provider, c.Account)
	end := int64(math.MaxInt64) / int64(time.Millisecond)
	profile := map[string]any{"id": "wire-profile", "name": "<wire & dated>", "provider": c.Provider, "selection": true, "memberships": []map[string]string{{"account": account, "start_ms": "1", "end_ms": strconv.FormatInt(end, 10)}}}
	patch := func(expected int64, body map[string]any) {
		t.Helper()
		body["expected_revision"] = strconv.FormatInt(expected, 10)
		req := forwardWiringRequest(t, "profiles", body)
		req.Method = "PATCH"
		r := forwardWiringCall(t, p, req, 202)
		var result struct {
			Revision    string `json:"revision"`
			Publication string `json:"publication"`
		}
		if err := json.Unmarshal(r.Body, &result); err != nil || result.Revision != strconv.FormatInt(expected+1, 10) || result.Publication != "pending" {
			t.Fatalf("CAS response %s: %v", r.Body, err)
		}
	}
	waitNext := func(previous string) string {
		t.Helper()
		return forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active && v.Latest != previous }).Latest
	}
	patch(0, map[string]any{"profile": profile})
	one := forwardProfileWireRead(t, p, 1)
	if got := one.Profiles[0].Memberships; !reflect.DeepEqual(got, []historyreports.ForwardMembership{{Account: account, Start: 1, End: end}}) {
		t.Fatal("membership integer millisecond bounds changed", got)
	}
	generation := waitNext(ready.Latest)
	profile["memberships"] = []map[string]string{}
	patch(1, map[string]any{"profile": profile})
	two := forwardProfileWireRead(t, p, 2)
	if two.Profiles[0].Archived {
		t.Fatal("clear archived profile")
	}
	generation = waitNext(generation)
	patch(2, map[string]any{"archive_id": "wire-profile"})
	three := forwardProfileWireRead(t, p, 3)
	if !three.Profiles[0].Archived {
		t.Fatal("archived flag missing")
	}
	waitNext(generation)
	// Revision 1 remains nonempty; revision 2 remains explicitly empty/unarchived.
	if !reflect.DeepEqual(forwardProfileWireRead(t, p, 1), one) || !reflect.DeepEqual(forwardProfileWireRead(t, p, 2), two) {
		t.Fatal("readback refreshed an immutable revision")
	}
	replay := forwardWiringCall(t, p, exportRequest, 200)
	if !bytes.Equal(replay.Body, saved.Body) {
		t.Fatal("profile read projection changed saved-pin/global JSON bytes")
	}
}

func TestForwardProfileWireLegacyNilAccountsAndAllConfigFields(t *testing.T) {
	f := preparedRuntime(t)
	// Offline fixture only: preserve legacy ownership and an original wide revision.
	var seed historyreports.ForwardProfileConfig
	if err := json.Unmarshal([]byte(`{
		"revision":9007199254740993,
		"profiles":[
			{"id":"legacy-nil","name":"<legacy & unknown>","provider":"claude","archived":false,"selection":true,"accounts":["unknown-legacy-account"],"active_account":"unknown-legacy-account","created_ms":1790000000123},
			{"id":"seed-empty","name":"Explicitly empty","provider":"claude","archived":true,"memberships":[],"active_account":"","created_ms":1790000000456}
		],
		"bindings":[{"id":"retained-binding","profile":"legacy-nil","account":"unknown-legacy-account","start_ms":1,"end_ms":1790000000789}],
		"plans":[{"account":"unknown-legacy-account","plan":"retained-plan","start_ms":2,"end_ms":1790000000999}]
	}`), &seed); err != nil {
		t.Fatal(err)
	}
	s, err := historyreports.OpenPreparedForward(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	branch, seedErr := s.SeedForwardProfiles(context.Background(), seed)
	closeErr := s.Close()
	if seedErr != nil || closeErr != nil || branch != 1 {
		t.Fatal(branch, seedErr, closeErr)
	}
	p := forwardWiringOpen(t, f, func(o *forwardOptions) { o.beforeTurn = func(ctx context.Context) { <-ctx.Done() } }, nil)
	forwardWiringRegister(t, p, hookRaw(t, forwardWiringRegistration()))
	got := forwardProfileWireRead(t, p, branch)
	if !reflect.DeepEqual(got, seed) {
		t.Fatal("legacy fields or original quoted revision were not retained", fmt.Sprint(got.Revision))
	}
}
