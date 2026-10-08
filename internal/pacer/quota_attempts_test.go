package pacer

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// quotaClock is an injectable clock. The observation store captures Plugin.now
// during registration, so it must be installed before startQuotaPlugin runs.
type quotaClock struct{ at time.Time }

func (c *quotaClock) Now() time.Time { return c.at }

func startQuotaPlugin(t *testing.T, path string, c *quotaClock) *Plugin {
	t.Helper()
	p := New(testHost)
	p.now = c.Now
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("enabled: true\npriority: 1\nstate_path: " + path + "\ncompact_on_switch: true\nrouting:\n  mode: off\n")})
	return p
}

const quotaObservationsPath = "/v0/management/plugins/pacer/observations"

func quotaGET(t *testing.T, p *Plugin, query string) (int, map[string]any) {
	t.Helper()
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	r := p.manage(pluginapi.ManagementRequest{Method: "GET", Path: quotaObservationsPath, Query: q})
	var body map[string]any
	if err = json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(string(r.Body), err)
	}
	return r.StatusCode, body
}

// quotaAttemptPage fetches one attempt page and fails unless the response is a
// 200 with a non-null attempts array and a boolean attempts_more.
func quotaAttemptPage(t *testing.T, p *Plugin, query string) ([]map[string]any, bool) {
	t.Helper()
	status, body := quotaGET(t, p, query)
	if status != 200 {
		t.Fatal(status, query, body)
	}
	raw, ok := body["attempts"].([]any)
	if !ok {
		t.Fatal("attempts missing or null", query, body)
	}
	more, ok := body["attempts_more"].(bool)
	if !ok {
		t.Fatal("attempts_more missing", query, body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(map[string]any))
	}
	return out, more
}

// quotaMaxAttemptID pages the whole window from zero and returns the largest
// attempt ID visible through the route (0 when there are none).
func quotaMaxAttemptID(t *testing.T, p *Plugin, index string) int64 {
	t.Helper()
	after := int64(0)
	for {
		items, more := quotaAttemptPage(t, p, "auth_index="+index+"&attempts_after="+strconv.FormatInt(after, 10))
		if len(items) > 0 {
			after = int64(items[len(items)-1]["id"].(float64))
		}
		if !more {
			return after
		}
	}
}

// quotaRun acquires the account's due job and finishes it with a 200, the same
// path a real provider call takes after the scheduler authorizes it.
func quotaRun(t *testing.T, p *Plugin, account string) {
	t.Helper()
	job, err := p.observer.Acquire(account)
	if err != nil || job == nil {
		t.Fatal("acquire refused", account, err)
	}
	if err = p.observer.Finish(*job, 200, nil); err != nil {
		t.Fatal(err)
	}
}

// quotaComplete queues one request for the account at a fresh instant, then
// runs it. The clock jump clears seed jitter and per-account minimum gaps.
func quotaComplete(t *testing.T, p *Plugin, clock *quotaClock, account, reason string) {
	t.Helper()
	clock.at = clock.at.Add(48 * time.Hour)
	if err := p.observer.Queue(account, "claude:usage", reason, clock.Now()); err != nil {
		t.Fatal(err)
	}
	quotaRun(t, p, account)
}

func newQuotaClock() *quotaClock {
	return &quotaClock{at: time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC)}
}

func TestQuotaAttemptWindowIsScopedToSelectedCredential(t *testing.T) {
	clock := newQuotaClock()
	p := startQuotaPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), clock)
	defer p.Close()
	a, b := p.creds["a"].Account, p.creds["b"].Account
	quotaComplete(t, p, clock, a, observation.ReasonManual)
	mark := quotaMaxAttemptID(t, p, "ia")
	quotaComplete(t, p, clock, a, observation.ReasonIdle)
	quotaComplete(t, p, clock, b, observation.ReasonIdle)
	items, more := quotaAttemptPage(t, p, "auth_index=ia&attempts_after="+strconv.FormatInt(mark, 10))
	if more || len(items) != 1 {
		t.Fatal("window not scoped to one attempt", items, more)
	}
	item := items[0]
	fields := []string{"id", "endpoint", "reason", "started", "finished", "status"}
	if len(item) != len(fields) {
		t.Fatal("unexpected attempt fields", item)
	}
	for _, k := range fields {
		if _, ok := item[k]; !ok {
			t.Fatal("missing attempt field", k, item)
		}
	}
	if item["endpoint"] != "claude:usage" || item["reason"] != observation.ReasonIdle || item["status"] != float64(200) {
		t.Fatal("wrong attempt", item)
	}
	finished, err := time.Parse(time.RFC3339Nano, item["finished"].(string))
	if err != nil || finished.IsZero() {
		t.Fatal("attempt not finished", item, err)
	}
}

