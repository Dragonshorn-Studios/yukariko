// Package daemon assembles Yukariko's components into a runnable daemon and
// provides the source-check and deployment dispatchers that route apps by
// their configured modes.
//
// The daemon owns no policy: the scheduler (#8) owns timing, locks, and the
// state machine; detection lives in git (#9) and registry (#10); mutation
// lives in the deploy pipelines (#11/#12); health lives in #13. This
// package wires them to the durable store (#3) and records observations
// separately from deployed versions.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/auth"
	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/deploy"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/git"
	"github.com/Dragonshorn-Studios/yukariko/internal/health"
	"github.com/Dragonshorn-Studios/yukariko/internal/httpapi"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/report"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
	"github.com/Dragonshorn-Studios/yukariko/internal/ui"
	"net/http"
	"os"

	"github.com/Dragonshorn-Studios/yukariko/internal/state"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// Options configures Assemble. Component overrides exist for tests; nil
// fields use the real implementations.
type Options struct {
	Config  *config.Config
	DataDir string
	// Platform resolves registry images when an image carries no platform.
	Platform string
	// Overrides (tests).
	Docker    docker.Client
	Git       *git.Client
	Registry  *registry.Resolver
	Health    *health.Service
	Preflight *schedule.Preflight
	// PipelineRun overrides deploy-pipeline command execution (tests).
	PipelineRun func(ctx context.Context, req runner.Request) (runner.Result, error)
	// Engine overrides the standalone mutation engine (tests).
	Engine deploy.Engine
}

// Assembled is the wired application.
type Assembled struct {
	Config    *config.Config
	Store     *store.Store
	Runner    *runner.Runner
	Checker   *SourceChecker
	Deployer  *DeployDispatcher
	Scheduler *schedule.Scheduler
	Monitor   *health.Monitor
	Preflight *schedule.Preflight
	Docker    docker.Client
	// API is non-nil when the configuration enables the read-only HTTP
	// server; the daemon serves it on APIBind.
	API     *httpapi.Server
	APIBind string
	APIOn   bool
	// ReportHandler is non-nil when inbound reporting is enabled; it mounts
	// at /report/v1/events, separate from the read-only dashboard API.
	ReportHandler http.Handler
	// Authenticator is non-nil when the OIDC gate is enabled (issue #61);
	// the run command wraps the whole root mux with Protect so /ui and /api
	// require a session while /report keeps its own HMAC channel.
	Authenticator *auth.Server
	// Reporter is non-nil when outbound reporting is enabled; the daemon
	// runs its drain loop and local sinks enqueue through it.
	Reporter *report.Reporter
	// UI is the embedded read-only dashboard; always present.
	UI http.Handler
}

