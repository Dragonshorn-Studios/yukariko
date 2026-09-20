package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/daemon"
	"github.com/Dragonshorn-Studios/yukariko/internal/exitcode"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
	"github.com/Dragonshorn-Studios/yukariko/internal/state"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

const e2eDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// pipelineRecorder records deploy commands for the no-deploy-on-check and
// update E2E assertions.
type pipelineRecorder struct {
	mu     sync.Mutex
	argvs  [][]string
	digest string
}

func (p *pipelineRecorder) run(_ context.Context, req runner.Request) (runner.Result, error) {
	p.mu.Lock()
	p.argvs = append(p.argvs, req.Argv)
	p.mu.Unlock()
	if strings.Contains(strings.Join(req.Argv, " "), "image inspect") {
		return runner.Result{
			Status: runner.StatusSuccess,
			Stdout: []byte(`["app@` + p.digest + `"]`),
		}, nil
	}
	return runner.Result{Status: runner.StatusSuccess}, nil
}

func (p *pipelineRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.argvs)
}

func (p *pipelineRecorder) all() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]string(nil), p.argvs...)
}

// recorderArgvs flattens the recorded argv lists.
func recorderArgvs(p *pipelineRecorder) []string {
	lines := make([]string, 0)
	for _, argv := range p.all() {
		lines = append(lines, strings.Join(argv, " "))
	}
	return lines
}

// e2eCLI wires an App against a fake registry, faked preflight, and
// recorded pipeline commands over a temp SQLite store.
func e2eCLI(t *testing.T, configBody string) (*App, *pipelineRecorder, string) {
	t.Helper()
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Write(manifest)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	configBody = strings.ReplaceAll(configBody, "REG_HOST", host)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configBody = strings.ReplaceAll(configBody, "COMPOSE_DIR", filepath.Join(dir, "compose.yaml"))
	configBody = strings.ReplaceAll(configBody, "WORK_DIR", dir)
	path := filepath.Join(dir, "yukariko.yaml")
	if err := os.WriteFile(path, []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}

	recorder := &pipelineRecorder{digest: digest}
	dataDir := filepath.Join(dir, "data")
	app := NewApp()
	app.daemonHook = func(o *daemon.Options) {
		o.Preflight = &schedule.Preflight{
			LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
			Exec:     func(context.Context, string, []string) error { return nil },
		}
		o.Registry = &registry.Resolver{EndpointOverride: map[string]string{host: srv.URL}}
		o.PipelineRun = recorder.run
		o.DataDir = dataDir
	}
	_ = configBody
	return app, recorder, path
}

const e2eConfig = `schema_version: 1
apps:
  - id: web
    source:
      mode: registry
      registry:
        images:
          - ref: REG_HOST/app:1
    deploy:
      mode: compose
      compose:
        work_dir: WORK_DIR
        files:
          - COMPOSE_DIR
        project_name: web
`

