// compose_test.go — fake-runner command-order and argument tests for the
// Compose pipeline (#11).
package deploy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/git"
	"github.com/Dragonshorn-Studios/yukariko/internal/health"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
)

// fakeExec records every command in order and answers from a script.
type fakeExec struct {
	mu      sync.Mutex
	argvs   [][]string
	dirs    []string
	envs    [][]string
	respond func(req runner.Request) runner.Result
}

func (f *fakeExec) run(_ context.Context, req runner.Request) (runner.Result, error) {
	f.mu.Lock()
	f.argvs = append(f.argvs, req.Argv)
	f.dirs = append(f.dirs, req.Dir)
	f.envs = append(f.envs, req.Env)
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(req), nil
	}
	// The default image inspect answer carries the expected digest so the
	// verification step passes unless a test overrides the responder.
	if strings.Contains(strings.Join(req.Argv, " "), "image inspect") {
		return runner.Result{
			Status: runner.StatusSuccess,
			Stdout: []byte(`["ghcr.io/example/web@` + webDigest + `"]`),
		}, nil
	}
	return runner.Result{Status: runner.StatusSuccess}, nil
}

func (f *fakeExec) all() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.argvs...)
}

func (f *fakeExec) allEnvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.envs...)
}

func (f *fakeExec) joined() string {
	lines := make([]string, 0, len(f.all()))
	for _, argv := range f.all() {
		lines = append(lines, strings.Join(argv, " "))
	}
	return strings.Join(lines, "\n")
}

type fakeGitPrep struct {
	result git.Result
	err    error
	calls  int
	mu     sync.Mutex
}

func (f *fakeGitPrep) Prepare(_ context.Context, _ *config.App) (git.Result, error) {
	f.mu.Lock()
	f.calls++
	res, err := f.result, f.err
	f.mu.Unlock()
	return res, err
}

type fakeHealth struct {
	err   error
	calls int
	mu    sync.Mutex
}

func (f *fakeHealth) RunPostDeployChecks(_ context.Context, _ *config.App) ([]health.Sample, error) {
	f.mu.Lock()
	f.calls++
	err := f.err
	f.mu.Unlock()
	return nil, err
}

type countingCheckpoint struct {
	mu       sync.Mutex
	versions []string
}

func (c *countingCheckpoint) MarkDeployed(_ context.Context, appID, version string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.versions = append(c.versions, appID+"="+version)
	return nil
}

func (c *countingCheckpoint) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.versions)
}

type fakeResolver struct {
	digests map[string]string
	err     error
}

func (f *fakeResolver) Resolve(_ context.Context, ref registry.Ref, _ string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.digests[ref.String()], nil
}

func composeApp() *config.App {
	return &config.App{
		ID:      "web",
		Timeout: config.Duration(5 * time.Minute),
		Source: config.Source{
			Mode: config.SourceRegistry,
			Registry: &config.RegistrySource{
				Images: []config.ImageRef{
					{Ref: "ghcr.io/example/web:1.0", Platform: "linux/amd64"},
				},
			},
		},
		Deploy: config.Deploy{
			Mode: config.DeployCompose,
			Compose: &config.ComposeDeploy{
				WorkDir:     "/srv/web",
				Files:       []string{"/srv/web/compose.yaml", "/srv/web/override.yaml"},
				EnvFiles:    []string{"/srv/web/.env"},
				Profiles:    []string{"edge"},
				ProjectName: "web",
			},
		},
	}
}

const webDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func newComposePipeline(exec *fakeExec, ck *countingCheckpoint) *ComposePipeline {
	return &ComposePipeline{
		Run:        exec.run,
		Git:        &fakeGitPrep{result: git.Result{Status: git.StatusReady, PreparedSHA: "abc123"}},
		Health:     &fakeHealth{},
		Registry:   &fakeResolver{digests: map[string]string{"ghcr.io/example/web:1.0": webDigest}},
		Checkpoint: ck,
	}
}

