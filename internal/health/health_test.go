package health

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
)

// --- fakes ------------------------------------------------------------------

type fakeDocker struct {
	mu      sync.Mutex
	calls   []string
	details map[string]docker.ContainerDetail
	errs    map[string]error
}

func (f *fakeDocker) ListContainers(ctx context.Context, all bool) ([]docker.ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "ListContainers")
	return nil, nil
}

func (f *fakeDocker) InspectContainer(ctx context.Context, id string) (docker.ContainerDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "InspectContainer "+id)
	if err, ok := f.errs[id]; ok {
		return docker.ContainerDetail{}, err
	}
	if d, ok := f.details[id]; ok {
		return d, nil
	}
	return docker.ContainerDetail{}, fmt.Errorf("no such container: %w", docker.ErrContainerMissing)
}

func (f *fakeDocker) recordedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type recordingSink struct {
	mu          sync.Mutex
	samples     []Sample
	transitions []Transition
}

func (s *recordingSink) RecordHealthSample(_ context.Context, smp Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, smp)
	return nil
}

func (s *recordingSink) RecordHealthTransition(_ context.Context, t Transition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitions = append(s.transitions, t)
	return nil
}

func (s *recordingSink) sampleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.samples)
}

func (s *recordingSink) all() ([]Sample, []Transition) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.samples...), append([]Transition(nil), s.transitions...)
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) NewTimer(d time.Duration) schedule.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, when: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t
}

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

// --- HTTP probe tests -------------------------------------------------------

func TestHTTPProbeSuccessAndBadStatus(t *testing.T) {
	// No t.Parallel: t.Setenv is incompatible with parallel tests.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			if req.Header.Get("X-Auth") != "resolved-secret" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, "all good")
			return
		}
		w.WriteHeader(500)
		fmt.Fprint(w, "kaput")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TEST_PROBE_SECRET", "resolved-secret")

	app := &config.App{ID: "web", Health: &config.Health{
		HTTP: &config.HTTPProbe{
			URL:     srv.URL + "/health",
			Timeout: config.Duration(time.Second),
			Status:  []int{200, 299},
			Headers: []config.HTTPHeader{{Name: "X-Auth", SecretRef: &config.SecretRef{Env: "TEST_PROBE_SECRET"}}},
		},
	}}
	s := &Service{}
	samples := s.probeAll(context.Background(), app)
	if len(samples) != 1 || samples[0].State != StateHealthy {
		t.Fatalf("samples = %+v, want one healthy", samples)
	}

	// Status outside the accepted range is unhealthy with bounded body text.
	app.Health.HTTP.URL = srv.URL + "/other"
	samples = s.probeAll(context.Background(), app)
	if samples[0].State != StateUnhealthy {
		t.Fatalf("state = %q, want unhealthy", samples[0].State)
	}
	if !strings.Contains(samples[0].Reason, "status 500") || !strings.Contains(samples[0].Reason, "kaput") {
		t.Errorf("reason = %q, want status and diagnostics", samples[0].Reason)
	}
}

func TestHTTPProbeTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	app := &config.App{ID: "slow", Health: &config.Health{
		HTTP: &config.HTTPProbe{URL: srv.URL, Timeout: config.Duration(50 * time.Millisecond), Status: []int{200, 399}},
	}}
	samples := (&Service{}).probeAll(context.Background(), app)
	if samples[0].State != StateUnhealthy || !strings.Contains(samples[0].Reason, "probe failed") {
		t.Fatalf("samples = %+v, want unhealthy on timeout", samples)
	}
}

func TestHTTPProbeOversizedBodyBounded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, strings.Repeat("e", 1<<20))
	}))
	t.Cleanup(srv.Close)
	app := &config.App{ID: "big", Health: &config.Health{
		HTTP: &config.HTTPProbe{URL: srv.URL, Status: []int{200, 399}},
	}}
	samples := (&Service{}).probeAll(context.Background(), app)
	if samples[0].State != StateUnhealthy {
		t.Fatalf("state = %q", samples[0].State)
	}
	if len(samples[0].Reason) > diagnosticsBytes+200 {
		t.Errorf("reason = %d bytes, want bounded diagnostics", len(samples[0].Reason))
	}
}

