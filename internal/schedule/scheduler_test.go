package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// --- fake clock -------------------------------------------------------------

// fakeClock is a deterministic Clock: time moves only via Advance, which
// fires timers in deadline order.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, when: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves time forward, firing every timer whose deadline falls at or
// before the new instant, in deadline order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if t.fired {
				continue
			}
			if next == nil || t.when.Before(next.when) {
				next = t
			}
		}
		if next == nil || next.when.After(target) {
			break
		}
		c.now = next.when
		next.fired = true
		next.ch <- next.when
	}
	c.now = target
	c.mu.Unlock()
}

type fakeTimer struct {
	clock *fakeClock
	when  time.Time
	ch    chan time.Time
	fired bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.fired {
		return false
	}
	t.fired = true
	return true
}

// --- fakes and helpers ------------------------------------------------------

var startTime = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingSink) RecordAppEvent(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *recordingSink) all() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func (s *recordingSink) count(kind string, appID string) int {
	n := 0
	for _, e := range s.all() {
		if e.Kind == kind && (appID == "" || e.AppID == appID) {
			n++
		}
	}
	return n
}

type countingChecker struct {
	mu      sync.Mutex
	counts  map[string]int
	changed bool
	err     error
	// gate, when set, runs inside Check after the in-flight bookkeeping.
	gate func(ctx context.Context, app *config.App)
}

func newCountingChecker() *countingChecker {
	return &countingChecker{counts: map[string]int{}}
}

func (c *countingChecker) Check(ctx context.Context, app *config.App) (CheckResult, error) {
	c.mu.Lock()
	c.counts[app.ID]++
	changed, err := c.changed, c.err
	c.mu.Unlock()
	if c.gate != nil {
		c.gate(ctx, app)
	}
	if err != nil {
		return CheckResult{}, err
	}
	return CheckResult{Changed: changed, Detail: "checked"}, nil
}

func (c *countingChecker) calls(appID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[appID]
}

func (c *countingChecker) setChanged(changed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changed, c.err = changed, err
}

type countingDeployer struct {
	mu      sync.Mutex
	counts  map[string]int
	success bool
}

func newCountingDeployer(success bool) *countingDeployer {
	return &countingDeployer{counts: map[string]int{}, success: success}
}

func (d *countingDeployer) Deploy(_ context.Context, app *config.App) (DeployResult, error) {
	d.mu.Lock()
	d.counts[app.ID]++
	success := d.success
	d.mu.Unlock()
	return DeployResult{Success: success, Detail: "deployed"}, nil
}

func okPreflight(dataDir string) *Preflight {
	return &Preflight{
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		Exec:     func(context.Context, string, []string) error { return nil },
		DataDir:  dataDir,
	}
}

func testApp(id string, interval time.Duration) *config.App {
	return &config.App{
		ID:       id,
		Interval: config.Duration(interval),
		Retry: config.Retry{
			Base: config.Duration(time.Minute),
			Max:  config.Duration(4 * time.Minute),
		},
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "app:1"}}},
		},
		Deploy: config.Deploy{
			Mode: config.DeployStandalone,
			Standalone: &config.StandaloneSpec{
				Image: "app:1",
				Name:  id + "-container",
			},
		},
	}
}

// zeroJitter disables jitter for deterministic fake-clock tests.
func zeroJitter(time.Duration) time.Duration { return 0 }

// newTestScheduler builds a scheduler with deterministic jitter unless the
// test overrides it.
func newTestScheduler(opts Options) *Scheduler {
	if opts.Jitter == nil {
		opts.Jitter = zeroJitter
	}
	return New(opts)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, what)
}

// --- fake-clock scheduler tests ---------------------------------------------

