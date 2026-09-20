package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

type restartRecorder struct {
	argvs  [][]string
	fail   bool
	failAt string
}

func (r *restartRecorder) run(_ context.Context, req runner.Request) (runner.Result, error) {
	r.argvs = append(r.argvs, req.Argv)
	if r.fail {
		return runner.Result{Status: runner.StatusFailed, ExitCode: 1, Err: r.failAt}, nil
	}
	return runner.Result{Status: runner.StatusSuccess}, nil
}

func (r *restartRecorder) joined() string {
	lines := make([]string, 0, len(r.argvs))
	for _, argv := range r.argvs {
		lines = append(lines, strings.Join(argv, " "))
	}
	return strings.Join(lines, "\n")
}

func restartAssemble(t *testing.T, app config.App, rec *restartRecorder) *Assembled {
	t.Helper()
	cfg := &config.Config{
		SchemaVersion: 1,
		Docker:        &config.DockerEndpoint{Context: "rootless"},
		Apps:          []config.App{app},
	}
	asm, err := Assemble(context.Background(), Options{
		Config:      cfg,
		DataDir:     t.TempDir(),
		PipelineRun: rec.run,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	t.Cleanup(func() { _ = asm.Store.Close() })
	return asm
}

func composeRestartApp(dir string) config.App {
	return config.App{
		ID:      "web",
		Timeout: config.Duration(time.Minute),
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "app:1"}}},
		},
		Deploy: config.Deploy{
			Mode: config.DeployCompose,
			Compose: &config.ComposeDeploy{
				WorkDir:     dir,
				Files:       []string{dir + "/compose.yaml"},
				EnvFiles:    []string{dir + "/.env"},
				Profiles:    []string{"edge"},
				ProjectName: "web",
			},
		},
	}
}

func standaloneRestartApp() config.App {
	return config.App{
		ID:     "box",
		Docker: &config.DockerEndpoint{Host: "unix:///run/user/1000/docker.sock"},
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "nginx:1"}}},
		},
		Deploy: config.Deploy{
			Mode:       config.DeployStandalone,
			Standalone: &config.StandaloneSpec{Image: "nginx:1", Name: "box-1"},
		},
	}
}

