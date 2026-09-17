package health

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
)

// backoffCap bounds how far consecutive probe errors may stretch the check
// interval (interval × 4): probing backs off but never goes quiet.
const backoffCap = 4

// Monitor runs periodic health checks independently of deployments. A
// failing check changes health only: the monitor has no ability to restart,
// redeploy, or touch containers beyond the read-only Docker interface.
type Monitor struct {
	Service *Service
	Clock   schedule.Clock // nil → real clock
	// Jitter returns extra delay added to each check interval; nil → up to
	// 1/10 of the interval.
	Jitter func(base time.Duration) time.Duration
	Sink   Sink

	readyOnce sync.Once
	ready     chan struct{}
}

// Ready is closed once every loop has created its first timer. Tests and
// embedders synchronize against the clock through it. All access is funneled
// through the sync.Once so the lazy allocation is race-free.
func (m *Monitor) Ready() <-chan struct{} { return m.readyChan() }

func (m *Monitor) readyChan() chan struct{} {
	m.readyOnce.Do(func() { m.ready = make(chan struct{}) })
	return m.ready
}

// Run starts one check loop per app that configures health and blocks until
// ctx is cancelled. It returns only after every loop has created its first
// timer, so a fake clock can never race loop startup. Apps without a health
// section are not probed.
func (m *Monitor) Run(ctx context.Context, apps []*config.App) {
	readyCh := m.readyChan()
	clock := m.Clock
	if clock == nil {
		clock = schedule.NewRealClock()
	}
	jitter := m.Jitter
	if jitter == nil {
		jitter = defaultJitter
	}
	var wg sync.WaitGroup
	var started []chan struct{}
	for _, app := range apps {
		if app == nil || app.Health == nil {
			continue
		}
		ch := make(chan struct{})
		started = append(started, ch)
		wg.Add(1)
		go m.loop(ctx, &wg, app, clock, jitter, ch)
	}
	for _, ch := range started {
		<-ch
	}
	close(readyCh)
	wg.Wait()
}

// loop probes one app forever, backing off when probes error and recording
// transitions only when a check's state actually changes (non-spammy). The
// started channel is closed after the first timer exists.
func (m *Monitor) loop(ctx context.Context, wg *sync.WaitGroup, app *config.App, clock schedule.Clock, jitter func(time.Duration) time.Duration, started chan struct{}) {
	defer wg.Done()
	interval := app.Health.Interval.D()
	if interval <= 0 {
		interval = config.DefaultHealthInterval.D()
	}
	last := map[string]string{} // check → last state
	failures := 0

	timer := clock.NewTimer(interval + jitter(interval))
	close(started)
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}

		samples := m.Service.probeAll(ctx, app)
		hadError := false
		for _, sample := range samples {
			m.record(sample, last)
			if sample.State == StateUnhealthy || sample.State == StateUnknown {
				hadError = true
			}
		}
		if hadError {
			failures++
		} else {
			failures = 0
		}
		if failures > 2 {
			failures = 2 // the delay factor is capped at 4× the interval
		}
		delay := interval
		if failures > 0 {
			delay = interval * time.Duration(1<<min(failures, 2)) // 2×, 4×, capped
		}
		timer = clock.NewTimer(delay + jitter(interval))
	}
}

// record stores a sample and reports a transition when the state changed.
func (m *Monitor) record(sample Sample, last map[string]string) {
	if m.Sink == nil {
		return
	}
	ctx := context.Background()
	prev, seen := last[sample.Check]
	last[sample.Check] = sample.State
	if err := m.Sink.RecordHealthSample(ctx, sample); err != nil {
		return // sink errors never stop monitoring; the store is local
	}
	if seen && prev != sample.State {
		_ = m.Sink.RecordHealthTransition(ctx, Transition{
			AppID:  sample.AppID,
			Check:  sample.Check,
			From:   prev,
			To:     sample.State,
			Time:   sample.Time,
			Reason: sample.Reason,
		})
	}
}

func defaultJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	maxJ := base / 10
	if maxJ <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maxJ) + 1))
}
