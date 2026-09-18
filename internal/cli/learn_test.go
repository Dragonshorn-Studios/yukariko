package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/exitcode"
	"github.com/Dragonshorn-Studios/yukariko/internal/learn"
)

// fakeDocker is the read-only discovery fake used by the CLI learn tests.
type fakeDocker struct {
	summaries []docker.ContainerSummary
	details   map[string]docker.ContainerDetail
}

func (f *fakeDocker) ListContainers(ctx context.Context, all bool) ([]docker.ContainerSummary, error) {
	out := make([]docker.ContainerSummary, len(f.summaries))
	copy(out, f.summaries)
	return out, nil
}

func (f *fakeDocker) InspectContainer(ctx context.Context, id string) (docker.ContainerDetail, error) {
	if d, ok := f.details[id]; ok {
		return d, nil
	}
	return docker.ContainerDetail{}, fmt.Errorf("no such container: %w", docker.ErrContainerMissing)
}

type fakeGit struct {
	byDir map[string]learn.GitInfo
	err   error
}

func (f *fakeGit) Probe(ctx context.Context, dir string) (learn.GitInfo, error) {
	if f.err != nil {
		return learn.GitInfo{}, f.err
	}
	return f.byDir[dir], nil
}

// webappFixture is a ready Git+Compose candidate.
func webappFixture() (*fakeDocker, *fakeGit) {
	client := &fakeDocker{
		summaries: []docker.ContainerSummary{
			{ID: "aaaa1111aaaa", Name: "webapp-web-1", Image: "nginx:1.27", State: "running"},
		},
		details: map[string]docker.ContainerDetail{
			"aaaa1111aaaa": {
				ID:       "aaaa1111aaaa",
				Name:     "webapp-web-1",
				ImageRef: "nginx:1.27",
				State:    "running",
				Running:  true,
				Labels: map[string]string{
					docker.LabelProject:     "webapp",
					docker.LabelService:     "web",
					docker.LabelWorkDir:     "/srv/webapp",
					docker.LabelConfigFiles: "/srv/webapp/compose.yaml",
				},
			},
		},
	}
	git := &fakeGit{byDir: map[string]learn.GitInfo{
		"/srv/webapp": {InWorkTree: true, Branch: "main", RemoteName: "origin", RemoteURL: "https://example.com/o/r.git"},
	}}
	return client, git
}

// runLearn executes the CLI learn command with scripted stdin and returns
// stdout+stderr combined.
func runLearn(t *testing.T, app *App, configPath, stdin string) (string, int, error) {
	t.Helper()
	if stdin != "" {
		app.stdin = strings.NewReader(stdin)
	} else {
		app.stdin = strings.NewReader("")
	}
	out := &strings.Builder{}
	err := app.Execute(context.Background(),
		[]string{"learn", "--config", configPath}, out, out)
	return out.String(), Code(err), err
}

