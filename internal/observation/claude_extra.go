package observation

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// ClaudeExtraUsage records only the explicit extra-usage setting. Enabled extra
// usage may remain available after subscription exhaustion; usage counters do
// not establish a remaining balance or an inference guarantee.
func ClaudeExtraUsage(body []byte, at time.Time) Credits {
	c := Credits{State: "unknown", ReadMS: at.UnixMilli(), ExpiresMS: at.Add(CreditMaxAge).UnixMilli(), Source: APISource}
	var v struct {
		Extra *struct {
			Enabled *bool `json:"is_enabled"`
		} `json:"extra_usage"`
	}
	if json.Unmarshal(body, &v) != nil || v.Extra == nil || v.Extra.Enabled == nil {
		return c
	}
	c.State = "empty"
	if *v.Extra.Enabled {
		c.State = "available"
	}
	return c
}

// ClaudeExtraHeaders reads overage evidence independently of shared and Fable
// statuses. Missing evidence must not refresh an earlier reading. Contradictory
// explicit fields are unknown, regardless of header order or capitalization.
func ClaudeExtraHeaders(headers http.Header, at time.Time) (Credits, bool) {
	c := Credits{State: "unknown", ReadMS: at.UnixMilli(), ExpiresMS: at.Add(CreditMaxAge).UnixMilli(), Source: HeaderSource}
	available, empty, ambiguous := false, false, false
	for name, values := range headers {
		switch strings.ToLower(name) {
		case "anthropic-ratelimit-unified-overage-status":
			if len(values) == 0 {
				ambiguous = true
			}
			for _, value := range values {
				for _, status := range strings.Split(value, ",") {
					switch strings.ToLower(strings.TrimSpace(status)) {
					case "allowed", "allowed_warning":
						available = true
					case "rejected":
						empty = true
					default:
						ambiguous = true
					}
				}
			}
		case "anthropic-ratelimit-unified-overage-disabled-reason":
			for _, value := range values {
				reason := strings.TrimSpace(value)
				if reason == "" {
					continue
				}
				if len(reason) > 512 || strings.ContainsAny(reason, "\r\n\x00,") {
					ambiguous = true
					continue
				}
				empty = true
			}
		}
	}
	if !available && !empty && !ambiguous {
		return c, false
	}
	if !ambiguous && available != empty {
		c.State = "empty"
		if available {
			c.State = "available"
		}
	}
	return c, true
}
