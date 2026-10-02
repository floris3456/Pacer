package handoff

import (
	"strings"
	"sync"
	"testing"
)

func TestConcurrentCompactionsSwitchEachChatAfterItsCheckpoint(t *testing.T) {
	for _, window := range []struct {
		name       string
		hour, week float64
	}{{"five-hour", 9, 100}, {"seven-day", 100, 4}} {
		t.Run(window.name, func(t *testing.T) {
			l := setup(t)
			claims := []Claim{claim("a"), claim("b"), claim("c")}
			for _, q := range claims {
				f, err := l.pick(q)
				if err != nil || f.Auth != "small" {
					t.Fatal(f, err)
				}
				l.e.Complete(f.Admission)
			}
			l.observe(t, "small", window.hour, window.week, 118000)
			for i := range claims {
				claims[i].Kind = "compaction"
				claims[i].Generation = 1
				claims[i].Fresh = false
				claims[i].Request = "summary"
				claims[i].Intent = l.snapshot(t, claims[i].Session).Intent
			}
			// Release all callers together and leave all admitted summaries in flight.
			start := make(chan struct{})
			var workers sync.WaitGroup
			for _, q := range claims {
				workers.Add(1)
				go func(q Claim) {
					defer workers.Done()
					<-start
					f, err := l.pick(q)
					if err != nil || f.Auth != "small" || f.Generation != 1 {
						t.Errorf("concurrent summary delayed or rerouted: %+v %v", f, err)
					}
				}(q)
			}
			close(start)
			workers.Wait()
			if t.Failed() {
				t.FailNow()
			}

			l.complete(claims[0])
			a := claims[0]
			a.Kind, a.Request, a.Checkpoint = "primary", "after-a", "persisted-a"
			f, err := l.pick(a)
			if err != nil || f.Auth != "large" || f.Generation != 2 {
				t.Fatal("ready chat waited for other chats", f, err)
			}
			for _, q := range claims[1:] {
				s := l.snapshot(t, q.Session)
				if s.Alias != "Small" || s.Generation != 1 || s.Intent != q.Intent || s.Consumed != "" {
					t.Fatal("another chat switched without acknowledging", s)
				}
				inflight, err := l.pick(q)
				if err != nil || inflight.Auth != "small" {
					t.Fatal("another chat's admitted compaction moved", inflight, err)
				}
			}

			b := claims[1]
			b.Kind, b.Request, b.Checkpoint = "primary", "after-b", "persisted-b"
			b.Intent = a.Intent
			if _, err := l.pick(b); err == nil || !strings.Contains(err.Error(), "quota_checkpoint_required") {
				t.Fatal("another chat's acknowledgement accepted", err)
			}
			b.Intent = claims[1].Intent
			if _, err := l.pick(b); err == nil || !strings.Contains(err.Error(), "quota_request_active") {
				t.Fatal("switched while own compaction was in flight", err)
			}
			l.complete(claims[1])
			b.Checkpoint = ""
			if _, err := l.pick(b); err == nil || !strings.Contains(err.Error(), "quota_checkpoint_required") {
				t.Fatal("request completion substituted for saved checkpoint", err)
			}
			b.Checkpoint = "persisted-b"
			if f, err := l.pick(b); err != nil || f.Auth != "large" || f.Generation != 2 {
				t.Fatal("second chat could not independently switch", f, err)
			}
			if s := l.snapshot(t, "c"); s.Alias != "Small" || s.Generation != 1 {
				t.Fatal("unfinished third chat was moved", s)
			}
		})
	}
}