func TestHTTPProbeURLQueryNeverInReason(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	app := &config.App{ID: "q", Health: &config.Health{
		HTTP: &config.HTTPProbe{URL: srv.URL + "/health?token=supersecretvalue", Status: []int{200, 399}},
	}}
	samples := (&Service{}).probeAll(context.Background(), app)
	if strings.Contains(samples[0].Reason, "supersecretvalue") {
		t.Errorf("query value leaked into reason: %q", samples[0].Reason)
	}
}

func TestUnresolvableHeaderSecretIsUnhealthy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	app := &config.App{ID: "hdr", Health: &config.Health{
		HTTP: &config.HTTPProbe{URL: srv.URL, Status: []int{200, 399},
			Headers: []config.HTTPHeader{{Name: "X-Auth", SecretRef: &config.SecretRef{Env: "DEFINITELY_UNSET_VAR"}}}},
	}}
	samples := (&Service{}).probeAll(context.Background(), app)
	if samples[0].State != StateUnhealthy || !strings.Contains(samples[0].Reason, "DEFINITELY_UNSET_VAR") {
		t.Fatalf("samples = %+v, want unhealthy naming the env var (never the value)", samples)
	}
}

// --- Docker probe tests -----------------------------------------------------

func TestDockerProbeStates(t *testing.T) {
	t.Parallel()
	dockerHealth := func(status string, streak int) docker.ContainerDetail {
		return docker.ContainerDetail{
			Name:   "app-1",
			Health: docker.HealthState{Configured: true, Status: status, FailingStreak: streak},
		}
	}
	missing := fmt.Errorf("no such container: %w", docker.ErrContainerMissing)
	cases := []struct {
		name      string
		details   map[string]docker.ContainerDetail
		errs      map[string]error
		wantState string
		wantSub   string
	}{
		{name: "healthy", details: map[string]docker.ContainerDetail{"app-1": dockerHealth("healthy", 0)}, wantState: StateHealthy},
		{name: "unhealthy", details: map[string]docker.ContainerDetail{"app-1": dockerHealth("unhealthy", 3)}, wantState: StateUnhealthy, wantSub: "streak 3"},
		{name: "starting", details: map[string]docker.ContainerDetail{"app-1": dockerHealth("starting", 0)}, wantState: StateChecking},
		{name: "no healthcheck", details: map[string]docker.ContainerDetail{"app-1": {}}, wantState: StateUnknown},
		{name: "container missing", errs: map[string]error{"app-1": missing}, wantState: StateUnhealthy, wantSub: "not found"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fd := &fakeDocker{details: tc.details, errs: tc.errs}
			app := standaloneHealthApp()
			s := &Service{Docker: fd}
			samples := s.probeAll(context.Background(), app)
			if len(samples) != 1 || samples[0].Check != "docker" {
				t.Fatalf("samples = %+v", samples)
			}
			if samples[0].State != tc.wantState {
				t.Errorf("state = %q, want %q (reason %q)", samples[0].State, tc.wantState, samples[0].Reason)
			}
			if tc.wantSub != "" && !strings.Contains(samples[0].Reason, tc.wantSub) {
				t.Errorf("reason = %q, want it to contain %q", samples[0].Reason, tc.wantSub)
			}
			for _, c := range fd.recordedCalls() {
				if !strings.HasPrefix(c, "InspectContainer") {
					t.Errorf("non-read docker call recorded: %s", c)
				}
			}
		})
	}
}

