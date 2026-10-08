package historyreports

import (
	"errors"
	"fmt"
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

// ErrForwardFrozenProfile is an ErrForwardConflict: profiles copied from the
// frozen artifact are never edited; new memberships use new forward profiles.
var ErrForwardFrozenProfile = fmt.Errorf("%w: frozen profile is read-only", ErrForwardConflict)

// ErrForwardCoverageGap refuses N0 > H unless the gap [H,N0) is declared.
var ErrForwardCoverageGap = errors.New("forward reports: native coverage starts after H without a declared coverage gap")

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
	BaseDigest   string                     `json:"base_digest"`
	Certificates []ForwardOriginCertificate `json:"certificates"`
}

type ForwardBase struct {
	Boundary       history.Boundary `json:"boundary"`
	Pin            Pin              `json:"pin"`
	ArtifactDigest string           `json:"artifact_digest"`
}

// ForwardProfileSeed is the provenance of profile revision 1 when it was copied
// from the frozen artifact's profile configuration at H.
type ForwardProfileSeed struct {
	BaseDigest     string `json:"base_digest"`
	ArtifactDigest string `json:"artifact_digest"`
	FrozenRevision int64  `json:"frozen_revision,string"`
	ConfigDigest   string `json:"config_digest"`
}

// ForwardCoverageGap is a declared interval without certified native capture.
// It is reported, never filled or interpolated.
type ForwardCoverageGap struct {
	FromMS   int64    `json:"from_ms"`
	ToMS     int64    `json:"to_ms"`
	Accounts []string `json:"accounts"`
}

// ForwardCutover is the frozen→live boundary: H is the frozen bound, N0 the
// explicitly declared start of certified native capture with its recorded
// basis (never derived from certificates). It is recorded once per store by
// offline preparation and copied into every generation manifest sealed with
// that base.
type ForwardCutover struct {
	HMS                 int64               `json:"h_ms"`
	EventsThrough       int64               `json:"events_through"`
	ObservationsThrough int64               `json:"observations_through"`
	N0MS                int64               `json:"n0_ms"`
	N0Basis             string              `json:"n0_basis"`
	CoverageGap         *ForwardCoverageGap `json:"coverage_gap,omitempty"`
}

type forwardCutoverRecord struct {
	BaseDigest   string         `json:"base_digest"`
	OriginDigest string         `json:"origin_digest"`
	Cutover      ForwardCutover `json:"cutover"`
}

// ForwardCutoverStatus is generation-level (unfiltered) cutover accounting.
type ForwardCutoverStatus struct {
	ForwardCutover
	PreCutoverNative int64 `json:"pre_cutover_native"`
	PostHBaseline    int64 `json:"post_h_baseline"`
}

// ForwardDedup labels a window touched by overlap events whose counterpart on
// the other side is not linked. Each such event is counted once, on its own side.
type ForwardDedup struct {
	UnlinkedBaseline int64 `json:"unlinked_baseline"`
	UnlinkedNative   int64 `json:"unlinked_native"`
}

// ForwardEvent is a sanitized projection. Raw usage/headers/transport metadata
// stay in their original stores. Event is the exact calculation input, not raw CPA.
type ForwardEvent struct {
	ID            string                      `json:"id"`
	Origin        string                      `json:"origin"`
	SourceID      string                      `json:"source_id"`
	PhysicalKey   string                      `json:"physical_key"`
	AttemptID     string                      `json:"attempt_id"`
	LifecycleID   string                      `json:"lifecycle_id,omitempty"`
	Revision      int64                       `json:"revision,string"`
	Sequence      int64                       `json:"sequence,string"`
	StartedAt     time.Time                   `json:"started_at"`
	EndedAt       time.Time                   `json:"ended_at"`
	ReceivedAt    time.Time                   `json:"received_at"`
	StartBasis    string                      `json:"start_basis"`
	Final         bool                        `json:"final"`
	Accounting    string                      `json:"accounting"`
	RawInputBasis string                      `json:"raw_input_basis"`
	Quality       string                      `json:"quality"`
	Attribution   measurement.Attribution     `json:"attribution"`
	Policy        measurement.PolicySnapshot  `json:"policy"`
	Usage         *measurement.Usage          `json:"normalized_source_usage,omitempty"`
	Native        *measurement.NativeMetadata `json:"native_provenance,omitempty"`
	Event         cpamp.Event                 `json:"event"`
	BodyDigest    string                      `json:"body_digest"`
	RawDigest     string                      `json:"raw_digest,omitempty"`
	Issues        []string                    `json:"issues"`
}

