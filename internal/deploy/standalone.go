package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
)

// Engine is the narrow container-mutation interface the standalone pipeline
// needs. The CLI engine implements it over the runner; tests inject fakes.
// Every method is a named mutation so the staged sequence is explicit.
type Engine interface {
	// Inspect reads the current container (read-only).
	Inspect(ctx context.Context, name string) (docker.ContainerDetail, error)
	// Pull pulls the image and verifies the expected digest arrived locally.
	Pull(ctx context.Context, image, expectedDigest string) error
	// Stop stops the named container.
	Stop(ctx context.Context, name string) error
	// Rename renames a container.
	Rename(ctx context.Context, oldName, newName string) error
	// Run creates and starts the new container from the exact argv,
	// passing extra environment for secret references.
	Run(ctx context.Context, argv []string, env []string) error
	// ConnectNetwork attaches an already-running container to a network.
	ConnectNetwork(ctx context.Context, network, container string) error
	// Remove deletes a stopped container.
	Remove(ctx context.Context, name string) error
}

// DeployedLookup reads the last successfully deployed version for an app.
// The store adapter implements it alongside VersionCheckpoint.
type DeployedLookup interface {
	DeployedVersion(ctx context.Context, appID string) (string, bool)
}

// StandalonePipeline recreates a standalone container from its canonical
// spec only when the remote digest changed. Staged sequence: resolve → pull
// + verify → refusal preflight → stop old → rename old → run new → connect
// networks → required health checks → remove old → checkpoint.
//
// Failure handling: before the stop stage nothing has been touched; after
// it, the old container remains stopped under its backup name and the new
// container keeps the configured name — recovery is manual by design
// (`docker stop <new>; docker rename <backup> <name>; docker start <name>`),
// and no automatic rollback is claimed or attempted.
type StandalonePipeline struct {
	Engine     Engine
	Registry   DigestResolver
	Health     HealthChecker
	Checkpoint VersionCheckpoint
	Deployed   DeployedLookup // optional: refuses to recreate an unchanged digest
	Runner     *runner.Runner
	Run        func(ctx context.Context, req runner.Request) (runner.Result, error)
	// Now stamps the backup container name; nil → time.Now.
	Now func() time.Time
	// Platform resolves registry images when an image carries no platform.
	Platform string
}

var _ schedule.Deployer = (*StandalonePipeline)(nil)

