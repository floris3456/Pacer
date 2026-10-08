package measurement

import (
	"encoding/json"
	"math"
	"time"
)

const PhysicalDomain = "cpa.claude.http.local-dispatch-sequence"
const PhysicalVersion = 1
const MaxPhysicalSelection = 4096

// Sequences are decoded as uint64, never float64. This implementation refuses
// values outside SQLite's signed INTEGER range; it never casts them first.
func physicalSequence(n uint64) bool { return n > 0 && n <= math.MaxInt64 }

// PhysicalCutIdentity binds a boundary to one source and local listener. It is
// deliberately not interchangeable with a timestamp or SourceSnapshot interval.
type PhysicalCutIdentity struct {
	SourceID     string    `json:"source_id"`
	ConsumerID   string    `json:"consumer_id"`
	MembershipID string    `json:"membership_id"`
	SourceRef    string    `json:"source_ref"`
	ManagerID    string    `json:"manager_id"`
	SinkID       string    `json:"sink_id"`
	Sequence     uint64    `json:"sequence"`
	At           time.Time `json:"at"`
}

func (c PhysicalCutIdentity) ID() string { return "physical-cut:" + key(c) }
func (c PhysicalCutIdentity) epoch() string {
	c.Sequence = 0
	c.At = time.Time{}
	return key(c)
}
func (c PhysicalCutIdentity) valid() bool {
	return nativeID(c.SourceID) && nativeID(c.ConsumerID) && nativeID(c.MembershipID) && validSourceRef(c.SourceRef) && nativeID(c.ManagerID) && nativeID(c.SinkID) && physicalSequence(c.Sequence) && validTime(c.At)
}

type PhysicalSelection struct {
	From    PhysicalCutIdentity `json:"from"`
	Through PhysicalCutIdentity `json:"through"`
}

type PhysicalFact struct {
	Version              int       `json:"version"`
	StartTimeBasis       string    `json:"start_time_basis"`
	EventID              string    `json:"event_id"`
	ConsumerID           string    `json:"consumer_id"`
	SourceID             string    `json:"source_id"`
	SourceRef            string    `json:"source_ref"`
	ManagerID            string    `json:"manager_id"`
	MembershipID         string    `json:"membership_id"`
	AccountID            string    `json:"account_id"`
	CredentialRef        string    `json:"credential_ref"`
	CredentialBindingRef string    `json:"credential_binding_ref"`
	AccessTokenRef       string    `json:"access_token_ref,omitempty"`
	Revision             int64     `json:"revision"`
	RecordDigest         string    `json:"record_digest"`
	AdmissionSequence    uint64    `json:"admission_sequence"`
	DispatchSequence     uint64    `json:"dispatch_sequence"`
	StartedAt            time.Time `json:"started_at"`
}

// Known certifies only the exact stored edge and joined local dispatch facts.
// No account totals, token correctness, physical end times or time coverage are
// asserted. A known empty Facts list is legitimate local-source silence only.
type PhysicalResult struct {
	Version   int               `json:"version"`
	Domain    string            `json:"domain"`
	Known     bool              `json:"known"`
	Reason    string            `json:"reason,omitempty"`
	Selection PhysicalSelection `json:"selection"`
	ProofID   string            `json:"proof_id,omitempty"`
	Facts     []PhysicalFact    `json:"facts"`
}

// Proofs retain a digest of the ordered exact joins, not repeated copies of the
// entire record set on every poll. Selection rechecks and returns those facts.
type physicalManifest struct {
	Version   int               `json:"version"`
	Domain    string            `json:"domain"`
	Selection PhysicalSelection `json:"selection"`
	ProofID   string            `json:"proof_id"`
	Count     int               `json:"count"`
	Digest    string            `json:"manifest_digest"`
}

// PhysicalCheckpoint is normalized by the wire adapter; top-level request-start
// gaps are negative evidence here, but their time bounds are never reused.
type PhysicalCheckpoint struct {
	Identity                 PhysicalCutIdentity
	From, Through            *PhysicalCutIdentity
	Coverage, Reason, Digest string
	HasGap, Invalid, Missing bool
}

func physicalRecordDigest(r Record) string {
	r.RawUsage = nil // canonical body also omits this field; raw accounting has its own immutable revision
	b, _ := json.Marshal(r)
	return digest(b)
}
