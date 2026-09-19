package schedule

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// DefaultConcurrency caps how many apps run a check→deploy pass at once.
const DefaultConcurrency = 2

// DefaultManualTimeout bounds how long a manual trigger queues for the app
// lock when another pass holds it.
const DefaultManualTimeout = time.Minute

// jitterDivisor is the fraction of the base delay used as maximum jitter by
// the default jitter hook: up to 1/10 of the interval or backoff.
const jitterDivisor = 10

// Checker reports whether a newer version of an app is available. The Git
// implementation lands in #9 and the registry implementation in #10; this
// build's stub always errors.
type Checker interface {
	Check(ctx context.Context, app *config.App) (CheckResult, error)
}

// CheckResult is one source-check outcome. Changed=true means the observed
// source differs from the deployed version; Observed is informational only
// and never a deploy checkpoint.
type CheckResult struct {
	Changed  bool
	Observed string
	Detail   string
}

// Deployer performs one deployment pipeline pass for an app. The Compose
// and standalone implementations land in #11/#12; this build's stub always
// errors.
type Deployer interface {
	Deploy(ctx context.Context, app *config.App) (DeployResult, error)
}

// DeployResult reports the pipeline outcome. Success=true is only believed
// while the context is alive — a cancelled pass is always recorded as
// interrupted and can never advance the deployed version.
type DeployResult struct {
	Success bool
	Detail  string
}

// Options configures a Scheduler. Apps must come from a validated config.
type Options struct {
	Apps        []*config.App
	DataDir     string
	Clock       Clock // nil → real clock
	Check       Checker
	Deploy      Deployer
	Preflight   *Preflight // nil → default preflight
	Sink        EventSink  // optional; nil drops events
	Concurrency int        // default 2
	// Jitter returns extra delay added to an interval or backoff delay.
	// nil → random 0..1/10 of the base delay.
	Jitter func(base time.Duration) time.Duration
	// ManualTimeout bounds the manual trigger's lock wait; default 1m.
	ManualTimeout time.Duration
}

// AppStatus is the observable snapshot of one app's control-plane state.
type AppStatus struct {
	AppID       string
	State       State
	Since       time.Time
	LastSuccess time.Time
	Failures    int // consecutive failed passes
	NextAttempt time.Time
	Detail      string
}

// Scheduler is the daemon control plane. Construct with New, run with Run,
// trigger manually with Trigger, observe with Status/Statuses, and hot-swap
// configuration with Reload.
type Scheduler struct {
	opts      Options
	clock     Clock
	locks     *LockSet
	sem       chan struct{}
	preflight *Preflight

	mu       sync.Mutex
	apps     map[string]*config.App
	statuses map[string]*AppStatus
	backoffs map[string]*Backoff
	running  map[string]bool
	wg       sync.WaitGroup

	readyOnce sync.Once
	ready     chan struct{} // closed once the loops are started
}

// Ready is closed after Run (or the first Reload) has started the loops.
// Tests and embedders use it to synchronize against the clock.
func (s *Scheduler) Ready() <-chan struct{} { return s.ready }

