package pacer

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUnchangedDiscoveryPreservesScopedMeasurementReadback(t *testing.T) {
	h := newCoverageHost(2)
	p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
	h.clock.Store(hookEpoch.Add(time.Minute).UnixNano())
	p.refreshMeasurements()
	if len(p.measurements) != 2 {
		t.Fatal("expected two background reports")
	}
	generation := p.coverageGeneration
	calls := len(h.checkpointCalls)
	// A real management read refreshes inventory after five seconds. An unchanged
	// list must not erase the background report immediately before rendering it.
	h.clock.Store(h.now().Add(6 * time.Second).UnixNano())
	response := p.manage(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/pacer/state"})
	var body struct {
		Accounts []struct {
			Measurement *struct {
				Coverage struct {
					Observed bool `json:"observed"`
				} `json:"coverage"`
			} `json:"measurement"`
		} `json:"accounts"`
	}
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &body) != nil || len(body.Accounts) != 2 {
		t.Fatal("state read failed")
	}
	for _, account := range body.Accounts {
		if account.Measurement == nil || account.Measurement.Coverage.Observed {
			t.Fatal("unchanged discovery erased scoped unknown report or fabricated coverage")
		}
	}
	var diagnostics struct {
		Measurement struct {
			Delivery   string `json:"delivery"`
			Checkpoint struct {
				Domain   string `json:"domain"`
				Calls    uint64 `json:"calls"`
				Complete bool   `json:"account_membership_complete"`
				Physical bool   `json:"physical_carry_in_proven"`
			} `json:"source_checkpoint"`
		} `json:"measurement"`
	}
	if json.Unmarshal(response.Body, &diagnostics) != nil || diagnostics.Measurement.Delivery != "native-acknowledged-scoped" || diagnostics.Measurement.Checkpoint.Domain != "request_start_only" || diagnostics.Measurement.Checkpoint.Calls != uint64(calls) || diagnostics.Measurement.Checkpoint.Complete || diagnostics.Measurement.Checkpoint.Physical {
		t.Fatal("native diagnostics overstated coverage or lost scope")
	}
	if p.coverageGeneration != generation || len(h.checkpointCalls) != calls {
		t.Fatal("read advanced proof or invalidated unchanged inventory")
	}
}

func TestChangedOrFailedDiscoveryStillInvalidatesReports(t *testing.T) {
	for _, mode := range []string{"removed", "billing", "unverified", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			h := newCoverageHost(2)
			p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
			h.clock.Store(hookEpoch.Add(time.Minute).UnixNano())
			p.refreshMeasurements()
			if len(p.measurements) != 2 {
				t.Fatal("background reports missing")
			}
			generation := p.coverageGeneration
			h.mu.Lock()
			switch mode {
			case "removed":
				h.files = h.files[:1]
			case "billing":
				h.billing[h.files[0].AuthIndex] = "different-billing-account"
			case "unverified":
				delete(h.billing, h.files[0].AuthIndex)
			case "unavailable":
				h.listFailure = true
			}
			h.mu.Unlock()
			p.discover(true)
			if len(p.measurements) != 0 || p.coverageGeneration <= generation {
				t.Fatal("changed or failed inventory retained stale report")
			}
		})
	}
}
