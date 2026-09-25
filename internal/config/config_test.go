package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func mustParseFile(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) failed: %v", path, err)
	}
	return cfg
}

func TestParseValidExamples(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		file  string
		check func(t *testing.T, cfg *Config)
	}{
		{
			name: "git + compose",
			file: "git-compose.yaml",
			check: func(t *testing.T, cfg *Config) {
				app, ok := cfg.App("ghost-blog")
				if !ok {
					t.Fatal("app ghost-blog missing")
				}
				if app.Source.Mode != SourceGit || app.Source.Git.Branch != "main" {
					t.Errorf("git source not parsed: %+v", app.Source)
				}
				if app.Source.Git.Remote != "origin" {
					t.Errorf("git remote default = %q, want origin", app.Source.Git.Remote)
				}
				if app.Deploy.Mode != DeployCompose || app.Deploy.Compose.ProjectName != "ghost" {
					t.Errorf("compose deploy not parsed: %+v", app.Deploy)
				}
				if app.Health == nil || app.Health.HTTP == nil || app.Health.HTTP.URL != "http://127.0.0.1:2368/" {
					t.Errorf("http health not parsed: %+v", app.Health)
				}
				if !app.IsEnabled() {
					t.Error("enabled default must be true")
				}
				if !app.Health.IsRequired() {
					t.Error("health.required default must be true")
				}
			},
		},
		{
			name: "registry + compose",
			file: "registry-compose.yaml",
			check: func(t *testing.T, cfg *Config) {
				app := cfg.Apps[0]
				if app.Source.Mode != SourceRegistry {
					t.Errorf("mode = %q, want registry", app.Source.Mode)
				}
				img := app.Source.Registry.Images[0]
				if img.Ref != "ghcr.io/example/wiki:latest" || img.Platform != "linux/arm64" {
					t.Errorf("image ref not parsed: %+v", img)
				}
				if len(app.Deploy.Compose.Profiles) != 1 || app.Deploy.Compose.Profiles[0] != "full" {
					t.Errorf("profiles not preserved: %v", app.Deploy.Compose.Profiles)
				}
				if app.Interval.D() != 2*time.Minute {
					t.Errorf("explicit interval = %v, want 2m", app.Interval.D())
				}
			},
		},
		{
			name: "standalone",
			file: "standalone.yaml",
			check: func(t *testing.T, cfg *Config) {
				s := cfg.Apps[0].Deploy.Standalone
				if s == nil || s.Image != "louislam/uptime-kuma:1" || s.Name != "uptime-kuma" {
					t.Fatalf("standalone spec not parsed: %+v", s)
				}
				if len(s.Env) != 2 || s.Env[0].Value != "Europe/Berlin" || s.Env[1].SecretRef == nil {
					t.Errorf("env not parsed: %+v", s.Env)
				}
				if s.Restart != "unless-stopped" {
					t.Errorf("restart = %q", s.Restart)
				}
				if s.HealthCheck == nil || len(s.HealthCheck.Test) != 3 {
					t.Errorf("health_check not parsed: %+v", s.HealthCheck)
				}
			},
		},
		{
			name: "health variants",
			file: "health.yaml",
			check: func(t *testing.T, cfg *Config) {
				h := cfg.Apps[0].Health
				if len(h.HTTP.Headers) != 2 {
					t.Fatalf("headers not parsed: %+v", h.HTTP.Headers)
				}
				if h.HTTP.Headers[1].SecretRef == nil || h.HTTP.Headers[1].SecretRef.File != "/etc/yukariko/secrets/api-key" {
					t.Errorf("header secret ref not parsed: %+v", h.HTTP.Headers[1])
				}
				if h.Docker.IsRequired() {
					t.Error("docker required must stay false when set false")
				}
				if len(h.Command) != 1 || h.Command[0] != "/usr/local/bin/verify-api.sh" {
					t.Errorf("health command not parsed: %v", h.Command)
				}
			},
		},
		{
			name: "reporting",
			file: "reporting.yaml",
			check: func(t *testing.T, cfg *Config) {
				o := cfg.Reporting.Outbound
				if !o.Enabled || o.HostID != "garage-pi" || o.SecretRef == nil || o.SecretRef.Env != "YUKARIKO_REPORT_SECRET" {
					t.Errorf("outbound reporting not parsed: %+v", o)
				}
				i := cfg.Reporting.Inbound
				if len(i.Hosts) != 1 || len(i.Hosts[0].Keys) != 2 {
					t.Fatalf("inbound hosts not parsed: %+v", i.Hosts)
				}
				if !i.IsRequiredTLS() {
					t.Error("require_tls must default true")
				}
				if i.MaxBodyBytes != 1<<20 {
					t.Errorf("max_body_bytes = %d", i.MaxBodyBytes)
				}
			},
		},
		{
			name: "minimal",
			file: "minimal.yaml",
			check: func(t *testing.T, cfg *Config) {
				if len(cfg.Apps) != 1 || cfg.Apps[0].ID != "tiny" {
					t.Fatalf("minimal app not parsed: %+v", cfg.Apps)
				}
			},
		},
		{
			name: "docker endpoints",
			file: "docker-endpoint.yaml",
			check: func(t *testing.T, cfg *Config) {
				if cfg.Docker == nil || cfg.Docker.Host != "unix:///run/user/1000/docker.sock" {
					t.Fatalf("global endpoint = %+v", cfg.Docker)
				}
				inherits, ok := cfg.App("inherits-global")
				if !ok {
					t.Fatal("inherits-global missing")
				}
				if got := cfg.EndpointFor(&inherits); got != cfg.Docker {
					t.Fatalf("EndpointFor(inherits) = %+v, want the global default", got)
				}
				named, ok := cfg.App("named-context")
				if !ok {
					t.Fatal("named-context missing")
				}
				if got := cfg.EndpointFor(&named); got == nil || got.Context != "rootless" {
					t.Fatalf("EndpointFor(named) = %+v, want the per-app context override", got)
				}
				if flags := named.Docker.Flags(); len(flags) != 2 || flags[0] != "--context" || flags[1] != "rootless" {
					t.Fatalf("context flags = %v", flags)
				}
				if env := cfg.Docker.Env(); len(env) != 1 || env[0] != "DOCKER_HOST=unix:///run/user/1000/docker.sock" {
					t.Fatalf("host env = %v", env)
				}
				if flags := (*DockerEndpoint)(nil).Flags(); flags != nil {
					t.Fatalf("nil endpoint flags = %v, want nil", flags)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustParseFile(t, filepath.Join("testdata", tt.file))
			tt.check(t, cfg)
		})
	}
}

