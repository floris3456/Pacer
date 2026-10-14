package pacer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// capacityHost models the host side of both contracts: a global CPA delivery
// sequence with per-scope quiescent cuts, and the inventory checkpoint RPC.
type capacityHost struct {
	mu         sync.Mutex
	clock      atomic.Int64
	seq        uint64
	lower      map[string]*pluginapi.UsagePhysicalCut
	closed     map[string]time.Time
	inventory  string // "", "unsupported", "unknown"
	invSeq     uint64
	binding    uint64
	accountRef string
	usage      [2]float64 // five_hour, seven_day utilization
	gap        bool
	httpCalls  int
	methods    []string
	onHTTP     func()
}

func newCapacityHost() *capacityHost {
	h := &capacityHost{seq: 10, lower: map[string]*pluginapi.UsagePhysicalCut{}, closed: map[string]time.Time{}, invSeq: 5, binding: 1, accountRef: "acct-ref-A"}
	h.clock.Store(hookEpoch.UnixNano())
	return h
}

func (h *capacityHost) now() time.Time          { return time.Unix(0, h.clock.Load()).UTC() }
func (h *capacityHost) advance(d time.Duration) { h.clock.Add(int64(d)) }

func (h *capacityHost) call(method string, input any) (json.RawMessage, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	switch method {
	case "host.auth.list":
		h.mu.Unlock()
		return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{{ID: "auth-000", AuthIndex: "index-000", Provider: "claude"}}})
	case "host.auth.get":
		h.mu.Unlock()
		return json.Marshal(map[string]any{"json": map[string]string{"account_uuid": "billing-A", "access_token": "dummy-fixture-token"}})
	case "host.auth.get_runtime":
		h.mu.Unlock()
		return json.Marshal(pluginapi.HostAuthGetRuntimeResponse{Auth: pluginapi.HostAuthFileEntry{ID: "auth-000"}})
	case "host.http.operation_open":
		h.mu.Unlock()
		return json.Marshal(map[string]string{"operation_id": "owned-fixture-operation"})
	case "host.http.do":
		h.httpCalls++
		hook := h.onHTTP
		h.onHTTP = nil
		h.mu.Unlock()
		if hook != nil {
			hook()
		}
		h.mu.Lock()
		body, _ := json.Marshal(map[string]any{
			"five_hour": map[string]any{"utilization": h.usage[0], "resets_at": hookEpoch.Add(4 * time.Hour).Format(time.RFC3339)},
			"seven_day": map[string]any{"utilization": h.usage[1], "resets_at": hookEpoch.Add(96 * time.Hour).Format(time.RFC3339)},
		})
		h.mu.Unlock()
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: body})
	case pluginabi.MethodHostUsageCheckpoint:
		defer h.mu.Unlock()
		s := input.(pluginapi.HostUsageCheckpointRequest).Scope
		h.seq++
		now := h.now()
		from := hookEpoch.Add(-time.Hour)
		r := pluginapi.HostUsageCheckpointResponse{Scope: s, Coverage: "known", ManagerIncarnationID: "manager-test", IncarnationID: "sink-test", Sequence: h.seq, From: &from, Through: &now, Gaps: []pluginapi.UsageDeliveryGap{}}
		p := &pluginapi.UsagePhysicalCheckpoint{Version: 1, Domain: pluginapi.ClaudePhysicalProofDomain, Coverage: "unknown"}
		if closed, ok := h.closed[s.AuthID]; ok {
			p.LastProducerClosedAt = &closed
		}
		cut := &pluginapi.UsagePhysicalCut{Sequence: h.seq, At: now}
		switch lower := h.lower[s.AuthID]; {
		case h.gap:
			p.GapCount, p.Reason = 1, "physical_incarnation_gap"
		case lower == nil:
			h.lower[s.AuthID] = cut
			p.FromCut, p.Reason = cut, "first_quiescent_cut"
		case !now.After(lower.At):
			p.FromCut, p.Reason = lower, "nonpositive_cut_interval"
		default:
			p.FromCut, p.ThroughCut, p.Coverage = lower, cut, "known"
		}
		r.Physical = p
		return json.Marshal(r)
	case pluginabi.MethodHostAuthInventoryCheckpoint:
		defer h.mu.Unlock()
		switch h.inventory {
		case "unsupported":
			return nil, errors.New("unsupported host method " + method)
		case "unknown":
			return json.Marshal(pluginapi.HostAuthInventoryCheckpointResponse{Version: 1, Provider: "claude", Coverage: "unknown", Reason: "overflow", ManagerIncarnationID: "inventory-test", Sequence: h.invSeq, At: h.now(), Tuples: []pluginapi.HostAuthInventoryTuple{}})
		}
		return json.Marshal(pluginapi.HostAuthInventoryCheckpointResponse{Version: 1, Provider: "claude", Coverage: "known", ManagerIncarnationID: "inventory-test", Sequence: h.invSeq, At: h.now(), Tuples: []pluginapi.HostAuthInventoryTuple{{AuthID: "auth-000", AuthIndex: "index-000", AccountRef: h.accountRef, BindingSequence: h.binding, State: "active"}}})
	}
	h.mu.Unlock()
	return []byte(`{}`), nil
}