func TestCheckEndToEndNeverDeploys(t *testing.T) {
	t.Parallel()
	app, recorder, path := e2eCLI(t, e2eConfig)
	app.stdin = strings.NewReader("")

	stdout := &strings.Builder{}
	err := app.Execute(context.Background(), []string{"check", "--config", path, "--json"}, stdout, &strings.Builder{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(stdout.String(), `"changed": true`) {
		t.Errorf("json output missing the changed verdict:\n%s", stdout)
	}
	if recorder.count() != 0 {
		t.Errorf("check issued %d deploy-pipeline commands; it must never deploy", recorder.count())
	}
}

func TestUpdateEndToEndDeploysAndCheckpoints(t *testing.T) {
	t.Parallel()
	app, recorder, path := e2eCLI(t, e2eConfig)
	dataDir := filepath.Join(filepath.Dir(path), "data")

	stdout := &strings.Builder{}
	err := app.Execute(context.Background(),
		[]string{"update", "--config", path, "--data-dir", dataDir, "--app", "web"}, stdout, &strings.Builder{})
	if err != nil {
		t.Fatalf("update: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout.String(), "succeeded") {
		t.Errorf("update output missing success state:\n%s", stdout)
	}
	if recorder.count() < 2 {
		t.Errorf("expected pull and up commands, got %d", recorder.count())
	}
	if !strings.Contains(strings.Join(recorderArgvs(recorder), "\n"), "up -d --wait") {
		t.Error("registry deploy must run up -d --wait")
	}

	// status now shows the deployed version and up-to-date state.
	stdout2 := &strings.Builder{}
	err = app.Execute(context.Background(),
		[]string{"status", "--config", path, "--data-dir", dataDir, "--json"}, stdout2, &strings.Builder{})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var rows []state.AppStatus
	if err := json.Unmarshal([]byte(stdout2.String()), &rows); err != nil {
		t.Fatalf("status json: %v\n%s", err, stdout2)
	}
	if len(rows) != 1 || rows[0].ID != "web" || rows[0].Deployed == "" || rows[0].State != "up-to-date" {
		t.Errorf("status rows = %+v", rows)
	}

	// A second update is a no-op: the digest did not change.
	before := recorder.count()
	stdout3 := &strings.Builder{}
	if err := app.Execute(context.Background(),
		[]string{"update", "--config", path, "--data-dir", dataDir, "--app", "web"}, stdout3, &strings.Builder{}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if strings.Contains(stdout3.String(), "succeeded: deployed") {
		t.Errorf("second update must not redeploy:\n%s", stdout3)
	}
	if recorder.count() > before {
		t.Error("second update ran deploy commands despite an unchanged digest")
	}
}

func TestUpdateDryRunNeverDeploys(t *testing.T) {
	t.Parallel()
	app, recorder, path := e2eCLI(t, e2eConfig)

	stdout := &strings.Builder{}
	err := app.Execute(context.Background(),
		[]string{"update", "--config", path, "--data-dir", filepath.Join(filepath.Dir(path), "data"), "--app", "web", "--dry-run"}, stdout, &strings.Builder{})
	if err != nil {
		t.Fatalf("dry-run update: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout.String(), "would deploy") {
		t.Errorf("dry-run output missing the verdict:\n%s", stdout)
	}
	if recorder.count() != 0 {
		t.Errorf("dry-run executed %d deploy commands", recorder.count())
	}
}

func TestRestartEndToEndDoesNotAdvanceCheckpoint(t *testing.T) {
	t.Parallel()
	app, recorder, path := e2eCLI(t, e2eConfig)
	dataDir := filepath.Join(filepath.Dir(path), "data")

	if err := app.Execute(context.Background(),
		[]string{"update", "--config", path, "--data-dir", dataDir, "--app", "web"}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	stdout := &strings.Builder{}
	if err := app.Execute(context.Background(),
		[]string{"status", "--config", path, "--data-dir", dataDir, "--json"}, stdout, &strings.Builder{}); err != nil {
		t.Fatalf("status: %v", err)
	}
	var before []state.AppStatus
	if err := json.Unmarshal([]byte(stdout.String()), &before); err != nil {
		t.Fatalf("status json: %v", err)
	}
	if len(before) != 1 || before[0].Deployed == "" {
		t.Fatalf("status before restart = %+v", before)
	}
	deployed := before[0].Deployed
	cmdCount := recorder.count()

	out := &strings.Builder{}
	if err := app.Execute(context.Background(),
		[]string{"restart", "--config", path, "--data-dir", dataDir, "--app", "web"}, out, &strings.Builder{}); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "restarted") {
		t.Errorf("restart output missing confirmation:\n%s", out)
	}
	if recorder.count() != cmdCount+1 {
		t.Fatalf("restart issued %d commands after %d deploy commands, want exactly one bounce", recorder.count(), cmdCount)
	}
	joined := strings.Join(recorderArgvs(recorder), "\n")
	if !strings.Contains(joined, "compose") || !strings.HasSuffix(strings.TrimSpace(recorderArgvs(recorder)[len(recorderArgvs(recorder))-1]), "restart") {
		t.Errorf("last command was not compose restart:\n%s", joined)
	}

	stdout2 := &strings.Builder{}
	if err := app.Execute(context.Background(),
		[]string{"status", "--config", path, "--data-dir", dataDir, "--json"}, stdout2, &strings.Builder{}); err != nil {
		t.Fatalf("status after restart: %v", err)
	}
	var after []state.AppStatus
	if err := json.Unmarshal([]byte(stdout2.String()), &after); err != nil {
		t.Fatalf("status json: %v", err)
	}
	if len(after) != 1 || after[0].Deployed != deployed {
		t.Fatalf("checkpoint moved: before %+v after %+v", before, after)
	}

	logs := &strings.Builder{}
	if err := app.Execute(context.Background(),
		[]string{"logs", "--config", path, "--data-dir", dataDir, "--app", "web", "--limit", "20"}, logs, &strings.Builder{}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(logs.String(), "restart") || !strings.Contains(logs.String(), "succeeded") {
		t.Errorf("logs missing restart success:\n%s", logs)
	}
}

func TestRestartStandaloneArgvAndUnknownApp(t *testing.T) {
	t.Parallel()
	standalone := `schema_version: 1
apps:
  - id: box
    source:
      mode: registry
      registry:
        images:
          - ref: REG_HOST/app:1
    deploy:
      mode: standalone
      standalone:
        image: REG_HOST/app:1
        name: box-1
    docker:
      context: rootless
`
	app, recorder, path := e2eCLI(t, standalone)
	dataDir := filepath.Join(filepath.Dir(path), "data")

	out := &strings.Builder{}
	if err := app.Execute(context.Background(),
		[]string{"restart", "--config", path, "--data-dir", dataDir, "--app", "box"}, out, &strings.Builder{}); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	joined := strings.Join(recorderArgvs(recorder), "\n")
	if joined != "docker --context rootless restart box-1" {
		t.Fatalf("argv = %q, want standalone restart with endpoint flags", joined)
	}

	err := app.Execute(context.Background(),
		[]string{"restart", "--config", path, "--data-dir", dataDir, "--app", "missing"}, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "no app") || Code(err) != exitcode.Usage {
		t.Fatalf("err = %v code %d, want unknown-app usage", err, Code(err))
	}

	err = app.Execute(context.Background(),
		[]string{"restart", "--config", path, "--data-dir", dataDir}, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "--app") {
		t.Fatalf("err = %v, want required --app", err)
	}
}

func TestRestartRefusesInProgressDeploy(t *testing.T) {
	t.Parallel()
	app, recorder, path := e2eCLI(t, e2eConfig)
	dataDir := filepath.Join(filepath.Dir(path), "data")
	// Prime the store so the unique-index lock is held as if the daemon
	// were mid-deploy, then restart must refuse without calling docker.
	if err := app.Execute(context.Background(),
		[]string{"status", "--config", path, "--data-dir", dataDir}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("status: %v", err)
	}
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.BeginDeployment(context.Background(), store.BeginDeploymentParams{
		AppID: "web", Cause: "scheduled", At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	err = app.Execute(context.Background(),
		[]string{"restart", "--config", path, "--data-dir", dataDir, "--app", "web"}, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("err = %v, want the lock conflict", err)
	}
	if recorder.count() != 0 {
		t.Fatalf("restart issued %d docker commands while a deploy was running", recorder.count())
	}
}

func TestUpdateRequiresAppSelection(t *testing.T) {
	t.Parallel()
	app, _, path := e2eCLI(t, e2eConfig)
	stdout := &strings.Builder{}
	err := app.Execute(context.Background(), []string{"update", "--config", path}, stdout, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "--app") {
		t.Fatalf("err = %v, want the selection usage error", err)
	}
}

func TestLogsShowsHistory(t *testing.T) {
	t.Parallel()
	app, _, path := e2eCLI(t, e2eConfig)
	dataDir := filepath.Join(filepath.Dir(path), "data")

	if err := app.Execute(context.Background(),
		[]string{"update", "--config", path, "--data-dir", dataDir, "--app", "web"}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("update: %v", err)
	}

	stdout := &strings.Builder{}
	err := app.Execute(context.Background(),
		[]string{"logs", "--config", path, "--data-dir", dataDir, "--app", "web", "--limit", "10"}, stdout, &strings.Builder{})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(stdout.String(), "web") {
		t.Errorf("logs missing app events:\n%s", stdout)
	}
	// JSON shape.
	stdout2 := &strings.Builder{}
	err = app.Execute(context.Background(),
		[]string{"logs", "--config", path, "--data-dir", dataDir, "--json", "--limit", "5"}, stdout2, &strings.Builder{})
	if err != nil {
		t.Fatalf("logs json: %v", err)
	}
	var events []state.LogEvent
	if err := json.Unmarshal([]byte(stdout2.String()), &events); err != nil {
		t.Fatalf("logs json parse: %v\n%s", err, stdout2)
	}
	if len(events) == 0 {
		t.Error("logs json returned no events")
	}
}

func TestAliasEquivalence(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{"run", "materialize"},
		{"check", "divine"},
		{"update", "bless"},
		{"status", "observe"},
		{"logs", "chronicle"},
	}
	root := NewApp().Command()
	for _, pair := range pairs {
		plain, _, err := root.Find([]string{pair[0]})
		if err != nil {
			t.Fatalf("find %s: %v", pair[0], err)
		}
		alias, _, err := root.Find([]string{pair[1]})
		if err != nil {
			t.Fatalf("find %s: %v", pair[1], err)
		}
		if plain != alias {
			t.Errorf("%s and %s resolved to different commands", pair[0], pair[1])
		}
	}
	// Behavioral equivalence: both forms fail identically without --config.
	app := NewApp()
	stdout := &strings.Builder{}
	err := app.Execute(context.Background(), []string{"status"}, stdout, &strings.Builder{})
	app2 := NewApp()
	stdout2 := &strings.Builder{}
	err2 := app2.Execute(context.Background(), []string{"observe"}, stdout2, &strings.Builder{})
	if err == nil || err2 == nil || err.Error() != err2.Error() {
		t.Errorf("alias behavior differs: %v vs %v", err, err2)
	}
}
