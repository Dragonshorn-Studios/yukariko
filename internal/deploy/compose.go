// Package deploy implements the deployment pipelines: Compose (#11) and
// standalone container recreation (#12). Pipelines run only after a real
// source change, through the per-app lock and preflight of the #8
// scheduler, and they advance the deployed SHA/digest only after every
// command and required post-deploy check has succeeded.
//
// # Stateful-application warning
//
// `compose pull` + `up -d --wait` recreates containers in place. For
// stateful applications (databases, Ghost, anything with schema migrations)
// a major-version update can migrate data irreversibly with no rollback
// path: Yukariko has none by design. Take a backup before deploying such
// apps, and prefer pinned versions reviewed manually over floating tags.
//
// # Ordering guarantees
//
// A deployment runs pre-steps → deploy steps (or pull/verify/up) →
// post-steps → required post-deploy health checks → exactly one deployed-
// version checkpoint. Any failure or cancellation before the checkpoint
// leaves the previously deployed version intact.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/git"
	"github.com/Dragonshorn-Studios/yukariko/internal/health"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
)

// VersionCheckpoint records the deployed version exactly once per
// successful deployment. The store adapter (#3 deployed_versions) implements
// it; the pipeline refuses to deploy when it is missing — failing closed.
type VersionCheckpoint interface {
	MarkDeployed(ctx context.Context, appID, version string) error
}

// GitPreparer is the slice of internal/git the pipeline needs.
type GitPreparer interface {
	Prepare(ctx context.Context, app *config.App) (git.Result, error)
}

// HealthChecker is the slice of internal/health the pipeline needs.
type HealthChecker interface {
	RunPostDeployChecks(ctx context.Context, app *config.App) ([]health.Sample, error)
}

// DigestResolver is the slice of internal/registry the pipeline needs.
type DigestResolver interface {
	Resolve(ctx context.Context, ref registry.Ref, platform string) (string, error)
}

// ComposePipeline deploys Compose apps through their own Compose context:
// exact files, env files, profiles, and project name in every invocation.
// Compose stays the source of truth — ports, networks, volumes, and
// environment are never reconstructed.
type ComposePipeline struct {
	// Runner executes commands when Run is nil.
	Runner *runner.Runner
	// Run overrides command execution (tests inject a recorder). When nil,
	// Runner.Run is used.
	Run func(ctx context.Context, req runner.Request) (runner.Result, error)

	Git        GitPreparer
	Health     HealthChecker
	Registry   DigestResolver
	Checkpoint VersionCheckpoint
	// Platform resolves registry images when an image carries no platform.
	Platform string
}

var _ schedule.Deployer = (*ComposePipeline)(nil)

// Deploy implements schedule.Deployer for Compose apps. The caller must hold
// the app's per-app lock; the scheduler guarantees this.
func (p *ComposePipeline) Deploy(ctx context.Context, app *config.App) (schedule.DeployResult, error) {
	if app == nil || app.Deploy.Mode != config.DeployCompose || app.Deploy.Compose == nil {
		return schedule.DeployResult{}, errors.New("app is not a compose deployment")
	}
	if p.Checkpoint == nil {
		return schedule.DeployResult{}, errors.New("deployed-version checkpoint is not configured; refusing to deploy")
	}

	var (
		stages  []string
		version string
		err     error
	)
	switch app.Source.Mode {
	case config.SourceRegistry:
		version, stages, err = p.deployRegistry(ctx, app)
	case config.SourceGit:
		version, stages, err = p.deployGit(ctx, app)
	default:
		err = fmt.Errorf("unsupported source mode %q", app.Source.Mode)
	}
	if err != nil {
		return schedule.DeployResult{Success: false, Detail: strings.Join(stages, "; ")}, fmt.Errorf("compose deploy failed: %w", err)
	}

	// Required post-deploy checks run inside the success transaction: a
	// failure here must leave the deployed version untouched.
	samples, checkErr := p.healthChecker().RunPostDeployChecks(ctx, app)
	if ctx.Err() != nil {
		return schedule.DeployResult{}, fmt.Errorf("compose deploy cancelled after commands; deployed version unchanged")
	}
	if checkErr != nil {
		return schedule.DeployResult{Success: false}, fmt.Errorf("compose deploy failed: %w", checkErr)
	}
	stages = append(stages, fmt.Sprintf("post-deploy checks: %d ok", len(samples)))

	if ctx.Err() != nil {
		return schedule.DeployResult{}, errors.New("compose deploy cancelled; deployed version unchanged")
	}
	if err := p.Checkpoint.MarkDeployed(ctx, app.ID, version); err != nil {
		return schedule.DeployResult{}, fmt.Errorf("checkpoint failed; deployed version unchanged: %w", err)
	}
	stages = append(stages, "checkpoint: "+version)
	return schedule.DeployResult{Success: true, Detail: strings.Join(stages, "; ")}, nil
}