func openCapacityPlugin(t *testing.T, h *capacityHost, config string) *Plugin {
	t.Helper()
	p := New(h.call)
	p.now = h.now
	p.workerOnce.Do(func() {})
	path := filepath.Join(t.TempDir(), "state.sqlite")
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("enabled: true\nstate_path: " + path + "\n" + config)})
	t.Cleanup(p.Close)
	return p
}

// dispatch delivers one native, physically sequenced final record for auth-000.
func (h *capacityHost) dispatch(t *testing.T, p *Plugin, id string) {
	t.Helper()
	c := p.creds["auth-000"]
	admitCoverage(t, p, id, c.ID)
	h.mu.Lock()
	admission, dispatch := h.seq+1, h.seq+2
	h.seq += 2
	h.mu.Unlock()
	requested := h.now()
	u := hookUsage(c, id, id+"-physical", requested)
	e := u["SourceEvidence"].(pluginapi.UsageSourceEvidence)
	e.Physical = &pluginapi.UsagePhysicalDispatch{Version: 1, AdmissionSequence: admission, DispatchSequence: dispatch, StartedAt: e.UpstreamStartedAt.Add(time.Nanosecond)}
	u["SourceEvidence"] = e
	h.advance(2 * time.Second)
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	h.mu.Lock()
	h.closed[c.ID] = e.FinalUsageAt
	h.mu.Unlock()
}

// sample runs the observer's own scheduled provider read once.
func (h *capacityHost) sample(t *testing.T, p *Plugin, fiveHour, sevenDay float64) {
	t.Helper()
	h.mu.Lock()
	h.usage = [2]float64{fiveHour, sevenDay}
	before := h.httpCalls
	h.mu.Unlock()
	p.reconcile()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.httpCalls != before+1 {
		t.Fatal("scheduled provider read did not run exactly once", h.httpCalls-before)
	}
}

type capacityReadback struct {
	Measurement struct {
		CapacityKnown *bool `json:"capacity_known"`
		Checkpoint    struct {
			Complete *bool `json:"account_membership_complete"`
			Physical *bool `json:"physical_carry_in_proven"`
		} `json:"source_checkpoint"`
	} `json:"measurement"`
	Accounts []struct {
		PaceUnit string                     `json:"pace_unit"`
		Capacity map[string]json.RawMessage `json:"capacity"`
	} `json:"accounts"`
}

