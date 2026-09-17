package config

// Health configures post-deploy checks (required, run inside the deployment
// success transaction) and independent periodic monitoring (issue #13).
// Health state never triggers restarts or rollbacks.
type Health struct {
	Required *bool        `yaml:"required"`
	Interval Duration     `yaml:"interval"`
	HTTP     *HTTPProbe   `yaml:"http"`
	Docker   *DockerProbe `yaml:"docker"`
	Command  []string     `yaml:"command"`
}

// HTTPProbe is a GET probe with bounded diagnostics. Header values are either
// plain non-secret values or secret references; query strings are treated as
// potentially secret when the URL contains them (issue #13 redacts them).
type HTTPProbe struct {
	URL     string       `yaml:"url"`
	Timeout Duration     `yaml:"timeout"`
	Status  []int        `yaml:"status"`
	Headers []HTTPHeader `yaml:"headers"`
}

// HTTPHeader is one probe header: exactly one of value or secret_ref.
type HTTPHeader struct {
	Name      string     `yaml:"name"`
	Value     string     `yaml:"value"`
	SecretRef *SecretRef `yaml:"secret_ref"`
}

// DockerProbe requires the container's Docker health status.
type DockerProbe struct {
	Required *bool `yaml:"required"`
}

// Health states used across store, API, and dashboard projections.
const (
	HealthChecking  = "checking"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthUnknown   = "unknown"
)

// ApplyDefaults fills unset optional health fields.
func (h *Health) ApplyDefaults() {
	if h.Required == nil {
		t := true
		h.Required = &t
	}
	if h.Interval == 0 {
		h.Interval = DefaultHealthInterval
	}
	if h.HTTP != nil {
		if h.HTTP.Timeout == 0 {
			h.HTTP.Timeout = DefaultProbeTimeout
		}
		if h.HTTP.Status == nil {
			h.HTTP.Status = []int{200, 399}
		}
	}
	if h.Docker != nil && h.Docker.Required == nil {
		t := true
		h.Docker.Required = &t
	}
}

// IsRequired reports whether these checks gate deployment success.
func (h *Health) IsRequired() bool {
	return h == nil || h.Required == nil || *h.Required
}

// IsRequired reports whether the Docker health status gates deployment.
func (d *DockerProbe) IsRequired() bool {
	return d == nil || d.Required == nil || *d.Required
}