func TestScheduledChecksFireWithIntervalPlusJitter(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	checker := newCountingChecker()
	sink := &recordingSink{}
	s := newTestScheduler(Options{
		Apps:  []*config.App{testApp("a", 10*time.Minute)},
		Clock: clock,
		Check: checker,
		Sink:  sink,
		Jitter: func(base time.Duration) time.Duration {
			return 2 * time.Minute
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	// First pass at interval + jitter = 12m.
	clock.Advance(11 * time.Minute)
	if checker.calls("a") != 0 {
		t.Fatal("check fired before interval+jitter elapsed")
	}
	clock.Advance(1 * time.Minute)
	waitFor(t, 2*time.Second, "first check", func() bool { return checker.calls("a") == 1 })
	waitFor(t, 2*time.Second, "back to succeeded", func() bool {
		st, _ := s.Status("a")
		return st.State == StateSucceeded
	})

	// Second pass at +10m interval (jitter hook is per-delay, fixed here).
	clock.Advance(12 * time.Minute)
	waitFor(t, 2*time.Second, "second check", func() bool { return checker.calls("a") == 2 })

	cancel()
	if err := s.Run(ctx); err != nil { // Run returns after ctx done
		t.Fatalf("Run: %v", err)
	}
	if n := sink.count(EventState, "a"); n == 0 {
		t.Error("no state transitions recorded")
	}
}

func TestBackoffBoundsAndReset(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	checker := newCountingChecker()
	checker.setChanged(false, errors.New("registry unreachable"))
	s := newTestScheduler(Options{
		Apps:   []*config.App{testApp("a", 10*time.Minute)},
		Clock:  clock,
		Check:  checker,
		Jitter: func(time.Duration) time.Duration { return 0 },
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	// First pass at the interval: it fails and schedules backoff.
	clock.Advance(10 * time.Minute)
	waitFor(t, 2*time.Second, "failure pass 1", func() bool { return checker.calls("a") == 1 })
	st, _ := s.Status("a")
	if st.State != StateBackoff || st.Failures != 1 {
		t.Fatalf("after pass 1: state %q failures %d", st.State, st.Failures)
	}
	lastNext := st.NextAttempt
	lastCount := 1

	// Each retry fires exactly when the backoff elapses, doubling up to max
	// (pass 1 already consumed the 1m base delay). Advance exactly to each
	// deadline so the fake clock never overshoots a pass in flight.
	for _, step := range []struct {
		advance, wantDelay time.Duration
	}{
		{time.Minute, 2 * time.Minute},
		{2 * time.Minute, 4 * time.Minute},
		{4 * time.Minute, 4 * time.Minute},
		{4 * time.Minute, 4 * time.Minute},
	} {
		clock.Advance(step.advance)
		want := lastCount + 1
		waitFor(t, 2*time.Second, fmt.Sprintf("failure pass %d", want), func() bool {
			return checker.calls("a") >= want
		})
		st, _ := s.Status("a")
		if st.State != StateBackoff {
			t.Fatalf("pass %d: state %q, want backoff", want, st.State)
		}
		if got := st.NextAttempt.Sub(lastNext); got != step.wantDelay {
			t.Errorf("pass %d: backoff delay %s, want %s", want, got, step.wantDelay)
		}
		lastCount = want
		lastNext = st.NextAttempt
	}

	// Recovery resets the backoff: the next success clears failures and
	// restores plain-interval polling.
	checker.setChanged(false, nil)
	clock.Advance(4 * time.Minute)
	waitFor(t, 2*time.Second, "recovery pass", func() bool {
		st, _ := s.Status("a")
		return st.State == StateSucceeded && st.Failures == 0
	})
	if st, _ := s.Status("a"); !st.NextAttempt.IsZero() {
		t.Errorf("NextAttempt = %v after success, want zero", st.NextAttempt)
	}
	cancel()
}

func TestManualAndScheduledPassesNeverOverlap(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	var inFlight, maxInFlight int32
	release := make(chan struct{})
	var firstBlocked atomic.Bool
	checker := &countingChecker{counts: map[string]int{}, changed: true}
	checker.gate = func(_ context.Context, _ *config.App) {
		// Only the first pass blocks; the queued manual pass runs freely.
		if firstBlocked.CompareAndSwap(false, true) {
			<-release
		}
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		atomic.AddInt32(&inFlight, -1)
	}
	deployer := newCountingDeployer(true)
	s := newTestScheduler(Options{
		Apps:        []*config.App{testApp("a", time.Hour)},
		Clock:       clock,
		Check:       checker,
		Deploy:      deployer,
		Preflight:   okPreflight(t.TempDir()),
		Concurrency: 4,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	// Fire the scheduled pass; it blocks inside Check holding the lock.
	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "scheduled pass in flight", func() bool {
		return checker.calls("a") == 1
	})

	// The manual trigger must queue on the app lock, not overlap.
	done := make(chan error, 1)
	go func() { done <- s.Trigger(ctx, "a") }()

	select {
	case err := <-done:
		t.Fatalf("Trigger returned while the scheduled pass held the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	waitFor(t, 2*time.Second, "manual pass completed", func() bool {
		return checker.calls("a") == 2
	})
	if err := <-done; err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Errorf("max in-flight passes = %d, want 1: same-app passes must never overlap", got)
	}
	cancel()
}

func TestScheduledSkipsWhenManualHoldsLock(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	sink := &recordingSink{}
	var inFlight, maxInFlight int32
	release := make(chan struct{})
	checker := &countingChecker{counts: map[string]int{}}
	checker.gate = func(_ context.Context, _ *config.App) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		<-release
		atomic.AddInt32(&inFlight, -1)
	}
	s := newTestScheduler(Options{
		Apps:        []*config.App{testApp("a", time.Hour)},
		Clock:       clock,
		Check:       checker,
		Sink:        sink,
		Preflight:   okPreflight(t.TempDir()),
		Concurrency: 4,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	done := make(chan error, 1)
	go func() { done <- s.Trigger(ctx, "a") }()
	waitFor(t, 2*time.Second, "manual pass in flight", func() bool {
		return checker.calls("a") == 1
	})

	// The scheduled tick fires while the manual pass holds the lock and
	// must skip, not queue.
	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "skip_locked event", func() bool {
		return sink.count(EventSkipLocked, "a") == 1
	})
	if n := checker.calls("a"); n != 1 {
		t.Errorf("checks = %d, want 1 (scheduled pass skipped)", n)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Errorf("max in-flight = %d, want 1", got)
	}
	cancel()
}

func TestGlobalConcurrencyLimit(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	var inFlight, maxInFlight int32
	release := make(chan struct{})
	checker := newCountingChecker()
	checker.gate = func(_ context.Context, _ *config.App) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		<-release
		atomic.AddInt32(&inFlight, -1)
	}
	var apps []*config.App
	for _, id := range []string{"a", "b", "c", "d"} {
		apps = append(apps, testApp(id, time.Hour))
	}
	s := newTestScheduler(Options{
		Apps:        apps,
		Clock:       clock,
		Check:       checker,
		Concurrency: 2,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "two apps in flight", func() bool {
		return atomic.LoadInt32(&inFlight) == 2
	})
	if got := atomic.LoadInt32(&maxInFlight); got > 2 {
		t.Errorf("max in-flight = %d, want <= 2", got)
	}

	close(release)
	waitFor(t, 2*time.Second, "all four apps checked", func() bool {
		total := 0
		for _, id := range []string{"a", "b", "c", "d"} {
			total += checker.calls(id)
		}
		return total == 4
	})
	if got := atomic.LoadInt32(&maxInFlight); got != 2 {
		t.Errorf("max in-flight = %d, want exactly 2", got)
	}
	cancel()
}

func TestAppsProgressIndependently(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	releaseA := make(chan struct{})
	checker := newCountingChecker()
	checker.gate = func(ctx context.Context, app *config.App) {
		if app.ID == "a" {
			<-releaseA
		}
	}
	s := newTestScheduler(Options{
		Apps:        []*config.App{testApp("a", time.Hour), testApp("b", time.Hour)},
		Clock:       clock,
		Check:       checker,
		Concurrency: 2,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "app b succeeded while a blocked", func() bool {
		st, ok := s.Status("b")
		return ok && st.State == StateSucceeded
	})
	if st, _ := s.Status("a"); st.State == StateSucceeded {
		t.Error("app a must still be blocked")
	}
	close(releaseA)
	waitFor(t, 2*time.Second, "app a done", func() bool {
		st, _ := s.Status("a")
		return st.State == StateSucceeded
	})
	cancel()
}

func TestCancellationNeverRecordsSuccess(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	sink := &recordingSink{}
	checker := newCountingChecker()
	checker.changed = true
	// The checker waits for cancellation, then reports as if done.
	checker.gate = func(ctx context.Context, _ *config.App) {
		<-ctx.Done()
	}
	// A lying deployer: claims success even though the pass was cancelled.
	deployer := deployerFunc(func(_ context.Context, _ *config.App) (DeployResult, error) {
		return DeployResult{Success: true, Detail: "deployed"}, nil
	})
	s := newTestScheduler(Options{
		Apps:        []*config.App{testApp("a", time.Hour)},
		Clock:       clock,
		Check:       checker,
		Deploy:      deployer,
		Preflight:   okPreflight(t.TempDir()),
		Sink:        sink,
		Concurrency: 2,
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = s.Run(ctx); close(runDone) }()
	<-s.Ready()

	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "pass in flight", func() bool {
		st, _ := s.Status("a")
		return st.State == StateChecking
	})
	cancel()
	<-runDone

	st, _ := s.Status("a")
	if st.State != StateInterrupted {
		t.Fatalf("state = %q, want interrupted; a cancelled pass must never succeed", st.State)
	}
	for _, e := range sink.all() {
		if e.AppID == "a" && e.Kind == EventState && e.To == StateSucceeded {
			t.Error("succeeded transition recorded after cancellation")
		}
		if e.AppID == "a" && e.Kind == EventState && e.To == StateDeploying {
			t.Error("deploying transition recorded, but cancellation hit during checking")
		}
	}
}

func TestShutdownInterruptsInFlightAndStopsWork(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	checker := newCountingChecker()
	checker.gate = func(ctx context.Context, _ *config.App) {
		<-ctx.Done()
	}
	s := newTestScheduler(Options{
		Apps:  []*config.App{testApp("a", time.Hour)},
		Clock: clock,
		Check: checker,
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = s.Run(ctx); close(runDone) }()
	<-s.Ready()

	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "pass in flight", func() bool {
		st, _ := s.Status("a")
		return st.State == StateChecking
	})
	cancel()
	<-runDone
	waitFor(t, 2*time.Second, "interrupted by shutdown", func() bool {
		st, _ := s.Status("a")
		return st.State == StateInterrupted
	})

	// No new work after shutdown.
	clock.Advance(10 * time.Hour)
	if n := checker.calls("a"); n != 1 {
		t.Errorf("checks = %d after shutdown, want 1", n)
	}
}

func TestPreflightRefusalBlocksDeploy(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	sink := &recordingSink{}
	checker := newCountingChecker()
	checker.changed = true
	deployer := newCountingDeployer(true)
	s := newTestScheduler(Options{
		Apps:   []*config.App{testApp("a", time.Hour)},
		Clock:  clock,
		Check:  checker,
		Deploy: deployer,
		Preflight: &Preflight{
			LookPath: func(name string) (string, error) { return "", errors.New("no docker here") },
			Exec:     func(context.Context, string, []string) error { return nil },
		},
		Sink: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "preflight refusal recorded", func() bool {
		return sink.count(EventPreflightRefused, "a") == 1
	})
	waitFor(t, 2*time.Second, "backing off", func() bool {
		st, _ := s.Status("a")
		return st.State == StateBackoff
	})
	if deployer.counts["a"] != 0 {
		t.Error("deployer must not run when preflight refuses")
	}
	st, _ := s.Status("a")
	if !strings.Contains(st.Detail, "retry in") {
		t.Errorf("detail = %q, want backoff hint", st.Detail)
	}
	cancel()
}

func TestStubCheckerProducesExplicitFailure(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	s := newTestScheduler(Options{
		Apps:  []*config.App{testApp("a", time.Hour)},
		Clock: clock,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "stub failure recorded", func() bool {
		st, _ := s.Status("a")
		return st.State == StateBackoff
	})
	st, _ := s.Status("a")
	if !strings.Contains(st.Detail, "not implemented") {
		t.Errorf("detail = %q, want the explicit stub error", st.Detail)
	}
	cancel()
}

func TestTriggerUnknownApp(t *testing.T) {
	t.Parallel()
	s := newTestScheduler(Options{Apps: []*config.App{testApp("a", time.Hour)}, Clock: newFakeClock(startTime)})
	if err := s.Trigger(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown app")
	}
}

func TestReloadRerunsPreflightAndSwapsApps(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	sink := &recordingSink{}
	checker := newCountingChecker()
	s := newTestScheduler(Options{
		Apps:      []*config.App{testApp("a", time.Hour)},
		Clock:     clock,
		Check:     checker,
		Preflight: okPreflight(t.TempDir()),
		Sink:      sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	<-s.Ready()

	s.Reload(ctx, []*config.App{testApp("a", time.Hour), testApp("b", time.Hour)})
	waitFor(t, 2*time.Second, "preflight events for both apps", func() bool {
		return sink.count(EventPreflightPassed, "a") == 1 && sink.count(EventPreflightPassed, "b") == 1
	})

	clock.Advance(time.Hour)
	waitFor(t, 2*time.Second, "new app b checked", func() bool {
		return checker.calls("b") == 1
	})

	// Removing app a: its loop exits on the next tick and nothing checks it.
	s.Reload(ctx, []*config.App{testApp("b", time.Hour)})
	clock.Advance(2 * time.Hour)
	if n := checker.calls("a"); n != 1 {
		t.Errorf("removed app a checked %d times, want 1", n)
	}
	cancel()
}

// deployerFunc adapts a function to Deployer.
type deployerFunc func(ctx context.Context, app *config.App) (DeployResult, error)

func (f deployerFunc) Deploy(ctx context.Context, app *config.App) (DeployResult, error) {
	return f(ctx, app)
}