func TestDefaultsApplied(t *testing.T) {
	t.Parallel()
	cfg := mustParseFile(t, filepath.Join("testdata", "minimal.yaml"))
	if cfg.Server.Bind != DefaultServerBind {
		t.Errorf("server.bind default = %q, want %q", cfg.Server.Bind, DefaultServerBind)
	}
	if cfg.Retention.EventsDays != DefaultRetentionEvents || cfg.Retention.HealthDays != DefaultRetentionHealth || cfg.Retention.DeploymentsDays != DefaultRetentionDeploys {
		t.Errorf("retention defaults = %+v", cfg.Retention)
	}
	if cfg.Limits.CommandOutputBytes != DefaultCommandOutputBytes {
		t.Errorf("command output default = %d", cfg.Limits.CommandOutputBytes)
	}
	app := cfg.Apps[0]
	if app.Interval.D() != 5*time.Minute || app.Timeout.D() != 10*time.Minute {
		t.Errorf("interval/timeout defaults = %v/%v", app.Interval.D(), app.Timeout.D())
	}
	if app.Retry.Base.D() != 30*time.Second || app.Retry.Max.D() != 30*time.Minute {
		t.Errorf("retry defaults = %v/%v", app.Retry.Base.D(), app.Retry.Max.D())
	}
	if !app.Health.IsRequired() {
		t.Error("absent health block must not gate deploys")
	}
	if got := cfg.AppIDs(); len(got) != 1 || got[0] != "tiny" {
		t.Errorf("AppIDs() = %v", got)
	}
}

func TestParseInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		doc  string
		want []string
		// plain marks cases that fail fast before validation with a plain
		// error instead of a collected *InvalidError.
		plain bool
	}{
		{
			name: "global endpoint sets both context and host",
			doc: `
schema_version: 1
docker:
  context: rootless
  host: unix:///run/user/1000/docker.sock
apps: []
`,
			want: []string{"docker: must set exactly one of context or host, both are set"},
		},
		{
			name: "global endpoint host with bad scheme",
			doc: `
schema_version: 1
docker:
  host: /run/user/1000/docker.sock
apps: []
`,
			want: []string{`docker.host: must be a unix://, tcp://, ssh://, or npipe:// endpoint URL, got "/run/user/1000/docker.sock"`},
		},
		{
			name: "app endpoint sets both context and host",
			doc: `
schema_version: 1
apps:
  - id: demo
    docker: {context: a, host: unix:///tmp/x.sock}
    source:
      mode: registry
      registry: {images: [{ref: nginx:1}]}
    deploy:
      mode: standalone
      standalone: {image: nginx:1, name: demo}
`,
			want: []string{"apps[0].docker: must set exactly one of context or host, both are set"},
		},
		{
			name: "missing git branch",
			doc: `
schema_version: 1
apps:
  - id: demo
    source:
      mode: git
      git:
        dir: /srv/demo
    deploy:
      mode: compose
      compose: {work_dir: /srv/demo, files: [compose.yaml]}
`,
			want: []string{`apps[0].source.git.branch: is required when source.mode is "git"`},
		},
		{
			name: "relative git dir",
			doc: `
schema_version: 1
apps:
  - id: demo
    source:
      mode: git
      git: {dir: src/demo, branch: main}
    deploy:
      mode: compose
      compose: {work_dir: /srv/demo, files: [compose.yaml]}
`,
			want: []string{"apps[0].source.git.dir: must be an absolute path"},
		},
		{
			name: "conflicting source blocks",
			doc: `
schema_version: 1
apps:
  - id: demo
    source:
      mode: git
      git: {dir: /srv/demo, branch: main}
      registry:
        images: [{ref: nginx:1}]
    deploy:
      mode: compose
      compose: {work_dir: /srv/demo, files: [compose.yaml]}
`,
			want: []string{`apps[0].source.registry: must not be set when source.mode is "git"`},
		},
		{
			name: "duplicate app ids",
			doc: `
schema_version: 1
apps:
  - id: demo
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/a, files: [compose.yaml]}}
  - id: demo
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/b, files: [compose.yaml]}}
`,
			want: []string{`apps[1].id: duplicate app id "demo"; app ids must be unique`},
		},
		{
			name: "standalone requires registry source",
			doc: `
schema_version: 1
apps:
  - id: demo
    source: {mode: git, git: {dir: /srv/demo, branch: main}}
    deploy:
      mode: standalone
      standalone: {image: nginx:1, name: demo}
`,
			want: []string{`standalone recreation requires source.mode "registry"`},
		},
		{
			name: "compose requires files",
			doc: `
schema_version: 1
apps:
  - id: demo
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy:
      mode: compose
      compose: {work_dir: /srv/demo}
`,
			want: []string{"apps[0].deploy.compose.files: requires at least one Compose file; Yukariko never guesses file paths"},
		},
		{
			name: "literal reporting secret rejected",
			doc: `
schema_version: 1
reporting:
  outbound:
    enabled: true
    url: https://peer.example.com/report
    host_id: peer
    secret: hunter2
apps: []
`,
			want: []string{"field secret not found in type config.OutboundReporting"},
		},
		{
			name: "unknown top-level field",
			doc: `
schema_version: 1
deploy_everything: true
apps: []
`,
			want: []string{"field deploy_everything not found in type config.Config"},
		},
		{
			name: "invalid duration",
			doc: `
schema_version: 1
apps:
  - id: demo
    interval: 5x
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml]}}
`,
			want:  []string{`invalid duration "5x"`},
			plain: true,
		},
		{
			name: "zero duration rejected",
			doc: `
schema_version: 1
apps:
  - id: demo
    interval: 0s
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml]}}
`,
			want:  []string{`duration "0s" must be positive`},
			plain: true,
		},
		{
			name: "unsupported schema version",
			doc: `
schema_version: 2
apps: []
`,
			want:  []string{"schema_version: 2 is not supported by this build (supports 1)"},
			plain: true,
		},
		{
			name: "missing schema version",
			doc: `
apps: []
`,
			want:  []string{"schema_version: is required (current version is 1)"},
			plain: true,
		},
		{
			name: "bad source mode",
			doc: `
schema_version: 1
apps:
  - id: demo
    source: {mode: svn}
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml]}}
`,
			want: []string{`apps[0].source.mode: must be "git" or "registry", got "svn"`},
		},
		{
			name: "outbound http to non-loopback rejected",
			doc: `
schema_version: 1
reporting:
  outbound:
    enabled: true
    url: http://peer.example.com/report
    host_id: peer
    secret_ref: {env: SECRET}
apps: []
`,
			want: []string{"reporting.outbound.url: must use https (plain http is allowed only on loopback hosts for tests)"},
		},
		{
			name: "inbound host without keys",
			doc: `
schema_version: 1
reporting:
  inbound:
    enabled: true
    hosts:
      - id: peer
apps: []
`,
			want: []string{"reporting.inbound.hosts[0].keys: requires at least one key; removing the host revokes it"},
		},
		{
			name: "secret ref with both env and file",
			doc: `
schema_version: 1
reporting:
  outbound:
    enabled: true
    url: https://peer.example.com/report
    host_id: peer
    secret_ref: {env: A, file: /etc/a}
apps: []
`,
			want: []string{"reporting.outbound.secret_ref: must set exactly one of env or file, both are set"},
		},
		{
			name: "http probe bad status range",
			doc: `
schema_version: 1
apps:
  - id: demo
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml]}}
    health:
      http: {url: http://127.0.0.1:80/health, status: [500, 200]}
`,
			want: []string{"apps[0].health.http.status: must satisfy 100 <= min <= max <= 599"},
		},
		{
			name: "standalone bad port",
			doc: `
schema_version: 1
apps:
  - id: demo
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy:
      mode: standalone
      standalone:
        image: nginx:1
        name: demo
        ports: ["70000:80"]
`,
			want: []string{"70000 is outside 1-65535"},
		},
		{
			name: "image digest must be sha256",
			doc: `
schema_version: 1
apps:
  - id: demo
    source:
      mode: registry
      registry:
        images: [{ref: "nginx@md5:abc"}]
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml]}}
`,
			want: []string{"digest references must be sha256:<64 hex digits>"},
		},
		{
			name: "retry max below base",
			doc: `
schema_version: 1
apps:
  - id: demo
    retry: {base: 1h, max: 5m}
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml]}}
`,
			want: []string{"apps[0].retry.max: must be greater than or equal to retry.base"},
		},
		{
			name: "two apps sharing a compose target",
			doc: `
schema_version: 1
apps:
  - id: one
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml], project_name: demo}}
  - id: two
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml], project_name: demo}}
`,
			want: []string{"apps[1]: deploys the same target as apps[0]; two apps must never share a deploy target or per-app locks cannot prevent overlap"},
		},
		{
			name: "env with both value and secret",
			doc: `
schema_version: 1
apps:
  - id: demo
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy:
      mode: standalone
      standalone:
        image: nginx:1
        name: demo
        env: [{name: TOKEN, value: abc, secret_ref: {env: TOKEN}}]
`,
			want: []string{"must set either value or secret_ref, not both; literal secret values are never allowed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.doc))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q\n  does not contain %q", err.Error(), want)
				}
			}
			if tt.plain {
				return
			}
			var inv *InvalidError
			if !errors.As(err, &inv) {
				t.Errorf("error is %T, want *InvalidError", err)
			}
		})
	}
}