func TestQuotaManualAttemptAppearsInWindow(t *testing.T) {
	clock := newQuotaClock()
	p := startQuotaPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), clock)
	defer p.Close()
	// Registration leaves a pending seed job, and a manual request that joins an
	// earlier-due queued job keeps that job's reason by design. Run the seed
	// first; its idle wake-up is an hour out. Then request manually after the
	// account spacing has elapsed but before that wake-up is due, so the manual
	// request sets its own reason instead of joining the idle job.
	quotaComplete(t, p, clock, p.creds["a"].Account, observation.ReasonSeed)
	mark := quotaMaxAttemptID(t, p, "ia")
	clock.at = clock.at.Add(observation.AccountMinGap + time.Minute)
	if r := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/pacer/observations/reconcile", Body: []byte(`{"auth_index":"ia"}`)}); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	quotaRun(t, p, p.creds["a"].Account)
	items, _ := quotaAttemptPage(t, p, "auth_index=ia&attempts_after="+strconv.FormatInt(mark, 10))
	if len(items) != 1 || items[0]["reason"] != observation.ReasonManual {
		t.Fatal("manual attempt missing from window", items)
	}
}

func TestQuotaWithoutAttemptCursorKeepsResponseShape(t *testing.T) {
	clock := newQuotaClock()
	p := startQuotaPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), clock)
	defer p.Close()
	quotaComplete(t, p, clock, p.creds["a"].Account, observation.ReasonIdle)
	status, body := quotaGET(t, p, "auth_index=ia")
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, k := range []string{"attempts", "attempts_more"} {
		if _, ok := body[k]; ok {
			t.Fatal("attempt key present without cursor", k, body)
		}
	}
}

func TestQuotaAttemptParametersAreStrictlyValidated(t *testing.T) {
	clock := newQuotaClock()
	p := startQuotaPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), clock)
	defer p.Close()
	for _, tc := range []struct{ query, want string }{
		{"attempts_after=-1", "invalid_attempt_cursor"},
		{"attempts_after=01", "invalid_attempt_cursor"},
		{"attempts_after=abc", "invalid_attempt_cursor"},
		{"attempts_after=99999999999999999999", "invalid_attempt_cursor"},
		{"attempts_after=9999999999999999999", "invalid_attempt_cursor"}, // 19 digits, above int64 max
		{"attempts_after=", "invalid_attempt_cursor"},
		{"attempts_after=0&attempts_after=1", "invalid_attempt_cursor"},
		{"attempts_limit=5", "invalid_attempt_cursor"},
		{"attempts_after=0&attempts_limit=0", "invalid_attempt_limit"},
		{"attempts_after=0&attempts_limit=1001", "invalid_attempt_limit"},
		{"attempts_after=0&attempts_limit=01", "invalid_attempt_limit"},
		{"attempts_after=0&attempts_limit=", "invalid_attempt_limit"},
	} {
		status, body := quotaGET(t, p, "auth_index=ia&"+tc.query)
		if status != 400 || body["error"] != tc.want {
			t.Fatal(tc.query, status, body)
		}
		if _, ok := body["attempts"]; ok {
			t.Fatal("refusal carried attempts", tc.query, body)
		}
	}
	// Existing lookup order: credential errors win over attempt parameter errors.
	if status, body := quotaGET(t, p, "auth_index=missing&attempts_after=-1"); status != 404 || body["error"] != "quota_credential_unavailable" {
		t.Fatal(status, body)
	}
}

func TestQuotaAttemptReadsDoNotScheduleWork(t *testing.T) {
	clock := newQuotaClock()
	p := startQuotaPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), clock)
	defer p.Close()
	a := p.creds["a"].Account
	quotaComplete(t, p, clock, a, observation.ReasonIdle)
	before, _, err := p.observer.Status(a)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		quotaAttemptPage(t, p, "auth_index=ia&attempts_after=0&attempts_limit=1000")
		quotaAttemptPage(t, p, "auth_index=ia&attempts_after=0&attempts_limit=1")
	}
	after, _, err := p.observer.Status(a)
	if err != nil || !before.Next.Equal(after.Next) || before.Joined != after.Joined || before.State != after.State {
		t.Fatal(before, after, err)
	}
}

func TestQuotaAttemptPagingFollowsReturnedCursor(t *testing.T) {
	clock := newQuotaClock()
	p := startQuotaPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), clock)
	defer p.Close()
	a := p.creds["a"].Account
	mark := quotaMaxAttemptID(t, p, "ia")
	quotaComplete(t, p, clock, a, observation.ReasonIdle)
	quotaComplete(t, p, clock, a, observation.ReasonIdle)
	first, more := quotaAttemptPage(t, p, "auth_index=ia&attempts_limit=1&attempts_after="+strconv.FormatInt(mark, 10))
	if len(first) != 1 || !more {
		t.Fatal("first page wrong", first, more)
	}
	id1 := int64(first[0]["id"].(float64))
	second, more := quotaAttemptPage(t, p, "auth_index=ia&attempts_limit=1&attempts_after="+strconv.FormatInt(id1, 10))
	if len(second) != 1 || !more {
		t.Fatal("second page wrong", second, more)
	}
	id2 := int64(second[0]["id"].(float64))
	if id2 <= id1 {
		t.Fatal("cursor not ascending", id1, id2)
	}
	empty, more := quotaAttemptPage(t, p, "auth_index=ia&attempts_limit=1&attempts_after="+strconv.FormatInt(id2, 10))
	if len(empty) != 0 || more {
		t.Fatal("window not exhausted", empty, more)
	}
}