func TestDockerProbeComposeAppUnknown(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{}
	app := &config.App{
		ID:     "stack",
		Health: &config.Health{Docker: &config.DockerProbe{}},
		Deploy: config.Deploy{Mode: config.DeployCompose, Compose: &config.ComposeDeploy{WorkDir: "/srv/x"}},
	}
	samples := (&Service{Docker: fd}).probeAll(context.Background(), app)
	if samples[0].State != StateUnknown {
		t.Errorf("state = %q, want unknown (no single container name in compose mode)", samples[0].State)
	}
	if n := len(fd.recordedCalls()); n != 0 {
		t.Errorf("docker calls = %d, want none", n)
	}
}

func standaloneHealthApp() *config.App {
	return &config.App{
		ID: "app",
		Health: &config.Health{
			Docker:   &config.DockerProbe{},
			Interval: config.Duration(time.Minute),
		},
		Deploy: config.Deploy{
			Mode:       config.DeployStandalone,
			Standalone: &config.StandaloneSpec{Image: "app:1", Name: "app-1"},
		},
	}
}

// --- command probe + post-deploy transaction --------------------------------

func TestCommandProbeAndPostDeployTransaction(t *testing.T) {
	t.Parallel()
	r := &runner.Runner{}
	s := &Service{Runner: r}

	app := &config.App{ID: "cmd", Health: &config.Health{Command: []string{"true"}}}
	samples, err := s.RunPostDeployChecks(context.Background(), app)
	if err != nil || len(samples) != 1 || samples[0].State != StateHealthy {
		t.Fatalf("passing command: %v / %+v", err, samples)
	}

	app.Health.Command = []string{"false"}
	samples, err = s.RunPostDeployChecks(context.Background(), app)
	if err == nil {
		t.Fatal("required failing check must fail the deployment")
	}
	if !strings.Contains(err.Error(), "not recorded as successful") {
		t.Errorf("error must say the deployment is not recorded: %v", err)
	}
	if len(samples) != 1 || samples[0].State != StateUnhealthy {
		t.Fatalf("samples = %+v, want the unhealthy sample too", samples)
	}

	req := false
	app.Health.Required = &req
	samples, err = s.RunPostDeployChecks(context.Background(), app)
	if err != nil || len(samples) != 1 {
		t.Fatalf("optional failing check: %v / %+v", err, samples)
	}
}

func TestPostDeployNoHealthConfigPasses(t *testing.T) {
	t.Parallel()
	s := &Service{}
	samples, err := s.RunPostDeployChecks(context.Background(), &config.App{ID: "bare"})
	if err != nil || samples != nil {
		t.Errorf("no health config: %v / %+v, want a pass", err, samples)
	}
}

// --- monitor ----------------------------------------------------------------