type capacityObject struct {
	Status        string     `json:"status"`
	Reasons       []string   `json:"reasons"`
	Exclusivity   string     `json:"exclusivity"`
	CapacityKnown *bool      `json:"capacity_known"`
	Window        string     `json:"window"`
	From          *time.Time `json:"interval_from"`
	To            *time.Time `json:"interval_to"`
	Age           *int64     `json:"age_seconds"`
	Basis         string     `json:"basis"`
	Value         *struct {
		LowerBound float64 `json:"lower_bound_usd_per_percent"`
		CPAValue   float64 `json:"cpa_value_usd"`
		Delta      float64 `json:"delta_percent"`
		Quantum    float64 `json:"quantum_percent"`
		Records    int     `json:"records"`
	} `json:"value"`
	History []json.RawMessage `json:"history"`
}

func readCapacity(t *testing.T, p *Plugin) (capacityReadback, capacityObject) {
	t.Helper()
	response := p.manage(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/pacer/state"})
	var state capacityReadback
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &state) != nil || len(state.Accounts) != 1 {
		t.Fatal("state read failed", response.StatusCode, string(response.Body))
	}
	raw, _ := json.Marshal(state.Accounts[0].Capacity)
	var object capacityObject
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if object.CapacityKnown == nil || *object.CapacityKnown || object.Exclusivity != "unproven" || state.Measurement.CapacityKnown == nil || *state.Measurement.CapacityKnown {
		t.Fatal("capacity_known must be present and false", string(raw))
	}
	if c := state.Measurement.Checkpoint; c.Complete == nil || *c.Complete || c.Physical == nil || *c.Physical {
		t.Fatal("existing readback fields changed", string(response.Body))
	}
	if state.Accounts[0].PaceUnit != "%/h" {
		t.Fatal("capacity lower bound reached routing", state.Accounts[0].PaceUnit)
	}
	return state, object
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// Lower cut, sandwiched sample s1, one dispatch, then sandwiched sample s2
// more than an hour later (the observer's idle interval).
func admissibleCapacity(t *testing.T, h *capacityHost, p *Plugin) {
	t.Helper()
	h.advance(time.Second)
	p.refreshSourceCoverage()
	h.advance(6 * time.Minute)
	h.sample(t, p, 10, 20)
	h.advance(time.Minute)
	h.dispatch(t, p, "between-samples")
	h.advance(63 * time.Minute)
	h.sample(t, p, 13, 21)
}

func TestCapacityLowerBoundFromSandwichedProviderSamples(t *testing.T) {
	h := newCapacityHost()
	p := openCapacityPlugin(t, h, "")
	if _, object := readCapacity(t, p); object.Status != "unknown" || object.Value != nil {
		t.Fatal("capacity before any interval", object)
	}
	admissibleCapacity(t, h, p)
	p.refreshMeasurements()
	_, object := readCapacity(t, p)
	if object.Status != "lower_bound" || len(object.Reasons) != 0 || object.Value == nil {
		t.Fatal("admissible interval not projected", object)
	}
	// One 1,165,000 nano-USD record; seven-day Δ 1% with a 1% quantum.
	if object.Window != "all:604800" || math.Abs(object.Value.LowerBound-0.0005825) > 1e-12 || object.Value.CPAValue != 0.001165 || object.Value.Delta != 1 || object.Value.Quantum != 1 || object.Value.Records != 1 {
		t.Fatal("lower bound value", object.Window, *object.Value)
	}
	if object.Basis != "sandwiched_cuts_provider_settle_assumed" || object.From == nil || object.To == nil || !object.To.After(*object.From) || object.Age == nil || *object.Age != 0 {
		t.Fatal("interval labels", object)
	}
	if len(object.History) != 1 || !strings.Contains(string(object.History[0]), `"all:18000"`) || !strings.Contains(string(object.History[0]), `"lower_bound"`) {
		t.Fatal("five-hour interval not reported beside", object.History)
	}
	h.mu.Lock()
	calls := h.httpCalls
	h.mu.Unlock()
	if calls != 2 {
		t.Fatal("sandwich added provider calls", calls)
	}
	// Each newer interval is reported beside, never averaged into, the older.
	h.advance(5 * time.Minute)
	h.dispatch(t, p, "second-interval")
	h.advance(65 * time.Minute)
	h.sample(t, p, 15, 23)
	p.refreshMeasurements()
	h.advance(30 * time.Second)
	_, newer := readCapacity(t, p)
	if newer.Status != "lower_bound" || newer.Value == nil || math.Abs(newer.Value.LowerBound-0.001165/3) > 1e-12 || *newer.Age != 30 || len(newer.History) != 3 {
		t.Fatal("newer interval", newer)
	}
	if !strings.Contains(string(newer.History[1]), fmt.Sprint(0.0005825)) {
		t.Fatal("older interval value lost or averaged", string(newer.History[1]))
	}
}

func TestCapacityLateGapRevokesLowerBound(t *testing.T) {
	h := newCapacityHost()
	p := openCapacityPlugin(t, h, "")
	admissibleCapacity(t, h, p)
	p.refreshMeasurements()
	if _, object := readCapacity(t, p); object.Status != "lower_bound" {
		t.Fatal("fixture", object)
	}
	h.mu.Lock()
	h.gap = true
	h.mu.Unlock()
	h.advance(time.Second)
	p.refreshMeasurements()
	_, object := readCapacity(t, p)
	if object.Status != "unknown" || object.Value != nil || !contains(object.Reasons, "carry_in_unproven") || !contains(object.Reasons, "physical_incarnation_gap") {
		t.Fatal("late invalidation did not revoke", object)
	}
}

func TestCapacityDispatchDuringProviderSampleIsNotSandwiched(t *testing.T) {
	h := newCapacityHost()
	p := openCapacityPlugin(t, h, "")
	h.advance(time.Second)
	p.refreshSourceCoverage()
	h.advance(6 * time.Minute)
	h.mu.Lock()
	h.onHTTP = func() {
		h.advance(time.Second)
		h.dispatch(t, p, "during-sample")
		h.advance(time.Second)
	}
	h.mu.Unlock()
	h.sample(t, p, 10, 20)
	h.advance(64 * time.Minute)
	h.sample(t, p, 13, 21)
	p.refreshMeasurements()
	_, object := readCapacity(t, p)
	if object.Status != "unknown" || !contains(object.Reasons, "dispatch_between_cuts") {
		t.Fatal("dispatch inside the sandwich accepted", object)
	}
}

func TestCapacityOldHostOrUnknownInventoryStaysUnknown(t *testing.T) {
	for _, mode := range []string{"unsupported", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			h := newCapacityHost()
			h.inventory = mode
			p := openCapacityPlugin(t, h, "")
			admissibleCapacity(t, h, p)
			p.refreshMeasurements()
			_, object := readCapacity(t, p)
			if object.Status != "unknown" || object.Value != nil || !contains(object.Reasons, "membership_unproven") || !contains(object.Reasons, "inventory_unknown") {
				t.Fatal("membership without inventory", object)
			}
			// Point discovery stays incomplete exactly as before.
			m, err := p.measurement.SourceMembership(t.Context(), p.creds["auth-000"].CoverageMembership)
			if err != nil || m.Complete {
				t.Fatal("point discovery promoted", m, err)
			}
			if p.measurementErrors != 0 {
				t.Fatal("old host produced errors", p.measurementErrors)
			}
		})
	}
}

