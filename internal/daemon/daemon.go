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

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/deploy"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/git"
	"github.com/Dragonshorn-Studios/yukariko/internal/health"
	"github.com/Dragonshorn-Studios/yukariko/internal/httpapi"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
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
	st, err := store.Open(opts.DataDir)
	if err != nil {
		return nil, err
	}
	// Startup retention cleanup; failures are non-fatal (bounded best effort).
	_, _ = st.Cleanup(ctx, store.RetentionPolicy{
		EventsDays:      opts.Config.Retention.EventsDays,
		HealthDays:      opts.Config.Retention.HealthDays,
		DeploymentsDays: opts.Config.Retention.DeploymentsDays,
	}, time.Now())

	runnerSvc := &runner.Runner{Sink: &commandRunSink{store: st}}
	dockerClient := opts.Docker
	if dockerClient == nil {
		dockerClient = &docker.CLIClient{Runner: runnerSvc}
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
		healthSvc = &health.Service{Docker: dockerClient, Runner: runnerSvc}
	}

	checker := &SourceChecker{store: st, git: gitClient, registry: registryClient}
	dispatcher := &DeployDispatcher{
		store:       st,
		runner:      runnerSvc,
		docker:      opts.Docker,
		git:         gitClient,
		registry:    registryClient,
		health:      healthSvc,
		platform:    opts.Platform,
		pipelineRun: opts.PipelineRun,
	}

	apps := make([]*config.App, 0, len(opts.Config.Apps))
	for i := range opts.Config.Apps {
		apps = append(apps, &opts.Config.Apps[i])
	}
	preflight := opts.Preflight
	if preflight == nil {
		preflight = &schedule.Preflight{DataDir: opts.DataDir}
	}
	schedOpts := schedule.Options{
		Apps:      apps,
		DataDir:   opts.DataDir,
		Check:     checker,
		Deploy:    dispatcher,
		Preflight: preflight,
		Sink:      &eventSink{store: st},
	}
	sched := schedule.New(schedOpts)
	monitor := &health.Monitor{
		Service: healthSvc,
		Sink:    &healthSink{store: st},
	}
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
		APIBind:   apiBind,
		APIOn:     api != nil,
	}, nil
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
		return schedule.CheckResult{
			Changed:  res.Status == git.StatusChanged,
			Observed: res.ObservedSHA,
			Detail:   res.Detail,
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
	git         *git.Client
	registry    *registry.Resolver
	health      *health.Service
	platform    string
	pipelineRun func(ctx context.Context, req runner.Request) (runner.Result, error)
}

// Deploy implements the scheduler's Deployer seam.
func (d *DeployDispatcher) Deploy(ctx context.Context, app *config.App) (schedule.DeployResult, error) {
	kind := store.KindDigest
	if app.Source.Mode == config.SourceGit {
		kind = store.KindGitSHA
	}
	depID, err := d.store.BeginDeployment(ctx, store.BeginDeploymentParams{
		AppID: app.ID, Cause: "update", At: time.Now(),
	})
	if err != nil {
		return schedule.DeployResult{}, err
	}
	checkpoint := &boundCheckpoint{store: d.store, appID: app.ID, kind: kind, deploymentID: depID}

	var dep schedule.Deployer
	switch app.Deploy.Mode {
	case config.DeployCompose:
		dep = &deploy.ComposePipeline{
			Runner:     d.runner,
			Run:        d.pipelineRun,
			Git:        d.git,
			Health:     d.health,
			Registry:   d.registry,
			Checkpoint: checkpoint,
			Platform:   d.platform,
		}
	case config.DeployStandalone:
		dep = &deploy.StandalonePipeline{
			Engine:     &deploy.CLIEngine{Docker: d.dockerClient(), Runner: d.runner},
			Registry:   d.registry,
			Health:     d.health,
			Checkpoint: checkpoint,
			Runner:     d.runner,
			Run:        d.pipelineRun,
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

func (d *DeployDispatcher) dockerClient() docker.Client {
	if d.docker != nil {
		return d.docker
	}
	return &docker.CLIClient{Runner: d.runner}
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

type eventSink struct{ store *store.Store }

func (s *eventSink) RecordAppEvent(ctx context.Context, e schedule.Event) error {
	level := store.LevelInfo
	if strings.Contains(e.Detail, "failed") || strings.Contains(e.Kind, "refused") {
		level = store.LevelWarn
	}
	_, err := s.store.RecordEvent(ctx, store.Event{
		Time: e.Time, AppID: e.AppID, Level: level, Kind: e.Kind, Message: e.Detail,
	})
	return err
}

type healthSink struct{ store *store.Store }

func (s *healthSink) RecordHealthSample(ctx context.Context, smp health.Sample) error {
	return s.store.RecordHealthSample(ctx, smp.AppID, smp.Check, smp.State, smp.Reason, nil, smp.Time)
}

func (s *healthSink) RecordHealthTransition(ctx context.Context, t health.Transition) error {
	_, err := s.store.RecordEvent(ctx, store.Event{
		Time: t.Time, AppID: t.AppID, Level: store.LevelWarn, Kind: "health_transition",
		Message: fmt.Sprintf("%s: %s -> %s: %s", t.Check, t.From, t.To, t.Reason),
	})
	return err
}
