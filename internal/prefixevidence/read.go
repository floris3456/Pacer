package prefixevidence

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// maxStateBytes matches the policy state cap enforced by policyevidence.
const maxStateBytes = 16 * 1024

// ownedField is the only state member this package adds or removes.
const ownedField = "prefix_cache_evidence"

// evidenceRow is the newest observation of one scope, as stored by resolve.
type evidenceRow struct {
	status, reason, relation, ttl string
	startedNS, untilNS, revision  int64
	entry, binding, snapshot      string
}

// observedShape and unverifiedShape are the only JSON shapes readback emits.
// Their field order is the order of the implementation spec (S5).
type observedShape struct {
	Version                 int       `json:"version"`
	Status                  string    `json:"status"`
	Basis                   string    `json:"basis"`
	Coverage                string    `json:"coverage"`
	Relation                string    `json:"relation"`
	ContextEpoch            string    `json:"context_epoch"`
	PolicyRevision          int64     `json:"policy_revision"`
	PrimarySnapshotRef      string    `json:"primary_snapshot_ref"`
	BindingRef              string    `json:"binding_ref"`
	EntryRef                string    `json:"entry_ref"`
	EffectiveTTL            string    `json:"effective_ttl"`
	UpstreamStartedAt       time.Time `json:"upstream_started_at"`
	ProtectionEstimateUntil time.Time `json:"protection_estimate_until"`
	RemainingPrefixes       string    `json:"remaining_prefixes"`
}

type unverifiedShape struct {
	Version           int      `json:"version"`
	Status            string   `json:"status"`
	Reasons           []string `json:"reasons"`
	RemainingPrefixes string   `json:"remaining_prefixes"`
}

// AugmentAuthorized is a read-only supplement for a fresh successful GET policy
// response AFTER the policy adapter has authorized the host-derived caller and
// private capability for that exact scope. callerDigest is the host SHA already
// used by the policy store, not an API key and not a digest to hash again.
//
// NEVER expose it as an HTTP endpoint or invoke it on client-provided state,
// failed RPCs, cached augmented responses, or POST command receipts. The parent
// must gate GET + StatusCode==200 + RPC success, and leave the canonical
// response unchanged if augmentation fails.
//
// Only prefix_cache_evidence is added, replaced or removed. Every other member
// keeps its original bytes. The method never writes to the store.
func (s *Store) AugmentAuthorized(ctx context.Context, callerDigest string, stateJSON []byte) ([]byte, error) {
	if !sha(callerDigest) || len(stateJSON) == 0 || len(stateJSON) > maxStateBytes {
		return nil, ErrInvalidState
	}
	members, err := object(stateJSON)
	if err != nil {
		return nil, ErrInvalidState
	}
	var protocol int
	var revision int64
	var installation, session, route string
	if !field(members, "protocol", &protocol) || protocol != 1 || !field(members, "revision", &revision) || revision < 1 || !field(members, "installation_id", &installation) || !field(members, "session_id", &session) || !field(members, "provider_route", &route) {
		return nil, ErrInvalidState
	}
	var active json.RawMessage
	if !field(members, "active", &active) {
		return nil, ErrInvalidState
	}
	activeMembers, err := object(active)
	if err != nil {
		return nil, ErrInvalidState
	}
	var epoch, selected string
	if !field(activeMembers, "context_epoch", &epoch) || !field(activeMembers, "ttl", &selected) {
		return nil, ErrInvalidState
	}
	key := scope{callerDigest, installation, session, route, epoch}
	if !key.valid() {
		return nil, ErrInvalidState
	}
	// Same gating as policyevidence: an unused or unknown-TTL active policy shows nothing.
	var used bool
	if !field(activeMembers, "used", &used) || !used || !validTTL(selected) {
		return augment(stateJSON, members, nil)
	}
	row, err := s.newest(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return augment(stateJSON, members, nil)
	}
	if err != nil {
		return nil, dbError(ctx, err)
	}
	// Newest-wins: if the newest observation postdates this state, it is not shown
	// at all, rather than falling back to an older one.
	if row.revision > revision {
		return augment(stateJSON, members, nil)
	}
	extra, err := renderEvidence(row, epoch, s.clock().UTC())
	if err != nil {
		return nil, err
	}
	return augment(stateJSON, members, extra)
}

// newest selects the newest observation for the exact scope by start, then attempt id.
func (s *Store) newest(ctx context.Context, key scope) (evidenceRow, error) {
	var r evidenceRow
	err := s.db.QueryRowContext(ctx, `SELECT obs_status, obs_reason, obs_relation, obs_ttl, started_ns, obs_until_ns, revision, obs_entry_ref, obs_binding_ref, obs_snapshot_ref FROM prefix_attempts WHERE fact<>'' AND caller=? AND installation=? AND session=? AND route=? AND epoch=? ORDER BY started_ns DESC, attempt_id DESC LIMIT 1`, key.args()...).
		Scan(&r.status, &r.reason, &r.relation, &r.ttl, &r.startedNS, &r.untilNS, &r.revision, &r.entry, &r.binding, &r.snapshot)
	return r, err
}

