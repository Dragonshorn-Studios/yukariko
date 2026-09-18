package schedule

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// preflightTimeout bounds each host command preflight runs.
const preflightTimeout = 15 * time.Second

// composeProjectPattern is compose's own project-name charset.
var composeProjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Finding is one failed readiness check. An empty findings list means the
// app is clear to deploy; any finding refuses deployment while observation
// and status stay available.
type Finding struct {
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

func (f Finding) String() string {
	return f.Check + ": " + f.Detail
}

// Preflight runs the readiness checks that gate deployment: required
// binaries, Docker reachability, Git worktree/remote, Compose context, env
// files, profiles, project name, writable data directory, resolvable secret
// references, and well-formed command definitions. It performs only reads
// plus secret resolutions whose values are discarded immediately.
type Preflight struct {
	// Exec runs one host command; nil uses the runner (docker argv only).
	// Tests inject fakes here.
	Exec func(ctx context.Context, name string, argv []string) error
	// LookPath resolves an executable; nil uses exec.LookPath.
	LookPath func(string) (string, error)
	// Runner executes the docker probes when Exec is nil.
	Runner *runner.Runner
	// DataDir is Yukariko's own state directory; probed for writability
	// when set.
	DataDir string
	// DefaultEndpoint is the configuration-wide Docker endpoint applied to
	// apps without their own docker block; reachability is probed against
	// the app's resolved endpoint, not just the default daemon.
	DefaultEndpoint *config.DockerEndpoint
}

// endpointFlags resolves the docker CLI global flags for one app.
func (p *Preflight) endpointFlags(app *config.App) []string {
	if app.Docker != nil {
		return app.Docker.Flags()
	}
	return p.DefaultEndpoint.Flags()
}

// Run executes all checks for one app and returns the findings in a fixed
// order. It never mutates Docker or Git state.
func (p *Preflight) Run(ctx context.Context, app *config.App) []Finding {
	var findings []Finding
	add := func(check, format string, args ...any) {
		findings = append(findings, Finding{Check: check, Detail: fmt.Sprintf(format, args...)})
	}

	// Binaries: docker everywhere, git for git sources.
	if _, err := p.look("docker"); err != nil {
		add("binary:docker", "docker executable not found in PATH")
	} else {
		psArgv := append([]string{"docker"}, p.endpointFlags(app)...)
		psArgv = append(psArgv, "ps")
		if err := p.exec(ctx, "docker", psArgv); err != nil {
			add("docker:reachable", "cannot reach the Docker daemon: %v", err)
		}
	}
	if app.Source.Mode == config.SourceGit {
		if _, err := p.look("git"); err != nil {
			add("binary:git", "git executable not found in PATH")
		}
	}

	switch app.Deploy.Mode {
	case config.DeployCompose:
		p.checkCompose(ctx, app, add)
	case config.DeployStandalone:
		p.checkStandalone(app, add)
	case "":
		add("deploy.mode", "deploy mode is unset")
	}

	if app.Source.Mode == config.SourceGit {
		if app.Source.Git == nil {
			add("source.git", "git source selected but git section is missing")
		} else {
			if app.Source.Git.Dir == "" {
				add("path:git_dir", "git worktree directory is unset")
			} else if fi, err := os.Stat(app.Source.Git.Dir); err != nil {
				add("path:git_dir", "git worktree %s: %v", app.Source.Git.Dir, err)
			} else if !fi.IsDir() {
				add("path:git_dir", "%s is not a directory", app.Source.Git.Dir)
			}
			if app.Source.Git.Branch == "" {
				add("value:git_branch", "git branch is unset")
			}
			if app.Source.Git.Remote == "" {
				add("value:git_remote", "git remote name is unset")
			}
		}
	}

	p.checkSteps(app, add)
	p.checkHealth(app, add)
	p.checkSecrets(app, add)
	p.checkDataDir(add)
	return findings
}

func (p *Preflight) checkCompose(ctx context.Context, app *config.App, add func(string, string, ...any)) {
	c := app.Deploy.Compose
	if c == nil {
		add("deploy.compose", "compose mode selected but compose section is missing")
		return
	}
	composeArgv := append([]string{"docker"}, p.endpointFlags(app)...)
	composeArgv = append(composeArgv, "compose", "version")
	if err := p.exec(ctx, "docker", composeArgv); err != nil {
		add("binary:compose", "docker compose is not usable: %v", err)
	}
	if c.WorkDir == "" {
		add("path:work_dir", "compose working directory is unset")
	} else if fi, err := os.Stat(c.WorkDir); err != nil {
		add("path:work_dir", "compose workdir %s: %v", c.WorkDir, err)
	} else if !fi.IsDir() {
		add("path:work_dir", "%s is not a directory", c.WorkDir)
	}
	if len(c.Files) == 0 {
		add("path:compose_files", "no compose files configured")
	}
	for _, f := range c.Files {
		if fi, err := os.Stat(f); err != nil {
			add("path:compose_files", "compose file %s: %v", f, err)
		} else if fi.IsDir() {
			add("path:compose_files", "compose file %s is a directory", f)
		}
	}
	for _, e := range c.EnvFiles {
		if _, err := os.Stat(e); err != nil {
			add("path:env_files", "env file %s: %v", e, err)
		}
	}
	for _, prof := range c.Profiles {
		if prof == "" || strings.ContainsAny(prof, " \t") {
			add("value:profiles", "profile %q must be non-empty without whitespace", prof)
		}
	}
	if c.ProjectName != "" && !composeProjectPattern.MatchString(c.ProjectName) {
		add("value:project_name", "project name %q must match %s", c.ProjectName, composeProjectPattern)
	}
}

func (p *Preflight) checkStandalone(app *config.App, add func(string, string, ...any)) {
	s := app.Deploy.Standalone
	if s == nil {
		add("deploy.standalone", "standalone mode selected but standalone section is missing")
		return
	}
	if s.Image == "" {
		add("value:image", "standalone image is unset")
	}
	if s.Name == "" {
		add("value:name", "standalone container name is unset")
	}
}

func (p *Preflight) checkSteps(app *config.App, add func(string, string, ...any)) {
	for _, list := range []struct {
		name  string
		steps []config.Step
	}{
		{"pre", app.Steps.Pre},
		{"deploy", app.Steps.Deploy},
		{"post", app.Steps.Post},
	} {
		seen := map[string]bool{}
		for i, s := range list.steps {
			path := fmt.Sprintf("value:steps.%s[%d]", list.name, i)
			if len(s.Command) == 0 || s.Command[0] == "" {
				add(path, "command argv is empty")
				continue
			}
			if s.Name != "" {
				if seen[s.Name] {
					add(path, "duplicate step name %q", s.Name)
				}
				seen[s.Name] = true
			}
			if s.Timeout <= 0 {
				add(path, "timeout must be positive")
			}
		}
	}
}

func (p *Preflight) checkHealth(app *config.App, add func(string, string, ...any)) {
	if app.Health == nil {
		return
	}
	if h := app.Health.HTTP; h != nil {
		if h.URL == "" {
			add("value:health.url", "http probe URL is unset")
		} else if u, err := url.Parse(h.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("value:health.url", "http probe URL %q is not an absolute http(s) URL", h.URL)
		}
	}
	if c := app.Health.Command; len(c) > 0 && (c[0] == "") {
		add("value:health.command", "health command argv starts empty")
	}
}

// checkSecrets verifies every secret reference in the app can be resolved
// right now. Values are read and dropped immediately — never stored,
// logged, or included in findings.
func (p *Preflight) checkSecrets(app *config.App, add func(string, string, ...any)) {
	refs := collectSecretRefs(app)
	for _, r := range refs {
		if _, err := r.Resolve(); err != nil {
			add("secrets:resolvable", "%v", err)
		}
	}
}

func collectSecretRefs(app *config.App) []*config.SecretRef {
	var refs []*config.SecretRef
	if app.Health != nil {
		if h := app.Health.HTTP; h != nil {
			for i := range h.Headers {
				if h.Headers[i].SecretRef != nil {
					refs = append(refs, h.Headers[i].SecretRef)
				}
			}
		}
	}
	if s := app.Deploy.Standalone; s != nil {
		for i := range s.Env {
			if s.Env[i].SecretRef != nil {
				refs = append(refs, s.Env[i].SecretRef)
			}
		}
	}
	return refs
}

func (p *Preflight) checkDataDir(add func(string, string, ...any)) {
	if p.DataDir == "" {
		return
	}
	probe, err := os.CreateTemp(p.DataDir, ".yukariko-preflight-*")
	if err != nil {
		add("path:data_dir", "data directory %s is not writable: %v", p.DataDir, err)
		return
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name) // best effort; the write already proved writability
}

func (p *Preflight) look(name string) (string, error) {
	if p.LookPath != nil {
		return p.LookPath(name)
	}
	return exec.LookPath(name)
}

func (p *Preflight) exec(ctx context.Context, name string, argv []string) error {
	if p.Exec != nil {
		return p.Exec(ctx, name, argv)
	}
	r := p.Runner
	if r == nil {
		r = &runner.Runner{}
	}
	res, err := r.Run(ctx, runner.Request{Name: name, Argv: argv, Timeout: preflightTimeout})
	if err != nil {
		return err
	}
	if res.Status != runner.StatusSuccess {
		if res.Err != "" {
			return errors.New(res.Err)
		}
		return fmt.Errorf("exited with status %s", res.Status)
	}
	return nil
}
