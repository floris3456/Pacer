package adaptive

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCPAMPContract(t *testing.T) {
	path := os.Getenv("QUOTA_CPAMP_CONTRACT_REPORT")
	if path == "" {
		t.Skip("cross-service fixture supplied by verification script")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var x Exchange
	if err = json.Unmarshal(raw, &x); err != nil {
		t.Fatal(err)
	}
	if x.Protocol != Protocol || len(x.Models) != 1 || !x.Models[0].Valid(x.Generated) {
		t.Fatalf("incompatible CPAMP cost model: %+v", x)
	}
	m := x.Models[0]
	p := m.Profiles[0]
	r := Request{Model: p.Model, Input: 40000, MaxOutput: 3000}
	e := m.Estimate(r, true, x.Generated)
	if !e.Known || e.High < e.Expected || e.Summary <= 0 {
		t.Fatal(e)
	}
	r.Tier = "unseen-priority"
	if m.Estimate(r, true, x.Generated).Known {
		t.Fatal("unknown tier treated as calibrated")
	}
	if m.Estimate(Request{Model: p.Model, Input: 40000}, true, m.Expires).Known {
		t.Fatal("expired calibration admitted")
	}
}

func TestRequestDescriptionDoesNotTreatOpaqueStateAsFree(t *testing.T) {
	for _, body := range []string{`{"previous_response_id":"opaque"}`, `{"input":[{"type":"image_url","image_url":"hidden"}]}`, `not-json`} {
		if !Describe("id", "model", []byte(body)).Opaque {
			t.Fatal(body)
		}
	}
	r := Describe("id", "model", []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":20,"service_tier":"priority"}`))
	if r.Opaque || !r.ByteInput || r.MaxOutput != 20 || r.Tier != "priority" || r.Input <= 2 {
		t.Fatal(r)
	}
}

func TestSerializedCompactionReserveCoversInstalledContextGrowth(t *testing.T) {
	m := Model{Account: "dummy", Version: "v1", Rule: "api-equivalent-v1", Generated: 100, Expires: 400, Profiles: []Profile{{Model: "dummy", Samples: 8, Weights: Weights{Input: 1, Read: .1, Write: 1.25, Output: 4}, InputMean: 2000, OutputMean: 20, OutputHigh: 1000}}}
	primary := m.Estimate(Request{Model: "dummy", Input: 34098, ByteInput: true, MaxOutput: 4096}, true, 100)
	summary := m.Estimate(Request{Model: "dummy", Input: 46576, ByteInput: true, MaxOutput: 4096, Kind: "compaction"}, false, 100)
	if primary.Summary < summary.High {
		t.Fatal("serialized summary overhead not reserved", primary, summary)
	}
	capped := m.Estimate(Request{Model: "dummy", Input: 100, MaxOutput: 1}, false, 100)
	if capped.Expected > capped.High {
		t.Fatal("explicit output cap produced an inverted forecast", capped)
	}
	if _, ok := m.Profile("dummy", "default"); !ok {
		t.Fatal("base tier not matched")
	}
	if _, ok := m.Profile("dummy", "priority"); ok {
		t.Fatal("unknown priced tier matched")
	}
	if _, ok := m.Profile("unmeasured-alias", ""); ok {
		t.Fatal("unknown model alias matched")
	}
}