// renderEvidence returns the member text `"prefix_cache_evidence":{...}`.
// An observed estimate is shown only before its deadline. A clock before the
// start, or at or after the deadline, is unverified.
func renderEvidence(r evidenceRow, epoch string, now time.Time) ([]byte, error) {
	var obj any
	switch {
	case r.status != statusObserved:
		obj = unverifiedShape{Version: 1, Status: statusUnverified, Reasons: []string{r.reason}, RemainingPrefixes: "unknown"}
	case now.Before(time.Unix(0, r.startedNS)):
		obj = unverifiedShape{Version: 1, Status: statusUnverified, Reasons: []string{reasonFutureEvidence}, RemainingPrefixes: "unknown"}
	case !now.Before(time.Unix(0, r.untilNS)):
		obj = unverifiedShape{Version: 1, Status: statusUnverified, Reasons: []string{reasonEstimateElapsed}, RemainingPrefixes: "unknown"}
	default:
		obj = observedShape{
			Version: 1, Status: statusObserved, Basis: "unique_write_destination", Coverage: "named_entry_only",
			Relation: r.relation, ContextEpoch: epoch, PolicyRevision: r.revision,
			PrimarySnapshotRef: r.snapshot, BindingRef: r.binding, EntryRef: r.entry,
			EffectiveTTL:            r.ttl,
			UpstreamStartedAt:       time.Unix(0, r.startedNS).UTC(),
			ProtectionEstimateUntil: time.Unix(0, r.untilNS).UTC(),
			RemainingPrefixes:       "unknown",
		}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, ErrUnavailable
	}
	return append([]byte(`"`+ownedField+`":`), b...), nil
}

type member struct {
	name       string
	raw, value []byte
}

// object keeps byte-exact member values while rejecting ambiguous duplicate keys,
// including escaped duplicate spellings. Unrelated values remain untouched.
func object(raw []byte) ([]member, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, ErrInvalidState
	}
	seen := map[string]bool{}
	var out []member
	for d.More() {
		start := int(d.InputOffset())
		for start < len(raw) && (raw[start] == ' ' || raw[start] == '\t' || raw[start] == '\r' || raw[start] == '\n' || raw[start] == ',') {
			start++
		}
		t, err = d.Token()
		if err != nil {
			return nil, ErrInvalidState
		}
		name, ok := t.(string)
		if !ok || seen[name] {
			return nil, ErrInvalidState
		}
		seen[name] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return nil, ErrInvalidState
		}
		out = append(out, member{name, raw[start:int(d.InputOffset())], value})
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') {
		return nil, ErrInvalidState
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, ErrInvalidState
	}
	return out, nil
}

func field(members []member, name string, dst any) bool {
	for _, m := range members {
		if m.name == name {
			return !bytes.Equal(bytes.TrimSpace(m.value), []byte("null")) && json.Unmarshal(m.value, dst) == nil
		}
	}
	return false
}

// augment returns original with ownedField removed and extra (a member text, or
// nil) appended. Without a stale member, the original bytes are kept verbatim and
// extra is inserted before the closing brace.
func augment(original []byte, members []member, extra []byte) ([]byte, error) {
	hadOwned := false
	for _, m := range members {
		if m.name == ownedField {
			hadOwned = true
			break
		}
	}
	if !hadOwned {
		if len(extra) == 0 {
			return bytes.Clone(original), nil
		}
		end := len(bytes.TrimRight(original, " \t\r\n")) - 1
		out := append([]byte(nil), original[:end]...)
		if len(members) > 0 {
			out = append(out, ',')
		}
		out = append(out, extra...)
		out = append(out, original[end:]...)
		if len(out) > maxStateBytes {
			return nil, ErrInvalidState
		}
		return out, nil
	}
	// Strip the stale member, then reassemble from the raw bytes of the others.
	out := []byte{'{'}
	count := 0
	for _, m := range members {
		if m.name == ownedField {
			continue
		}
		if count > 0 {
			out = append(out, ',')
		}
		out = append(out, m.raw...)
		count++
	}
	if len(extra) > 0 {
		if count > 0 {
			out = append(out, ',')
		}
		out = append(out, extra...)
	}
	out = append(out, '}')
	if len(out) > maxStateBytes {
		return nil, ErrInvalidState
	}
	return out, nil
}
