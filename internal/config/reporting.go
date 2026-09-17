package config

// Reporting models optional peer status reporting (issues #17/#18). Reports
// carry status only and can never select or provide commands. Secrets are
// references only; a literal secret in YAML is not representable by this
// schema and is rejected as an unknown field.
type Reporting struct {
	Outbound OutboundReporting `yaml:"outbound,omitempty"`
	Inbound  InboundReporting  `yaml:"inbound,omitempty"`
}

// OutboundReporting sends signed status events to one peer receiver.
type OutboundReporting struct {
	Enabled           bool       `yaml:"enabled,omitempty"`
	URL               string     `yaml:"url,omitempty"`
	HostID            string     `yaml:"host_id,omitempty"`
	SecretRef         *SecretRef `yaml:"secret_ref,omitempty"`
	HeartbeatInterval Duration   `yaml:"heartbeat_interval,omitempty"`
}

// InboundReporting accepts signed reports from allowlisted hosts.
type InboundReporting struct {
	Enabled      bool         `yaml:"enabled,omitempty"`
	RequireTLS   *bool        `yaml:"require_tls,omitempty"`
	MaxBodyBytes int          `yaml:"max_body_bytes,omitempty"`
	ClockSkew    Duration     `yaml:"clock_skew,omitempty"`
	ReplayWindow Duration     `yaml:"replay_window,omitempty"`
	RateLimit    RateLimit    `yaml:"rate_limit,omitempty"`
	Hosts        []ReportHost `yaml:"hosts,omitempty"`
}

// RateLimit bounds accepted reports per host per window.
type RateLimit struct {
	Events int      `yaml:"events,omitempty"`
	Per    Duration `yaml:"per,omitempty"`
}

// ReportHost is one allowlisted peer. Removing a host revokes it. Multiple
// keys support rotation.
type ReportHost struct {
	ID   string      `yaml:"id"`
	Keys []ReportKey `yaml:"keys,omitempty"`
}

// ReportKey is one accepted signing key for a host.
type ReportKey struct {
	KeyID     string     `yaml:"key_id"`
	SecretRef *SecretRef `yaml:"secret_ref,omitempty"`
}

// ApplyDefaults fills unset optional reporting fields.
func (r *Reporting) ApplyDefaults() {
	if r.Outbound.HeartbeatInterval == 0 {
		r.Outbound.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if r.Inbound.RequireTLS == nil {
		t := true
		r.Inbound.RequireTLS = &t
	}
	if r.Inbound.MaxBodyBytes == 0 {
		r.Inbound.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if r.Inbound.ClockSkew == 0 {
		r.Inbound.ClockSkew = DefaultClockSkew
	}
	if r.Inbound.ReplayWindow == 0 {
		r.Inbound.ReplayWindow = DefaultReplayWindow
	}
	if r.Inbound.RateLimit.Events == 0 {
		r.Inbound.RateLimit.Events = DefaultRateLimitEvents
	}
	if r.Inbound.RateLimit.Per == 0 {
		r.Inbound.RateLimit.Per = DefaultRateLimitPer
	}
}

// IsRequiredTLS reports whether HTTPS is enforced for inbound reports.
func (i *InboundReporting) IsRequiredTLS() bool {
	return i.RequireTLS == nil || *i.RequireTLS
}
