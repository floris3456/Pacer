package historyreports

// Native inputs for the continuous usage record (docs/usage-unification.md):
// Pacer's own meter readings become the same observations CPAMP recorded, so
// windows after H are derived and normalized by the same CPAMP functions.

import (
	"math"
	"strconv"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// NativeReading is one provider meter reading Pacer recorded (one
// observation_history row). Remaining is the provider's remaining percent.
type NativeReading struct {
	ID        int64
	Account   string // Pacer's native account ID
	Provider  string
	Scope     string
	Seconds   int64
	Remaining *float64
	Reset     time.Time
	At        time.Time // source time of the reading (response or API answer)
	Source    string    // header, api or event
}

func nativeKind(seconds int64) string {
	switch seconds {
	case 5 * 3600:
		return "5h"
	case 7 * 86400:
		return "weekly"
	case 86400:
		return "daily"
	}
	return ""
}

// nativeObservation converts a reading into CPAMP's observation shape for the
// given (legacy or native) account ID. Readings without a usage value, a
// reset, a known window length or a valid time are not meter readings.
func nativeObservation(r NativeReading, account string) (cpamp.Observation, bool) {
	kind := nativeKind(r.Seconds)
	if kind == "" || r.Remaining == nil || math.IsNaN(*r.Remaining) || math.IsInf(*r.Remaining, 0) || *r.Remaining < 0 || *r.Remaining > 100 || r.Reset.IsZero() || r.At.IsZero() || r.Scope == "" || account == "" {
		return cpamp.Observation{}, false
	}
	reset, at := r.Reset.UnixMilli(), r.At.UnixMilli()
	duration := r.Seconds * 1000
	used := 100 - *r.Remaining
	// Header readings get CPAMP's provider-date clock margin; API answers are
	// poll receipts, exactly as CPAMP treated its own API queries.
	source := "api_query"
	if r.Source == "header" {
		source = "response_header"
	}
	o := cpamp.Observation{ID: "native:" + strconv.FormatInt(r.ID, 10), Account: account, Provider: r.Provider, Kind: kind, Scope: r.Scope, Start: reset - duration, Reset: reset, Duration: duration, Used: &used, Observed: at, Received: at, Source: source, Evidence: []byte(`{}`), Boundary: "native"}
	o.WindowID = cpamp.WindowID(o)
	return o, true
}
