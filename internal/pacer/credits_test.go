package pacer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCreditUpgradeLeavesExistingMeasurementSchemaUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	p.Close()
	// The preceding release's main schema is identical, without a credit file.
	if err := os.Remove(path + ".credits.sqlite"); err != nil {
		t.Fatal(err)
	}
	witness := func() (int64, int64) {
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var version, saved, tables int64
		if err := db.QueryRow("PRAGMA schema_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT schema_cookie FROM measurement_report_meta WHERE id=1").Scan(&saved); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='observation_credits'").Scan(&tables); err != nil || tables != 0 {
			t.Fatal(tables, err)
		}
		return version, saved
	}
	before, saved := witness()
	if before != saved {
		t.Fatal("invalid initial witness", before, saved)
	}
	p = startPlugin(t, path)
	if p.credits == nil {
		t.Fatal("credit store missing")
	}
	c := observation.CodexCredits([]byte(`{"credits":{"has_credits":true,"unlimited":false}}`), p.now())
	if _, err := p.credits.ObserveCredits("account", c); err != nil {
		t.Fatal(err)
	}
	if _, err := p.measurement.ReportFeedHead(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p = startPlugin(t, path)
	defer p.Close()
	if got := p.readCredits("account"); got != c {
		t.Fatal(got)
	}
	if version, saved := witness(); version != before || saved != before {
		t.Fatal("upgrade changed guarded schema", version, saved)
	}
	if _, err := p.measurement.ReportFeedHead(context.Background()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + ".credits.sqlite"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}

func TestCreditStoreFailureKeepsStartupAndUnknownCreditStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	if err := os.Mkdir(path+".credits.sqlite", 0700); err != nil {
		t.Fatal(err)
	}
	p := startPlugin(t, path)
	defer p.Close()
	if p.credits != nil || p.creditDB != nil {
		t.Fatal("unexpected store")
	}
	c := credential{ID: "codex", Account: "credit-fixture", Provider: "codex"}
	encoded, ok := p.quotaReplyValue(p.observer, c, "gpt-fixture")
	if !ok {
		t.Fatal("missing header")
	}
	reply := decodeQuota(t, http.Header{quotaReplyHeader: {encoded}})
	if reply.Credits == nil || reply.Credits.State != "unknown" || reply.Credits.ReadMS != 0 {
		t.Fatal(reply)
	}
	if _, err := p.measurement.ReportFeedHead(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClosedCreditStoreDoesNotHideQuotasAndConcurrentCloseIsSafe(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	c := credential{ID: "codex", Account: "credit-fixture", Provider: "codex"}
	windows, err := observation.API("codex", []byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1800000000}}}`), p.now())
	if err != nil {
		t.Fatal(err)
	}
	p.storeObservations(c, windows)
	if err := p.creditDB.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, ok := p.quotaReplyValue(p.observer, c, "gpt-fixture")
	if !ok {
		t.Fatal("optional credit failure removed quota header")
	}
	reply := decodeQuota(t, http.Header{quotaReplyHeader: {encoded}})
	if reply.Credits == nil || reply.Credits.State != "unknown" || len(reply.Windows) != 1 || *reply.Windows[0].Left != 0 {
		t.Fatal(reply)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				_ = p.readCredits(c.Account)
			}
		})
	}
	p.Close()
	wg.Wait()
	if got := p.readCredits(c.Account); got.State != "unknown" || got.ReadMS != 0 {
		t.Fatal(got)
	}
}

func TestScheduledCreditsReachScopedUsageAndHeadersWithoutAdditionalReads(t *testing.T) {
	f := newRoutingFixture(t, "dry-run", false)
	p := f.p
	c := credential{ID: "codex", Index: "codex-index", Provider: "codex", Account: "codex-account", Alias: "Codex"}
	p.creds[c.ID] = c
	alias := c
	alias.ID = "hidden-alias"
	p.creds[alias.ID] = alias
	at := p.now()
	body := []byte(fmt.Sprintf(`{"credits":{"has_credits":true,"unlimited":false,"balance":"50157.8642475000"},"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":%d}}}`, at.Add(24*time.Hour).Unix()))
	reads := 0
	p.Host = func(method string, input any) (json.RawMessage, error) {
		switch method {
		case "host.auth.get_runtime":
			return json.Marshal(pluginapi.HostAuthGetRuntimeResponse{Auth: pluginapi.HostAuthFileEntry{ID: c.ID}})
		case "host.auth.get":
			return json.Marshal(map[string]any{"json": map[string]string{"access_token": "dummy-token", "account_id": c.Account}})
		case "host.http.operation_open":
			return json.Marshal(map[string]string{"operation_id": "fixture-operation"})
		case "host.http.do":
			reads++
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: body})
		default:
			return testHost(method, input)
		}
	}
	if err := p.observer.Queue(c.Account, observation.Endpoint(c.Provider), observation.ReasonSeed, at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	job, err := p.observer.Acquire(c.Account)
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	cursor := p.usageChanges.cursor()
	p.retrieve(c, *job)
	if p.usageChanges.cursor() == cursor {
		t.Fatal("credit update did not notify readers")
	}
	checkHeader := func(want string) {
		t.Helper()
		encoded, ok := p.quotaReplyValue(p.observer, c, "gpt-fixture")
		if !ok {
			t.Fatal("missing quota header")
		}
		v := decodeQuota(t, http.Header{quotaReplyHeader: {encoded}})
		if v.Credits == nil || v.Credits.State != want || v.Credits.ReadMS != at.UnixMilli() || len(v.Windows) != 1 || *v.Windows[0].Left != 0 {
			t.Fatal(v)
		}
	}
	checkHeader("available")
	if got := p.readCredits(c.Account); got.Balance != "50157.8642475" {
		t.Fatal("provider balance did not reach the existing quota observation", got)
	}
	// A credit-only successful refresh must wake readers even without windows.
	f.clock.Add(int64(31 * time.Minute))
	job, err = p.observer.Acquire(c.Account)
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	fullBody := body
	body = []byte(`{"credits":{"has_credits":true,"unlimited":false}}`)
	cursor = p.usageChanges.cursor()
	beforeWindows, err := p.observer.ReadWindows(c.Account)
	if err != nil {
		t.Fatal(err)
	}
	p.retrieve(c, *job)
	if p.usageChanges.cursor() == cursor {
		t.Fatal("credit-only update did not notify readers")
	}
	afterWindows, err := p.observer.ReadWindows(c.Account)
	credits := p.readCredits(c.Account)
	if err != nil || !reflect.DeepEqual(beforeWindows, afterWindows) || credits.ReadMS != p.now().UnixMilli() {
		t.Fatal("credit-only read changed windows or lost credit evidence", err, credits)
	}
	body = fullBody
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer allowed-key" && r.Header.Get("Authorization") != "Bearer denied-key" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer auth.Close()
	p.cfg.UsageAuthURL = auth.URL + "/v1/models"
	p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: clientCaller("allowed-key"), AuthIDs: []string{c.ID}}, {CallerScope: clientCaller("denied-key"), AuthIDs: []string{}}}}
	for _, key := range []string{"allowed-key", "denied-key"} {
		for i := 0; i < 3; i++ {
			r := p.events(pluginapi.ManagementRequest{Method: "GET", Headers: http.Header{"Authorization": {"Bearer " + key}}, Query: url.Values{"auth": {"api-key"}, "view": {"usage"}, "v": {"1"}, "all": {"true"}}})
			if key == "denied-key" {
				if r.StatusCode != 403 {
					t.Fatal("ungranted caller not refused", r.StatusCode)
				}
				continue
			}
			var view struct {
				Credentials []struct {
					ID      string               `json:"id"`
					Credits *observation.Credits `json:"credits"`
					Shared  []string             `json:"shared_with"`
				} `json:"credentials"`
			}
			if r.StatusCode != 200 || json.Unmarshal(r.Body, &view) != nil {
				t.Fatal(r.StatusCode, string(r.Body))
			}
			if len(view.Credentials) != 1 || view.Credentials[0].ID != c.ID || view.Credentials[0].Credits == nil || view.Credentials[0].Credits.State != "available" || len(view.Credentials[0].Shared) != 0 {
				t.Fatal(view)
			}
		}
	}
	for _, state := range []string{"available", "empty", "unlimited", "unknown", "stale"} {
		f.clock.Add(int64(time.Millisecond))
		sample := observation.Credits{State: state, ReadMS: p.now().UnixMilli(), Source: "api"}
		if state == "stale" {
			sample.State, sample.ReadMS = "available", at.Add(-observation.CreditMaxAge).UnixMilli()
			c.Account = "stale-account"
			p.creds[c.ID] = c
		}
		if _, err := p.credits.ObserveCredits(c.Account, sample); err != nil {
			t.Fatal(err)
		}
		input := routingPreviewInput{Caller: clientCaller("allowed-key"), Provider: "codex", Model: "gpt-fixture", Candidates: []string{c.ID, alias.ID, "a"}}
		if state == "empty" {
			old, _, _ := p.routingInputsLocked(input, p.now())
			if len(old) != 1 || !old[0].QuotaExhaustionUncertain {
				t.Fatal("new empty credits combined with old subscription exhaustion", old)
			}
		}
		windows, _ := observation.API("codex", body, p.now())
		p.storeObservations(c, windows)
		accounts, _, _ := p.routingInputsLocked(input, p.now())
		if len(accounts) != 1 || accounts[0].QuotaExhaustionUncertain != (state != "empty") {
			t.Fatal(state, accounts)
		}
		d := routing.Pick(routing.Default(), p.now(), routing.Chat{Account: c.Account}, accounts)
		if state != "empty" && (!d.Handled || d.Forced || d.Auth != c.ID) {
			t.Fatal(state, d)
		}
	}
	if reads != 2 {
		t.Fatal("metadata reads triggered provider I/O", reads)
	}
}
