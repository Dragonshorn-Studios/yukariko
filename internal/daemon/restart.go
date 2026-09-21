package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/deploy"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// EventRestart is the durable event kind for operator-initiated bounces.
// Success and failure are both recorded; neither path advances a checkpoint.
const EventRestart = "restart"

// Restart bounces one configured app's containers without deploying.
// It takes the same per-app lock as a manual update (scheduler queue + the
// store's unique running-deployment row) so it cannot overlap a concurrent
// update or daemon pass, records durable events, and never calls
// CommitDeploymentSuccess.
func (a *Assembled) Restart(ctx context.Context, appID string) error {
	app, err := a.appByID(appID)
	if err != nil {
		return err
	}

	release, err := a.Scheduler.HoldAppLock(ctx, appID)
	if err != nil {
		a.recordRestart(ctx, appID, store.LevelWarn, "restart refused: "+err.Error())
		return err
	}
	defer release()

	depID, err := a.Deployer.claimDeploymentCause(ctx, app, "restart")
	if err != nil {
		a.recordRestart(ctx, appID, store.LevelWarn, "restart refused: "+err.Error())
		return err
	}
	// The running row is only a lock token. Delete it whether the bounce
	// succeeds or fails so restart is not a deployment record and cannot
	// advance the checkpoint. A crash leaves the row for the existing
	// stale-row reap, same as a crashed deploy. WithoutCancel detaches the
	// release from the request ctx: a cancelled bounce still frees the lock.
	defer func() { _ = a.Store.ReleaseRunningDeployment(context.WithoutCancel(ctx), depID) }()

	a.recordRestart(ctx, appID, store.LevelInfo, "restart requested")
	restarter := &deploy.Restarter{
		Runner:          a.Runner,
		Run:             a.Deployer.pipelineRun,
		DefaultEndpoint: a.Config.Docker,
	}
	if err := restarter.Restart(ctx, app); err != nil {
		a.recordRestart(ctx, appID, store.LevelError, "restart failed: "+err.Error())
		return err
	}
	a.recordRestart(ctx, appID, store.LevelInfo, "restart succeeded; deployed version unchanged")
	return nil
}

func (a *Assembled) appByID(id string) (*config.App, error) {
	for i := range a.Config.Apps {
		if a.Config.Apps[i].ID == id {
			return &a.Config.Apps[i], nil
		}
	}
	return nil, fmt.Errorf("unknown app %q", id)
}

func (a *Assembled) recordRestart(ctx context.Context, appID, level, message string) {
	// WithoutCancel keeps the outcome durable when the request ctx died
	// mid-bounce (SIGINT), which is exactly when the audit record matters.
	_, _ = a.Store.RecordEvent(context.WithoutCancel(ctx), store.Event{
		Time:    time.Now(),
		AppID:   appID,
		Level:   level,
		Kind:    EventRestart,
		Message: message,
	})
}