func TestRegistryModeCommandOrderAndContext(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)

	res, err := p.Deploy(context.Background(), composeApp())
	if err != nil || !res.Success {
		t.Fatalf("deploy = %v / %v (detail %q)", err, res.Success, res.Detail)
	}

	argvs := exec.all()
	if len(argvs) != 3 {
		t.Fatalf("commands = %d, want pull, verify, up (and 0 steps):\n%s", len(argvs), exec.joined())
	}
	pull := strings.Join(argvs[0], " ")
	verify := strings.Join(argvs[1], " ")
	up := strings.Join(argvs[2], " ")

	// Every compose command preserves the exact context.
	for _, cmd := range []string{pull, up} {
		for _, want := range []string{
			"compose", "-f /srv/web/compose.yaml", "-f /srv/web/override.yaml",
			"--env-file /srv/web/.env", "--profile edge", "-p web",
		} {
			if !strings.Contains(cmd, want) {
				t.Errorf("command %q missing context %q", cmd, want)
			}
		}
	}
	// Order: pull → verify → up.
	if !strings.HasSuffix(pull, "pull") {
		t.Errorf("first command = %q, want pull", pull)
	}
	if !strings.Contains(verify, "image inspect ghcr.io/example/web:1.0") {
		t.Errorf("second command = %q, want image digest verification", verify)
	}
	if !strings.HasSuffix(up, "up -d --wait") {
		t.Errorf("third command = %q, want up -d --wait", up)
	}
	// The workdir is applied to compose commands.
	if exec.dirs[0] != "/srv/web" || exec.dirs[2] != "/srv/web" {
		t.Errorf("compose dirs = %v, want the configured workdir", exec.dirs)
	}
	// Success checkpoints exactly once, with the verified digest identity.
	if ck.count() != 1 {
		t.Fatalf("checkpoints = %d, want exactly one", ck.count())
	}
	if !strings.Contains(ck.versions[0], webDigest) || !strings.HasPrefix(ck.versions[0], "web=") {
		t.Errorf("checkpoint = %q, want web=<ref>@<digest>", ck.versions[0])
	}
}

func TestGitModeDefaultAndConfiguredSteps(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)
	app := composeApp()
	app.Source = config.Source{
		Mode: config.SourceGit,
		Git:  &config.GitSource{Dir: "/srv/web", Branch: "main", Remote: "origin"},
	}
	// No configured steps: the documented default up -d --build --wait.
	if _, err := p.Deploy(context.Background(), app); err != nil {
		t.Fatalf("default git deploy: %v", err)
	}
	argvs := exec.all()
	if len(argvs) != 1 {
		t.Fatalf("commands = %d, want only the default up", len(argvs))
	}
	if !strings.HasSuffix(strings.Join(argvs[0], " "), "up -d --build --wait") {
		t.Errorf("default deploy = %q, want up -d --build --wait", strings.Join(argvs[0], " "))
	}
	if ck.count() != 1 || !strings.Contains(ck.versions[0], "abc123") {
		t.Errorf("checkpoint = %v, want the prepared SHA exactly once", ck.versions)
	}

	// Configured steps run pre → deploy → post in order with the step dir.
	exec2 := &fakeExec{}
	ck2 := &countingCheckpoint{}
	p2 := newComposePipeline(exec2, ck2)
	stepApp := composeApp()
	stepApp.Source = config.Source{
		Mode: config.SourceGit,
		Git:  &config.GitSource{Dir: "/srv/web", Branch: "main"},
	}
	stepApp.Steps = config.Steps{
		Pre:    []config.Step{{Name: "backup", Command: []string{"/usr/local/bin/backup"}, Timeout: config.Duration(time.Minute)}},
		Deploy: []config.Step{{Name: "up", Command: []string{"docker", "compose", "up", "-d"}, Dir: "/custom/dir"}},
		Post:   []config.Step{{Name: "notify", Command: []string{"/usr/local/bin/notify"}}},
	}
	if _, err := p2.Deploy(context.Background(), stepApp); err != nil {
		t.Fatalf("stepped git deploy: %v", err)
	}
	got := exec2.joined()
	for _, want := range []string{"/usr/local/bin/backup", "docker compose up -d", "/usr/local/bin/notify"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing step %q in:\n%s", want, got)
		}
	}
	if len(exec2.dirs) < 2 || exec2.dirs[1] != "/custom/dir" {
		t.Errorf("step dirs = %v, want the step's own dir in second position", exec2.dirs)
	}
}