const existingConfig = `schema_version: 1
apps:
  - id: webapp
    display_name: My Webapp
    interval: 15m
    source:
      mode: registry
      registry:
        images:
          - ref: nginx:1.26
    deploy:
      mode: compose
      compose:
        work_dir: /srv/webapp
        files:
          - /srv/webapp/compose.yaml
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yukariko.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLearnJSONModeNonInteractive(t *testing.T) {
	t.Parallel()
	client, git := webappFixture()
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)

	stdout := &strings.Builder{}
	app.stdin = strings.NewReader("") // must never be read
	err := app.Execute(context.Background(),
		[]string{"learn", "--config", path, "--json"}, stdout, &strings.Builder{})
	if err != nil {
		t.Fatalf("json mode: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, `"verdict": "ready"`) || !strings.Contains(out, `"webapp"`) {
		t.Errorf("json output missing proposals: %s", out)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != existingConfig {
		t.Error("--json must never write")
	}
}

func TestLearnEndToEndMergePreservesManualSettings(t *testing.T) {
	t.Parallel()
	client, git := webappFixture()
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)

	out, code, err := runLearn(t, app, path, "1\ny\n")
	if err != nil || code != exitcode.OK {
		t.Fatalf("exit %d err %v:\n%s", code, err, out)
	}
	if !strings.Contains(out, "wrote "+path) {
		t.Errorf("output missing write confirmation:\n%s", out)
	}
	if !strings.Contains(out, "learn does not deploy anything") {
		t.Errorf("output must state that learn does not deploy:\n%s", out)
	}
	// Backup exists next to the config.
	entries, _ := filepath.Glob(path + ".learn-backup-*")
	if len(entries) != 1 {
		t.Fatalf("backups = %v, want exactly one", entries)
	}
	if backup, _ := os.ReadFile(entries[0]); string(backup) != existingConfig {
		t.Error("backup must hold the original content")
	}
	written, _ := os.ReadFile(path)
	y := string(written)
	for _, want := range []string{
		"branch: main",            // learn-owned source updated
		"display_name: My Webapp", // manual field preserved
		"interval: 15m",           // manual field preserved
	} {
		if !strings.Contains(y, want) {
			t.Errorf("written config missing %q:\n%s", want, y)
		}
	}
	if strings.Contains(y, "nginx:1.26") {
		t.Errorf("old source must be replaced:\n%s", y)
	}
}

func TestLearnGoldenDiffLines(t *testing.T) {
	t.Parallel()
	client, git := webappFixture()
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)

	out, _, err := runLearn(t, app, path, "1\n") // select, then EOF before write
	if err != nil && !errors.Is(err, learn.ErrAborted) {
		t.Fatalf("unexpected error: %v", err)
	}
	var removedOld, addedBranch bool
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "-") && strings.Contains(line, "nginx:1.26") {
			removedOld = true
		}
		if strings.HasPrefix(line, "+") && strings.Contains(line, "branch: main") {
			addedBranch = true
		}
	}
	if !removedOld || !addedBranch {
		t.Errorf("diff missing expected changes:\n%s", out)
	}
}

func TestLearnIdempotentRerun(t *testing.T) {
	t.Parallel()
	client, git := webappFixture()
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)

	if _, code, err := runLearn(t, app, path, "1\ny\n"); err != nil || code != exitcode.OK {
		t.Fatalf("first run: exit %d err %v", code, err)
	}
	afterFirst, _ := os.ReadFile(path)

	out, code, err := runLearn(t, app, path, "1\ny\n")
	if err != nil || code != exitcode.OK {
		t.Fatalf("second run: exit %d err %v:\n%s", code, err, out)
	}
	if !strings.Contains(out, "no changes") {
		t.Errorf("second run must be a no-op:\n%s", out)
	}
	afterSecond, _ := os.ReadFile(path)
	if string(afterFirst) != string(afterSecond) {
		t.Error("idempotent rerun must not change the file")
	}
}

func TestLearnDeclineWritesNothing(t *testing.T) {
	t.Parallel()
	client, git := webappFixture()
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)

	out, code, err := runLearn(t, app, path, "1\nn\n")
	if err != nil || code != exitcode.OK {
		t.Fatalf("decline must be a clean outcome: exit %d err %v", code, err)
	}
	if !strings.Contains(out, "no changes written") {
		t.Errorf("output must say nothing was written:\n%s", out)
	}
	after, _ := os.ReadFile(path)
	if string(after) != existingConfig {
		t.Error("declined run must not modify the file")
	}
	if backups, _ := filepath.Glob(path + ".learn-backup-*"); len(backups) != 0 {
		t.Errorf("declined run must not create backups: %v", backups)
	}
}

func TestLearnValidationFailureLeavesOriginalIntact(t *testing.T) {
	t.Parallel()
	// The existing app "tooling" already deploys standalone container
	// "tool"; importing the discovered "tool" candidate (a different app ID)
	// produces two apps sharing one deploy target, which the strict
	// validator must reject.
	const conflictingConfig = `schema_version: 1
apps:
  - id: tooling
    source:
      mode: registry
      registry:
        images:
          - ref: tool:1
    deploy:
      mode: standalone
      standalone:
        image: tool:1
        name: tool