// New builds a scheduler over the given apps. Missing Check/Deploy
// interfaces become explicit stubs that fail every pass, so the control
// plane stays observable even before #9–#12 land.
func New(opts Options) *Scheduler {
	if opts.Clock == nil {
		opts.Clock = NewRealClock()
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.ManualTimeout <= 0 {
		opts.ManualTimeout = DefaultManualTimeout
	}
	if opts.Jitter == nil {
		opts.Jitter = defaultJitter
	}
	if opts.Preflight == nil {
		opts.Preflight = &Preflight{DataDir: opts.DataDir}
	}
	s := &Scheduler{
		ready:     make(chan struct{}),
		opts:      opts,
		clock:     opts.Clock,
		locks:     NewLockSet(),
		sem:       make(chan struct{}, opts.Concurrency),
		preflight: opts.Preflight,
		apps:      map[string]*config.App{},
		statuses:  map[string]*AppStatus{},
		backoffs:  map[string]*Backoff{},
		running:   map[string]bool{},
	}
	s.adopt(opts.Apps)
	return s
}

// adopt registers apps and their bookkeeping.
func (s *Scheduler) adopt(apps []*config.App) {
	for _, app := range apps {
		if app == nil || app.ID == "" {
			continue
		}
		s.apps[app.ID] = app
		if _, ok := s.statuses[app.ID]; !ok {
			s.statuses[app.ID] = &AppStatus{AppID: app.ID, State: StateIdle, Since: s.clock.Now()}
		}
		if _, ok := s.backoffs[app.ID]; !ok {
			b := NewBackoff(app.Retry.Base.D(), app.Retry.Max.D())
			b.Jitter = s.opts.Jitter
			s.backoffs[app.ID] = b
		}
	}
}

// stubChecker is the explicit seam stub until #9/#10 land.
type stubChecker struct{}

func (stubChecker) Check(context.Context, *config.App) (CheckResult, error) {
	return CheckResult{}, errors.New("source check not implemented: git detection lands in #9, registry digest resolution in #10")
}

// stubDeployer is the explicit seam stub until #11/#12 land.
type stubDeployer struct{}

func (stubDeployer) Deploy(context.Context, *config.App) (DeployResult, error) {
	return DeployResult{}, errors.New("deployment not implemented: compose pipeline lands in #11, standalone recreation in #12")
}

func (s *Scheduler) checker() Checker {
	if s.opts.Check == nil {
		return stubChecker{}
	}
	return s.opts.Check
}

func (s *Scheduler) deployer() Deployer {
	if s.opts.Deploy == nil {
		return stubDeployer{}
	}
	return s.opts.Deploy
}

// Run starts one loop per app and blocks until ctx is cancelled. On
// shutdown no new work is started and passes in flight are recorded as
// interrupted. It returns nil on clean shutdown.
func (s *Scheduler) Run(ctx context.Context) error {
	s.startLoops(ctx)
	s.readyOnce.Do(func() { close(s.ready) })
	<-ctx.Done()
	s.wg.Wait()
	s.markInterrupted()
	return nil
}

// startLoops launches loops for every registered app that has none. It
// returns only after every new loop has created its first timer, so a
// caller that advances the clock afterwards cannot race loop startup.
func (s *Scheduler) startLoops(ctx context.Context) {
	s.mu.Lock()
	var started []chan struct{}
	for id := range s.apps {
		if s.running[id] {
			continue
		}
		s.running[id] = true
		ch := make(chan struct{})
		started = append(started, ch)
		s.wg.Add(1)
		go s.loop(ctx, id, ch)
	}
	s.mu.Unlock()
	for _, ch := range started {
		<-ch
	}
}

// loop is one app's polling goroutine. It exits when ctx is cancelled or
// the app disappears in a reload. The started channel is closed after the
// first timer exists.
func (s *Scheduler) loop(ctx context.Context, id string, started chan struct{}) {
	defer s.wg.Done()
	app, ok := s.app(id)
	if !ok {
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
		close(started)
		return
	}
	delay := s.nextDelay(id, app)
	timer := s.clock.NewTimer(delay)
	close(started)
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		app, ok = s.app(id)
		if !ok {
			s.mu.Lock()
			delete(s.running, id)
			s.mu.Unlock()
			return
		}
		release, ok := s.locks.TryAcquire(id)
		if !ok {
			s.record(Event{AppID: id, Kind: EventSkipLocked, Detail: "scheduled pass skipped; another pass holds the app lock"})
		} else {
			s.pass(ctx, app, "scheduled")
			release()
		}
		if ctx.Err() != nil {
			return
		}
		timer = s.clock.NewTimer(s.nextDelay(id, app))
	}
}

// Trigger requests an immediate manual update pass. It queues on the app
// lock (bounded by ManualTimeout and ctx) so a scheduled pass can finish
// first; the scheduled path, in turn, never queues.
func (s *Scheduler) Trigger(ctx context.Context, appID string) error {
	app, ok := s.app(appID)
	if !ok {
		return fmt.Errorf("unknown app %q", appID)
	}
	s.record(Event{AppID: appID, Kind: EventManualTrigger, Detail: "manual update requested"})

	waitCtx := ctx
	if s.opts.ManualTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, s.opts.ManualTimeout)
		defer cancel()
	}
	release, ok := s.locks.TryAcquire(appID)
	if !ok {
		s.record(Event{AppID: appID, Kind: EventManualTrigger, Detail: "manual pass queued behind a running pass"})
		var err error
		if release, err = s.locks.Acquire(waitCtx, appID); err != nil {
			return fmt.Errorf("app %q is busy: %w", appID, err)
		}
	}
	defer release()
	s.pass(ctx, app, "manual")
	return nil
}

