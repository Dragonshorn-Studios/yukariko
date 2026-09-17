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

	"github.com/Dragonshorn-Studios/yukariko/internal/daemon"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
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
	var rows []daemon.AppStatus
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
	var events []daemon.LogEvent
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
