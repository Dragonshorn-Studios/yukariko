package config

// DockerEndpoint selects which local Docker daemon is addressed: a named
// Docker CLI context or a direct host URL such as
// unix:///run/user/1000/docker.sock for a rootless daemon. Exactly one of
// the two is set (validated); a nil endpoint means the invoking user's
// default daemon, which is also the only mode versions before this field
// knew. Resolution never persists back into the document.
type DockerEndpoint struct {
	Context string `yaml:"context,omitempty"`
	Host    string `yaml:"host,omitempty"`
}

// Flags renders the endpoint as docker CLI global flags, placed right after
// the binary: docker --context NAME ... or docker -H URL ... Nil when unset.
func (e *DockerEndpoint) Flags() []string {
	if e == nil {
		return nil
	}
	if e.Context != "" {
		return []string{"--context", e.Context}
	}
	if e.Host != "" {
		return []string{"-H", e.Host}
	}
	return nil
}

// Env renders the endpoint for user-owned argv (compose steps, health
// command probes), which Yukariko cannot flag-preface: DOCKER_CONTEXT or
// DOCKER_HOST, both on the runner's env allowlist. Nil when unset.
func (e *DockerEndpoint) Env() []string {
	if e == nil {
		return nil
	}
	if e.Context != "" {
		return []string{"DOCKER_CONTEXT=" + e.Context}
	}
	if e.Host != "" {
		return []string{"DOCKER_HOST=" + e.Host}
	}
	return nil
}

// EndpointFor resolves the effective Docker endpoint for one app: the app's
// override, else the configuration-wide default.
func (c *Config) EndpointFor(app *App) *DockerEndpoint {
	if app != nil && app.Docker != nil {
		return app.Docker
	}
	return c.Docker
}