// deployRegistry pulls, verifies the expected digests were obtained, and
// brings the stack up. It returns the deployed version identity.
func (p *ComposePipeline) deployRegistry(ctx context.Context, app *config.App) (version string, stages []string, err error) {
	expected, err := p.expectedDigests(ctx, app)
	if err != nil {
		return "", stages, err
	}

	// Pull everything through the project's own context.
	if err := p.compose(ctx, app, app.Timeout.D(), "pull"); err != nil {
		return "", stages, fmt.Errorf("pull: %w", err)
	}
	stages = append(stages, "pull ok")

	// Verify each expected manifest digest actually arrived.
	for _, img := range app.Source.Registry.Images {
		if err := p.verifyImageDigest(ctx, img.Ref, expected[img.Ref]); err != nil {
			return "", stages, err
		}
	}
	stages = append(stages, fmt.Sprintf("digests verified: %d image(s)", len(app.Source.Registry.Images)))

	if err := p.composeUp(ctx, app, false); err != nil {
		return "", stages, err
	}
	stages = append(stages, "up ok")

	return expectedVersion(expected), stages, nil
}

// deployGit prepares the worktree (fetch + fast-forward-only) and runs the
// configured steps. An empty deploy-step list uses the documented default:
// `docker compose <context> up -d --build --wait`.
func (p *ComposePipeline) deployGit(ctx context.Context, app *config.App) (version string, stages []string, err error) {
	res, err := p.git().Prepare(ctx, app)
	if err != nil {
		return "", stages, err
	}
	if res.Status != git.StatusReady {
		return "", stages, fmt.Errorf("worktree preparation blocked: %s", res.Detail)
	}
	stages = append(stages, "worktree prepared: "+res.PreparedSHA)

	for _, step := range app.Steps.Pre {
		if err := p.runStep(ctx, app, step, "pre"); err != nil {
			return "", stages, fmt.Errorf("pre step %q: %w", step.Name, err)
		}
		stages = append(stages, "pre ok")
	}
	if len(app.Steps.Deploy) == 0 {
		if err := p.composeUp(ctx, app, true); err != nil {
			return "", stages, err
		}
		stages = append(stages, "up ok (default up -d --build --wait)")
	} else {
		for _, step := range app.Steps.Deploy {
			if err := p.runStep(ctx, app, step, "deploy"); err != nil {
				return "", stages, fmt.Errorf("deploy step %q: %w", step.Name, err)
			}
			stages = append(stages, "deploy step ok")
		}
	}
	for _, step := range app.Steps.Post {
		if err := p.runStep(ctx, app, step, "post"); err != nil {
			return "", stages, fmt.Errorf("post step %q: %w", step.Name, err)
		}
		stages = append(stages, "post ok")
	}
	return res.PreparedSHA, stages, nil
}

// expectedDigests resolves the remote manifest digest for every configured
// image, per-image platform first, then the pipeline default.
func (p *ComposePipeline) expectedDigests(ctx context.Context, app *config.App) (map[string]string, error) {
	resolver := p.Registry
	if resolver == nil {
		return nil, errors.New("registry resolver is not configured; refusing to pull unverified images")
	}
	expected := map[string]string{}
	for _, img := range app.Source.Registry.Images {
		ref, err := registry.ParseRef(img.Ref)
		if err != nil {
			return nil, fmt.Errorf("image %q: %w", img.Ref, err)
		}
		platform := img.Platform
		if platform == "" {
			platform = p.Platform
		}
		digest, err := resolver.Resolve(ctx, ref, platform)
		if err != nil {
			return nil, fmt.Errorf("image %q: %w", img.Ref, err)
		}
		expected[img.Ref] = digest
	}
	return expected, nil
}

