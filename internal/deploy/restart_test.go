package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

func TestRestartArgv(t *testing.T) {
	t.Parallel()

	compose := composeApp()
	compose.Docker = &config.DockerEndpoint{Context: "rootless"}
	standalone := &config.App{
		ID:      "box",
		Timeout: config.Duration(2 * time.Minute),
		Docker:  &config.DockerEndpoint{Host: "unix:///run/user/1000/docker.sock"},
		Deploy: config.Deploy{
			Mode: config.DeployStandalone,
			Standalone: &config.StandaloneSpec{
				Image: "nginx:1",
				Name:  "box-1",
			},
		},
	}

	tests := []struct {
		name     string
		app      *config.App
		fallback *config.DockerEndpoint
		want     string
		wantDir  string
	}{
		{
			name:    "compose uses exact context and app endpoint flags",
			app:     compose,
			want:    "docker --context rootless compose -f /srv/web/compose.yaml -f /srv/web/override.yaml --env-file /srv/web/.env --profile edge -p web restart",
			wantDir: "/srv/web",
		},
		{
			name:    "standalone restarts the configured name with host flags",
			app:     standalone,
			want:    "docker -H unix:///run/user/1000/docker.sock restart box-1",
			wantDir: "",
		},
		{
			name:     "compose falls back to the configuration-wide endpoint",
			app:      composeApp(),
			fallback: &config.DockerEndpoint{Context: "desktop-linux"},
			want:     "docker --context desktop-linux compose -f /srv/web/compose.yaml -f /srv/web/override.yaml --env-file /srv/web/.env --profile edge -p web restart",
			wantDir:  "/srv/web",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exec := &fakeExec{}
			r := &Restarter{Run: exec.run, DefaultEndpoint: tt.fallback}
			if err := r.Restart(context.Background(), tt.app); err != nil {
				t.Fatalf("restart: %v", err)
			}
			if got := exec.joined(); got != tt.want {
				t.Fatalf("argv = %q, want %q", got, tt.want)
			}
			if len(exec.dirs) != 1 || exec.dirs[0] != tt.wantDir {
				t.Fatalf("dir = %v, want %q", exec.dirs, tt.wantDir)
			}
			if envs := exec.allEnvs(); len(envs) != 1 || len(envs[0]) != 0 {
				t.Fatalf("env = %v, want empty (endpoint lives in argv flags)", envs)
			}
		})
	}
}

func TestRestartRejectsUnknownModeAndFailedCommand(t *testing.T) {
	t.Parallel()

	r := &Restarter{Run: (&fakeExec{}).run}
	if err := r.Restart(context.Background(), &config.App{ID: "x", Deploy: config.Deploy{Mode: "k8s"}}); err == nil || !strings.Contains(err.Error(), "unsupported deploy mode") {
		t.Fatalf("err = %v, want unsupported mode", err)
	}

	failing := &fakeExec{respond: func(runner.Request) runner.Result {
		return runner.Result{Status: runner.StatusFailed, ExitCode: 1, Err: "container not running"}
	}}
	app := &config.App{
		ID: "box",
		Deploy: config.Deploy{
			Mode:       config.DeployStandalone,
			Standalone: &config.StandaloneSpec{Name: "box-1"},
		},
	}
	err := (&Restarter{Run: failing.run}).Restart(context.Background(), app)
	if err == nil || !strings.Contains(err.Error(), "container not running") {
		t.Fatalf("err = %v, want the docker failure", err)
	}
	if failing.joined() != "docker restart box-1" {
		t.Fatalf("argv = %q", failing.joined())
	}
}

func TestRestartDoesNotInvokeCheckpoint(t *testing.T) {
	t.Parallel()
	// Restarter has no VersionCheckpoint field; a successful bounce must
	// be impossible to confuse with a deploy. The type assertion documents
	// that boundary.
	var r any = &Restarter{Run: (&fakeExec{}).run}
	if _, ok := r.(VersionCheckpoint); ok {
		t.Fatal("Restarter must not implement VersionCheckpoint")
	}
	if err := r.(*Restarter).Restart(context.Background(), composeApp()); err != nil {
		t.Fatal(err)
	}
}