func TestGoldenValidationMessage(t *testing.T) {
	t.Parallel()
	doc := `
schema_version: 1
apps:
  - id: demo
    source:
      mode: git
      git:
        dir: /srv/demo
    deploy:
      mode: compose
      compose: {work_dir: /srv/demo, files: [compose.yaml]}
`
	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse succeeded, want error")
	}
	want := `apps[0].source.git.branch: is required when source.mode is "git"`
	if err.Error() != want {
		t.Errorf("error = %q\n want %q", err.Error(), want)
	}
}

func TestMultipleFindingsCollected(t *testing.T) {
	t.Parallel()
	doc := `
schema_version: 1
apps:
  - id: demo
    source: {mode: registry}
    deploy: {mode: compose, compose: {work_dir: relative, files: []}}
`
	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse succeeded, want error")
	}
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("error type %T, want *InvalidError", err)
	}
	if len(inv.Findings) < 3 {
		t.Errorf("collected %d findings, want at least 3:\n%s", len(inv.Findings), err.Error())
	}
	for _, want := range []string{"source.registry", "compose.work_dir", "compose.files"} {
		found := false
		for _, f := range inv.Findings {
			if strings.Contains(f.Path, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no finding anchored at %s in:\n%s", want, err.Error())
		}
	}
}

