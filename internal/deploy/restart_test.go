package deploy

import (
	"context"
	"errors"
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
		name        string
		app         *config.App
		fallback    *config.DockerEndpoint
		want        string
		wantDir     string
		wantTimeout time.Duration
	}{
		{
			name:        "compose uses exact context and app endpoint flags",
			app:         compose,
			want:        "docker --context rootless compose -f /srv/web/compose.yaml -f /srv/web/override.yaml --env-file /srv/web/.env --profile edge -p web restart",
			wantDir:     "/srv/web",
			wantTimeout: appTimeout(compose),
		},
		{
			name:        "standalone restarts the configured name with host flags",
			app:         standalone,
			want:        "docker -H unix:///run/user/1000/docker.sock restart box-1",
			wantDir:     "",
			wantTimeout: 2 * time.Minute,
		},
		{
			name:        "compose falls back to the configuration-wide endpoint",
			app:         composeApp(),
			fallback:    &config.DockerEndpoint{Context: "desktop-linux"},
			want:        "docker --context desktop-linux compose -f /srv/web/compose.yaml -f /srv/web/override.yaml --env-file /srv/web/.env --profile edge -p web restart",
			wantDir:     "/srv/web",
			wantTimeout: appTimeout(composeApp()),
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
			reqs := exec.requests()
			if len(reqs) != 1 || reqs[0].Timeout != tt.wantTimeout {
				t.Fatalf("timeout = %v, want %s", reqs, tt.wantTimeout)
			}
			if reqs[0].AppID != tt.app.ID {
				t.Fatalf("app id = %q, want %q", reqs[0].AppID, tt.app.ID)
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

// TestRestartErrorDetailFallbacks pins execChecked's failure detail chain:
// runner error propagates, res.Err is preferred, then stderr, then status.
func TestRestartErrorDetailFallbacks(t *testing.T) {
	t.Parallel()

	app := &config.App{
		ID: "box",
		Deploy: config.Deploy{
			Mode:       config.DeployStandalone,
			Standalone: &config.StandaloneSpec{Name: "box-1"},
		},
	}

	t.Run("runner error", func(t *testing.T) {
		t.Parallel()
		r := &Restarter{Run: func(context.Context, runner.Request) (runner.Result, error) {
			return runner.Result{}, errors.New("exec failed")
		}}
		err := r.Restart(context.Background(), app)
		if err == nil || !strings.Contains(err.Error(), "docker restart: exec failed") {
			t.Fatalf("err = %v, want the runner error under the command name", err)
		}
	})

	t.Run("stderr when err is empty", func(t *testing.T) {
		t.Parallel()
		r := &Restarter{Run: (&fakeExec{respond: func(runner.Request) runner.Result {
			return runner.Result{Status: runner.StatusFailed, ExitCode: 1, Stderr: []byte("  daemon unreachable  ")}
		}}).run}
		err := r.Restart(context.Background(), app)
		if err == nil || !strings.Contains(err.Error(), "daemon unreachable") {
			t.Fatalf("err = %v, want the stderr detail", err)
		}
	})

	t.Run("status when err and stderr are empty", func(t *testing.T) {
		t.Parallel()
		r := &Restarter{Run: (&fakeExec{respond: func(runner.Request) runner.Result {
			return runner.Result{Status: runner.StatusTimeout, ExitCode: -1}
		}}).run}
		err := r.Restart(context.Background(), app)
		if err == nil || !strings.Contains(err.Error(), "docker restart: timeout") {
			t.Fatalf("err = %v, want the status detail", err)
		}
	})
}