// Deploy implements schedule.Deployer for standalone apps.
func (p *StandalonePipeline) Deploy(ctx context.Context, app *config.App) (schedule.DeployResult, error) {
	if app == nil || app.Deploy.Mode != config.DeployStandalone || app.Deploy.Standalone == nil {
		return schedule.DeployResult{}, errors.New("app is not a standalone deployment")
	}
	spec := app.Deploy.Standalone
	if p.Engine == nil {
		return schedule.DeployResult{}, errors.New("docker engine is not configured")
	}
	if p.Checkpoint == nil {
		return schedule.DeployResult{}, errors.New("deployed-version checkpoint is not configured; refusing to deploy")
	}
	// Refuse compose-managed targets before anything else.
	if existing, err := p.Engine.Inspect(ctx, spec.Name); err == nil {
		if finding := composeManagedFinding(existing); finding != "" {
			return schedule.DeployResult{Success: false},
				fmt.Errorf("refusing to recreate %q (nothing was stopped): %s", spec.Name, finding)
		}
	}

	resolver := p.Registry
	if resolver == nil {
		return schedule.DeployResult{}, errors.New("registry resolver is not configured; refusing to pull unverified images")
	}
	ref, err := registry.ParseRef(spec.Image)
	if err != nil {
		return schedule.DeployResult{}, fmt.Errorf("image %q: %w", spec.Image, err)
	}
	platform := p.Platform
	digest, err := resolver.Resolve(ctx, ref, platform)
	if err != nil {
		return schedule.DeployResult{}, fmt.Errorf("digest resolution failed: %w", err)
	}

	// An unchanged digest means no stop/recreate, even if invoked.
	if p.Deployed != nil {
		if last, ok := p.Deployed.DeployedVersion(ctx, app.ID); ok && last == digest {
			return schedule.DeployResult{Success: true, Detail: "digest unchanged; nothing to recreate"}, nil
		}
	}

	// Pull and verify before any mutation.
	if err := p.Engine.Pull(ctx, spec.Image, digest); err != nil {
		return schedule.DeployResult{Success: false}, fmt.Errorf("pull failed; nothing was touched: %w", err)
	}
	// Secret references are resolved before the mutation sequence starts so
	// an unresolvable secret can never leave a half-recreated pair.
	runEnv, err := resolveRunEnv(spec)
	if err != nil {
		return schedule.DeployResult{Success: false}, fmt.Errorf("refusing to recreate (nothing was stopped): %w", err)
	}

	// Refusal preflight against the CURRENT container, before stopping it.
	existing, inspectErr := p.Engine.Inspect(ctx, spec.Name)
	if inspectErr == nil {
		if finding := recreationRefusal(existing, spec); finding != "" {
			return schedule.DeployResult{Success: false},
				fmt.Errorf("refusing to recreate %q (nothing was stopped): %s", spec.Name, finding)
		}
	} else if !errors.Is(inspectErr, docker.ErrContainerMissing) {
		return schedule.DeployResult{Success: false},
			fmt.Errorf("cannot inspect the current container (refusing to proceed on unknown state): %w", inspectErr)
	}

	// Staged mutation sequence.
	now := p.Now
	if now == nil {
		now = time.Now
	}
	backupName := fmt.Sprintf("%s-yukariko-old-%d", spec.Name, now().Unix())
	hadOld := inspectErr == nil

	if hadOld {
		if err := p.Engine.Stop(ctx, spec.Name); err != nil {
			return schedule.DeployResult{Success: false}, fmt.Errorf("stop of %q failed; nothing was renamed: %w", spec.Name, err)
		}
		if err := p.Engine.Rename(ctx, spec.Name, backupName); err != nil {
			return schedule.DeployResult{Success: false}, fmt.Errorf("rename of %q failed: %w", spec.Name, err)
		}
	}

	argv, env := runArgs(spec)
	env = append(env, runEnv...)
	if err := p.Engine.Run(ctx, argv, env); err != nil {
		return schedule.DeployResult{Success: false},
			fmt.Errorf("starting the new container failed; manual recovery: docker start %s (the previous container is stopped as %q): %w",
				spec.Name, backupName, err)
	}
	for _, net := range extraNetworks(spec) {
		if err := p.Engine.ConnectNetwork(ctx, net, spec.Name); err != nil {
			return schedule.DeployResult{Success: false},
				fmt.Errorf("network %q connect failed; manual recovery: docker start %s (stopped as %q): %w",
					net, spec.Name, backupName, err)
		}
	}

	if p.Health != nil {
		if _, checkErr := p.Health.RunPostDeployChecks(ctx, app); checkErr != nil {
			return schedule.DeployResult{Success: false},
				fmt.Errorf("%w; manual recovery: docker stop %s && docker rename %s %s && docker start %s",
					checkErr, spec.Name, backupName, spec.Name, backupName)
		}
	}

	if hadOld {
		if err := p.Engine.Remove(ctx, backupName); err != nil {
			return schedule.DeployResult{Success: false},
				fmt.Errorf("removing the previous container %q failed; the new container is healthy: %w", backupName, err)
		}
	}

	if ctx.Err() != nil {
		return schedule.DeployResult{}, errors.New("cancelled after recreation; deployed digest unchanged")
	}
	if err := p.Checkpoint.MarkDeployed(ctx, app.ID, digest); err != nil {
		return schedule.DeployResult{}, fmt.Errorf("checkpoint failed; deployed digest unchanged: %w", err)
	}
	return schedule.DeployResult{Success: true, Detail: "recreated " + spec.Name + " at " + digest}, nil
}

