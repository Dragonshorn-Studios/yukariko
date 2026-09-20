package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// Restarter bounces a configured app's containers without pulling,
// rebuilding, or advancing the deployed-version checkpoint. Compose keeps
// its exact project context; standalone restarts the configured name
// through the same Docker CLI endpoint flags as every other mutation.
type Restarter struct {
	// Runner executes commands when Run is nil.
	Runner *runner.Runner
	// Run overrides command execution (tests inject a recorder).
	Run func(ctx context.Context, req runner.Request) (runner.Result, error)
	// DefaultEndpoint is the configuration-wide Docker endpoint applied to
	// apps without their own docker block.
	DefaultEndpoint *config.DockerEndpoint
}

// Restart runs one bounce for the app. It never calls VersionCheckpoint.
func (r *Restarter) Restart(ctx context.Context, app *config.App) error {
	if app == nil {
		return errors.New("app is required")
	}
	if ctx.Err() != nil {
		return fmt.Errorf("restart cancelled: %w", ctx.Err())
	}
	switch app.Deploy.Mode {
	case config.DeployCompose:
		if app.Deploy.Compose == nil {
			return errors.New("app is not a compose deployment")
		}
		return r.composeRestart(ctx, app)
	case config.DeployStandalone:
		if app.Deploy.Standalone == nil || app.Deploy.Standalone.Name == "" {
			return errors.New("app is not a standalone deployment")
		}
		return r.standaloneRestart(ctx, app)
	default:
		return fmt.Errorf("unsupported deploy mode %q", app.Deploy.Mode)
	}
}

func (r *Restarter) composeRestart(ctx context.Context, app *config.App) error {
	argv := append([]string{"docker"}, r.endpointFlags(app)...)
	argv = append(argv, composeContextArgs(app)...)
	argv = append(argv, "restart")
	return r.execChecked(ctx, runner.Request{
		Name:    "docker compose restart",
		Argv:    argv,
		Dir:     app.Deploy.Compose.WorkDir,
		Timeout: appTimeout(app),
		AppID:   app.ID,
	})
}

func (r *Restarter) standaloneRestart(ctx context.Context, app *config.App) error {
	name := app.Deploy.Standalone.Name
	argv := append([]string{"docker"}, r.endpointFlags(app)...)
	argv = append(argv, "restart", name)
	return r.execChecked(ctx, runner.Request{
		Name:    "docker restart",
		Argv:    argv,
		Timeout: appTimeout(app),
		AppID:   app.ID,
	})
}

func (r *Restarter) endpointFlags(app *config.App) []string {
	if app.Docker != nil {
		return app.Docker.Flags()
	}
	return r.DefaultEndpoint.Flags()
}

func (r *Restarter) execChecked(ctx context.Context, req runner.Request) error {
	res, err := r.exec(ctx, req)
	if err != nil {
		return fmt.Errorf("%s: %w", req.Name, err)
	}
	if res.Status == runner.StatusSuccess {
		return nil
	}
	detail := strings.TrimSpace(res.Err)
	if detail == "" {
		detail = strings.TrimSpace(string(res.Stderr))
	}
	if detail == "" {
		detail = res.Status
	}
	return fmt.Errorf("%s: %s", req.Name, detail)
}

func (r *Restarter) exec(ctx context.Context, req runner.Request) (runner.Result, error) {
	if r.Run != nil {
		return r.Run(ctx, req)
	}
	runnerSvc := r.Runner
	if runnerSvc == nil {
		runnerSvc = &runner.Runner{}
	}
	return runnerSvc.Run(ctx, req)
}