func TestCapacityMembershipChangeInsideSplits(t *testing.T) {
	h := newCapacityHost()
	p := openCapacityPlugin(t, h, "")
	h.advance(time.Second)
	p.refreshSourceCoverage()
	h.advance(6 * time.Minute)
	h.sample(t, p, 10, 20)
	h.advance(time.Minute)
	h.dispatch(t, p, "between-samples")
	h.mu.Lock()
	h.invSeq, h.binding = 9, 8 // token-independent rebind of the tuple
	h.mu.Unlock()
	h.advance(63 * time.Minute)
	h.sample(t, p, 13, 21)
	p.refreshMeasurements()
	if _, object := readCapacity(t, p); object.Status != "unknown" || !contains(object.Reasons, "binding_changed_inside") {
		t.Fatal("binding change inside accepted", object)
	}
	h.advance(5 * time.Minute)
	h.dispatch(t, p, "after-change")
	h.advance(65 * time.Minute)
	h.sample(t, p, 15, 23)
	p.refreshMeasurements()
	if _, object := readCapacity(t, p); object.Status != "lower_bound" {
		t.Fatal("interval after the change not judged separately", object)
	}
}

func TestCapacityMutationGuardNoSettingSeedsSizeOrKnown(t *testing.T) {
	for _, config := range []string{"capacity_known: true\n", "capacity:\n  size: 100\n", "plan: max-20\n", "routing:\n  mode: off\n  capacity: 5\n", "cache_policy:\n  capacity_ratio: 3\n", "size: 1\n", "declared_capacity: 9\n"} {
		p := New(newCapacityHost().call)
		_, err := p.Handle(pluginabi.MethodPluginRegister, hookRaw(t, map[string]any{"config_yaml": []byte("state_path: " + filepath.Join(t.TempDir(), "s.sqlite") + "\n" + config)}))
		p.Close()
		if err == nil {
			t.Fatal("capacity-like setting accepted", config)
		}
	}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.ToLower(f.Name + "|" + f.Tag.Get("yaml"))
			for _, banned := range []string{"capacity", "size", "plan", "tier", "multiplier", "known", "declared"} {
				if strings.Contains(name, banned) {
					t.Fatal("configuration can seed capacity", path+f.Name)
				}
			}
			typ := f.Type
			if typ.Kind() == reflect.Slice {
				typ = typ.Elem()
			}
			if typ.Kind() == reflect.Struct {
				walk(typ, path+f.Name+".")
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	for _, config := range []string{"", "routing:\n  mode: dry-run\n", "compact_on_switch: true\npriority: 7\n"} {
		h := newCapacityHost()
		p := openCapacityPlugin(t, h, config)
		admissibleCapacity(t, h, p)
		p.refreshMeasurements()
		readCapacity(t, p) // asserts capacity_known false and routing unchanged
	}
}

func TestCapacityInventoryProvenance(t *testing.T) {
	root := "../../third_party/cpa-sdk"
	raw, err := os.ReadFile(filepath.Join(root, "INVENTORY-PROVENANCE.json"))
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		Commit string            `json:"commit"`
		Files  map[string]string `json:"files"`
	}
	// Pinned to the landed CPA commit (full 40-hex SHA), never a placeholder.
	if err = json.Unmarshal(raw, &provenance); err != nil || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(provenance.Commit) || len(provenance.Files) != 2 {
		t.Fatal("inventory provenance", provenance, err)
	}
	for path, want := range provenance.Files {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatal("inventory SDK file changed", path)
		}
	}
	if pluginabi.MethodHostAuthInventoryCheckpoint != "host.auth.inventory_checkpoint" {
		t.Fatal(pluginabi.MethodHostAuthInventoryCheckpoint)
	}
}

// Lab regression (run 2b capacity window): the host's first cut of the sink
// was taken before this consumer existed, so the consumer never sees
// first_quiescent_cut. It must adopt its own first known cut and still reach a
// lower bound, never borrowing the host's earlier cut.
func TestCapacityLowerBoundWhenHostFirstCutPredatesConsumer(t *testing.T) {
	h := newCapacityHost()
	h.lower["auth-000"] = &pluginapi.UsagePhysicalCut{Sequence: 3, At: hookEpoch.Add(-2 * time.Hour)}
	p := openCapacityPlugin(t, h, "")
	admissibleCapacity(t, h, p)
	p.refreshMeasurements()
	_, object := readCapacity(t, p)
	if object.Status != "lower_bound" || len(object.Reasons) != 0 || object.Value == nil || object.Value.Records != 1 {
		t.Fatal("host first cut before the consumer blocked capacity", object)
	}
}
