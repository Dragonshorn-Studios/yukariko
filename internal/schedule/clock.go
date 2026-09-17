// Package schedule provides the daemon control plane: per-app polling loops
// with jitter and bounded backoff, an explicit per-app state machine, keyed
// per-app deployment locks shared by scheduled and manual triggers, a global
// concurrency limit, and preflight checks that refuse deployment when the
// host or configuration is not ready.
//
// # Concurrency model
//
//   - Each app runs its own loop goroutine; unrelated apps never block each
//     other.
//   - A global semaphore caps how many apps run their check→deploy pass at
//     once (default 2).
//   - A keyed per-app lock serializes scheduled and manual passes: the
//     scheduled path tries the lock and skips when busy; the manual path
//     queues on it until the caller's deadline.
//   - Shutdown cancels the context; passes in flight are recorded as
//     interrupted and can never advance to succeeded, so a cancelled pass
//     cannot advance the deployed version.
//
// # Ownership
//
// The scheduler owns timing and state only. Source detection (Git in #9,
// registry digests in #10) and deployment pipelines (#11/#12) are the
// Checker and Deployer interfaces; this build ships explicit stub errors
// for both. Nothing here performs network reporting: reporting outages
// cannot block checks or deploys because reporting is not on the critical
// path at all.
package schedule

import "time"

// Clock abstracts time for deterministic tests. Consumers fake it; the
// scheduler only ever waits through this interface.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is the subset of time.Timer the scheduler needs.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// NewRealClock returns a Clock backed by the time package.
func NewRealClock() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer {
	t := time.NewTimer(d)
	return realTimer{t}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }

func (r realTimer) Stop() bool { return r.t.Stop() }