// runArgs builds the deterministic `docker run` argv and the extra
// environment for secret references. See the package documentation for the
// documented entrypoint/healthcheck limitations.
func runArgs(spec *config.StandaloneSpec) (argv []string, env []string) {
	argv = []string{"docker", "run", "-d", "--name", spec.Name}
	if spec.Restart != "" && spec.Restart != "no" {
		argv = append(argv, "--restart", spec.Restart)
	}
	if spec.User != "" {
		argv = append(argv, "--user", spec.User)
	}
	if spec.WorkDir != "" {
		argv = append(argv, "--workdir", spec.WorkDir)
	}
	if len(spec.Entrypoint) > 0 {
		argv = append(argv, "--entrypoint", spec.Entrypoint[0])
	}
	for _, b := range spec.Binds {
		argv = append(argv, "--mount", bindToMount(b))
	}
	for _, p := range spec.Ports {
		if strings.Contains(p, ":") {
			argv = append(argv, "-p", p)
		} else {
			argv = append(argv, "--expose", p)
		}
	}
	for _, net := range spec.Networks {
		argv = append(argv, "--network", net)
	}
	for _, kv := range sortedLabels(spec.Labels) {
		argv = append(argv, "--label", kv[0]+"="+kv[1])
	}
	if hc := spec.HealthCheck; hc != nil && len(hc.Test) > 0 {
		switch {
		case hc.Test[0] == "NONE":
			argv = append(argv, "--no-healthcheck")
		case hc.Test[0] == "CMD-SHELL" && len(hc.Test) == 2:
			argv = append(argv, "--health-cmd", hc.Test[1])
		}
		if hc.Interval > 0 {
			argv = append(argv, "--health-interval", hc.Interval.D().String())
		}
		if hc.Timeout > 0 {
			argv = append(argv, "--health-timeout", hc.Timeout.D().String())
		}
		if hc.Retries > 0 {
			argv = append(argv, "--health-retries", fmt.Sprintf("%d", hc.Retries))
		}
		if hc.StartPeriod > 0 {
			argv = append(argv, "--health-start-period", hc.StartPeriod.D().String())
		}
	}
	for _, e := range spec.Env {
		switch {
		case e.SecretRef != nil && e.SecretRef.Env != "":
			// Pass-through form: the value travels through the runner's
			// environment (see resolveRunEnv), never argv.
			argv = append(argv, "--env", e.Name)
		case e.SecretRef != nil:
			argv = append(argv, "--env", e.Name)
		default:
			argv = append(argv, "--env", e.Name+"="+e.Value)
		}
	}
	argv = append(argv, spec.Image)
	argv = append(argv, spec.Command...)
	return argv, env
}

// resolveRunEnv materializes secret-reference values into the command
// environment at run time. Values travel through the runner's controlled
// environment, never argv, YAML, or storage.
func resolveRunEnv(spec *config.StandaloneSpec) ([]string, error) {
	var env []string
	for _, e := range spec.Env {
		if e.SecretRef == nil {
			continue
		}
		value, err := e.SecretRef.Resolve()
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", e.Name, err)
		}
		env = append(env, e.SecretRef.Env+"="+value)
	}
	return env, nil
}

// bindToMount converts the spec's src:dst[:ro] bind form to --mount syntax.
func bindToMount(b string) string {
	parts := strings.SplitN(b, ":", 3)
	m := "type=bind,src=" + parts[0] + ",dst=" + parts[1]
	if len(parts) == 2 || (len(parts) == 3 && parts[2] != "ro") {
		return m
	}
	if len(parts) == 3 && parts[2] == "ro" {
		m += ",readonly"
	}
	return m
}

func extraNetworks(spec *config.StandaloneSpec) []string {
	if len(spec.Networks) <= 1 {
		return nil
	}
	return spec.Networks[1:]
}

func sortedLabels(labels map[string]string) [][2]string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, labels[k]})
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// composeManagedFinding refuses compose-managed containers.
func composeManagedFinding(d docker.ContainerDetail) string {
	if project := d.Labels["com.docker.compose.project"]; project != "" {
		return fmt.Sprintf("the container is compose-managed (compose project %q); deploy it through a compose app instead", project)
	}
	return ""
}

// recreationRefusal refuses recreation when the running container has state
// the spec does not cover — the mutation would silently drop options.
func recreationRefusal(existing docker.ContainerDetail, spec *config.StandaloneSpec) string {
	if existing.Labels["com.docker.compose.project"] != "" {
		return "the current container is compose-managed; recreate it through compose instead"
	}
	if existing.Privileged {
		return "the current container runs privileged; the spec cannot reproduce that safely"
	}
	if sharedNs := unsafeNamespace(existing.PidMode, "pid"); sharedNs != "" {
		return sharedNs
	}
	if sharedNs := unsafeNamespace(existing.IpcMode, "ipc"); sharedNs != "" {
		return sharedNs
	}
	// Environment: every name on the running container must exist in the
	// spec (values are never compared — the spec holds references).
	specEnv := map[string]bool{}
	for _, e := range spec.Env {
		specEnv[e.Name] = true
	}
	var missing []string
	for _, key := range existing.EnvKeys {
		if !specEnv[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return "the current container has environment variables missing from the spec: " + strings.Join(missing, ", ")
	}
	// Healthcheck: a configured one must be represented in the spec.
	if existing.Health.Configured && spec.HealthCheck == nil {
		return "the current container has a docker healthcheck that the spec does not configure"
	}
	return ""
}

func unsafeNamespace(mode, what string) string {
	switch {
	case mode == "host":
		return fmt.Sprintf("the current container shares the host %s namespace; the spec cannot reproduce that safely", what)
	case strings.HasPrefix(mode, "container:"):
		return fmt.Sprintf("the current container shares another container's %s namespace (%s); the spec cannot reproduce that", what, mode)
	default:
		return ""
	}
}
