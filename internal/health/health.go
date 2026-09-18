// Package health implements post-deploy checks and independent health
// monitoring: HTTP GET probes, Docker health state read through the
// discovery interface, and explicitly configured local commands.
//
// # What health can and cannot do
//
// Health failure changes health — nothing else. There is no restart path,
// no rollback path, and no deploy trigger in this package: the Docker
// client it uses is read-only by construction (#5), and the runner only
// ever executes the explicitly configured health command. Container
// running-state, Docker health, HTTP health, and deployed-version status
// remain separate facts recorded as separate samples.
//
// # States and secrecy
//
// States are checking / healthy / unhealthy / unknown, each sample carrying
// a timestamp and a bounded, redacted reason. Probe URLs are rendered
// without query strings (queries can carry secrets), header values come
// from SecretRefs resolved at call time, and diagnostic body excerpts are
// capped and redacted.
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
)

// States re-use the config package's shared vocabulary so the store, API,
// and dashboard render one set of strings.
const (
	StateChecking  = config.HealthChecking  // "checking"
	StateHealthy   = config.HealthHealthy   // "healthy"
	StateUnhealthy = config.HealthUnhealthy // "unhealthy"
	StateUnknown   = config.HealthUnknown   // "unknown"
)

// diagnosticsBytes bounds the response excerpt kept in a sample reason.
const diagnosticsBytes = 512

// healthCommandTimeout bounds the configured health command.
const healthCommandTimeout = time.Minute

// Sample is one health observation for the history and current projection.
type Sample struct {
	AppID  string
	Check  string // "http" | "docker" | "command"
	State  string // checking | healthy | unhealthy | unknown
	Time   time.Time
	Reason string // bounded, redacted
}

// Transition is recorded when a check's state changes between runs.
type Transition struct {
	AppID  string
	Check  string
	From   string
	To     string
	Time   time.Time
	Reason string
}

// Sink receives samples (history + current projection). The durable store
// adapter lands with the daemon wiring; it must be fast and local.
type Sink interface {
	RecordHealthSample(ctx context.Context, s Sample) error
	RecordHealthTransition(ctx context.Context, t Transition) error
}

// Service runs the configured health checks for apps.
type Service struct {
	// HTTPClient probes HTTP endpoints; nil uses one sized to the probe
	// timeout.
	HTTPClient *http.Client
	// Docker reads container health through the read-only discovery
	// interface.
	Docker docker.Client
	// DockerFor resolves the read-only client for one app; nil uses Docker.
	// The daemon wires it so per-app Docker endpoints (rootless and
	// multi-daemon hosts) probe the daemon the app actually deploys to.
	DockerFor func(*config.App) docker.Client
	// EndpointFor resolves the app's Docker endpoint for user-owned command
	// probes (DOCKER_HOST/DOCKER_CONTEXT env); nil means none.
	EndpointFor func(*config.App) *config.DockerEndpoint
	// Runner executes the explicitly configured health command.
	Runner *runner.Runner
	// Clock stamps samples; nil → real time. Injectable for tests.
	Clock schedule.Clock
}

// dockerFor resolves the read-only client for one app.
func (s *Service) dockerFor(app *config.App) docker.Client {
	if s.DockerFor != nil {
		return s.DockerFor(app)
	}
	return s.Docker
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

func (s *Service) httpClientFor(timeout time.Duration) *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: timeout + 5*time.Second}
}

// probeAll runs every check configured on the app.
func (s *Service) probeAll(ctx context.Context, app *config.App) []Sample {
	if app.Health == nil {
		return nil
	}
	var samples []Sample
	if h := app.Health.HTTP; h != nil {
		samples = append(samples, s.probeHTTP(ctx, app.ID, h))
	}
	if app.Health.Docker != nil {
		samples = append(samples, s.probeDocker(ctx, app))
	}
	if len(app.Health.Command) > 0 {
		samples = append(samples, s.probeCommand(ctx, app))
	}
	return samples
}

// RunPostDeployChecks runs the app's checks synchronously for the deploy
// pipeline. When the health configuration is required (the default) any
// failed check returns an error: the caller must treat the deployment as
// failed and not advance the deployed SHA/digest. When checks are optional,
// failures are reported as samples only.
func (s *Service) RunPostDeployChecks(ctx context.Context, app *config.App) ([]Sample, error) {
	if app.Health == nil {
		return nil, nil // nothing configured: the deploy pipeline proceeds
	}
	samples := s.probeAll(ctx, app)
	var failed []string
	for _, sample := range samples {
		if sample.State == StateUnhealthy {
			failed = append(failed, sample.Check+": "+sample.Reason)
		}
	}
	if app.Health.IsRequired() && len(failed) > 0 {
		return samples, fmt.Errorf("required post-deploy checks failed (%s); the deployment is not recorded as successful",
			strings.Join(failed, "; "))
	}
	return samples, nil
}