type ForwardProvenance struct {
	ConsumerID string                       `json:"consumer_id,omitempty"`
	Snapshots  []measurement.SourceSnapshot `json:"snapshots,omitempty"`
	Reasons    []string                     `json:"reasons"`
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
	Cutover         *ForwardCutover            `json:"cutover,omitempty"`
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
	SourceRecords     int64            `json:"source_record_count"`
	Rows              int64            `json:"canonical_rows"`
	UniqueAttempts    *int64           `json:"unique_attempts"`
	ValuedCount       int64            `json:"valued_count"`
	ValuedSubset      ForwardValue     `json:"valued_subset"`
	SelectionValue    *ForwardValue    `json:"selection_value"`
	CapturedCostNano  int64            `json:"captured_cost_subset_nano_usd"`
	CapturedCount     int64            `json:"captured_count"`
	CapturedMissing   int64            `json:"captured_cost_unavailable_count"`
	CapturedSelection *int64           `json:"captured_cost_selection_nano_usd"`
	Reasons           map[string]int64 `json:"reason_counts"`
	CoverageReasons   map[string]int64 `json:"coverage_reason_counts"`
	DedupUnverified   *ForwardDedup    `json:"dedup_unverified,omitempty"`
}
type ForwardReport struct {
	Pin       ForwardPin               `json:"pin"`
	Manifest  forwardManifest          `json:"manifest"`
	Base      *ForwardBase             `json:"base,omitempty"`
	PriceBook ForwardBook              `json:"price_book"`
	Totals    ForwardTotals            `json:"totals"`
	Daily     map[string]ForwardTotals `json:"daily"`
	Models    map[string]ForwardTotals `json:"models"`
	Accounts  map[string]ForwardTotals `json:"accounts"`
	Profiles  map[string]ForwardTotals `json:"profiles"`
	Capacity  string                   `json:"capacity"`
	Cutover   *ForwardCutoverStatus    `json:"cutover,omitempty"`
}
type ForwardEvidence struct {
	Pin               ForwardPin     `json:"pin"`
	Valuation         string         `json:"valuation"`
	PriceBookID       string         `json:"price_book_id"`
	PriceBookDigest   string         `json:"price_book_digest"`
	ProvenanceDigest  string         `json:"provenance_digest"`
	HistoricalHMS     int64          `json:"historical_h_ms,omitempty"`
	Capacity          string         `json:"capacity"`
	ID                string         `json:"id"`
	Account           string         `json:"account"`
	Origins           []ForwardEvent `json:"origins"`
	Value             *ForwardValue  `json:"value"`
	CapturedCost      *int64         `json:"captured_cost_nano_usd"`
	CapturedCostBasis string         `json:"captured_cost_basis"`
	IdentityResolved  bool           `json:"identity_resolved"`
	Issues            []string       `json:"issues"`
	CoverageReasons   []string       `json:"coverage_reasons"`
	DedupUnverified   string         `json:"dedup_unverified,omitempty"` // unlinked_baseline or unlinked_native
}
type ForwardPage struct {
	Pin  ForwardPin        `json:"pin"`
	Rows []ForwardEvidence `json:"rows"`
	Next string            `json:"next_cursor,omitempty"`
}

func ForwardAccountRef(source, provider, account string) string {
	return "native-account:" + forwardDigest([]string{source, provider, account})
}