// Reload swaps the app set (hot config reload) and re-runs preflight for
// every app, recording findings as events. Loops for removed apps exit on
// their next tick; loops for added apps start immediately.
func (s *Scheduler) Reload(ctx context.Context, apps []*config.App) {
	s.adopt(apps)
	live := map[string]bool{}
	for _, app := range apps {
		if app == nil || app.ID == "" {
			continue
		}
		live[app.ID] = true
		findings := s.preflight.Run(ctx, app)
		if len(findings) > 0 {
			details := make([]string, len(findings))
			for i, f := range findings {
				details[i] = f.String()
			}
			s.record(Event{AppID: app.ID, Kind: EventPreflightRefused, Detail: strings.Join(details, "; ")})
		} else {
			s.record(Event{AppID: app.ID, Kind: EventPreflightPassed, Detail: "config reload: preflight passed"})
		}
	}
	s.mu.Lock()
	for id := range s.apps {
		if !live[id] {
			delete(s.apps, id)
		}
	}
	needStart := false
	for id := range s.apps {
		if !s.running[id] {
			needStart = true
		}
	}
	s.mu.Unlock()
	if needStart && ctx.Err() == nil {
		s.startLoops(ctx)
	}
}

// Status returns the snapshot for one app.
func (s *Scheduler) Status(appID string) (AppStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.statuses[appID]
	if !ok {
		return AppStatus{}, false
	}
	return *st, true
}

// Statuses returns snapshots for all apps, sorted by ID.
func (s *Scheduler) Statuses() []AppStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AppStatus, 0, len(s.statuses))
	for _, st := range s.statuses {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AppID < out[j].AppID })
	return out
}

// pass runs one full update attempt for an app. The per-app lock is already
// held by the caller.
func (s *Scheduler) pass(ctx context.Context, app *config.App, origin string) {
	// Global concurrency gate: at most N apps work at once; the rest queue
	// here, independently of their per-app lock.
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		s.transition(app.ID, StateInterrupted, "cancelled while waiting for a concurrency slot")
		return
	}
	defer func() { <-s.sem }()

	s.transition(app.ID, StateChecking, "update pass started ("+origin+")")

	res, err := s.checker().Check(ctx, app)
	if ctx.Err() != nil {
		s.transition(app.ID, StateInterrupted, "cancelled during source check")
		return
	}
	if err != nil {
		s.fail(app.ID, err.Error())
		return
	}
	if !res.Changed {
		s.succeed(app.ID, orDetail(res.Detail, "no change"))
		return
	}

	s.transition(app.ID, StatePreflight, "change detected: "+orDetail(res.Detail, "source differs"))
	findings := s.preflight.Run(ctx, app)
	if ctx.Err() != nil {
		s.transition(app.ID, StateInterrupted, "cancelled during preflight")
		return
	}
	if len(findings) > 0 {
		details := make([]string, len(findings))
		for i, f := range findings {
			details[i] = f.String()
		}
		s.record(Event{AppID: app.ID, Kind: EventPreflightRefused, Detail: strings.Join(details, "; ")})
		s.transition(app.ID, StateFailed, "preflight refused deployment")
		s.toBackoff(app.ID, "preflight refused deployment")
		return
	}

	s.transition(app.ID, StateDeploying, "preflight passed")
	dres, derr := s.deployer().Deploy(ctx, app)
	if ctx.Err() != nil {
		s.transition(app.ID, StateInterrupted,
			"cancelled during deployment; deployed version not advanced")
		return
	}
	if derr != nil {
		s.fail(app.ID, derr.Error())
		return
	}
	if !dres.Success {
		s.fail(app.ID, orDetail(dres.Detail, "deployment reported failure"))
		return
	}
	s.succeed(app.ID, orDetail(dres.Detail, "deployed"))
}

// fail records a failed pass and chains into bounded backoff.
func (s *Scheduler) fail(appID, detail string) {
	s.transition(appID, StateFailed, detail)
	s.toBackoff(appID, detail)
}

