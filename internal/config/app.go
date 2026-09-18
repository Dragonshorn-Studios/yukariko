package config

// Source and deploy modes. An app declares where new versions come from and
// how it is deployed; Yukariko never guesses the missing half.
const (
	SourceGit      = "git"
	SourceRegistry = "registry"

	DeployCompose    = "compose"
	DeployStandalone = "standalone"
)

// App is one deployed application. ID is the stable identity used by the
// store, scheduler, locks, and reports.
type App struct {
	ID          string          `yaml:"id"`
	DisplayName string          `yaml:"display_name,omitempty"`
	Enabled     *bool           `yaml:"enabled,omitempty"`
	Interval    Duration        `yaml:"interval,omitempty"`
	Timeout     Duration        `yaml:"timeout,omitempty"`
	Retry       Retry           `yaml:"retry,omitempty"`
	Docker      *DockerEndpoint `yaml:"docker,omitempty"`
	Source      Source          `yaml:"source"`
	Deploy      Deploy          `yaml:"deploy"`
	Steps       Steps           `yaml:"steps,omitempty"`
	Health      *Health         `yaml:"health,omitempty"`
}

// Retry bounds exponential backoff after transient failures (issue #8).
type Retry struct {
	Base Duration `yaml:"base,omitempty"`
	Max  Duration `yaml:"max,omitempty"`
}

// Source declares where version changes come from.
type Source struct {
	Mode     string          `yaml:"mode"`
	Git      *GitSource      `yaml:"git,omitempty"`
	Registry *RegistrySource `yaml:"registry,omitempty"`
}

// GitSource points at an existing local worktree deployed through Git.
// Yukariko uses the host's Git credentials and never persists them.
type GitSource struct {
	Dir    string `yaml:"dir,omitempty"`
	Branch string `yaml:"branch,omitempty"`
	Remote string `yaml:"remote,omitempty"`
}

// RegistrySource lists the images a registry-backed app tracks.
type RegistrySource struct {
	Images []ImageRef `yaml:"images,omitempty"`
}

// ImageRef is a container image reference and optional target platform such
// as "linux/arm64". A digest reference ("name@sha256:...") is immutable.
type ImageRef struct {
	Ref      string `yaml:"ref"`
	Platform string `yaml:"platform,omitempty"`
}

// Deploy declares how the app is deployed.
type Deploy struct {
	Mode       string          `yaml:"mode"`
	Compose    *ComposeDeploy  `yaml:"compose,omitempty"`
	Standalone *StandaloneSpec `yaml:"standalone,omitempty"`
}

// ComposeDeploy captures the exact Compose invocation context. Every command
// Yukariko runs uses these values verbatim; nothing is reconstructed.
type ComposeDeploy struct {
	WorkDir     string   `yaml:"work_dir,omitempty"`
	Files       []string `yaml:"files,omitempty"`
	EnvFiles    []string `yaml:"env_files,omitempty"`
	Profiles    []string `yaml:"profiles,omitempty"`
	ProjectName string   `yaml:"project_name,omitempty"`
}

// Steps are explicit local commands run through the controlled runner around
// a deployment (issue #4/#11). Deploy steps left empty use the documented
// safe default for the app's mode.
type Steps struct {
	Pre    []Step `yaml:"pre,omitempty"`
	Deploy []Step `yaml:"deploy,omitempty"`
	Post   []Step `yaml:"post,omitempty"`
}

// Step is one argv-based command. Shell reinterprets the argv through a shell
// only when explicitly opted in; see docs/config.md for the risk.
type Step struct {
	Name    string   `yaml:"name,omitempty"`
	Command []string `yaml:"command"`
	Dir     string   `yaml:"dir,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty"`
	Shell   bool     `yaml:"shell,omitempty"`
}

// StandaloneSpec is the canonical, reproducible launch specification for a
// standalone container (issue #12). Environment values are references only;
// literal values are allowed solely for non-secret configuration and are
// marked with the value field, never with secret_ref.
type StandaloneSpec struct {
	Image       string                `yaml:"image"`
	Name        string                `yaml:"name"`
	Entrypoint  []string              `yaml:"entrypoint,omitempty"`
	Command     []string              `yaml:"command,omitempty"`
	Env         []EnvVar              `yaml:"env,omitempty"`
	Binds       []string              `yaml:"binds,omitempty"`
	Ports       []string              `yaml:"ports,omitempty"`
	Networks    []string              `yaml:"networks,omitempty"`
	Restart     string                `yaml:"restart,omitempty"`
	Labels      map[string]string     `yaml:"labels,omitempty"`
	User        string                `yaml:"user,omitempty"`
	WorkDir     string                `yaml:"work_dir,omitempty"`
	HealthCheck *ContainerHealthCheck `yaml:"health_check,omitempty"`
}

// EnvVar is one environment variable: either a plain non-secret value (safe
// to log) or a secret reference (never logged). Exactly one must be set.
type EnvVar struct {
	Name      string     `yaml:"name"`
	Value     string     `yaml:"value,omitempty"`
	SecretRef *SecretRef `yaml:"secret_ref,omitempty"`
}

// ContainerHealthCheck mirrors the supported subset of Docker healthcheck
// options for standalone containers.
type ContainerHealthCheck struct {
	Test        []string `yaml:"test,omitempty"`
	Interval    Duration `yaml:"interval,omitempty"`
	Timeout     Duration `yaml:"timeout,omitempty"`
	Retries     int      `yaml:"retries,omitempty"`
	StartPeriod Duration `yaml:"start_period,omitempty"`
}

// ApplyDefaults fills unset optional app fields.
func (a *App) ApplyDefaults() {
	if a.Enabled == nil {
		t := true
		a.Enabled = &t
	}
	if a.Interval == 0 {
		a.Interval = DefaultInterval
	}
	if a.Timeout == 0 {
		a.Timeout = DefaultTimeout
	}
	if a.Retry.Base == 0 {
		a.Retry.Base = DefaultRetryBase
	}
	if a.Retry.Max == 0 {
		a.Retry.Max = DefaultRetryMax
	}
	if a.Source.Mode == SourceGit && a.Source.Git != nil && a.Source.Git.Remote == "" {
		a.Source.Git.Remote = "origin"
	}
	for i := range a.Steps.Pre {
		a.Steps.Pre[i].applyDefault()
	}
	for i := range a.Steps.Deploy {
		a.Steps.Deploy[i].applyDefault()
	}
	for i := range a.Steps.Post {
		a.Steps.Post[i].applyDefault()
	}
	if a.Health != nil {
		a.Health.ApplyDefaults()
	}
}

func (s *Step) applyDefault() {
	if s.Timeout == 0 {
		s.Timeout = DefaultStepTimeout
	}
}

// IsEnabled reports whether the app participates in scheduling.
func (a *App) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}
