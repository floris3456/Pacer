// Package cachepolicy owns durable, caller-scoped cache TTL coordination.
// It does not dispatch requests, reserve quota, or infer compaction from account moves.
package cachepolicy

import (
	"fmt"
	"time"
)

const Protocol = 1

// ContractRevision identifies the protocol-1 extension requiring transition-bound
// checkpoint receipts and supporting compaction-gated existing-history enrollment.
const ContractRevision = 2

const CheckpointReceiptVersion = 1

type TTL string

const (
	TTL5m TTL = "5m"
	TTL1h TTL = "1h"
)

type Scope struct {
	InstallationID string `json:"installation_id"`
	SessionID      string `json:"session_id"`
	ProviderRoute  string `json:"provider_route"`
}

// Credentials are constructed by the host, never decoded from request JSON.
// Capability is a private, client-generated, base64url 32-byte random secret.
type Credentials struct {
	CallerDigest string `json:"-"`
	Capability   string `json:"-"`
}

// Evidence is trusted host input. NewEmpty may be attested by an authenticated,
// trusted client plugin after inspecting its local Core session. It never follows
// merely from a missing server policy, and does not require CPA to query client DBs.
type Evidence struct {
	NewEmpty bool
}

type Command struct {
	Protocol  int    `json:"protocol"`
	Operation string `json:"operation"`
	Scope
	ContextEpoch          string             `json:"context_epoch"`
	ExpectedRevision      int64              `json:"expected_revision"`
	TTL                   TTL                `json:"ttl"`
	OperationID           string             `json:"operation_id"`
	TransitionID          string             `json:"transition_id,omitempty"`
	Checkpoint            string             `json:"checkpoint,omitempty"`
	CompactionStartedAt   time.Time          `json:"compaction_started_at,omitempty"`
	CheckpointCommittedAt time.Time          `json:"checkpoint_committed_at,omitempty"`
	Enrollment            string             `json:"enrollment,omitempty"`
	CheckpointReceipt     *CheckpointReceipt `json:"checkpoint_receipt,omitempty"`
}

// CheckpointReceipt is the authenticated client's durable Core acknowledgement.
// The client binds this transition and previous checkpoint BEFORE compaction and
// marks Committed only after a new checkpoint durably commits. Timestamps are not
// the ordering authority; this receipt is an attestation, not a contents proof.
type CheckpointReceipt struct {
	Version            int    `json:"version"`
	TransitionID       string `json:"transition_id"`
	PreviousCheckpoint string `json:"previous_checkpoint"`
	Checkpoint         string `json:"checkpoint"`
	Committed          bool   `json:"committed"`
}

type CommitProvenance struct {
	Receipt               CheckpointReceipt `json:"checkpoint_receipt"`
	CompactionStartedAt   time.Time         `json:"compaction_started_at,omitempty"`
	CheckpointCommittedAt time.Time         `json:"checkpoint_committed_at,omitempty"`
	AcknowledgedAt        time.Time         `json:"acknowledged_at"`
}

type Policy struct {
	ContextEpoch string `json:"context_epoch"`
	Checkpoint   string `json:"checkpoint"`
	TTL          TTL    `json:"ttl"`
	Used         bool   `json:"used"`
}

type Transition struct {
	ID               string    `json:"transition_id"`
	TTL              TTL       `json:"ttl"`
	FromEpoch        string    `json:"from_epoch"`
	FromCheckpoint   string    `json:"from_checkpoint"`
	PreparedAt       time.Time `json:"prepared_at"`
	PreparedRevision int64     `json:"prepared_revision"`
}

type State struct {
	Protocol int `json:"protocol"`
	Scope
	Revision        int64       `json:"revision"`
	Active          Policy      `json:"active"`
	Pending         *Transition `json:"pending,omitempty"`
	ActivePrimary   int64       `json:"active_primary"`
	ActiveWarming   int64       `json:"active_warming"`
	ActiveAuxiliary int64       `json:"active_auxiliary"`
	ActiveNative    int64       `json:"active_native,omitempty"`
	ActiveAttempts  int64       `json:"active_attempts"`
	WarmingPaused   bool        `json:"warming_paused"`
	// Until initial existing-history compaction commits, Active describes only
	// the old checkpoint: TTL is unknown (empty), Used=true, and primary is blocked.
	EnrollmentRequired bool              `json:"enrollment_required,omitempty"`
	LastCommit         *CommitProvenance `json:"last_commit,omitempty"`
}

// Reference is mandatory on every managed lifecycle request. Scope, revision,
// epoch and TTL are checked together; a missing state never enrolls a request.
type Reference struct {
	Scope
	ContextEpoch string `json:"context_epoch"`
	Revision     int64  `json:"revision"`
	TTL          TTL    `json:"ttl"`
}

type Kind string

const (
	Primary   Kind = "primary"
	Warming   Kind = "warming"
	Auxiliary Kind = "auxiliary"
	// NativeCompaction is attested by the host's consumed exact-account replay
	// lease. An ordinary request header must never select this kind.
	NativeCompaction Kind = "native_compaction"
)

// Kind must be classified by the host. A summary is Auxiliary, never Primary.
type Request struct {
	Reference
	RequestID string `json:"request_id"`
	Kind      Kind   `json:"kind"`
}

// Snapshot is a value receipt, durably copied into every physical attempt.
// Auxiliary snapshots have Managed=false and no selected TTL.
type Snapshot struct {
	Protocol int `json:"protocol"`
	Scope
	ContextEpoch string    `json:"context_epoch"`
	Revision     int64     `json:"revision"`
	TTL          TTL       `json:"ttl,omitempty"`
	RequestID    string    `json:"request_id"`
	AttemptID    string    `json:"attempt_id,omitempty"`
	Kind         Kind      `json:"kind"`
	Managed      bool      `json:"managed"`
	StartedAt    time.Time `json:"started_at"`
}

type Attempt struct {
	Scope
	RequestID string `json:"request_id"`
	AttemptID string `json:"attempt_id"`
}

type Outcome string

const (
	Succeeded Outcome = "succeeded"
	Failed    Outcome = "failed"
	Canceled  Outcome = "canceled"
)

// Route declares final-boundary support already verified by the host.
type Route struct {
	ProviderRoute string
	TTLs          []TTL
}

type Options struct {
	Clock  func() time.Time
	Routes []Route
}

// Error is safe to put on the client wire. Internal database error details are
// deliberately excluded. HTTP adapters use Status, Code and Message.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func failure(status int, code, message string) error {
	return &Error{Status: status, Code: "pacer_cache_" + code, Message: message}
}

func unavailable() error {
	return failure(503, "unavailable", "Durable cache policy is unavailable; reconcile before retrying.")
}
func denied() error {
	return failure(403, "scope_denied", "The cache policy scope or capability is not authorized.")
}
func conflict(code, message string) error { return failure(409, code, message) }
func invalid(message string) error        { return failure(400, "invalid", message) }
