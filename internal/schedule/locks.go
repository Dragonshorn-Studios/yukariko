package schedule

import (
	"context"
	"sync"
)

// LockSet is a keyed mutual-exclusion set enforcing that the same app never
// runs overlapping update passes, no matter which path (scheduled loop or
// manual trigger) asks. Unrelated keys are fully independent.
type LockSet struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	held bool
	// wait is closed on every release so queued acquirers re-check.
	wait chan struct{}
}

// NewLockSet returns an empty lock set.
func NewLockSet() *LockSet {
	return &LockSet{locks: map[string]*lockEntry{}}
}

// TryAcquire takes the lock without waiting. The returned release func must
// be called exactly once. ok=false means the key is currently held.
func (l *LockSet) TryAcquire(key string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(key)
	if e.held {
		return nil, false
	}
	e.held = true
	return l.releaseFor(key), true
}

// Acquire waits for the lock, returning when it is taken or ctx ends. The
// manual update path uses this to queue behind a scheduled pass.
func (l *LockSet) Acquire(ctx context.Context, key string) (release func(), err error) {
	for {
		l.mu.Lock()
		e := l.entry(key)
		if !e.held {
			e.held = true
			l.mu.Unlock()
			return l.releaseFor(key), nil
		}
		wait := e.wait
		l.mu.Unlock()

		select {
		case <-wait: // released; loop and re-check
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (l *LockSet) entry(key string) *lockEntry {
	e := l.locks[key]
	if e == nil {
		e = &lockEntry{wait: make(chan struct{})}
		l.locks[key] = e
	}
	return e
}

// releaseFor captures the entry lookup at acquire time; release closes the
// current wait channel so all queued acquirers wake and re-check.
func (l *LockSet) releaseFor(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			e := l.locks[key]
			if e == nil || !e.held {
				return // double release is a caller bug; stay inert
			}
			e.held = false
			close(e.wait)
			e.wait = make(chan struct{})
		})
	}
}