// toBackoff schedules the next attempt after the bounded exponential delay,
// keeping the failure reason visible in the status detail.
func (s *Scheduler) toBackoff(appID, reason string) {
	now := s.clock.Now()
	delay := time.Duration(0)
	s.mu.Lock()
	if b := s.backoffs[appID]; b != nil {
		delay = b.Next()
	}
	if st := s.statuses[appID]; st != nil {
		st.Failures++
		if delay > 0 {
			st.NextAttempt = now.Add(delay)
		}
	}
	s.mu.Unlock()
	s.transitionAt(now, appID, StateBackoff, fmt.Sprintf("%s; retry in %s", reason, delay))
}

// succeed records a successful pass and resets backoff.
func (s *Scheduler) succeed(appID, detail string) {
	s.mu.Lock()
	if b := s.backoffs[appID]; b != nil {
		b.Reset()
	}
	if st := s.statuses[appID]; st != nil {
		st.LastSuccess = s.clock.Now()
		st.Failures = 0
		st.NextAttempt = time.Time{}
	}
	s.mu.Unlock()
	s.transition(appID, StateSucceeded, detail)
}

// transition validates and applies one state change, recording an event.
// Invalid transitions are recorded as such but never applied.
func (s *Scheduler) transition(appID string, to State, detail string) {
	s.transitionAt(s.clock.Now(), appID, to, detail)
}

func (s *Scheduler) transitionAt(now time.Time, appID string, to State, detail string) {
	e := Event{AppID: appID, Time: now, Kind: EventState, To: to, Detail: detail}
	s.mu.Lock()
	st := s.statuses[appID]
	if st == nil {
		s.mu.Unlock()
		return
	}
	e.From = st.State
	if !allowedTransitions[st.State][to] {
		s.mu.Unlock()
		s.dispatch(Event{AppID: appID, Time: now, Kind: EventInvalidTransition, From: e.From, To: to, Detail: detail})
		return
	}
	st.State = to
	st.Since = now
	st.Detail = detail
	if to == StateChecking {
		st.NextAttempt = time.Time{}
	}
	s.mu.Unlock()
	s.dispatch(e)
}

// record dispatches a non-transition event.
func (s *Scheduler) record(e Event) {
	e.Time = s.clock.Now()
	s.dispatch(e)
}

// dispatch hands an event to the sink, if any. It never blocks the
// scheduler on slow sinks beyond the sink's own contract: sinks must be
// fast and local.
func (s *Scheduler) dispatch(e Event) {
	if s.opts.Sink == nil {
		return
	}
	_ = s.opts.Sink.RecordAppEvent(context.Background(), e)
}

// nextDelay computes how long the loop waits before its next pass: the
// remaining backoff when one is active (firing immediately once it has
// elapsed), otherwise the interval plus jitter.
func (s *Scheduler) nextDelay(id string, app *config.App) time.Duration {
	now := s.clock.Now()
	s.mu.Lock()
	st := s.statuses[id]
	s.mu.Unlock()
	if st != nil && !st.NextAttempt.IsZero() {
		if after := st.NextAttempt.Sub(now); after > 0 {
			return after
		}
		return 0 // backoff elapsed; run the retry pass now
	}
	interval := app.Interval.D()
	if interval <= 0 {
		interval = config.DefaultInterval.D()
	}
	return interval + s.opts.Jitter(interval)
}

func defaultJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	maxJ := base / jitterDivisor
	if maxJ <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maxJ) + 1))
}

func (s *Scheduler) app(id string) (*config.App, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[id]
	return app, ok
}

// markInterrupted flags any pass still marked in flight as interrupted. It
// runs after the loops have exited; nothing new can start afterwards.
func (s *Scheduler) markInterrupted() {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, st := range s.statuses {
		switch st.State {
		case StateChecking, StatePreflight, StateDeploying, StatePostchecks:
			from := st.State
			detail := "shutdown with pass in flight; deployed version not advanced"
			st.State = StateInterrupted
			st.Since = now
			st.Detail = detail
			s.dispatch(Event{AppID: id, Time: now, Kind: EventState, From: from, To: StateInterrupted, Detail: detail})
		}
	}
}

func orDetail(detail, fallback string) string {
	if detail == "" {
		return fallback
	}
	return detail
}
