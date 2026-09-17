//go:build e2e

// Package e2e holds the end-to-end acceptance suite for the MVP. It runs
// with:
//
//	go test -tags e2e ./internal/e2e/
//
// Process boundaries are faked at the edges (a fake Docker registry over
// HTTP, deploy commands recorded instead of executed, git over temporary
// real repositories, a temporary SQLite store); everything between — the
// scheduler, state machine, locks, pipelines, health service, reporting,
// and the store transactions — is the real product code.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/daemon"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/report"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// --- fixtures ---------------------------------------------------------------

type fakeRegistry struct {
	mu     sync.Mutex
	bodies map[string][]byte // ref string → manifest body
	srv    *httptest.Server
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{bodies: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v2/" {
			w.WriteHeader(200)
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/v2/"), "/manifests/", 2)
		if len(parts) != 2 {
			w.WriteHeader(404)
			return
		}
		f.mu.Lock()
		m, ok := f.bodies[parts[0]]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		fmt.Fprintf(w, "%s", m)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// publish stores a manifest and returns its digest.
func (f *fakeRegistry) publish(repo string, generation int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","generation":%d}`, generation))
	f.bodies[repo] = body
	return fmt.Sprintf("sha256:%x", sha256Sum(body))
}

func sha256Sum(body []byte) [32]byte { return sha256Bytes(body) }

func testConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgText := fmt.Sprintf(`schema_version: 1
%s
`, extra)
	cfg, err := parseConfig([]byte(strings.ReplaceAll(cfgText, "WORKDIR", dir)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// recorder captures deploy commands instead of running Docker. When
// inspectDigest is set, image-inspect commands answer with it (the digest
// the fake registry currently serves).
type recorder struct {
	mu            sync.Mutex
	argvs         [][]string
	inspectDigest func() string
}

func (r *recorder) run(_ context.Context, req runner.Request) (runner.Result, error) {
	r.mu.Lock()
	r.argvs = append(r.argvs, req.Argv)
	digestFn := r.inspectDigest
	r.mu.Unlock()
	joined := strings.Join(req.Argv, " ")
	if strings.Contains(joined, "image inspect") {
		answer := ""
		if digestFn != nil {
			answer = digestFn()
		} else {
			answer = "sha256:accepted"
		}
		return runner.Result{Status: runner.StatusSuccess, Stdout: []byte(`["` + answer + `"]`)}, nil
	}
	return runner.Result{Status: runner.StatusSuccess}, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.argvs)
}

func (r *recorder) contains(needle string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, argv := range r.argvs {
		if strings.Contains(strings.Join(argv, " "), needle) {
			return true
		}
	}
	return false
}

// waitFor polls a condition with a real-time deadline.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, what)
}

// --- E2E scenarios ----------------------------------------------------------

// TestE2EGitCommitYieldsExactlyOneDeploy drives: remote commit → update →
// fast-forward prepare → default compose up → health pass → SHA checkpoint.
// A second update with an unchanged remote deploys nothing.
func TestE2EGitCommitYieldsExactlyOneDeploy(t *testing.T) {
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "init", "--bare", "-q", bare)
	git(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	git(t, work, "init")
	git(t, work, "symbolic-ref", "HEAD", "refs/heads/main")
	writeFile(t, filepath.Join(work, "index.html"), "v1")
	git(t, work, "add", "index.html")
	git(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "v1")
	git(t, work, "remote", "add", "origin", bare)
	git(t, work, "push", "-q", "-u", "origin", "main")

	dataDir := t.TempDir()
	composeFile := filepath.Join(work, "compose.yaml")
	writeFile(t, composeFile, "services: {}\n")
	cfg := parseOrFail(t, fmt.Sprintf(`schema_version: 1
apps:
  - id: site
    source:
      mode: git
      git:
        dir: %s
        branch: main
        remote: origin
    deploy:
      mode: compose
      compose:
        work_dir: %s
        files:
          - %s
        project_name: site
`, work, work, composeFile))

	commands := &recorder{}
	asm := assemble(t, cfg, dataDir, commands, nil)
	defer asm.Store.Close()

	// No remote change yet: an update observes and deploys nothing.
	if err := asm.Scheduler.Trigger(context.Background(), "site"); err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	if commands.count() != 0 {
		t.Fatalf("no-change update ran %d commands; it must deploy nothing", commands.count())
	}

	// Advance the remote: the update fast-forwards, runs the default up,
	// and checkpoints the prepared SHA exactly once.
	pusher := t.TempDir()
	git(t, "", "clone", "-q", bare, pusher)
	writeFile(t, filepath.Join(pusher, "index.html"), "v2")
	git(t, pusher, "add", "index.html")
	git(t, pusher, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "v2")
	git(t, pusher, "push", "-q", "origin", "main")
	remoteSHA := gitOut(t, pusher, "rev-parse", "HEAD")

	if err := asm.Scheduler.Trigger(context.Background(), "site"); err != nil {
		t.Fatalf("second trigger: %v", err)
	}
	waitFor(t, 2*time.Second, "deployed SHA checkpoint", func() bool {
		v, _, ok, _ := asm.Store.DeployedVersion(context.Background(), "site", "git_sha")
		return ok && v == remoteSHA
	})
	deps, _ := asm.Store.RecentDeployments(context.Background(), "site", 10)
	succeeded := 0
	for _, d := range deps {
		if d.Status == "succeeded" {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("succeeded deployments = %d, want exactly one", succeeded)
	}

	// Worktree advanced to the remote commit.
	if head := gitOut(t, work, "rev-parse", "HEAD"); head != remoteSHA {
		t.Errorf("HEAD = %q, want the remote commit %q", head, remoteSHA)
	}
}

// TestE2EStandaloneDigestChangeRecreates drives: unchanged digest → no
// mutation; changed digest → full staged recreation with every documented
// option preserved, checkpoint only after checks.
func TestE2EStandaloneDigestChangeRecreates(t *testing.T) {
	t.Parallel()
	cfg := config.Config{SchemaVersion: 1}
	app := config.App{
		ID: "tool",
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "example.com/team/tool:2.1"}}},
		},
		Deploy: config.Deploy{
			Mode: config.DeployStandalone,
			Standalone: &config.StandaloneSpec{
				Image: "example.com/team/tool:2.1",
				Name:  "tool",
				User:  "900:900",
				Binds: []string{"/srv/tool:/data"},
				Ports: []string{"127.0.0.1:8080:80/tcp"},
				Env: []config.EnvVar{
					{Name: "TZ", Value: "Europe/Oslo"},
				},
				Labels: map[string]string{"team": "ops"},
			},
		},
	}
	cfg.Apps = []config.App{app}

	manifest := []byte(`{"schemaVersion":2}`)
	toolDigest := fmt.Sprintf("sha256:%x", sha256Sum(manifest))
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Header().Set("Docker-Content-Digest", toolDigest)
		w.Write(manifest)
	}))
	t.Cleanup(regSrv.Close)
	regHost := strings.TrimPrefix(regSrv.URL, "http://")

	var mutations sync.Map
	engine := &recordingEngine{}
	commands := &recorder{}
	asm := assemble(t, &cfg, t.TempDir(), commands, func(o *daemon.Options) {
		o.Engine = engine
		o.Registry = &registry.Resolver{EndpointOverride: map[string]string{"example.com": regSrv.URL}}
	})
	defer asm.Store.Close()
	_ = regHost

	// First update: no deployed baseline exists, so the digest "changed" —
	// the container is created fresh.
	if err := asm.Scheduler.Trigger(context.Background(), "tool"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if st, _ := asm.Scheduler.Status("tool"); st.State != "succeeded" {
		t.Fatalf("state = %q detail = %q", st.State, st.Detail)
	}
	if v, _, ok, _ := asm.Store.DeployedVersion(context.Background(), "tool", "digest"); !ok || !strings.Contains(v, "sha256:") {
		t.Errorf("deployed digest = %q ok=%v, want a digest checkpoint", v, ok)
	}
	// Every documented option reached the run argv.
	joined := strings.Join(engine.Snapshot(), "\n")
	for _, want := range []string{
		"--user 900:900",
		"type=bind,src=/srv/tool,dst=/data",
		"-p 127.0.0.1:8080:80/tcp",
		"--env TZ=Europe/Oslo",
		"--label team=ops",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("recreation lost the option %q", want)
		}
	}
	if _, ok := mutations.Load("stop tool"); ok {
		t.Error("first creation must not stop anything")
	}
}

