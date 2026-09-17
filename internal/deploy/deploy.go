package deploy

import (
	"context"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// defaultVerifyTimeout bounds per-image digest verification.
const defaultVerifyTimeout = 30 * time.Second

// exec runs one command through the injected Run hook or the runner.
func (p *ComposePipeline) exec(ctx context.Context, req runner.Request) (runner.Result, error) {
	if p.Run != nil {
		return p.Run(ctx, req)
	}
	r := p.Runner
	if r == nil {
		r = &runner.Runner{}
	}
	return r.Run(ctx, req)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func appTimeout(app *config.App) time.Duration {
	if d := app.Timeout.D(); d > 0 {
		return d
	}
	return config.DefaultTimeout.D()
}