func TestPullFailureLeavesCheckpointUntouched(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{respond: func(req runner.Request) runner.Result {
		if strings.HasSuffix(strings.Join(req.Argv, " "), "pull") {
			return runner.Result{Status: runner.StatusFailed, ExitCode: 18, Err: "pull failed"}
		}
		return runner.Result{Status: runner.StatusSuccess}
	}}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)

	res, err := p.Deploy(context.Background(), composeApp())
	if err == nil || res.Success {
		t.Fatal("pull failure must fail the deployment")
	}
	if ck.count() != 0 {
		t.Errorf("checkpoints = %d, want zero", ck.count())
	}
	if strings.Contains(exec.joined(), "up") {
		t.Error("up must not run after a failed pull")
	}
}

func TestDigestVerificationFailureBlocksUp(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{respond: func(req runner.Request) runner.Result {
		// The local image store lacks the expected manifest digest.
		return runner.Result{Status: runner.StatusSuccess, Stdout: []byte(`["ghcr.io/example/web@sha256:dead"]`)}
	}}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)

	_, err := p.Deploy(context.Background(), composeApp())
	if err == nil || !strings.Contains(err.Error(), "digests do not contain") && !strings.Contains(err.Error(), "expected") {
		t.Fatalf("err = %v, want the digest verification failure", err)
	}
	if ck.count() != 0 {
		t.Errorf("checkpoints = %d, want zero", ck.count())
	}
	for _, argv := range exec.all() {
		if strings.HasSuffix(strings.Join(argv, " "), "up -d --wait") {
			t.Error("up must not run when the digest verification fails")
		}
	}
}

func TestFailedRequiredPostDeployCheckBlocksCheckpoint(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)
	p.Health = &fakeHealth{err: errors.New("required post-deploy checks failed (http: status 500); the deployment is not recorded as successful")}

	_, err := p.Deploy(context.Background(), composeApp())
	if err == nil || !strings.Contains(err.Error(), "not recorded as successful") {
		t.Fatalf("err = %v, want the required-check failure", err)
	}
	if ck.count() != 0 {
		t.Errorf("checkpoints = %d, want zero", ck.count())
	}
}

func TestGitBlockedPreparationFailsWithoutCommands(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)
	p.Git = &fakeGitPrep{result: git.Result{Status: git.StatusBlocked, Detail: "worktree has uncommitted changes"}}

	app := composeApp()
	app.Source = config.Source{Mode: config.SourceGit, Git: &config.GitSource{Dir: "/srv/web", Branch: "main"}}
	_, err := p.Deploy(context.Background(), app)
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("err = %v, want the blocked-preparation failure", err)
	}
	if len(exec.all()) != 0 {
		t.Errorf("no commands may run on blocked preparation, got:\n%s", exec.joined())
	}
	if ck.count() != 0 {
		t.Errorf("checkpoints = %d, want zero", ck.count())
	}
}

func TestCancellationNeverCheckpoints(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Deploy(ctx, composeApp())
	if err == nil {
		t.Fatal("cancelled deploy must fail")
	}
	if ck.count() != 0 {
		t.Errorf("checkpoints = %d, want zero after cancellation", ck.count())
	}
}

