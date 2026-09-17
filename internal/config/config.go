// Package config defines Yukariko's declarative YAML configuration contract.
//
// Loading is side-effect free: it reads one file, strictly decodes it, applies
// documented defaults, and validates. It never touches Docker, Git, the
// network, or secret values. Host Docker, Compose, and Git state remains the
// source of truth; this configuration only tells Yukariko what to observe and
// how to invoke existing local tooling.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentSchemaVersion is the configuration schema this build understands.
// Bumping it requires a migration path documented in docs/config.md.
const CurrentSchemaVersion = 1

// Config is the root configuration document.
type Config struct {
	SchemaVersion int       `yaml:"schema_version"`
	Server        Server    `yaml:"server"`
	Reporting     Reporting `yaml:"reporting"`
	Retention     Retention `yaml:"retention"`
	Limits        Limits    `yaml:"limits"`
	Apps          []App     `yaml:"apps"`
}

// Server configures the read-only HTTP API and dashboard (issue #15/#16).
type Server struct {
	Enabled bool   `yaml:"enabled"`
	Bind    string `yaml:"bind"`
}

// Retention bounds how long operational history is kept. Zero values are
// replaced by defaults; cleanup primitives live in the store (issue #3).
type Retention struct {
	EventsDays      int `yaml:"events_days"`
	HealthDays      int `yaml:"health_days"`
	DeploymentsDays int `yaml:"deployments_days"`
}

// Limits bound resource usage of untrusted-sized inputs such as command output.
type Limits struct {
	CommandOutputBytes int `yaml:"command_output_bytes"`
}

// DefaultServerBind is deliberately loopback-only; exposing the dashboard is a
// documented, explicit user decision (issue #15).
const DefaultServerBind = "127.0.0.1:8484"

// Default values applied by ApplyDefaults when a field is unset.
const (
	DefaultInterval           = Duration(5 * time.Minute)  // 5m
	DefaultTimeout            = Duration(10 * time.Minute) // 10m
	DefaultRetryBase          = Duration(30 * time.Second) // 30s
	DefaultRetryMax           = Duration(30 * time.Minute) // 30m
	DefaultStepTimeout        = Duration(1 * time.Minute)  // 1m
	DefaultHealthInterval     = Duration(30 * time.Second) // 30s
	DefaultProbeTimeout       = Duration(10 * time.Second) // 10s
	DefaultHeartbeatInterval  = Duration(1 * time.Minute)  // 1m
	DefaultClockSkew          = Duration(5 * time.Minute)  // 5m
	DefaultReplayWindow       = Duration(24 * time.Hour)   // 24h
	DefaultRateLimitEvents    = 60                         // events
	DefaultRateLimitPer       = Duration(1 * time.Minute)  // 1m
	DefaultMaxBodyBytes       = 1 << 20                    // 1 MiB
	DefaultCommandOutputBytes = 64 << 10                   // 64 KiB
	DefaultRetentionEvents    = 30                         // days
	DefaultRetentionHealth    = 14                         // days
	DefaultRetentionDeploys   = 365                        // days
)

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse strictly decodes, defaults, and validates configuration bytes. Unknown
// fields anywhere in the document are rejected; findings are collected and
// returned together as *InvalidError.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configuration is empty")
		}
		return nil, decodeError(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected exactly one YAML document")
	}

	if cfg.SchemaVersion == 0 {
		return nil, errors.New("schema_version: is required (current version is 1)")
	}
	if cfg.SchemaVersion != CurrentSchemaVersion {
		return nil, fmt.Errorf("schema_version: %d is not supported by this build (supports %d); migrate the configuration as described in docs/config.md", cfg.SchemaVersion, CurrentSchemaVersion)
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// decodeError converts yaml type errors into InvalidError findings so callers
// get one consistent error type.
func decodeError(err error) error {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		findings := make([]PathError, len(typeErr.Errors))
		for i, e := range typeErr.Errors {
			findings[i] = PathError{Path: "yaml", Msg: e}
		}
		return &InvalidError{Findings: findings}
	}
	return err
}

// ApplyDefaults fills unset optional fields with the documented defaults.
func (c *Config) ApplyDefaults() {
	if c.Server.Bind == "" {
		c.Server.Bind = DefaultServerBind
	}
	if c.Retention.EventsDays == 0 {
		c.Retention.EventsDays = DefaultRetentionEvents
	}
	if c.Retention.HealthDays == 0 {
		c.Retention.HealthDays = DefaultRetentionHealth
	}
	if c.Retention.DeploymentsDays == 0 {
		c.Retention.DeploymentsDays = DefaultRetentionDeploys
	}
	if c.Limits.CommandOutputBytes == 0 {
		c.Limits.CommandOutputBytes = DefaultCommandOutputBytes
	}
	c.Reporting.ApplyDefaults()
	for i := range c.Apps {
		c.Apps[i].ApplyDefaults()
	}
}

// App returns the app with the given ID.
func (c *Config) App(id string) (App, bool) {
	for _, a := range c.Apps {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// AppIDs returns the configured app IDs in file order.
func (c *Config) AppIDs() []string {
	ids := make([]string, len(c.Apps))
	for i, a := range c.Apps {
		ids[i] = a.ID
	}
	return ids
}

// validBind reports whether bind is a host:port pair with a usable port.
func validBind(bind string) bool {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	p := 0
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil || p < 1 || p > 65535 {
		return false
	}
	return true
}
