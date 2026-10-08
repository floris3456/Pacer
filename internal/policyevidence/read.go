package policyevidence

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const maxStateBytes = 16 * 1024
const ttlField = "last_effective_ttl"
const evidenceField = "last_effective_ttl_evidence"

type provenance struct {
	Status            string    `json:"status"` // observed | uncertain
	Basis             string    `json:"basis"`
	UpstreamStartedAt time.Time `json:"upstream_started_at,omitempty,omitzero"`
	PolicyRevision    int64     `json:"policy_revision,omitempty"`
	Reason            string    `json:"reason,omitempty"`
}

// AugmentAuthorized is a read-only supplement for a fresh successful GET policy
// response AFTER the policy adapter has authorized the host-derived caller and
// private capability for that exact scope. callerDigest is the host SHA already
// used by the policy store, not an API key and not a digest to hash again.
//
// This method cannot authorize a capability: NEVER expose it as an HTTP endpoint
// or invoke it on client-provided state, failed RPCs, cached augmented responses,
// or POST command receipts. The parent must gate GET + StatusCode==200 + RPC
// success, and leave canonical response/status unchanged if augmentation fails.
//
// Only last_effective_ttl and last_effective_ttl_evidence are added/replaced. No
// effective_ttl, cache_refresh_at, protected_until, or warming fields are written.
// Original member values (including policy hash/receipt JSON bytes) are preserved.
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
	var used bool
	if !field(activeMembers, "used", &used) || !used {
		return augment(stateJSON, members, nil)
	}
	// Existing-history enrollment can deliberately have no known active TTL.
	// Never borrow a pending target's selection or an old epoch's evidence.
	if !ttl(selected) {
		return augment(stateJSON, members, nil)
	}
	h, err := readHead(ctx, s.db, key)
	if errors.Is(err, sql.ErrNoRows) {
		return augment(stateJSON, members, nil)
	}
	if err != nil {
		return nil, dbError(ctx, err)
	}
	if h.Start <= 0 || h.Known && (h.PolicyRevision < 1 || !ttl(h.Selected) || !ttl(h.Effective) || h.Selected != h.Effective || h.Reason != "") {
		return nil, ErrUnavailable
	}
	p := provenance{Status: "uncertain", Basis: "successful_native_caller_cpa_markers_only"}
	known := h.Known
	now := s.clock().UTC()
	switch {
	case !validTime(now) || h.Start > now.UnixNano():
		known = false
		p.Reason = "unverified_read_clock"
	case h.PolicyRevision > revision:
		known = false
		p.Reason = "state_snapshot_precedes_evidence"
	case h.Known && h.Selected != selected:
		known = false
		p.Reason = "active_selection_differs"
	default:
		p.UpstreamStartedAt = time.Unix(0, h.Start).UTC()
		p.PolicyRevision = h.PolicyRevision
		if !h.Known {
			if !unknownReason(h.Reason) {
				return nil, ErrUnavailable
			}
			p.Reason = h.Reason
		}
	}
	var effective string
	if known {
		effective = h.Effective
		p.Status = "observed"
	}
	fields, err := json.Marshal(struct {
		TTL      string     `json:"last_effective_ttl,omitempty"`
		Evidence provenance `json:"last_effective_ttl_evidence"`
	}{effective, p})
	if err != nil {
		return nil, ErrUnavailable
	}
	return augment(stateJSON, members, fields[1:len(fields)-1])
}

func unknownReason(s string) bool {
	switch s {
	case "equal_start_conflict", "no_caller_cpa_markers", "unknown_marker_ttl", "selected_effective_mismatch":
		return true
	default:
		return false
	}
}

type member struct {
	name       string
	raw, value []byte
}

// object keeps byte-exact member values while rejecting ambiguous duplicate keys,
// including escaped duplicate spellings. It parses only the two state objects
// whose fields determine the scope; unrelated receipt values remain untouched.
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

func augment(original []byte, members []member, extra []byte) ([]byte, error) {
	hadOwned := false
	for _, m := range members {
		if m.name == ttlField || m.name == evidenceField {
			hadOwned = true
			break
		}
	}
	if !hadOwned {
		if len(extra) == 0 {
			return bytes.Clone(original), nil
		}
		// Append only; preserve the entire canonical state's existing bytes.
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
	// Strip stale owned optional fields even when this scope has no evidence.
	// Reassemble without re-encoding unrelated member values or receipt objects.
	out := []byte{'{'}
	count := 0
	for _, m := range members {
		if m.name == ttlField || m.name == evidenceField {
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
