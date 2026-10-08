package historyreports

import (
	"errors"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/model"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

const ForwardVersion = "pacer-forward-ledger-v1"
const ForwardConverterVersion = "cpa-normalized-v2-cpamp-e849-v1"

var ErrForwardConflict = errors.New("forward reports: immutable reference conflict")
var ErrForwardPending = errors.New("forward reports: projection not ready")

type ForwardComparisonBook = cpamp.ComparisonPriceBook
type ForwardModelPrice = model.ModelPrice
type ForwardContextTier = model.ModelPriceContextTier
type ForwardServiceTier = model.ModelPriceServiceTier
type ForwardProfileConfig = cpamp.ProfileConfig
type ForwardProfile = cpamp.Profile
type ForwardMembership = cpamp.ProfileMembership
type ForwardBinding = cpamp.Binding
type ForwardValue = cpamp.APIValue

// Qualification is exact, not a wildcard: an empty route matches an empty route.
// Baseline source is "cpamp:"+installation lineage. A book is trusted offline
// metadata; its qualification does not certify native capture or billing coverage.
type ForwardPriceQualification struct{ SourceID, Provider, Model, Route, AccountID, CredentialRef string }
type ForwardBook struct {
	ID             string                      `json:"id"`
	Book           ForwardComparisonBook       `json:"book"`
	Qualifications []ForwardPriceQualification `json:"qualifications"`
}

// A certificate relates retained legacy physical IDs to one native producer and
// credential/account tuple. No fuzzy matching and no completeness override exist.
// Completeness of the baseline index is computed from admitted rows.
type ForwardOriginCertificate struct {
	LegacyAccount, Provider, SourceID, NativeAccount, NativeCredentialRef string
	StartMS, EndMS                                                        int64
	EvidenceRef, ProducerVersion, ParserVersion                           string
}
type ForwardOrigins struct {
	Certificates []ForwardOriginCertificate `json:"certificates"`
}

type ForwardBase struct {
	Boundary       history.Boundary `json:"boundary"`
	Pin            Pin              `json:"pin"`
	ArtifactDigest string           `json:"artifact_digest"`
}

// ForwardEvent is a sanitized projection. Raw usage/headers/transport metadata
// stay in their original stores. Event is the exact calculation input, not raw CPA.
type ForwardEvent struct {
	ID          string                      `json:"id"`
	Origin      string                      `json:"origin"`
	SourceID    string                      `json:"source_id"`
	PhysicalKey string                      `json:"physical_key"`
	AttemptID   string                      `json:"attempt_id"`
	LifecycleID string                      `json:"lifecycle_id,omitempty"`
	Revision    int64                       `json:"revision,string"`
	Sequence    int64                       `json:"sequence,string"`
	StartedAt   time.Time                   `json:"started_at"`
	EndedAt     time.Time                   `json:"ended_at"`
	ReceivedAt  time.Time                   `json:"received_at"`
	StartBasis  string                      `json:"start_basis"`
	Final       bool                        `json:"final"`
	Accounting  string                      `json:"accounting"`
	Quality     string                      `json:"quality"`
	Attribution measurement.Attribution     `json:"attribution"`
	Policy      measurement.PolicySnapshot  `json:"policy"`
	Usage       *measurement.Usage          `json:"normalized_source_usage,omitempty"`
	Native      *measurement.NativeMetadata `json:"native_provenance,omitempty"`
	Event       cpamp.Event                 `json:"event"`
	BodyDigest  string                      `json:"body_digest"`
	RawDigest   string                      `json:"raw_digest,omitempty"`
	Issues      []string                    `json:"issues"`
}

type ForwardProvenance struct {
	Snapshots []measurement.SourceSnapshot `json:"snapshots,omitempty"`
	Reasons   []string                     `json:"reasons"`
}
type ForwardSeal struct {
	Head                                 measurement.ReportFeedHead
	BaseDigest, BookDigest, OriginDigest string
	ProfileRevision                      int64
	At                                   time.Time
	Provenance                           ForwardProvenance
}
type forwardManifest struct {
	Version         string                     `json:"version"`
	Converter       string                     `json:"converter"`
	Head            measurement.ReportFeedHead `json:"head"`
	BaseDigest      string                     `json:"base_digest"`
	BookDigest      string                     `json:"book_digest"`
	OriginDigest    string                     `json:"origin_digest"`
	ProfileRevision int64                      `json:"profile_revision,string"`
	ProfileDigest   string                     `json:"profile_digest"`
	At              time.Time                  `json:"report_at"`
	Provenance      ForwardProvenance          `json:"provenance"`
}
type ForwardFilter struct {
	Domain   string    `json:"domain,omitempty"` // combined (default), native, baseline
	From     time.Time `json:"from,omitempty,omitzero"`
	To       time.Time `json:"to,omitempty,omitzero"`
	Profile  string    `json:"profile,omitempty"` // empty=global; unassigned supported
	Account  string    `json:"account,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	KeyHash  string    `json:"key_hash,omitempty"`
}
type ForwardPin struct {
	Version    string        `json:"version"`
	Generation string        `json:"generation"`
	Filter     ForwardFilter `json:"filter"`
}
type ForwardTotals struct {
	SourceRecords    int64            `json:"source_record_count"`
	Rows             int64            `json:"canonical_rows"`
	UniqueAttempts   *int64           `json:"unique_attempts"`
	ValuedCount      int64            `json:"valued_count"`
	ValuedSubset     ForwardValue     `json:"valued_subset"`
	SelectionValue   *ForwardValue    `json:"selection_value"`
	CapturedCostNano int64            `json:"captured_cost_subset_nano_usd"`
	CapturedCount    int64            `json:"captured_count"`
	Reasons          map[string]int64 `json:"reason_counts"`
}
type ForwardReport struct {
	Pin      ForwardPin               `json:"pin"`
	Manifest forwardManifest          `json:"manifest"`
	Totals   ForwardTotals            `json:"totals"`
	Daily    map[string]ForwardTotals `json:"daily"`
	Models   map[string]ForwardTotals `json:"models"`
	Accounts map[string]ForwardTotals `json:"accounts"`
	Profiles map[string]ForwardTotals `json:"profiles"`
	Capacity string                   `json:"capacity"`
}
type ForwardEvidence struct {
	Pin              ForwardPin     `json:"pin"`
	ID               string         `json:"id"`
	Account          string         `json:"account"`
	Origins          []ForwardEvent `json:"origins"`
	Value            *ForwardValue  `json:"value"`
	IdentityResolved bool           `json:"identity_resolved"`
	Issues           []string       `json:"issues"`
}
type ForwardPage struct {
	Pin  ForwardPin        `json:"pin"`
	Rows []ForwardEvidence `json:"rows"`
	Next string            `json:"next_cursor,omitempty"`
}

func ForwardAccountRef(source, provider, account string) string {
	return "native-account:" + forwardDigest([]string{source, provider, account})
}