func TestMonitorProbesAndRecordsTransitions(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	sink := &recordingSink{}
	var unhealthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if unhealthy.Load() {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	app := &config.App{
		ID: "web",
		Health: &config.Health{
			HTTP:     &config.HTTPProbe{URL: srv.URL, Status: []int{200, 399}},
			Interval: config.Duration(time.Minute),
		},
	}
	monitor := &Monitor{
		Service: &Service{},
		Clock:   clock,
		Jitter:  func(time.Duration) time.Duration { return 0 },
		Sink:    sink,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { monitor.Run(ctx, []*config.App{app}); close(done) }()
	<-monitor.Ready()

	clock.Advance(time.Minute)
	waitFor(t, 2*time.Second, "first sample", func() bool { return sink.sampleCount() == 1 })

	unhealthy.Store(true)
	clock.Advance(time.Minute)
	waitFor(t, 2*time.Second, "unhealthy transition", func() bool {
		_, transitions := sink.all()
		return len(transitions) == 1 && transitions[0].From == StateHealthy && transitions[0].To == StateUnhealthy
	})

	// Still unhealthy next tick: a sample is stored, but no repeated
	// transition — failures never spam. (The failing tick also doubled the
	// next delay, so advance two minutes.)
	clock.Advance(2 * time.Minute)
	waitFor(t, 2*time.Second, "third sample", func() bool { return sink.sampleCount() == 3 })
	samples, transitions := sink.all()
	if len(transitions) != 1 {
		t.Errorf("transitions = %d, want 1 (change-only)", len(transitions))
	}
	if samples[2].State != StateUnhealthy {
		t.Errorf("latest sample = %q, want unhealthy", samples[2].State)
	}
	cancel()
	<-done
}

func TestMonitorBacksOffOnErrors(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(startTime)
	sink := &recordingSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	app := &config.App{
		ID: "web",
		Health: &config.Health{
			HTTP:     &config.HTTPProbe{URL: srv.URL, Status: []int{200, 399}},
			Interval: config.Duration(time.Minute),
		},
	}
	monitor := &Monitor{
		Service: &Service{},
		Clock:   clock,
		Jitter:  func(time.Duration) time.Duration { return 0 },
		Sink:    sink,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { monitor.Run(ctx, []*config.App{app}); close(done) }()
	<-monitor.Ready()

	// With backoff the probes land at 1m, 3m (1+2), and 7m (1+2+4): advance
	// exactly to each deadline so the fake clock never overshoots a probe
	// in flight.
	clock.Advance(1 * time.Minute)
	waitFor(t, 2*time.Second, "probe 1", func() bool { return sink.sampleCount() == 1 })
	clock.Advance(2 * time.Minute)
	waitFor(t, 2*time.Second, "probe 2", func() bool { return sink.sampleCount() == 2 })
	clock.Advance(4 * time.Minute)
	waitFor(t, 2*time.Second, "probe 3", func() bool { return sink.sampleCount() == 3 })
	// By minute 10 the backoff holds the count at 3 (a naive 1m loop: 10).
	clock.Advance(3 * time.Minute)
	if n := sink.sampleCount(); n != 3 {
		t.Errorf("probes = %d at minute 10, want 3 (backoff must prevent hammering)", n)
	}
	cancel()
	<-done
}

var startTime = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func TestMonitorUnhealthyNeverTriggersMutations(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{details: map[string]docker.ContainerDetail{
		"app-1": {Name: "app-1", Health: docker.HealthState{Configured: true, Status: "unhealthy", FailingStreak: 2}},
	}}
	sink := &recordingSink{}
	r := &runner.Runner{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	app := standaloneHealthApp()
	app.Health.HTTP = &config.HTTPProbe{
		URL: srv.URL, Status: []int{200, 399}, Timeout: config.Duration(time.Second),
	}
	clock := newFakeClock(startTime)
	monitor := &Monitor{
		Service: &Service{Docker: fd, Runner: r},
		Clock:   clock,
		Jitter:  func(time.Duration) time.Duration { return 0 },
		Sink:    sink,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { monitor.Run(ctx, []*config.App{app}); close(done) }()
	<-monitor.Ready()

	clock.Advance(3 * time.Minute)
	waitFor(t, 2*time.Second, "unhealthy samples recorded", func() bool {
		return sink.sampleCount() >= 2
	})
	cancel()
	<-done

	for _, c := range fd.recordedCalls() {
		if !strings.HasPrefix(c, "InspectContainer") {
			t.Errorf("mutation-path docker call recorded on unhealthy: %s", c)
		}
	}
	// The regression: health failure produced samples only — no deploy,
	// restart, or container mutation exists to call. Both checks stay
	// unhealthy the whole time, so there is nothing to transition.
	samples, transitions := sink.all()
	for _, smp := range samples {
		if smp.State == StateHealthy {
			t.Errorf("endpoint must stay unhealthy: %+v", smp)
		}
	}
	if len(samples) < 2 {
		t.Errorf("samples = %d, want the repeated unhealthy observations", len(samples))
	}
	if len(transitions) != 0 {
		t.Errorf("transitions = %d, want none: an always-unhealthy app never transitions", len(transitions))
	}
}
