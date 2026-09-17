package learn

import (
	"context"
	"fmt"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

// buildApp turns one resolved candidate into a config.App. The generated app
// carries only evidence-backed fields: policy settings (interval, timeout,
// retry, steps) stay unset so the documented defaults apply and the user's
// existing values survive merges.
func buildApp(ctx context.Context, p *Proposal, res resolution) (config.App, error) {
	app := config.App{ID: p.ID, DisplayName: displayName(p)}
	switch p.Mode {
	case "compose":
		workDir := firstNonEmpty(res.workDir, p.Compose.WorkDir)
		files := p.Compose.ConfigFiles
		if len(res.files) > 0 {
			files = res.files
		}
		if workDir == "" {
			return config.App{}, fmt.Errorf("no compose working directory")
		}
		if len(files) == 0 {
			return config.App{}, fmt.Errorf("no compose config files")
		}
		switch {
		case p.Compose.SourceMode == config.SourceGit && p.Compose.Git != nil:
			branch := firstNonEmpty(res.branch, p.Compose.Git.Branch)
			if branch == "" {
				return config.App{}, fmt.Errorf("no git branch")
			}
			remote := firstNonEmpty(res.remote, p.Compose.Git.Remote)
			app.Source = config.Source{
				Mode: config.SourceGit,
				Git: &config.GitSource{
					Dir:    workDir,
					Branch: branch,
					Remote: remote,
				},
			}
		case p.Compose.SourceMode == config.SourceRegistry && res.registryOK:
			if len(p.Compose.RegistryImages) == 0 {
				return config.App{}, fmt.Errorf("no observed images to track")
			}
			refs := make([]config.ImageRef, 0, len(p.Compose.RegistryImages))
			for _, r := range p.Compose.RegistryImages {
				refs = append(refs, config.ImageRef{Ref: r})
			}
			app.Source = config.Source{Mode: config.SourceRegistry, Registry: &config.RegistrySource{Images: refs}}
		default:
			return config.App{}, fmt.Errorf("source mode not confirmed")
		}
		app.Deploy = config.Deploy{
			Mode: config.DeployCompose,
			Compose: &config.ComposeDeploy{
				WorkDir:     workDir,
				Files:       files,
				EnvFiles:    p.Compose.EnvFiles,
				ProjectName: p.Compose.ProjectName,
			},
		}
		return app, nil

	case "standalone":
		image := firstNonEmpty(res.image, p.Standalone.Image)
		if image == "" {
			return config.App{}, fmt.Errorf("no image reference")
		}
		app.Source = config.Source{
			Mode: config.SourceRegistry,
			Registry: &config.RegistrySource{
				Images: []config.ImageRef{{Ref: image}},
			},
		}
		sp := p.Standalone
		spec := &config.StandaloneSpec{
			Image:       image,
			Name:        sp.ContainerName,
			Entrypoint:  sp.Entrypoint,
			Command:     sp.Command,
			Env:         res.env,
			Binds:       sp.Binds,
			Ports:       sp.Ports,
			Networks:    sp.Networks,
			Restart:     sp.Restart,
			Labels:      sp.Labels,
			User:        sp.User,
			WorkDir:     sp.WorkDir,
			HealthCheck: healthCheckConfig(sp.HealthCheck),
		}
		app.Deploy = config.Deploy{Mode: config.DeployStandalone, Standalone: spec}
		if res.healthDocker {
			app.Health = &config.Health{Docker: &config.DockerProbe{}}
		}
		return app, nil

	default:
		return config.App{}, fmt.Errorf("unknown mode %q", p.Mode)
	}
}

func displayName(p *Proposal) string {
	switch p.Mode {
	case "compose":
		return p.Compose.ProjectName
	case "standalone":
		return p.Standalone.ContainerName
	default:
		return p.ID
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// healthCheckConfig maps the captured docker healthcheck definition onto the
// config subset. A nil definition or an explicitly disabled one yields nil.
func healthCheckConfig(hc *docker.HealthCheckDef) *config.ContainerHealthCheck {
	if hc == nil || (len(hc.Test) == 1 && hc.Test[0] == "NONE") {
		return nil
	}
	return &config.ContainerHealthCheck{
		Test:        hc.Test,
		Interval:    config.Duration(hc.IntervalNS),
		Timeout:     config.Duration(hc.TimeoutNS),
		Retries:     hc.Retries,
		StartPeriod: config.Duration(hc.StartPeriodNS),
	}
}