func checkpoint(t *testing.T, st *store.Store, appID, kind, value string) {
	t.Helper()
	depID, err := st.BeginDeployment(context.Background(), store.BeginDeploymentParams{
		AppID: appID, Cause: "test", At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitDeploymentSuccess(context.Background(), appID, kind, value, depID, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestRestartComposeAndStandaloneArgv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("compose", func(t *testing.T) {
		t.Parallel()
		rec := &restartRecorder{}
		asm := restartAssemble(t, composeRestartApp("/srv/web"), rec)
		checkpoint(t, asm.Store, "web", store.KindDigest, "app:1@sha256:abc")
		if err := asm.Restart(ctx, "web"); err != nil {
			t.Fatal(err)
		}
		want := "docker --context rootless compose -f /srv/web/compose.yaml --env-file /srv/web/.env --profile edge -p web restart"
		if rec.joined() != want {
			t.Fatalf("argv = %q, want %q", rec.joined(), want)
		}
	})

	t.Run("standalone", func(t *testing.T) {
		t.Parallel()
		rec := &restartRecorder{}
		asm := restartAssemble(t, standaloneRestartApp(), rec)
		if err := asm.Restart(ctx, "box"); err != nil {
			t.Fatal(err)
		}
		want := "docker -H unix:///run/user/1000/docker.sock restart box-1"
		if rec.joined() != want {
			t.Fatalf("argv = %q, want %q", rec.joined(), want)
		}
	})
}

func TestRestartLeavesCheckpointUnchanged(t *testing.T) {
	t.Parallel()
	rec := &restartRecorder{}
	asm := restartAssemble(t, composeRestartApp("/srv/web"), rec)
	ctx := context.Background()
	checkpoint(t, asm.Store, "web", store.KindDigest, "app:1@sha256:keep")

	if err := asm.Restart(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	got, _, ok, err := asm.Store.DeployedVersion(ctx, "web", store.KindDigest)
	if err != nil || !ok || got != "app:1@sha256:keep" {
		t.Fatalf("deployed = %q ok=%v err=%v, want the original checkpoint", got, ok, err)
	}
	deps, err := asm.Store.RecentDeployments(ctx, "web", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].Status != store.StatusSucceeded {
		t.Fatalf("deployments after restart = %+v, want only the original succeeded row", deps)
	}
	if open, ok, _ := asm.Store.OpenDeployment(ctx, "web"); ok {
		t.Fatalf("restart left a running lock row: %+v", open)
	}
}

func TestRestartRecordsEventsAndRefusesUnknownApp(t *testing.T) {
	t.Parallel()
	rec := &restartRecorder{}
	asm := restartAssemble(t, composeRestartApp("/srv/web"), rec)
	ctx := context.Background()

	if err := asm.Restart(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "unknown app") {
		t.Fatalf("err = %v, want unknown app", err)
	}

	if err := asm.Restart(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	events, err := asm.Store.Events(ctx, store.EventsQuery{AppID: "web", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var sawSuccess bool
	for _, e := range events {
		if e.Kind != EventRestart {
			continue
		}
		kinds = append(kinds, e.Level+":"+e.Message)
		if e.Level == store.LevelInfo && strings.Contains(e.Message, "succeeded") {
			sawSuccess = true
		}
	}
	if !sawSuccess {
		t.Fatalf("restart events = %v, want a success event", kinds)
	}
}

func TestRestartLockConflictAndFailureEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("in-progress deploy is refused", func(t *testing.T) {
		t.Parallel()
		rec := &restartRecorder{}
		asm := restartAssemble(t, composeRestartApp("/srv/web"), rec)
		if _, err := asm.Store.BeginDeployment(ctx, store.BeginDeploymentParams{
			AppID: "web", Cause: "manual", At: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		err := asm.Restart(ctx, "web")
		if err == nil || !strings.Contains(err.Error(), "already in progress") {
			t.Fatalf("err = %v, want the in-progress lock message", err)
		}
		if rec.joined() != "" {
			t.Fatalf("restart ran docker despite the lock: %s", rec.joined())
		}
		events, _ := asm.Store.Events(ctx, store.EventsQuery{AppID: "web", Limit: 10})
		var refused bool
		for _, e := range events {
			if e.Kind == EventRestart && strings.Contains(e.Message, "refused") {
				refused = true
			}
		}
		if !refused {
			t.Fatalf("events = %+v, want a refused restart event", events)
		}
	})

	t.Run("command failure records an error and releases the lock", func(t *testing.T) {
		t.Parallel()
		rec := &restartRecorder{fail: true, failAt: "compose restart failed"}
		asm := restartAssemble(t, composeRestartApp("/srv/web"), rec)
		checkpoint(t, asm.Store, "web", store.KindDigest, "app:1@sha256:keep")
		err := asm.Restart(ctx, "web")
		if err == nil || !strings.Contains(err.Error(), "compose restart failed") {
			t.Fatalf("err = %v, want the docker failure", err)
		}
		got, _, ok, _ := asm.Store.DeployedVersion(ctx, "web", store.KindDigest)
		if !ok || got != "app:1@sha256:keep" {
			t.Fatalf("checkpoint moved after a failed restart: %q", got)
		}
		if _, err := asm.Store.BeginDeployment(ctx, store.BeginDeploymentParams{
			AppID: "web", Cause: "manual", At: time.Now(),
		}); err != nil {
			t.Fatalf("lock was not released after a failed restart: %v", err)
		}
		events, _ := asm.Store.Events(ctx, store.EventsQuery{AppID: "web", Limit: 20})
		var failed bool
		for _, e := range events {
			if e.Kind == EventRestart && e.Level == store.LevelError && strings.Contains(e.Message, "failed") {
				failed = true
			}
		}
		if !failed {
			t.Fatalf("events = %+v, want a failure event", events)
		}
	})
}