// verifyImageDigest confirms the pulled image carries the expected manifest
// digest. The match is on the digest string so repository-name
// normalization in the local image store cannot cause false failures.
func (p *ComposePipeline) verifyImageDigest(ctx context.Context, imageRef, expectedDigest string) error {
	if expectedDigest == "" {
		return fmt.Errorf("image %q: no expected digest resolved", imageRef)
	}
	res, err := p.exec(ctx, runner.Request{
		Name:    "docker image inspect",
		Argv:    []string{"docker", "image", "inspect", imageRef, "--format", "{{json .RepoDigests}}"},
		Timeout: defaultVerifyTimeout,
	})
	if err != nil {
		return fmt.Errorf("image %q: %w", imageRef, err)
	}
	if !strings.Contains(string(res.Stdout), expectedDigest) {
		return fmt.Errorf("image %q: local RepoDigests do not contain the expected %s; the pull did not deliver the verified manifest", imageRef, expectedDigest)
	}
	return nil
}

// compose runs one compose verb with the exact configured context.
func (p *ComposePipeline) compose(ctx context.Context, app *config.App, timeout time.Duration, verb ...string) error {
	argv := append([]string{"docker"}, composeContextArgs(app)...)
	argv = append(argv, verb...)
	_, err := p.exec(ctx, runner.Request{
		Name:    "docker compose " + strings.Join(verb, " "),
		Argv:    argv,
		Dir:     app.Deploy.Compose.WorkDir,
		Timeout: timeout,
		AppID:   app.ID,
	})
	return err
}

// composeUp is the `up -d --wait` (registry) or `up -d --build --wait`
// (git default) invocation.
func (p *ComposePipeline) composeUp(ctx context.Context, app *config.App, build bool) error {
	verb := []string{"up", "-d"}
	if build {
		verb = append(verb, "--build")
	}
	verb = append(verb, "--wait")
	timeout := app.Timeout.D()
	if timeout <= 0 {
		timeout = config.DefaultTimeout.D()
	}
	return p.compose(ctx, app, timeout, verb...)
}

// runStep executes one user-configured step.
func (p *ComposePipeline) runStep(ctx context.Context, app *config.App, step config.Step, kind string) error {
	timeout := step.Timeout.D()
	if timeout <= 0 {
		timeout = config.DefaultStepTimeout.D()
	}
	dir := step.Dir
	if dir == "" {
		dir = app.Deploy.Compose.WorkDir
	}
	_, err := p.exec(ctx, runner.Request{
		Name:    kind + " step " + orDefault(step.Name, "(unnamed)"),
		Argv:    step.Command,
		Dir:     dir,
		Timeout: timeout,
		Shell:   step.Shell,
		AppID:   app.ID,
	})
	return err
}

// composeContextArgs builds the project-context arguments shared by every
// compose invocation: files in override order, env files, profiles, and
// the project name.
func composeContextArgs(app *config.App) []string {
	c := app.Deploy.Compose
	args := []string{"compose"}
	for _, f := range c.Files {
		args = append(args, "-f", f)
	}
	for _, e := range c.EnvFiles {
		args = append(args, "--env-file", e)
	}
	for _, p := range c.Profiles {
		args = append(args, "--profile", p)
	}
	if c.ProjectName != "" {
		args = append(args, "-p", c.ProjectName)
	}
	return args
}

// expectedVersion renders the registry-mode deployed identity: sorted
// ref@digest pairs so the same deployment always checkpoints identically.
func expectedVersion(expected map[string]string) string {
	pairs := make([]string, 0, len(expected))
	for ref, digest := range expected {
		pairs = append(pairs, ref+"@"+digest)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (p *ComposePipeline) git() GitPreparer {
	if p.Git == nil {
		return errorGitPreparer{}
	}
	return p.Git
}

func (p *ComposePipeline) healthChecker() HealthChecker {
	if p.Health == nil {
		return errorHealthChecker{}
	}
	return p.Health
}

type errorGitPreparer struct{}

func (errorGitPreparer) Prepare(context.Context, *config.App) (git.Result, error) {
	return git.Result{}, errors.New("git preparer is not configured")
}

type errorHealthChecker struct{}

func (errorHealthChecker) RunPostDeployChecks(context.Context, *config.App) ([]health.Sample, error) {
	return nil, errors.New("health checker is not configured")
}