`
	app := NewApp()
	app.dockerClient = &fakeDocker{
		summaries: []docker.ContainerSummary{
			{ID: "dddd4444dddd", Name: "tool", Image: "tool@sha256:1111111111111111111111111111111111111111111111111111111111111111", State: "running"},
		},
		details: map[string]docker.ContainerDetail{
			"dddd4444dddd": {
				ID:            "dddd4444dddd",
				Name:          "tool",
				ImageRef:      "tool@sha256:1111111111111111111111111111111111111111111111111111111111111111",
				State:         "running",
				Running:       true,
				RestartPolicy: "unless-stopped",
				NetworkMode:   "bridge",
			},
		},
	}
	path := writeConfig(t, conflictingConfig)

	out, code, err := runLearn(t, app, path, "1\ny\n")
	if err == nil {
		t.Fatalf("expected a validation error:\n%s", out)
	}
	if code != exitcode.Error {
		t.Errorf("exit %d, want error", code)
	}
	if !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("error must say nothing was written: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != conflictingConfig {
		t.Error("failed validation must leave the original intact")
	}
	if backups, _ := filepath.Glob(path + ".learn-backup-*"); len(backups) != 0 {
		t.Errorf("no backup may be created when validation fails: %v", backups)
	}
}

func TestLearnEOFAbortsAtEachStep(t *testing.T) {
	t.Parallel()
	// EOF at the selection prompt.
	client, git := webappFixture()
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)
	_, code, err := runLearn(t, app, path, "")
	if !errors.Is(err, learn.ErrAborted) || code != exitcode.Interrupted {
		t.Fatalf("EOF at selection: err %v code %d, want abort/interrupted", err, code)
	}
	// EOF at the write confirmation.
	app2 := NewApp()
	app2.dockerClient, app2.gitProber = webappFixture()
	path2 := writeConfig(t, existingConfig)
	_, code, err = runLearn(t, app2, path2, "1\n")
	if !errors.Is(err, learn.ErrAborted) || code != exitcode.Interrupted {
		t.Fatalf("EOF at confirm: err %v code %d, want abort/interrupted", err, code)
	}
	after, _ := os.ReadFile(path2)
	if string(after) != existingConfig {
		t.Error("aborted run must not modify the file")
	}
}

func TestLearnDryRunNeverWrites(t *testing.T) {
	t.Parallel()
	client, git := webappFixture()
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)

	app.stdin = strings.NewReader("1\n")
	stdout := &strings.Builder{}
	err := app.Execute(context.Background(), []string{"learn", "--config", path, "--dry-run"}, stdout, &strings.Builder{})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(stdout.String(), "dry-run: no changes written") {
		t.Errorf("dry-run output missing marker:\n%s", stdout)
	}
	after, _ := os.ReadFile(path)
	if string(after) != existingConfig {
		t.Error("dry-run must not write")
	}
}

func TestLearnStaleTempFilesCleaned(t *testing.T) {
	t.Parallel()
	client, git := webappFixture()
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)
	stale := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".learn-tmp-stale")
	if err := os.WriteFile(stale, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runLearn(t, app, path, "1\nn\n"); err != nil {
		t.Fatalf("learn: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale temp file must be removed on the next run")
	}
}

func TestLearnSystemCandidatesExcludedByDefault(t *testing.T) {
	t.Parallel()
	client, git := webappFixture()
	client.summaries = append(client.summaries, docker.ContainerSummary{
		ID: "cccc3333cccc", Name: "portainer", Image: "portainer/portainer-ce:latest", State: "running",
	})
	client.details["cccc3333cccc"] = docker.ContainerDetail{
		ID: "cccc3333cccc", Name: "portainer", ImageRef: "portainer/portainer-ce:latest",
		State: "running", Running: true,
	}
	app := NewApp()
	app.dockerClient, app.gitProber = client, git
	path := writeConfig(t, existingConfig)

	// A bare Enter (empty selection) ends the run cleanly.
	out, _, err := runLearn(t, app, path, "\n")
	if err != nil {
		t.Fatalf("learn: %v", err)
	}
	if strings.Contains(out, "portainer") {
		t.Errorf("system candidate must be hidden by default:\n%s", out)
	}
}

func TestLearnRequiresConfigFlag(t *testing.T) {
	t.Parallel()
	app := NewApp()
	// Pinned to a non-root uid so the root-default contract cannot change
	// the outcome when the suite runs as root.
	app.euid = func() int { return 1000 }
	stdout := &strings.Builder{}
	err := app.Execute(context.Background(), []string{"learn"}, stdout, &strings.Builder{})
	if err == nil {
		t.Fatal("expected error")
	}
	if Code(err) != exitcode.Usage {
		t.Errorf("exit %d, want usage", Code(err))
	}
}

// maomaoFixture is a compose candidate on a user-owned worktree whose git
// probe fails (the sudo "dubious ownership" case).
func maomaoFixture() (*fakeDocker, *fakeGit) {
	client := &fakeDocker{
		summaries: []docker.ContainerSummary{
			{ID: "aaaa1111aaaa", Name: "maomao-web-1", Image: "ghcr.io/x/maomao:1", State: "running"},
		},
		details: map[string]docker.ContainerDetail{
			"aaaa1111aaaa": {
				ID: "aaaa1111aaaa", Name: "maomao-web-1", ImageRef: "ghcr.io/x/maomao:1",
				State: "running", Running: true,
				Labels: map[string]string{
					docker.LabelProject:     "maomao",
					docker.LabelService:     "web",
					docker.LabelWorkDir:     "/home/u/.maomao",
					docker.LabelConfigFiles: "/home/u/.maomao/compose.yaml",
				},
			},
		},
	}
	return client, &fakeGit{err: errors.New("fatal: detected dubious ownership")}
}

// The user-facing bug: after selecting the candidate, the flow printed
// nothing and skipped silently. The registry fallback must prompt, and a
// decline must say why the import did not happen.
func TestLearnProbeErrorRegistryFallbackFlow(t *testing.T) {
	t.Run("decline prints the skip reason", func(t *testing.T) {
		client, git := maomaoFixture()
		app := NewApp()
		app.dockerClient = client
		app.gitProber = git
		path := writeConfig(t, "schema_version: 1\napps: []\n")
		out, code, err := runLearn(t, app, path, "1\nn\n")
		if err != nil || code != exitcode.OK {
			t.Fatalf("learn = %v (code %d)", err, code)
		}
		for _, want := range []string{
			"Track the observed images from a registry?",
			"maomao skipped: registry tracking declined",
			"nothing selected to import.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("confirm imports registry mode", func(t *testing.T) {
		client, git := maomaoFixture()
		app := NewApp()
		app.dockerClient = client
		app.gitProber = git
		path := writeConfig(t, "schema_version: 1\napps: []\n")
		out, code, err := runLearn(t, app, path, "1\ny\ny\n") // select, registry confirm, write
		if err != nil || code != exitcode.OK {
			t.Fatalf("learn = %v (code %d)\n%s", err, code, out)
		}
		written, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"mode: registry", "ghcr.io/x/maomao:1", "/home/u/.maomao/compose.yaml"} {
			if !strings.Contains(string(written), want) {
				t.Errorf("config missing %q:\n%s", want, written)
			}
		}
	})
}