// probeHTTP performs one HTTP GET probe. The sample reason names the probe
// URL with query and credentials stripped.
func (s *Service) probeHTTP(ctx context.Context, appID string, h *config.HTTPProbe) Sample {
	sample := Sample{AppID: appID, Check: "http", Time: s.now(), State: StateUnknown}
	prefix := displayURL(h.URL) + ": "
	timeout := h.Timeout.D()
	if timeout <= 0 {
		timeout = config.DefaultProbeTimeout.D()
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, h.URL, nil)
	if err != nil {
		sample.Reason = prefix + "invalid probe URL"
		sample.State = StateUnhealthy
		return sample
	}
	for _, header := range h.Headers {
		value := header.Value
		if header.SecretRef != nil {
			secret, err := header.SecretRef.Resolve()
			if err != nil {
				sample.State = StateUnhealthy
				sample.Reason = prefix + "probe header could not be resolved: " + redact(err.Error())
				return sample
			}
			value = secret // used for this request only, then dropped
		}
		req.Header.Set(header.Name, value)
	}

	resp, err := s.httpClientFor(timeout).Do(req)
	if err != nil {
		sample.State = StateUnhealthy
		sample.Reason = prefix + "probe failed: " + redact(err.Error())
		return sample
	}
	defer resp.Body.Close()

	statusMin, statusMax := acceptedRange(h)
	inRange := statusMin == 0 && statusMax == 0 ||
		(resp.StatusCode >= statusMin && resp.StatusCode <= statusMax)
	diag := boundedDiagnostics(resp)
	if !inRange {
		sample.State = StateUnhealthy
		sample.Reason = fmt.Sprintf("%sstatus %d outside accepted range: %s", prefix, resp.StatusCode, diag)
		return sample
	}
	sample.State = StateHealthy
	sample.Reason = fmt.Sprintf("%sstatus %d: %s", prefix, resp.StatusCode, diag)
	return sample
}

// acceptedRange returns the inclusive [min,max] status range; a zero pair
// means "any status".
func acceptedRange(h *config.HTTPProbe) (min, max int) {
	switch len(h.Status) {
	case 2:
		return h.Status[0], h.Status[1]
	case 1:
		return h.Status[0], h.Status[0]
	default:
		return 0, 0
	}
}

// boundedDiagnostics caps and sanitizes a response excerpt.
func boundedDiagnostics(resp *http.Response) string {
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, diagnosticsBytes))
	text := strings.TrimSpace(string(excerpt))
	if len(excerpt) == diagnosticsBytes {
		text += "…"
	}
	if text == "" {
		return "no body"
	}
	return redact(text)
}

// probeDocker reads the container's Docker health through the read-only
// discovery interface. Standalone apps probe their configured container;
// Compose apps have no single container name, so their Docker health is
// unknown in this build.
func (s *Service) probeDocker(ctx context.Context, app *config.App) Sample {
	sample := Sample{AppID: app.ID, Check: "docker", Time: s.now()}
	if s.Docker == nil {
		sample.State = StateUnknown
		sample.Reason = "docker client not configured"
		return sample
	}
	if app.Deploy.Mode != config.DeployStandalone || app.Deploy.Standalone == nil || app.Deploy.Standalone.Name == "" {
		sample.State = StateUnknown
		sample.Reason = "no single container name to inspect for this app mode"
		return sample
	}
	detail, err := s.dockerFor(app).InspectContainer(ctx, app.Deploy.Standalone.Name)
	if err != nil {
		if errors.Is(err, docker.ErrContainerMissing) {
			sample.State = StateUnhealthy
			sample.Reason = "container not found: " + app.Deploy.Standalone.Name
			return sample
		}
		sample.State = StateUnknown
		sample.Reason = "inspect failed: " + redact(err.Error())
		return sample
	}
	if !detail.Health.Configured {
		sample.State = StateUnknown
		sample.Reason = "container has no docker healthcheck"
		return sample
	}
	switch detail.Health.Status {
	case "healthy":
		sample.State = StateHealthy
		sample.Reason = "docker health: healthy"
	case "unhealthy":
		sample.State = StateUnhealthy
		sample.Reason = fmt.Sprintf("docker health: unhealthy (failing streak %d)", detail.Health.FailingStreak)
	case "starting":
		sample.State = StateChecking
		sample.Reason = "docker health: starting"
	default:
		sample.State = StateUnknown
		sample.Reason = "docker health: unknown status"
	}
	return sample
}

// probeCommand runs the explicitly configured health command. The command
// is the user's own configuration, executed through the controlled runner.
func (s *Service) probeCommand(ctx context.Context, app *config.App) Sample {
	sample := Sample{AppID: app.ID, Check: "command", Time: s.now()}
	r := s.Runner
	if r == nil {
		r = &runner.Runner{}
	}
	res, err := r.Run(ctx, runner.Request{
		Name:    "health command",
		Argv:    app.Health.Command,
		Timeout: healthCommandTimeout,
		Env:     s.endpointEnv(app),
		AppID:   app.ID,
	})
	if err != nil {
		sample.State = StateUnknown
		sample.Reason = redact(err.Error())
		return sample
	}
	if res.Status != runner.StatusSuccess {
		sample.State = StateUnhealthy
		sample.Reason = fmt.Sprintf("health command %s: %s", res.Status, orDetail(redact(res.Err), "no output"))
		return sample
	}
	sample.State = StateHealthy
	sample.Reason = "health command succeeded"
	return sample
}

// endpointEnv renders the app's Docker endpoint for the user-owned command
// probe argv: DOCKER_CONTEXT/DOCKER_HOST on the runner allowlist.
func (s *Service) endpointEnv(app *config.App) []string {
	if s.EndpointFor != nil {
		return s.EndpointFor(app).Env()
	}
	return nil
}

// displayURL strips query and fragment: probe URLs may carry secrets.
func displayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	u.RawQuery = ""
	u.Fragment = ""
	if u.User != nil {
		u.User = nil
	}
	return u.String()
}

// redactor scrubs credential patterns from any text that could reach a
// reason, reusing the runner's rules.
var redactor = runner.NewRedactor(nil)

func redact(s string) string { return redactor.String(s) }

func orDetail(detail, fallback string) string {
	if detail == "" {
		return fallback
	}
	return detail
}
