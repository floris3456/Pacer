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
	ID        int64     `json:"id"`
	Account   string    `json:"account"` // Pacer's native account ID
	Provider  string    `json:"provider"`
	Scope     string    `json:"scope"`
	Seconds   int64     `json:"seconds"`
	Remaining *float64  `json:"remaining"`
	Reset     time.Time `json:"reset"`
	At        time.Time `json:"at"`     // source time of the reading (response or API answer)
	Source    string    `json:"source"` // header, api or event
}

// nativeKind names a reading's window. Monthly meters (OpenCode Go) report a
// calendar-month window whose length varies between 28 and 31 days.
func nativeKind(provider string, seconds int64) string {
	switch {
	case seconds == 5*3600:
		return "5h"
	case seconds == 7*86400:
		return "weekly"
	case seconds == 86400:
		return "daily"
	case seconds >= 28*86400 && seconds <= 31*86400:
		return "monthly"
	}
	return ""
}

// nativeObservation converts a reading into CPAMP's observation shape for the
// given (legacy or native) account ID, in its canonical pool. Readings without
// a usage value, a reset, a known window length or a valid time are not meter
// readings.
func nativeObservation(r NativeReading, account string) (cpamp.Observation, bool) {
	kind := nativeKind(r.Provider, r.Seconds)
	if kind == "" || r.Remaining == nil || math.IsNaN(*r.Remaining) || math.IsInf(*r.Remaining, 0) || *r.Remaining < 0 || *r.Remaining > 100 || r.Reset.IsZero() || r.At.IsZero() || r.Scope == "" || account == "" {
		return cpamp.Observation{}, false
	}
	reset, at := r.Reset.UnixMilli(), r.At.UnixMilli()
	start := reset - r.Seconds*1000
	if kind == "monthly" {
		start = r.Reset.UTC().AddDate(0, -1, 0).UnixMilli()
	}
	used := 100 - *r.Remaining
	// Header readings get CPAMP's provider-date clock margin; API answers are
	// poll receipts, exactly as CPAMP treated its own API queries.
	source := "api_query"
	if r.Source == "header" {
		source = "response_header"
	}
	o := cpamp.Observation{ID: "native:" + strconv.FormatInt(r.ID, 10), Account: account, Provider: r.Provider, Kind: kind, Scope: canonicalScope(r.Provider, r.Scope), Start: start, Reset: reset, Duration: reset - start, Used: &used, Observed: at, Received: at, Source: source, Evidence: []byte(`{}`), Boundary: "native"}
	o.WindowID = cpamp.WindowID(o)
	return o, true
}

// readingAccount puts a native reading on the account its requests use: the
// legacy account when an origin certificate covers the native account at the
// reading's time, otherwise the native account reference the forward ledger
// gives that account's requests. Readings are per account, not per credential,
// so certificates that disagree about the legacy account make it ambiguous.
func readingAccount(o ForwardOrigins, source string, r NativeReading) (string, bool) {
	at := r.At.UnixMilli()
	legacy := ""
	for _, c := range o.Certificates {
		if c.Provider != r.Provider || c.NativeAccount != r.Account || at < c.StartMS || c.EndMS != 0 && at >= c.EndMS {
			continue
		}
		if legacy != "" && legacy != c.LegacyAccount {
			return "", false
		}
		legacy = c.LegacyAccount
	}
	if legacy != "" {
		return legacy, true
	}
	return ForwardAccountRef(source, r.Provider, r.Account), true
}
