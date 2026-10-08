package measurement

import (
	"errors"
	"time"
)

const ClaudeHTTPSource = "cpa.claude.http.first-party"
const MaxSourceMembers = 128

var ErrStaleSource = errors.New("measurement: stale source proof")

// SourceScope is compared in full, then hashed with the stable host namespace.
// Raw AuthID can contain a filename/email and is never stored in native records.
type SourceScope struct {
	Provider  string `json:"provider"`
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Kind      string `json:"kind"`
	Version   int    `json:"version"`
}

func ValidSourceScope(s SourceScope) bool {
	return s.Provider == "claude" && s.Kind == ClaudeHTTPSource && s.Version == 1 && len(s.AuthID) > 0 && len(s.AuthID) <= 512 && len(s.AuthIndex) <= 128 && nativeID(s.AuthIndex)
}

func SourceReference(host string, scope SourceScope) string {
	return "sha256:" + key(struct {
		Host  string
		Scope SourceScope
	}{host, scope})
}

type SourceMember struct {
	SourceRef     string `json:"source_ref"`
	CredentialRef string `json:"credential_ref"`
}

// SourceMembership is an immutable caller-supplied inventory fact. Verified
// means verified billing-account mapping, not a fallback credential identifier.
// Complete means all runtime tuples for this account in [From,Through) are listed.
// Neither flag asserts absence of traffic outside this host/source.
type SourceMembership struct {
	ID                 string         `json:"id"`
	SourceID           string         `json:"source_id"`
	Provider           string         `json:"provider"`
	AccountID          string         `json:"account_id"`
	Revision           int64          `json:"revision"`
	From               time.Time      `json:"from"`
	Through            time.Time      `json:"through"`
	Verified           bool           `json:"verified"`
	Complete           bool           `json:"complete"`
	MappingEvidenceRef string         `json:"mapping_evidence_ref"`
	Members            []SourceMember `json:"members"`
}

// NativeSource is a detached allowlisted projection, not arbitrary wire JSON.
type NativeSource struct {
	SourceRef         string     `json:"source_ref,omitempty"`
	Kind              string     `json:"kind"`
	Version           int        `json:"version"`
	ManagerID         string     `json:"manager_incarnation_id"`
	Tracked           bool       `json:"tracked"`
	RequestedAt       time.Time  `json:"requested_at"`
	UpstreamStartedAt *time.Time `json:"upstream_started_at,omitempty"`
	FinalUsageAt      time.Time  `json:"final_usage_at"`
	Gap               bool       `json:"gap"`
	InvalidFrom       *time.Time `json:"invalid_from,omitempty"`
}

// SourceCheckpoint has no relationship to the legacy global Checkpoint method.
// Invalid inputs still carry their bounded invalidation before being refused.
type SourceCheckpoint struct {
	ConsumerID, MembershipID, SourceRef string
	ManagerID, SinkID                   string
	Sequence                            uint64
	Coverage                            string
	From, Through                       *time.Time
	HasGap                              bool
	InvalidFrom                         *time.Time
	Invalid                             bool
	PayloadDigest                       string
}

type SourceRun struct {
	ConsumerID    string    `json:"consumer_id"`
	MembershipID  string    `json:"membership_id"`
	SourceRef     string    `json:"source_ref"`
	ManagerID     string    `json:"manager_id"`
	SinkID        string    `json:"sink_id"`
	Sequence      uint64    `json:"sequence"`
	Coverage      string    `json:"coverage"`
	From          time.Time `json:"from"`
	Through       time.Time `json:"through"`
	HostFrom      time.Time `json:"host_from"`
	PayloadDigest string    `json:"payload_digest"`
}

// SourceSnapshot describes REQUEST-start coverage only. PhysicalCarryInProven is
// deliberately false: ace073 has no physical-domain carry-in closure contract.
type SourceSnapshot struct {
	Known                 bool        `json:"known"`
	Provider              string      `json:"provider"`
	AccountID             string      `json:"account_id"`
	MembershipID          string      `json:"membership_id"`
	From                  time.Time   `json:"from"`
	Through               time.Time   `json:"through"`
	Runs                  []SourceRun `json:"runs"`
	Reasons               []string    `json:"reasons,omitempty"`
	HasGap                bool        `json:"has_gap"`
	InvalidFrom           *time.Time  `json:"invalid_from,omitempty"`
	PhysicalCarryInProven bool        `json:"physical_carry_in_proven"`
}
