package pacer

import (
	"crypto/rand"
	"strconv"
	"sync"
	"time"
)

// Native resource responses are finite JSON, so use a bounded held GET instead
// of SSE. One shared channel fans out a persisted observation to every viewer;
// inference never waits for a viewer, reads a provider, or processes tokens here.
type usageChanges struct {
	mu       sync.Mutex
	epoch    string
	revision uint64
	changed  chan struct{}
	waiters  int
}

func (u *usageChanges) init() {
	if u.changed == nil {
		u.changed = make(chan struct{})
		u.epoch = rand.Text()
	}
}

func (u *usageChanges) cursor() string {
	return u.epoch + ":" + strconv.FormatUint(u.revision, 10)
}

func (u *usageChanges) notify() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.init()
	u.revision++
	close(u.changed)
	u.changed = make(chan struct{})
}

func (u *usageChanges) wait(after string, duration time.Duration, done <-chan struct{}) (string, bool) {
	u.mu.Lock()
	u.init()
	cursor, changed := u.cursor(), u.changed
	if duration <= 0 || after != cursor {
		u.mu.Unlock()
		return cursor, true
	}
	// The native ABI cannot propagate HTTP cancellation. Bound both lifetime
	// and concurrent held calls, including clients that disconnected early.
	if u.waiters >= 256 {
		u.mu.Unlock()
		return "", false
	}
	u.waiters++
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.waiters--; u.mu.Unlock() }()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-changed:
	case <-timer.C:
	case <-done:
		return "", false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cursor(), true
}
