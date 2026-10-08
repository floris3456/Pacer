// Package capacity judges lower-bound $/% intervals for one provider account
// from three independent facts: interval membership (M) from host inventory
// checkpoints, physical carry-in (K) from quiescent cuts bracketing provider
// samples, and CPA value. Exclusivity is never provable here, so no result is
// a known capacity, and no declared, claimed or configured size exists.
package capacity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const InventoryVersion = 1
const MaxInventoryTuples = 1024
const maxInventoryBytes = 1 << 20

// Tuple is one host credential tuple at one inventory checkpoint. Ref is a
// digest: a raw AuthID can be a filename or e-mail and is never stored.
type Tuple struct {
	Ref             string `json:"tuple"`
	AccountRef      string `json:"account_ref,omitempty"`
	BindingSequence uint64 `json:"binding_sequence"`
	State           string `json:"state"`
}

// Checkpoint is one inventory observation. Only Coverage "known" has tuples.
type Checkpoint struct {
	ID          int64     `json:"id"`
	Coverage    string    `json:"coverage"`
	Reason      string    `json:"reason,omitempty"`
	Incarnation string    `json:"incarnation"`
	Sequence    uint64    `json:"sequence"`
	At          time.Time `json:"at"`
	Tuples      []Tuple   `json:"tuples"`
}

// Scope keeps the raw tuple identity in memory, for checkpoint reads only.
type Scope struct{ AuthID, AuthIndex string }

type Inventory struct {
	Checkpoint Checkpoint
	Scopes     map[string]Scope
}

func TupleRef(authID, authIndex string) string {
	h := sha256.Sum256([]byte("pacer-capacity-tuple\x00" + authID + "\x00" + authIndex))
	return "tuple:" + hex.EncodeToString(h[:])
}

func sortTuples(t []Tuple) { sort.Slice(t, func(i, j int) bool { return t[i].Ref < t[j].Ref }) }

func unknownInventory(reason string) Inventory {
	return Inventory{Checkpoint: Checkpoint{Coverage: "unknown", Reason: reason}}
}

// ParseInventory never fails: an old host (unknown method or any RPC error), a
// foreign version, an invalid or oversized reply all become unknown inventory.
func ParseInventory(raw []byte, err error) Inventory {
	if err != nil {
		return unknownInventory("inventory_unsupported")
	}
	var r pluginapi.HostAuthInventoryCheckpointResponse
	if len(raw) == 0 || len(raw) > maxInventoryBytes || json.Unmarshal(raw, &r) != nil {
		return unknownInventory("inventory_invalid")
	}
	if r.Version != InventoryVersion || r.Provider != "claude" {
		return unknownInventory("inventory_unsupported")
	}
	if r.Coverage != "known" {
		return unknownInventory("inventory_unknown")
	}
	if len(r.Tuples) > MaxInventoryTuples {
		return unknownInventory("inventory_overflow")
	}
	if r.ManagerIncarnationID == "" || len(r.ManagerIncarnationID) > 256 || r.Sequence > math.MaxInt64 || !validTime(r.At) {
		return unknownInventory("inventory_invalid")
	}
	c := Checkpoint{Coverage: "known", Incarnation: r.ManagerIncarnationID, Sequence: r.Sequence, At: r.At.Round(0).UTC(), Tuples: []Tuple{}}
	scopes := map[string]Scope{}
	for _, t := range r.Tuples {
		if t.AuthID == "" || len(t.AuthID) > 512 || t.AuthIndex == "" || len(t.AuthIndex) > 128 || len(t.AccountRef) > 256 || t.BindingSequence > r.Sequence || t.State != "active" && t.State != "removed" {
			return unknownInventory("inventory_invalid")
		}
		ref := TupleRef(t.AuthID, t.AuthIndex)
		if _, duplicate := scopes[ref]; duplicate {
			return unknownInventory("inventory_invalid")
		}
		scopes[ref] = Scope{t.AuthID, t.AuthIndex}
		c.Tuples = append(c.Tuples, Tuple{Ref: ref, AccountRef: t.AccountRef, BindingSequence: t.BindingSequence, State: t.State})
	}
	sortTuples(c.Tuples)
	return Inventory{Checkpoint: c, Scopes: scopes}
}

func validTime(t time.Time) bool {
	return !t.IsZero() && t.UnixNano() > 0 && time.Unix(0, t.UnixNano()).Equal(t)
}

// ResolveAccountRef maps Pacer's own tuples for one account to the host's
// account_ref. All of them must carry the same, known reference.
func ResolveAccountRef(c Checkpoint, tuples []string) (string, string) {
	if c.Coverage != "known" {
		return "", "inventory_unknown"
	}
	if len(tuples) == 0 {
		return "", "pacer_membership_unavailable"
	}
	index := map[string]Tuple{}
	for _, t := range c.Tuples {
		index[t.Ref] = t
	}
	ref := ""
	for _, id := range tuples {
		t, ok := index[id]
		switch {
		case !ok:
			return "", "pacer_tuple_not_in_inventory"
		case t.AccountRef == "":
			return "", "account_ref_unknown"
		case ref != "" && ref != t.AccountRef:
			return "", "account_ref_ambiguous"
		}
		ref = t.AccountRef
	}
	return ref, ""
}