func TestEmptyAndMultiDocumentRejected(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"empty":          "",
		"whitespace":     "   \n\n",
		"multi-document": "schema_version: 1\napps: []\n---\nschema_version: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatal("Parse succeeded, want error")
			}
		})
	}
}

func TestSecretRefNeverDisclosesValues(t *testing.T) {
	const sentinel = "super-secret-value"
	t.Setenv("YUKARIKO_TEST_SECRET", sentinel)

	cfg := mustParseFile(t, filepath.Join("testdata", "reporting.yaml"))
	renderings := []string{
		cfg.Reporting.Outbound.SecretRef.String(),
		fmt.Sprintf("%v", cfg.Reporting.Outbound.SecretRef),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg.Reporting.Outbound.SecretRef),
	}
	for _, got := range renderings {
		if strings.Contains(got, sentinel) {
			t.Errorf("rendering leaked secret value: %q", got)
		}
	}
}

func TestSecretRefResolve(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		t.Setenv("YUKARIKO_TEST_SECRET", "env-value")
		ref := SecretRef{Env: "YUKARIKO_TEST_SECRET"}
		got, err := ref.Resolve()
		if err != nil || got != "env-value" {
			t.Errorf("Resolve() = %q, %v", got, err)
		}
	})
	t.Run("file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := (SecretRef{File: path}).Resolve()
		if err != nil || got != "file-secret" {
			t.Errorf("Resolve() = %q, %v", got, err)
		}
	})
	t.Run("missing env", func(t *testing.T) {
		t.Parallel()
		if _, err := (SecretRef{Env: "YUKARIKO_TEST_UNSET_1234"}).Resolve(); err == nil {
			t.Error("Resolve succeeded for unset env var")
		}
	})
	t.Run("empty env", func(t *testing.T) {
		t.Setenv("YUKARIKO_TEST_EMPTY", "")
		if _, err := (SecretRef{Env: "YUKARIKO_TEST_EMPTY"}).Resolve(); err == nil {
			t.Error("Resolve succeeded for empty env var")
		}
	})
	t.Run("both set", func(t *testing.T) {
		t.Parallel()
		if _, err := (SecretRef{Env: "A", File: "/etc/a"}).Resolve(); err == nil {
			t.Error("Resolve succeeded with both env and file")
		}
	})
	t.Run("neither set", func(t *testing.T) {
		t.Parallel()
		if _, err := (SecretRef{}).Resolve(); err == nil {
			t.Error("Resolve succeeded with neither env nor file")
		}
	})
}

func TestDurationMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	doc := `
schema_version: 1
apps:
  - id: demo
    interval: 7m30s
    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}
    deploy: {mode: compose, compose: {work_dir: /srv/demo, files: [compose.yaml]}}
`
	cfg, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if !strings.Contains(string(out), "interval: 7m30s") {
		t.Errorf("marshaled interval should be human-readable, got:\n%s", out)
	}
	var cfg2 Config
	if err := yaml.Unmarshal(out, &cfg2); err != nil {
		t.Fatalf("re-Unmarshal failed: %v", err)
	}
	if len(cfg2.Apps) != 1 {
		t.Fatalf("round-trip lost apps: %d", len(cfg2.Apps))
	}
	if cfg2.Apps[0].Interval.D() != 7*time.Minute+30*time.Second {
		t.Errorf("round-trip interval = %v", cfg2.Apps[0].Interval.D())
	}
}

func TestLoadIsSideEffectFree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "yukariko.yaml")
	doc, err := os.ReadFile(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("Load modified the config file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("Load created extra files in dir: %v", entries)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Errorf("err = %v, want read config failure", err)
	}
}

func TestExampleFileIsValid(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "examples", "yukariko.yaml")
	if _, err := Load(path); err != nil {
		t.Fatalf("examples/yukariko.yaml must stay valid: %v", err)
	}
}

func TestValidImageRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref  string
		want bool
	}{
		{"nginx:1.27", true},
		{"library/nginx", true},
		{"ghcr.io/acme/app:latest", true},
		{"registry:5000/app@sha256:" + strings.Repeat("a", 64), true},
		{"app@sha256:short", false},
		{"app@md5:" + strings.Repeat("a", 32), false},
		{"nginx:bad tag", false},
		{"/leading/slash", false},
		{"", false},
	}
	for _, tt := range tests {
		got := validImageRef(tt.ref) == nil
		if got != tt.want {
			t.Errorf("validImageRef(%q) ok = %v, want %v", tt.ref, got, tt.want)
		}
	}
}

func TestValidPortSpec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		spec string
		want bool
	}{
		{"8080:80", true},
		{"127.0.0.1:8080:80", true},
		{"53:53/udp", true},
		{"8080", false},
		{"a:80", false},
		{"1:2:3:4", false},
		{"8080:80/sctp", false},
	}
	for _, tt := range tests {
		got := validPortSpec(tt.spec) == nil
		if got != tt.want {
			t.Errorf("validPortSpec(%q) ok = %v, want %v", tt.spec, got, tt.want)
		}
	}
}

// TestDocumentRoundTrip pins the omitempty rendering contract that learn
// (#7) relies on: parse, re-marshal, and re-parse must succeed, and omitted
// optional fields must not reappear as zeros.
func TestDocumentRoundTrip(t *testing.T) {
	t.Parallel()
	original := `
schema_version: 1
apps:
  - id: web
    display_name: Web
    enabled: false
    source:
      mode: git
      git:
        dir: /srv/web
        branch: main
    deploy:
      mode: compose
      compose:
        work_dir: /srv/web
        files:
          - /srv/web/compose.yaml
    health:
      http:
        url: http://web.local/health
`
	cfg, err := DecodeRaw([]byte(original))
	if err != nil {
		t.Fatalf("DecodeRaw: %v", err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(out, []byte("0s")) {
		t.Errorf("re-marshaled document fabricates zero durations:\n%s", out)
	}
	for _, noise := range []string{"retry:", "interval:", "server:", "retention:", "limits:", "reporting:"} {
		if bytes.Contains(out, []byte(noise)) {
			t.Errorf("re-marshaled document emits unset %s:\n%s", noise, out)
		}
	}
	// An explicitly false `enabled` must survive the round trip (a pointer
	// to false is a set value, not the omitted zero).
	if !bytes.Contains(out, []byte("enabled: false")) {
		t.Errorf("explicit enabled: false lost:\n%s", out)
	}
	// The rendered document must validate again and preserve the fields.
	again, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v\n%s", err, out)
	}
	if again.Apps[0].ID != "web" || again.Apps[0].Source.Git.Branch != "main" {
		t.Errorf("round trip changed the app: %+v", again.Apps[0])
	}
	if again.Apps[0].IsEnabled() {
		t.Error("enabled: false must stay disabled after defaults")
	}
}

// validAuthDoc is the minimal valid OIDC gate used by the auth tests.
const validAuthDoc = `
schema_version: 1
server: {enabled: true}
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com/application/o/yukariko/
    client_id: yukariko
    client_secret_ref: {env: YUKARIKO_OIDC_CLIENT_SECRET}
    redirect_base: https://yukariko.example.com
apps: []
`

func TestAuthOIDCValidParsesAndDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validAuthDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o := cfg.Auth.OIDC
	if !o.Enabled || o.Issuer == "" || o.ClientID != "yukariko" {
		t.Fatalf("auth section lost: %+v", o)
	}
	if slices.Compare(o.Scopes, DefaultOIDCScopes) != 0 {
		t.Errorf("default scopes = %v, want %v", o.Scopes, DefaultOIDCScopes)
	}
	if o.SessionTTL != DefaultOIDCSessionTTL {
		t.Errorf("default session_ttl = %v, want %v", o.SessionTTL, DefaultOIDCSessionTTL)
	}
	if got, want := o.RedirectURI(), "https://yukariko.example.com/auth/callback"; got != want {
		t.Errorf("RedirectURI = %q, want %q", got, want)
	}
}

func TestAuthOIDCInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "enabled without server",
			doc: `
schema_version: 1
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com
    client_id: yukariko
    client_secret_ref: {env: X}
    redirect_base: https://yukariko.example.com
apps: []
`,
			want: "auth.oidc.enabled: requires server.enabled so there is a listener to protect",
		},
		{
			name: "plain http issuer off loopback",
			doc: `
schema_version: 1
server: {enabled: true}
auth:
  oidc:
    enabled: true
    issuer: http://auth.lan/application/o/yukariko/
    client_id: yukariko
    client_secret_ref: {env: X}
    redirect_base: https://yukariko.example.com
apps: []
`,
			want: `auth.oidc.issuer: must use https`,
		},
		{
			name: "missing client id",
			doc: `
schema_version: 1
server: {enabled: true}
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com
    client_secret_ref: {env: X}
    redirect_base: https://yukariko.example.com
apps: []
`,
			want: "auth.oidc.client_id: is required when oidc authentication is enabled",
		},
		{
			name: "missing secret ref",
			doc: `
schema_version: 1
server: {enabled: true}
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com
    client_id: yukariko
    redirect_base: https://yukariko.example.com
apps: []
`,
			want: "auth.oidc.client_secret_ref: is required when oidc authentication is enabled; secrets are references only",
		},
		{
			name: "redirect base with a path",
			doc: `
schema_version: 1
server: {enabled: true}
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com
    client_id: yukariko
    client_secret_ref: {env: X}
    redirect_base: https://example.com/yukariko
apps: []
`,
			want: "auth.oidc.redirect_base: must be a bare origin without path, query, or fragment",
		},
		{
			name: "scopes without openid",
			doc: `
schema_version: 1
server: {enabled: true}
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com
    client_id: yukariko
    client_secret_ref: {env: X}
    redirect_base: https://yukariko.example.com
    scopes: [profile, email]
apps: []
`,
			want: `auth.oidc.scopes: must include "openid"`,
		},
		{
			name: "session ttl out of bounds",
			doc: `
schema_version: 1
server: {enabled: true}
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com
    client_id: yukariko
    client_secret_ref: {env: X}
    redirect_base: https://yukariko.example.com
    session_ttl: 10s
apps: []
`,
			want: "auth.oidc.session_ttl: must be between 1m and 720h",
		},
		{
			name: "empty allowed group",
			doc: `
schema_version: 1
server: {enabled: true}
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com
    client_id: yukariko
    client_secret_ref: {env: X}
    redirect_base: https://yukariko.example.com
    allowed_groups: ["", ops]
apps: []
`,
			want: "auth.oidc.allowed_groups[0]: must not be empty",
		},
		{
			name: "unknown auth field rejected",
			doc: `
schema_version: 1
auth:
  oidc:
    client_secret: literal
apps: []
`,
			want: "field client_secret not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.doc))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q\n  does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestAuthOIDCDisabledSectionIsInert(t *testing.T) {
	t.Parallel()
	// A disabled or absent section never demands anything...
	cfg, err := Parse([]byte("schema_version: 1\nauth:\n  oidc:\n    enabled: false\napps: []\n"))
	if err != nil {
		t.Fatalf("Parse of a disabled auth section: %v", err)
	}
	if cfg.Auth.OIDC.Enabled {
		t.Error("auth.oidc must default to disabled")
	}
	// ...and rendering (which flows through DecodeRaw, never Parse) never
	// materializes auth defaults the user did not write.
	raw, err := DecodeRaw([]byte("schema_version: 1\nauth:\n  oidc:\n    enabled: false\napps: []\n"))
	if err != nil {
		t.Fatalf("DecodeRaw: %v", err)
	}
	out, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, noise := range []string{"scopes:", "session_ttl:", "issuer:", "client_id:"} {
		if bytes.Contains(out, []byte(noise)) {
			t.Errorf("rendered document emits unset %s:\n%s", noise, out)
		}
	}
	// A user-written auth section round-trips byte-faithfully in shape.
	raw, err = DecodeRaw([]byte(validAuthDoc))
	if err != nil {
		t.Fatalf("DecodeRaw: %v", err)
	}
	out, err = yaml.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	again, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v\n%s", err, out)
	}
	if !again.Auth.OIDC.Enabled || again.Auth.OIDC.ClientID != "yukariko" {
		t.Errorf("auth round trip changed the section: %+v", again.Auth.OIDC)
	}
}
