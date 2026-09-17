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
	ID          string   `yaml:"id"`
	DisplayName string   `yaml:"display_name"`
	Enabled     *bool    `yaml:"enabled"`
	Interval    Duration `yaml:"interval"`
	Timeout     Duration `yaml:"timeout"`
	Retry       Retry    `yaml:"retry"`
	Source      Source   `yaml:"source"`
	Deploy      Deploy   `yaml:"deploy"`
	Steps       Steps    `yaml:"steps"`
	Health      *Health  `yaml:"health"`
}

// Retry bounds exponential backoff after transient failures (issue #8).
type Retry struct {
	Base Duration `yaml:"base"`
	Max  Duration `yaml:"max"`
}

// Source declares where version changes come from.
type Source struct {
	Mode     string          `yaml:"mode"`
	Git      *GitSource      `yaml:"git"`
	Registry *RegistrySource `yaml:"registry"`
}

// GitSource points at an existing local worktree deployed through Git.
// Yukariko uses the host's Git credentials and never persists them.
type GitSource struct {
	Dir    string `yaml:"dir"`
	Branch string `yaml:"branch"`
	Remote string `yaml:"remote"`
}

// RegistrySource lists the images a registry-backed app tracks.
type RegistrySource struct {
	Images []ImageRef `yaml:"images"`
}

// ImageRef is a container image reference and optional target platform such
// as "linux/arm64". A digest reference ("name@sha256:...") is immutable.
type ImageRef struct {
	Ref      string `yaml:"ref"`
	Platform string `yaml:"platform"`
}

// Deploy declares how the app is deployed.
type Deploy struct {
	Mode       string          `yaml:"mode"`
	Compose    *ComposeDeploy  `yaml:"compose"`
	Standalone *StandaloneSpec `yaml:"standalone"`
}

// ComposeDeploy captures the exact Compose invocation context. Every command
// Yukariko runs uses these values verbatim; nothing is reconstructed.
type ComposeDeploy struct {
	WorkDir     string   `yaml:"work_dir"`
	Files       []string `yaml:"files"`
	EnvFiles    []string `yaml:"env_files"`
	Profiles    []string `yaml:"profiles"`
	ProjectName string   `yaml:"project_name"`
}

// Steps are explicit local commands run through the controlled runner around
// a deployment (issue #4/#11). Deploy steps left empty use the documented
// safe default for the app's mode.
type Steps struct {
	Pre    []Step `yaml:"pre"`
	Deploy []Step `yaml:"deploy"`
	Post   []Step `yaml:"post"`
}

// Step is one argv-based command. Shell reinterprets the argv through a shell
// only when explicitly opted in; see docs/config.md for the risk.
type Step struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	Dir     string   `yaml:"dir"`
	Timeout Duration `yaml:"timeout"`
	Shell   bool     `yaml:"shell"`
}

// StandaloneSpec is the canonical, reproducible launch specification for a
// standalone container (issue #12). Environment values are references only;
// literal values are allowed solely for non-secret configuration and are
// marked with the value field, never with secret_ref.
type StandaloneSpec struct {
	Image       string                `yaml:"image"`
	Name        string                `yaml:"name"`
	Entrypoint  []string              `yaml:"entrypoint"`
	Command     []string              `yaml:"command"`
	Env         []EnvVar              `yaml:"env"`
	Binds       []string              `yaml:"binds"`
	Ports       []string              `yaml:"ports"`
	Networks    []string              `yaml:"networks"`
	Restart     string                `yaml:"restart"`
	Labels      map[string]string     `yaml:"labels"`
	User        string                `yaml:"user"`
	WorkDir     string                `yaml:"work_dir"`
	HealthCheck *ContainerHealthCheck `yaml:"health_check"`
}

// EnvVar is one environment variable: either a plain non-secret value (safe
// to log) or a secret reference (never logged). Exactly one must be set.
type EnvVar struct {
	Name      string     `yaml:"name"`
	Value     string     `yaml:"value"`
	SecretRef *SecretRef `yaml:"secret_ref"`
}

// ContainerHealthCheck mirrors the supported subset of Docker healthcheck
// options for standalone containers.
type ContainerHealthCheck struct {
	Test        []string `yaml:"test"`
	Interval    Duration `yaml:"interval"`
	Timeout     Duration `yaml:"timeout"`
	Retries     int      `yaml:"retries"`
	StartPeriod Duration `yaml:"start_period"`
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