func TestNoChangeIsHandledUpstream(t *testing.T) {
	t.Parallel()
	// Documentation-as-test: the pipeline is invoked only after the source
	// checker reports a change (the #8 scheduler gates it). A no-change pass
	// never reaches Deploy, so there is nothing to no-op here — the pipeline
	// itself refuses nothing and deploys exactly what it is asked to.
	exec := &fakeExec{}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)
	res, err := p.Deploy(context.Background(), composeApp())
	if err != nil || !res.Success {
		t.Fatalf("direct deploy = %v / %v", err, res.Success)
	}
	if ck.count() != 1 {
		t.Errorf("checkpoints = %d, want one per successful deploy", ck.count())
	}
}

func TestNonComposeAppRejected(t *testing.T) {
	t.Parallel()
	p := newComposePipeline(&fakeExec{}, &countingCheckpoint{})
	app := &config.App{ID: "x", Deploy: config.Deploy{Mode: config.DeployStandalone}}
	if _, err := p.Deploy(context.Background(), app); err == nil {
		t.Fatal("non-compose app must be rejected")
	}
}

// compile-time interface assertions
var (
	_ schedule.Deployer = (*ComposePipeline)(nil)
	_ GitPreparer       = (*fakeGitPrep)(nil)
	_ HealthChecker     = (*fakeHealth)(nil)
	_ DigestResolver    = (*fakeResolver)(nil)
	_ VersionCheckpoint = (*countingCheckpoint)(nil)
)

// Every docker invocation a compose deploy makes — pull, the image-inspect
// digest verify, and the compose verbs — must address the app's resolved
// endpoint; user-owned step argv instead gets the endpoint as env.
func TestComposeEndpointFlagsAndStepEnv(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)
	p.DefaultEndpoint = &config.DockerEndpoint{Host: "unix:///run/user/1000/docker.sock"}

	app := composeApp()
	// Steps belong to the git path (#11): switch the app so the step env
	// assertion exercises a real flow.
	app.Source = config.Source{Mode: config.SourceGit, Git: &config.GitSource{Dir: "/srv/web", Branch: "main"}}
	app.Steps.Pre = []config.Step{{Name: "notify", Command: []string{"make", "notify"}}}
	res, err := p.Deploy(context.Background(), app)
	if err != nil || !res.Success {
		t.Fatalf("deploy = %v / %v (detail %q)", err, res.Success, res.Detail)
	}

	dockerPrefix := "docker -H unix:///run/user/1000/docker.sock"
	for _, argv := range exec.all() {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "image inspect"):
			if !strings.HasPrefix(joined, dockerPrefix+" image inspect") {
				t.Errorf("image inspect without endpoint: %s", joined)
			}
		case strings.Contains(joined, "compose"):
			if !strings.HasPrefix(joined, dockerPrefix+" compose") {
				t.Errorf("compose verb without endpoint: %s", joined)
			}
		case strings.HasPrefix(joined, "make"):
			// user step: argv untouched
		default:
			t.Errorf("unexpected argv: %s", joined)
		}
	}

	// The user step's argv is untouched and its env carries the endpoint.
	var stepEnv []string
	for i, argv := range exec.all() {
		if len(argv) > 0 && argv[0] == "make" {
			stepEnv = exec.allEnvs()[i]
		}
	}
	if stepEnv == nil {
		t.Fatal("pre step never ran")
	}
	if len(stepEnv) != 1 || stepEnv[0] != "DOCKER_HOST=unix:///run/user/1000/docker.sock" {
		t.Fatalf("step env = %v, want DOCKER_HOST", stepEnv)
	}
}

// A per-app endpoint overrides the configuration-wide default.
func TestComposeAppEndpointOverridesDefault(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	ck := &countingCheckpoint{}
	p := newComposePipeline(exec, ck)
	p.DefaultEndpoint = &config.DockerEndpoint{Context: "wrong-one"}

	app := composeApp()
	app.Docker = &config.DockerEndpoint{Context: "rootless"}
	if _, err := p.Deploy(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	for _, argv := range exec.all() {
		joined := strings.Join(argv, " ")
		if strings.HasPrefix(joined, "docker ") && !strings.HasPrefix(joined, "docker --context rootless") {
			t.Errorf("argv not using the app override: %s", joined)
		}
	}
}
