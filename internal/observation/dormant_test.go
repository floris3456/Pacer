package observation

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func codexUsage(used float64, reset time.Time) []byte {
	return []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":` + strconv.FormatFloat(used, 'f', -1, 64) + `,"reset_at":` + strconv.FormatInt(reset.Unix(), 10) + `}}}`)
}

// Recorded CT104 read (2026-10-13T07:09:09.136Z): 0 % used, reset 0.136 s
// before read time + 7 days. Started windows were at least 52 minutes off.
func TestCodexUnstartedMeterReadIsDormant(t *testing.T) {
	at := instant("2026-10-13T07:09:09.136347327Z")
	got, err := API("codex", []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":0,"reset_at":1792480149}}}`), at)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	w := got[0]
	if !w.Dormant || !w.Reset.IsZero() || w.Remaining == nil || *w.Remaining != 100 || w.StatusEvidence.Raw != "1792480149" || w.StatusEvidence.Source != APISource || !w.Valid() {
		t.Fatalf("unstarted window not dormant: %#v", w)
	}
	if !w.FreshComplete(at.Add(time.Minute), time.Hour) || w.State(at) != "dormant" {
		t.Fatal("dormant reading unusable")
	}
	for name, body := range map[string][]byte{
		"started 52 min ago":   codexUsage(0, at.Add(7*24*time.Hour-52*time.Minute)),
		"used":                 codexUsage(1, at.Add(7*24*time.Hour)),
		"11 s off":             codexUsage(0, at.Add(7*24*time.Hour+11*time.Second)),
		"relative reset only":  []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":0,"reset_after_seconds":604800}}}`),
		"stream event (codex)": []byte(`{"type":"codex.rate_limits","rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":0,"reset_at":` + strconv.FormatInt(at.Add(7*24*time.Hour).Unix(), 10) + `}}}`),
	} {
		got, err := API("codex", body, at)
		if err != nil || len(got) != 1 || got[0].Dormant || got[0].Reset.IsZero() {
			t.Fatalf("%s: %#v %v", name, got, err)
		}
	}
	// A response header always follows a request, which starts the window.
	headers := Headers("codex", http.Header{
		"X-Codex-Primary-Window-Minutes": {"10080"},
		"X-Codex-Primary-Used-Percent":   {"0"},
		"X-Codex-Primary-Reset-At":       {strconv.FormatInt(at.Add(7*24*time.Hour).Unix(), 10)},
	}, at)
	if len(headers) != 1 || headers[0].Dormant {
		t.Fatalf("header read as dormant: %#v", headers)
	}
}

func TestDormantReadingRetiresTheRunningCycle(t *testing.T) {
	s, c, _ := testStore(t)
	start := c.Now()
	running := sample(start, start.Add(5*24*time.Hour), 40)
	running.Usage.Source, running.ResetEvidence.Source = APISource, APISource
	observe(t, s, "account", "codex", running)
	// Codex reset early: the next meter read shows an unstarted window.
	c.Add(time.Hour)
	dormant, err := API("codex", codexUsage(0, c.Now().Add(7*24*time.Hour)), c.Now())
	if err != nil || len(dormant) != 1 || !dormant[0].Dormant {
		t.Fatal(dormant, err)
	}
	observe(t, s, "account", "codex", dormant...)
	if w := s.Windows("account")[0]; !w.Dormant || w.Cycle != "" {
		t.Fatalf("dormant not current: %#v", w)
	}
	// A delayed reading of the replaced cycle, taken after the dormant read, is refused.
	c.Add(time.Minute)
	late := sample(c.Now(), running.Reset, 39)
	observe(t, s, "account", "codex", late)
	if w := s.Windows("account")[0]; !w.Dormant {
		t.Fatalf("retired cycle revived: %#v", w)
	}
	h, _ := s.History("account", 0, 10)
	if len(h) != 3 || h[1].Disposition != "accepted-status" || h[2].Disposition != "stale-cycle" {
		t.Fatalf("audit: %#v", h)
	}
	// The window's first request starts a new cycle.
	c.Add(time.Minute)
	started := sample(c.Now(), c.Now().Add(7*24*time.Hour), 99)
	observe(t, s, "account", "codex", started)
	if w := s.Windows("account")[0]; w.Dormant || !w.Reset.Equal(started.Reset) {
		t.Fatalf("started window not accepted: %#v", w)
	}
}
