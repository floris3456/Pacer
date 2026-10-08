package observation

import (
	"net/http"
	"testing"
	"time"
)

func TestCapturedHeadersNeverReanchorEarlierCountdown(t *testing.T) {
	early := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	late := early.Add(10 * time.Minute)
	h := http.Header{"X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Used-Percent": {"25"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}
	clocks := map[string]time.Time{"X-Codex-Primary-Window-Minutes": early, "X-Codex-Primary-Used-Percent": late, "X-Codex-Primary-Reset-After-Seconds": early}
	w := HeadersCaptured("codex", h, clocks)
	if len(w) != 2 || !w[0].Reset.Equal(early.Add(time.Hour)) || !w[0].ResetEvidence.At.Equal(early) || w[0].Remaining != nil || w[1].Remaining == nil || !w[1].Usage.At.Equal(late) || !w[1].Reset.IsZero() {
		t.Fatal(w)
	}
	delete(clocks, "X-Codex-Primary-Reset-After-Seconds")
	w = HeadersCaptured("codex", h, clocks)
	if len(w) != 1 || !w[0].Reset.IsZero() {
		t.Fatal("missing clock manufactured a countdown reset", w)
	}
}

func TestLaterCapturedScopeCannotRelabelOlderBalance(t *testing.T) {
	early := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Minute)
	h := http.Header{"X-Codex-Active-Limit": {"bengalfox"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Used-Percent": {"25"}}
	clocks := map[string]time.Time{"X-Codex-Active-Limit": late, "X-Codex-Primary-Window-Minutes": late, "X-Codex-Primary-Used-Percent": early}
	if w := HeadersCaptured("codex", h, clocks); len(w) != 0 {
		t.Fatal("new dimensions assigned to old usage", w)
	}
	clocks["X-Codex-Primary-Window-Minutes"] = early
	if w := HeadersCaptured("codex", h, clocks); len(w) != 0 {
		t.Fatal("unknown earlier scope became shared allowance", w)
	}
}
