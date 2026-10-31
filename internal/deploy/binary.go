package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
)

// ReleaseInstaller is the slice of internal/release a binary deployment
// needs: download, verify, and atomically replace the binary. The pipeline
// restarts the configured systemd user unit afterwards and does not execute
// the downloaded bytes itself.
type ReleaseInstaller interface {
	Install(ctx context.Context, src *config.ReleaseSource, dir, bin string) (string, error)
}

// BinaryPipeline updates a release-installed Go binary. The systemd user
// unit remains the process supervisor: a failed restart leaves the
// deployed-version checkpoint untouched.
type BinaryPipeline struct {
	Releases   ReleaseInstaller
	Health     HealthChecker
	Checkpoint VersionCheckpoint
	Runner     *runner.Runner
	Run        func(ctx context.Context, req runner.Request) (runner.Result, error)
}

// Deploy installs the newest verified release and restarts the unit.
func (p *BinaryPipeline) Deploy(ctx context.Context, app *config.App) (schedule.DeployResult, error) {
	if app == nil || app.Deploy.Mode != config.DeployBinary || app.Deploy.Binary == nil {
		return schedule.DeployResult{}, errors.New("app is not a binary deployment")
	}
	if app.Source.Mode != config.SourceRelease || app.Source.Release == nil {
		return schedule.DeployResult{}, errors.New("binary deployment requires a release source")
	}
	if p.Checkpoint == nil {
		return schedule.DeployResult{}, errors.New("deployed-version checkpoint is not configured; refusing to deploy")
	}
	if p.Releases == nil {
		return schedule.DeployResult{}, errors.New("release installer is not configured; refusing to deploy")
	}
	b := app.Deploy.Binary
	tag, err := p.Releases.Install(ctx, app.Source.Release, b.Dir, b.Bin)
	if err != nil {
		return schedule.DeployResult{Success: false}, fmt.Errorf("binary deploy failed: %w", err)
	}
	stages := []string{"installed " + tag}
	if ctx.Err() != nil {
		return schedule.DeployResult{}, fmt.Errorf("binary deploy cancelled after install; deployed version unchanged")
	}
	if err := p.restart(ctx, app); err != nil {
		return schedule.DeployResult{Success: false, Detail: strings.Join(stages, "; ")}, fmt.Errorf("binary deploy failed: %w", err)
	}
	stages = append(stages, "restarted "+b.Unit)

	samples, checkErr := p.healthChecker().RunPostDeployChecks(ctx, app)
	if ctx.Err() != nil {
		return schedule.DeployResult{}, errors.New("binary deploy cancelled after restart; deployed version unchanged")
	}
	if checkErr != nil {
		return schedule.DeployResult{Success: false, Detail: strings.Join(stages, "; ")}, fmt.Errorf("binary deploy failed: %w", checkErr)
	}
	stages = append(stages, fmt.Sprintf("post-deploy checks: %d ok", len(samples)))
	if err := p.Checkpoint.MarkDeployed(ctx, app.ID, tag); err != nil {
		return schedule.DeployResult{}, fmt.Errorf("checkpoint failed; deployed version unchanged: %w", err)
	}
	stages = append(stages, "checkpoint: "+tag)
	return schedule.DeployResult{Success: true, Detail: strings.Join(stages, "; ")}, nil
}

func (p *BinaryPipeline) restart(ctx context.Context, app *config.App) error {
	unit := app.Deploy.Binary.Unit
	return execChecked(ctx, p.Run, p.Runner, runner.Request{
		Name:    "systemctl --user restart",
		Argv:    binaryRestartArgv(unit),
		Timeout: appTimeout(app),
		AppID:   app.ID,
	})
}

func (p *BinaryPipeline) healthChecker() HealthChecker {
	if p.Health == nil {
		return errorHealthChecker{}
	}
	return p.Health
}

// binaryRestartArgv is the only command a binary app restart runs.
func binaryRestartArgv(unit string) []string {
	return []string{"systemctl", "--user", "restart", unit}
}

func execChecked(ctx context.Context, run func(context.Context, runner.Request) (runner.Result, error), rn *runner.Runner, req runner.Request) error {
	var (
		res runner.Result
		err error
	)
	if run != nil {
		res, err = run(ctx, req)
	} else {
		if rn == nil {
			rn = &runner.Runner{}
		}
		res, err = rn.Run(ctx, req)
	}
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