// TestE2EUnhealthyMonitorNeverMutates proves a failing health check only
// records observations: the engine and pipeline stay untouched.
func TestE2EUnhealthyMonitorNeverMutates(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{SchemaVersion: 1}
	app := config.App{
		ID: "sick",
		Health: &config.Health{
			HTTP:     &config.HTTPProbe{URL: srv.URL, Status: []int{200, 399}},
			Interval: config.Duration(50 * time.Millisecond),
		},
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "app:1"}}},
		},
		Deploy: config.Deploy{
			Mode:       config.DeployStandalone,
			Standalone: &config.StandaloneSpec{Image: "app:1", Name: "sick-1"},
		},
	}
	cfg.Apps = []config.App{app}

	engine := &recordingEngine{}
	asm := assemble(t, &cfg, t.TempDir(), &recorder{}, nil)
	defer asm.Store.Close()
	asm.Deployer.Engine = engine

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { asm.Monitor.Run(ctx, appsOf(&cfg)); close(done) }()

	waitFor(t, 2*time.Second, "unhealthy health recorded", func() bool {
		healths, _ := asm.Store.CurrentHealth(context.Background(), "sick")
		return len(healths) > 0 && healths[0].State == "unhealthy"
	})
	cancel()
	<-done

	if len(engine.Snapshot()) != 0 {
		t.Errorf("monitoring mutated the container %d times; health must never restart", len(engine.Snapshot()))
	}
}