// Assemble opens the store, builds every component, and wires the
// scheduler, monitor, and dispatchers. The caller owns the store's Close.
func Assemble(ctx context.Context, opts Options) (*Assembled, error) {
	if opts.Config == nil || opts.DataDir == "" {
		return nil, errors.New("daemon: config and data dir are required")
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// A root CLI pass sharing the service user's data dir must not leave
	// root-owned store files behind. Heal before Open — files an unclean
	// earlier root run left would make Open itself fail before we could
	// fix them — and again after, for whatever Open created as root.
	healRootOwnedStoreFiles(opts.DataDir)
	st, err := store.Open(opts.DataDir)
	if err != nil {
		return nil, err
	}
	healRootOwnedStoreFiles(opts.DataDir)
	// Interrupt running-deployment rows no live pass could still own (the
	// process died mid-deploy); they would otherwise block the app's next
	// pass forever. Generous by design - real budgets are minutes.
	_, _ = st.ReapStaleDeployments(ctx, time.Hour, "stale: reaped at startup (the owning process is gone)", time.Now())
	// Startup retention cleanup; failures are non-fatal (bounded best effort).
	_, _ = st.Cleanup(ctx, store.RetentionPolicy{
		EventsDays:      opts.Config.Retention.EventsDays,
		HealthDays:      opts.Config.Retention.HealthDays,
		DeploymentsDays: opts.Config.Retention.DeploymentsDays,
	}, time.Now())
	// Drop sessions and pending logins whose TTL passed while the daemon
	// was down; failures stay non-fatal.
	_, _ = st.SweepAuth(ctx, time.Now())

	runnerSvc := &runner.Runner{Sink: &commandRunSink{store: st}}
	cfg := opts.Config
	var dockerCLI *docker.CLIClient
	dockerClient := opts.Docker
	if dockerClient == nil {
		dockerCLI = &docker.CLIClient{Runner: runnerSvc}
		dockerClient = dockerCLI
	}
	gitClient := opts.Git
	if gitClient == nil {
		gitClient = &git.Client{Runner: runnerSvc}
	}
	registryClient := opts.Registry
	if registryClient == nil {
		registryClient = &registry.Resolver{Runner: runnerSvc}
	}
	healthSvc := opts.Health
	if healthSvc == nil {
		healthSvc = &health.Service{
			Docker: dockerClient,
			Runner: runnerSvc,
			// Docker probes hit the daemon the app actually deploys to;
			// with an injected test client there is no endpoint layer.
			DockerFor: func(app *config.App) docker.Client {
				if dockerCLI == nil {
					return dockerClient
				}
				return dockerCLI.WithFlags(cfg.EndpointFor(app).Flags())
			},
			EndpointFor: cfg.EndpointFor,
		}
	}

	reporter := reporterFor(opts.Config, st, runnerSvc)
	checker := &SourceChecker{store: st, git: gitClient, registry: registryClient}
	dispatcher := &DeployDispatcher{
		store:       st,
		runner:      runnerSvc,
		docker:      opts.Docker,
		Engine:      opts.Engine,
		git:         gitClient,
		Registry:    registryClient,
		Health:      healthSvc,
		Platform:    opts.Platform,
		Default:     cfg.Docker,
		pipelineRun: opts.PipelineRun,
	}

	apps := make([]*config.App, 0, len(opts.Config.Apps))
	for i := range opts.Config.Apps {
		apps = append(apps, &opts.Config.Apps[i])
	}
	preflight := opts.Preflight
	if preflight == nil {
		preflight = &schedule.Preflight{DataDir: opts.DataDir, DefaultEndpoint: cfg.Docker}
	}
	schedOpts := schedule.Options{
		Apps:      apps,
		DataDir:   opts.DataDir,
		Check:     checker,
		Deploy:    dispatcher,
		Preflight: preflight,
		Sink:      &eventSink{store: st, reporter: reporter},
	}
	sched := schedule.New(schedOpts)
	monitor := &health.Monitor{
		Service: healthSvc,
		Sink:    &healthSink{store: st, reporter: reporter},
	}
	reportHandler := reportHandlerFor(opts.Config, st)
	authenticator := authenticatorFor(opts.Config, st)
	uiServer := &ui.Server{Store: st, Config: opts.Config}
	var api *httpapi.Server
	apiBind := ""
	if opts.Config.Server.Enabled {
		apiBind = opts.Config.Server.Bind
		if apiBind == "" {
			apiBind = config.DefaultServerBind
		}
		api = &httpapi.Server{Store: st, Config: opts.Config}
	}
	return &Assembled{
		Config:    opts.Config,
		Store:     st,
		Runner:    runnerSvc,
		Checker:   checker,
		Deployer:  dispatcher,
		Scheduler: sched,
		Monitor:   monitor,
		Preflight: schedOpts.Preflight,
		Docker:    dockerClient,
		API:       api,
		UI:        uiServer.Handler(),
		APIBind:   apiBind,
		APIOn:     api != nil,

		ReportHandler: reportHandler,
		Reporter:      reporter,
		Authenticator: authenticator,
	}, nil
}

// reporterFor builds the outbound reporter when the configuration enables
// it; nil means reporting is off and nothing enqueues.
func reporterFor(cfg *config.Config, st *store.Store, rn *runner.Runner) *report.Reporter {
	out := cfg.Reporting.Outbound
	if !out.Enabled || out.URL == "" || out.HostID == "" || out.SecretRef == nil {
		return nil
	}
	interval := out.HeartbeatInterval.D()
	if interval <= 0 {
		interval = config.DefaultHeartbeatInterval.D()
	}
	return &report.Reporter{
		Endpoint:          out.URL,
		HostID:            out.HostID,
		KeyRef:            out.SecretRef,
		Store:             st,
		HeartbeatInterval: interval,
	}
}

// reportHandlerFor builds the authenticated inbound-report receiver when
// the configuration enables it. Key material resolves at request time via
// SecretRefs and is never stored or logged.
func reportHandlerFor(cfg *config.Config, st *store.Store) http.Handler {
	inbound := cfg.Reporting.Inbound
	if !inbound.Enabled {
		return nil
	}
	return report.NewReceiver(inbound, func(hostID string) ([][]byte, bool) {
		for _, host := range inbound.Hosts {
			if host.ID != hostID {
				continue
			}
			keys := make([][]byte, 0, len(host.Keys))
			for _, k := range host.Keys {
				value, err := k.SecretRef.Resolve()
				if err != nil {
					return nil, false // unresolvable key: treat as revoked
				}
				keys = append(keys, []byte(value))
			}
			return keys, true
		}
		return nil, false
	}, st).Handler()
}

// authenticatorFor builds the optional OIDC gate when the configuration
// enables it (issue #61). The client secret stays a SecretRef and resolves
// only inside the token exchange; the provider is discovered lazily at
// first login so an unreachable IdP never blocks daemon startup.
func authenticatorFor(cfg *config.Config, st *store.Store) *auth.Server {
	if !cfg.Auth.OIDC.Enabled {
		return nil
	}
	return &auth.Server{OIDC: cfg.Auth.OIDC, Store: st}
}

// --- source checking --------------------------------------------------------

// SourceChecker routes change detection by the app's source mode and
// records every observed SHA/digest in the store, separately from the
// deployed version.
type SourceChecker struct {
	store    *store.Store
	git      *git.Client
	registry *registry.Resolver
}

// Check implements the observation half of the scheduler's Checker seam.
func (c *SourceChecker) Check(ctx context.Context, app *config.App) (schedule.CheckResult, error) {
	switch app.Source.Mode {
	case config.SourceGit:
		res, err := c.git.Check(ctx, app)
		if err != nil {
			return schedule.CheckResult{}, err
		}
		if res.ObservedSHA != "" {
			if err := c.store.RecordObservation(ctx, app.ID, store.KindGitSHA, res.ObservedSHA, time.Now()); err != nil {
				return schedule.CheckResult{}, err
			}
		}
		changed, detail := res.Status == git.StatusChanged, res.Detail
		// The worktree can already sit at the remote SHA after a cancelled
		// or failed pass (prepare merged, the success checkpoint never
		// advanced). A deployment stays owed until the checkpoint catches
		// up — the mirror of "only the checkpoint advances the deployed
		// version" — or a cancelled pass would be silently forgotten.
		if !changed && res.ObservedSHA != "" {
			if deployed, _, ok, err := c.store.DeployedVersion(ctx, app.ID, store.KindGitSHA); err != nil {
				return schedule.CheckResult{}, err
			} else if !ok || deployed != res.ObservedSHA {
				changed = true
				detail = "remote already in the worktree but not checkpointed as deployed (cancelled or failed pass); deployment owed"
			}
		}
		return schedule.CheckResult{
			Changed:  changed,
			Observed: res.ObservedSHA,
			Detail:   detail,
		}, nil

	case config.SourceRegistry:
		if app.Source.Registry == nil || len(app.Source.Registry.Images) == 0 {
			return schedule.CheckResult{}, errors.New("registry source has no images configured")
		}
		deployed := map[string]string{}
		if version, _, ok, err := c.store.DeployedVersion(ctx, app.ID, store.KindDigest); err != nil {
			return schedule.CheckResult{}, err
		} else if ok {
			deployed = parseDigestVersion(version)
		}
		res, err := c.registry.Check(ctx, app.Source.Registry.Images, deployed)
		if err != nil {
			return schedule.CheckResult{}, err
		}
		for _, img := range res.Images {
			if err := c.store.RecordObservation(ctx, app.ID, store.KindDigest+":"+img.Ref, img.RemoteDigest, time.Now()); err != nil {
				return schedule.CheckResult{}, err
			}
		}
		detail := "all images unchanged"
		if res.Changed {
			detail = "changed images: " + strings.Join(res.ChangedImages, ", ")
		}
		return schedule.CheckResult{Changed: res.Changed, Detail: detail}, nil

	default:
		return schedule.CheckResult{}, fmt.Errorf("unsupported source mode %q", app.Source.Mode)
	}
}

// parseDigestVersion splits the stored "ref@digest,ref@digest" identity.
func parseDigestVersion(version string) map[string]string { return state.ParseDigestVersion(version) }

// --- deployment dispatch ----------------------------------------------------

// DeployDispatcher routes deployments to the Compose or standalone pipeline
// and binds each run to a durable deployment record whose success
// transaction is the only path that advances the deployed version.
type DeployDispatcher struct {
	store       *store.Store
	runner      *runner.Runner
	docker      docker.Client // test override for the standalone engine
	Engine      deploy.Engine // DI override; nil uses the CLI engine
	git         *git.Client
	Registry    *registry.Resolver
	Health      *health.Service
	Platform    string
	Default     *config.DockerEndpoint // configuration-wide Docker endpoint
	pipelineRun func(ctx context.Context, req runner.Request) (runner.Result, error)
}

// endpointFlags resolves the docker CLI global flags for one app: its
// override, else the configuration-wide default.
func (d *DeployDispatcher) endpointFlags(app *config.App) []string {
	if app.Docker != nil {
		return app.Docker.Flags()
	}
	return d.Default.Flags()
}

// Deploy implements the scheduler's Deployer seam.
func (d *DeployDispatcher) Deploy(ctx context.Context, app *config.App) (schedule.DeployResult, error) {
	kind := store.KindDigest
	if app.Source.Mode == config.SourceGit {
		kind = store.KindGitSHA
	}
	depID, err := d.claimDeploymentCause(ctx, app, "update")
	if err != nil {
		return schedule.DeployResult{}, err
	}
	checkpoint := &boundCheckpoint{store: d.store, appID: app.ID, kind: kind, deploymentID: depID}

	var dep schedule.Deployer
	switch app.Deploy.Mode {
	case config.DeployCompose:
		dep = &deploy.ComposePipeline{
			Runner:          d.runner,
			Run:             d.pipelineRun,
			Git:             d.git,
			Health:          d.Health,
			Registry:        d.Registry,
			Checkpoint:      checkpoint,
			Platform:        d.Platform,
			DefaultEndpoint: d.Default,
		}
	case config.DeployStandalone:
		engine := d.Engine
		if engine == nil {
			engine = &deploy.CLIEngine{Docker: d.docker, Runner: d.runner, GlobalFlags: d.endpointFlags(app)}
		}
		dep = &deploy.StandalonePipeline{
			Engine:          engine,
			Registry:        d.Registry,
			Health:          d.Health,
			Checkpoint:      checkpoint,
			Runner:          d.runner,
			Run:             d.pipelineRun,
			DefaultEndpoint: d.Default,
		}
	default:
		err := fmt.Errorf("unsupported deploy mode %q", app.Deploy.Mode)
		_ = d.store.FinishDeployment(ctx, depID, store.StatusFailed, err.Error(), time.Now())
		return schedule.DeployResult{}, err
	}

	res, deployErr := dep.Deploy(ctx, app)
	if deployErr != nil {
		if fErr := d.store.FinishDeployment(ctx, depID, store.StatusFailed, deployErr.Error(), time.Now()); fErr != nil {
			return schedule.DeployResult{}, errors.Join(deployErr, fErr)
		}
		return schedule.DeployResult{Success: false, Detail: res.Detail}, deployErr
	}
	// Success: the pipeline's checkpoint already committed the deployment
	// transaction; nothing further mutates the deployed version here.
	return res, nil
}

// deploymentBudget is the longest a pass may run before its open
// deployment row is provably orphaned; no live pass outlives it.
func deploymentBudget(app *config.App) time.Duration {
	budget := app.Timeout.D()
	if budget <= 0 {
		budget = config.DefaultTimeout.D()
	}
	return budget + 5*time.Minute
}

// claimDeployment opens the app's single running deployment row. The
// partial unique index makes this the cross-process counterpart of the
// scheduler's in-memory per-app lock: when another pass (daemon or manual
// CLI) holds a fresh row, this pass bails with a clear message; when the
// row is older than any live pass could be, the owning process is gone
// and the row is reaped so one crash cannot block the app forever.
func (d *DeployDispatcher) claimDeploymentCause(ctx context.Context, app *config.App, cause string) (string, error) {
	if cause == "" {
		cause = "update"
	}
	begin := func() (string, error) {
		return d.store.BeginDeployment(ctx, store.BeginDeploymentParams{
			AppID: app.ID, Cause: cause, At: time.Now(),
		})
	}
	depID, err := begin()
	if err == nil || !errors.Is(err, store.ErrDeploymentInProgress) {
		return depID, err
	}
	open, ok, oerr := d.store.OpenDeployment(ctx, app.ID)
	if oerr != nil {
		return "", oerr
	}
	if !ok {
		// The row vanished between insert and read; retry the claim once.
		return begin()
	}
	age := time.Since(open.StartedAt)
	if age > deploymentBudget(app) {
		if ferr := d.store.FinishDeployment(ctx, open.ID, store.StatusInterrupted,
			fmt.Sprintf("stale: pass ran %s, past its budget; the owning process is gone", age.Round(time.Second)), time.Now()); ferr != nil {
			return "", ferr
		}
		return begin()
	}
	return "", fmt.Errorf("deployment already in progress for %s (started %s ago, cause %q) - not starting a second pass; retry after it finishes",
		app.ID, age.Round(time.Second), open.Cause)
}

// boundCheckpoint is the per-run VersionCheckpoint bound to one open
// deployment record. CommitDeploymentSuccess is the single transaction that
// advances the deployed version and finishes the record.
type boundCheckpoint struct {
	store        *store.Store
	appID, kind  string
	deploymentID string
}

func (b *boundCheckpoint) MarkDeployed(ctx context.Context, _, version string) error {
	return b.store.CommitDeploymentSuccess(ctx, b.appID, b.kind, version, b.deploymentID, time.Now())
}

// --- store sinks ------------------------------------------------------------

type commandRunSink struct{ store *store.Store }

// health transition and state sinks optionally forward to the outbound
// reporter (#18); reporting failures never affect local behavior because
// Enqueue only writes to the durable outbox.

func (s *commandRunSink) RecordCommandSummary(ctx context.Context, sum runner.Summary) error {
	var exitCode *int
	if sum.Result.Status == runner.StatusSuccess || sum.Result.Status == runner.StatusFailed {
		code := sum.Result.ExitCode
		exitCode = &code
	}
	return s.store.RecordCommandRun(ctx, store.CommandRun{
		AppID:     sum.Request.AppID,
		Name:      sum.Request.Name,
		Argv:      sum.ArgvJoined,
		Status:    sum.Result.Status,
		ExitCode:  exitCode,
		StartedAt: sum.Result.StartedAt,
		EndedAt:   sum.Result.EndedAt,
	})
}

type eventSink struct {
	store    *store.Store
	reporter *report.Reporter
}

func (s *eventSink) RecordAppEvent(ctx context.Context, e schedule.Event) error {
	level := store.LevelInfo
	if strings.Contains(e.Detail, "failed") || strings.Contains(e.Kind, "refused") {
		level = store.LevelWarn
	}
	_, err := s.store.RecordEvent(ctx, store.Event{
		Time: e.Time, AppID: e.AppID, Level: level, Kind: e.Kind, Message: e.Detail,
	})
	// Deployment outcomes are reported outbound (audit events: never
	// coalesced or dropped). Check outcomes are not — heartbeats carry
	// liveness.
	if s.reporter != nil && e.Kind == schedule.EventState &&
		(e.To == schedule.StateSucceeded || e.To == schedule.StateFailed) && e.From == schedule.StateDeploying {
		status := "succeeded"
		if e.To == schedule.StateFailed {
			status = "failed"
		}
		if err := s.reporter.EnqueueDeployment(ctx, e.AppID, e.Detail, status); err != nil {
			return err
		}
	}
	return err
}

type healthSink struct {
	store    *store.Store
	reporter *report.Reporter
}

func (s *healthSink) RecordHealthSample(ctx context.Context, smp health.Sample) error {
	return s.store.RecordHealthSample(ctx, smp.AppID, smp.Check, smp.State, smp.Reason, nil, smp.Time)
}

func (s *healthSink) RecordHealthTransition(ctx context.Context, t health.Transition) error {
	_, err := s.store.RecordEvent(ctx, store.Event{
		Time: t.Time, AppID: t.AppID, Level: store.LevelWarn, Kind: "health_transition",
		Message: fmt.Sprintf("%s: %s -> %s: %s", t.Check, t.From, t.To, t.Reason),
	})
	if err == nil && s.reporter != nil {
		err = s.reporter.Enqueue(ctx, report.TypeHealth, t.AppID,
			report.HealthData{Check: t.Check, State: t.To, Reason: t.Reason},
			"health:"+t.AppID+":"+t.Check)
	}
	return err
}
