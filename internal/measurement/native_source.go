package measurement

import "time"

type nativeSourceEvidence struct {
	Scope             SourceScope `json:"scope"`
	ManagerID         string      `json:"manager_incarnation_id"`
	Tracked           bool        `json:"tracked"`
	RequestedAt       time.Time   `json:"requested_at"`
	UpstreamStartedAt *time.Time  `json:"upstream_started_at"`
	FinalUsageAt      time.Time   `json:"final_usage_at"`
	Gap               *struct {
		From       *time.Time `json:"from"`
		Through    *time.Time `json:"through"`
		ObservedAt time.Time  `json:"observed_at"`
	} `json:"gap"`
}

func applyNativeSource(r *Record, n nativeEnvelope, c NativeCredential, source string) error {
	e := n.SourceEvidence
	if e == nil {
		return nil
	}
	if !nativeID(e.ManagerID) || !validTime(e.RequestedAt) || !e.RequestedAt.Equal(n.RequestedAt) || !validTime(e.FinalUsageAt) || e.FinalUsageAt.Before(r.EndedAt) {
		return nativeError("invalid_source_clock_or_incarnation")
	}
	if e.Scope.Provider != n.Provider || e.Scope.AuthID != n.AuthID || e.Scope.AuthIndex != n.AuthIndex || e.Scope.AuthID != "" && e.Scope.AuthID != c.AuthID || e.Scope.AuthIndex != "" && e.Scope.AuthIndex != c.AuthIndex {
		return nativeError("source_credential_identity_mismatch")
	}
	valid := ValidSourceScope(e.Scope)
	if e.Tracked && !valid {
		return nativeError("unsupported_tracked_source")
	}
	meta := &NativeSource{ManagerID: e.ManagerID, Tracked: e.Tracked, RequestedAt: utc(e.RequestedAt), FinalUsageAt: utc(e.FinalUsageAt), Kind: "unsupported"}
	if valid {
		meta.SourceRef = SourceReference(source, e.Scope)
		meta.Kind = e.Scope.Kind
		meta.Version = e.Scope.Version
	}
	if e.UpstreamStartedAt != nil {
		at := utc(*e.UpstreamStartedAt)
		if !validTime(at) || at.Before(e.RequestedAt) || at.After(e.FinalUsageAt) {
			return nativeError("invalid_source_dispatch_clock")
		}
		meta.UpstreamStartedAt = &at
		if e.Tracked {
			// Managed policy validation precedes the RoundTrip observation, so its
			// earlier timestamp need not be equal. A reversed ordering is invalid.
			if knownStart(*r) && r.StartedAt.After(at) {
				return nativeError("conflicting_dispatch_clocks")
			}
			r.StartedAt = at
			r.StartTimeBasis = "upstream"
			r.Policy.RequestStartedAt = at
			r.Native.Dispatched = true
		}
	}
	if e.Gap != nil {
		meta.Gap = true
		if e.Gap.From != nil {
			if !validTime(*e.Gap.From) || e.Gap.From.After(e.FinalUsageAt) {
				return nativeError("invalid_source_gap_clock")
			}
			at := utc(*e.Gap.From)
			meta.InvalidFrom = &at
		}
		if !validTime(e.Gap.ObservedAt) || e.Gap.Through != nil && (!validTime(*e.Gap.Through) || e.Gap.From != nil && e.Gap.Through.Before(*e.Gap.From)) {
			return nativeError("invalid_source_gap_clock")
		}
	}
	r.Native.SourceEvidence = meta
	r.EndedAt = utc(e.FinalUsageAt)
	r.EndTimeBasis = "source_final_usage_observation"
	r.Native.Diagnostics = append(r.Native.Diagnostics, "source_coverage_uses_requested_at", "physical_carry_in_unproven")
	return nil
}