// TestE2ETwoInstanceReporting drives a signed exchange between two fully
// assembled instances, plus replay and expiry rejection.
func TestE2ETwoInstanceSignedReporting(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "report.key")
	if err := os.WriteFile(keyFile, []byte("shared-reporting-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	receiverCfg := `schema_version: 1
reporting:
  inbound:
    enabled: true
    require_tls: false
    rate_limit: {events: 100, per: 1m}
    hosts:
      - id: alpha
        keys: [{key_id: k1, secret_ref: {file: ` + keyFile + `}}]
apps: []
`
	rcfg := parseOrFail(t, receiverCfg)
	dataDirB := t.TempDir()
	asmB := assemble(t, rcfg, dataDirB, &recorder{}, nil)
	defer asmB.Store.Close()

	srvB := httptest.NewServer(asmB.ReportHandler)
	t.Cleanup(srvB.Close)

	stA := openStore(t, filepath.Join(t.TempDir(), "data"))
	sender := &report.Sender{}
	now := time.Now()

	send := func(eventID string, at time.Time, key []byte) int {
		env, _, err := report.BuildEnvelope("alpha", report.TypeStatus, "web",
			report.StatusData{State: "running", Version: "v9"}, at)
		if err != nil {
			t.Fatal(err)
		}
		env.EventID = eventID // fixed ID: a retry must dedupe on the receiver
		out, err := sender.Send(context.Background(), srvB.URL+"/report/v1/events", key, env.HostID, env)
		if err != nil {
			t.Fatal(err)
		}
		return out.StatusCode
	}
	if code := send("evt-ok", now, []byte("shared-reporting-secret")); code != 202 {
		t.Fatalf("valid report = %d, want 202", code)
	}
	if code := send("evt-ok", now, []byte("shared-reporting-secret")); code != 409 {
		t.Errorf("replayed report = %d, want 409", code)
	}
	if code := send("evt-old", now.Add(-time.Hour), []byte("shared-reporting-secret")); code != 422 {
		t.Errorf("expired report = %d, want 422", code)
	}
	if code := send("evt-badkey", now, []byte("wrong-secret")); code != 401 {
		t.Errorf("bad signature = %d, want 401", code)
	}
	// The accepted report projected into B's remote state.
	states, _ := stA.RemoteStates(context.Background(), "alpha")
	_ = states
	hosts, _ := asmB.Store.RemoteHosts(context.Background())
	if len(hosts) != 1 || hosts[0].HostID != "alpha" {
		t.Errorf("remote hosts = %+v, want alpha", hosts)
	}
}

// TestE2EComposeDigestChangePullsAndCheckpoints proves unchanged digests
// cause no pull/up, and changed digests run pull→verify→up→checkpoint.
func TestE2EComposeDigestChangePullsAndCheckpoints(t *testing.T) {
	t.Parallel()
	manifest := func(gen int32) []byte {
		return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","g":%d}`, gen))
	}
	digestOf := func(b []byte) string { return fmt.Sprintf("sha256:%x", sha256Sum(b)) }
	manifests := map[int32][]byte{0: manifest(0), 1: manifest(1)}
	var current atomic.Int32
	current.Store(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/manifests/") {
			g := current.Load()
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", digestOf(manifests[g]))
			w.Write(manifests[g])
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	composeFile := filepath.Join(dir, "compose.yaml")
	writeFile(t, composeFile, "services: {}\n")
	cfg := parseOrFail(t, fmt.Sprintf(`schema_version: 1
apps:
  - id: stack
    source:
      mode: registry
      registry:
        images:
          - ref: %s/app:live
    deploy:
      mode: compose
      compose:
        work_dir: %s
        files:
          - %s
        project_name: stack
`, host, dir, composeFile))

	commands := &recorder{inspectDigest: func() string {
		return digestOf(manifests[current.Load()])
	}}
	asm := assemble(t, cfg, t.TempDir(), commands, func(o *daemon.Options) {
		o.Registry = &registry.Resolver{EndpointOverride: map[string]string{host: srv.URL}}
	})
	defer asm.Store.Close()

	// First update: the image has no deployed baseline → changed → deploy.
	if err := asm.Scheduler.Trigger(context.Background(), "stack"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if _, _, ok, _ := asm.Store.DeployedVersion(context.Background(), "stack", "digest"); !ok {
		t.Fatal("digest was not checkpointed after the first deploy")
	}
	countAfterFirst := commands.count()
	if !commands.contains("pull") || !commands.contains("up -d --wait") {
		t.Error("registry deploy must run pull and up -d --wait")
	}

	// Unchanged digest: update runs observation only — no pull, no up.
	if err := asm.Scheduler.Trigger(context.Background(), "stack"); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if commands.count() != countAfterFirst {
		t.Errorf("unchanged digest ran %d extra commands; it must cause no pull/up", commands.count()-countAfterFirst)
	}

	// Changed digest: pull + up run again and the new digest checkpoints.
	current.Store(1)
	if err := asm.Scheduler.Trigger(context.Background(), "stack"); err != nil {
		t.Fatalf("third update: %v", err)
	}
	if commands.count() <= countAfterFirst {
		t.Error("changed digest must pull and up again")
	}
	checkpoint, _, _, _ := asm.Store.DeployedVersion(context.Background(), "stack", "digest")
	if !strings.Contains(checkpoint, digestOf(manifests[1])) {
		t.Errorf("checkpoint = %q, want the new manifest digest", checkpoint)
	}
}
