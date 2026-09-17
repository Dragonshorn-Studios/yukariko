package schedule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// preflightCase builds one app variant and the finding checks expected to
// fail. Everything not mentioned defaults to valid.
type preflightCase struct {
	name    string
	mutate  func(app *config.App, dir string)
	lookErr map[string]error // binary name → LookPath error
	execErr map[string]error // argv[0] joined → error
	wantStr []string         // substrings expected among the findings
	wantOK  bool             // true when zero findings are expected
}

func TestPreflightMatrix(t *testing.T) {
	t.Parallel()
	base := func(mode string) *config.App {
		app := &config.App{
			ID:       "app",
			Interval: config.Duration(time.Minute),
			Retry:    config.Retry{Base: config.Duration(time.Second), Max: config.Duration(time.Minute)},
			Source: config.Source{
				Mode:     config.SourceRegistry,
				Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "app:1"}}},
			},
			Deploy: config.Deploy{Mode: mode},
		}
		return app
	}
	composeApp := func(dir string) *config.App {
		app := base(config.DeployCompose)
		app.Deploy.Compose = &config.ComposeDeploy{
			WorkDir:     dir,
			Files:       []string{filepath.Join(dir, "compose.yaml")},
			ProjectName: "app",
		}
		return app
	}
	standaloneApp := func() *config.App {
		app := base(config.DeployStandalone)
		app.Deploy.Standalone = &config.StandaloneSpec{Image: "app:1", Name: "app-1"}
		return app
	}

	cases := []preflightCase{
		{
			name:   "valid compose app passes",
			mutate: func(app *config.App, dir string) { *app = *composeApp(dir) },
			wantOK: true,
		},
		{
			name:   "valid standalone app passes",
			mutate: func(app *config.App, dir string) { *app = *standaloneApp() },
			wantOK: true,
		},
		{
			name: "docker binary missing",
			mutate: func(app *config.App, dir string) {
				*app = *standaloneApp()
			},
			lookErr: map[string]error{"docker": errors.New("not found")},
			wantStr: []string{"binary:docker"},
		},
		{
			name: "docker daemon unreachable",
			mutate: func(app *config.App, dir string) {
				*app = *standaloneApp()
			},
			execErr: map[string]error{"docker ps": errors.New("cannot connect to the Docker daemon")},
			wantStr: []string{"docker:reachable"},
		},
		{
			name: "compose plugin missing",
			mutate: func(app *config.App, dir string) {
				*app = *composeApp(dir)
			},
			execErr: map[string]error{"docker compose version": errors.New("docker compose is not a docker command")},
			wantStr: []string{"binary:compose"},
		},
		{
			name: "compose file missing",
			mutate: func(app *config.App, dir string) {
				fixed := composeApp(dir)
				fixed.Deploy.Compose.Files = []string{filepath.Join(dir, "missing.yaml")}
				*app = *fixed
			},
			wantStr: []string{"path:compose_files"},
		},
		{
			name: "env file missing",
			mutate: func(app *config.App, dir string) {
				fixed := composeApp(dir)
				fixed.Deploy.Compose.EnvFiles = []string{filepath.Join(dir, "missing.env")}
				*app = *fixed
			},
			wantStr: []string{"path:env_files"},
		},
		{
			name: "bad profile and project name",
			mutate: func(app *config.App, dir string) {
				fixed := composeApp(dir)
				fixed.Deploy.Compose.Profiles = []string{"", "two words"}
				fixed.Deploy.Compose.ProjectName = "Bad Name!"
				*app = *fixed
			},
			wantStr: []string{"value:profiles", "value:project_name"},
		},
		{
			name: "git source without git binary or worktree",
			mutate: func(app *config.App, dir string) {
				fixed := composeApp(dir)
				fixed.Source = config.Source{
					Mode: config.SourceGit,
					Git:  &config.GitSource{Dir: filepath.Join(dir, "nowhere"), Branch: "main"},
				}
				*app = *fixed
			},
			lookErr: map[string]error{"git": errors.New("not found")},
			wantStr: []string{"binary:git", "path:git_dir"},
		},
		{
			name: "git branch unset",
			mutate: func(app *config.App, dir string) {
				fixed := composeApp(dir)
				fixed.Source = config.Source{
					Mode: config.SourceGit,
					Git:  &config.GitSource{Dir: dir},
				}
				*app = *fixed
			},
			wantStr: []string{"value:git_branch"},
		},
		{
			name: "unresolvable secret reference",
			mutate: func(app *config.App, dir string) {
				fixed := standaloneApp()
				fixed.Deploy.Standalone.Env = []config.EnvVar{
					{Name: "API_KEY", SecretRef: &config.SecretRef{Env: "APP_API_KEY"}},
				}
				*app = *fixed
			},
			wantStr: []string{"secrets:resolvable"},
		},
		{
			name: "empty step command",
			mutate: func(app *config.App, dir string) {
				fixed := standaloneApp()
				fixed.Steps.Pre = []config.Step{{Name: "prepare", Timeout: config.Duration(time.Second)}}
				*app = *fixed
			},
			wantStr: []string{"value:steps.pre[0]"},
		},
		{
			name: "unparseable health URL",
			mutate: func(app *config.App, dir string) {
				fixed := standaloneApp()
				fixed.Health = &config.Health{
					HTTP: &config.HTTPProbe{URL: "not-a-url", Status: []int{200}},
				}
				*app = *fixed
			},
			wantStr: []string{"value:health.url"},
		},
		{
			name: "data dir unwritable",
			mutate: func(app *config.App, dir string) {
				*app = *standaloneApp()
			},
			wantStr: []string{"path:data_dir"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			app := base(config.DeployCompose)
			tc.mutate(app, dir)

			dataDir := t.TempDir()
			p := &Preflight{
				LookPath: func(name string) (string, error) {
					if err, ok := tc.lookErr[name]; ok {
						return "", err
					}
					return "/usr/bin/" + name, nil
				},
				Exec: func(_ context.Context, _ string, argv []string) error {
					if err, ok := tc.execErr[strings.Join(argv, " ")]; ok {
						return err
					}
					return nil
				},
				DataDir: dataDir,
			}
			// The unwritable-data-dir case points the preflight at a file.
			if tc.name == "data dir unwritable" {
				p.DataDir = filepath.Join(dir, "compose.yaml") // a file, not a dir
			}

			findings := p.Run(context.Background(), app)
			if tc.wantOK {
				if len(findings) != 0 {
					t.Fatalf("want zero findings, got: %v", findings)
				}
				return
			}
			joined := make([]string, len(findings))
			for i, f := range findings {
				joined[i] = f.String()
			}
			all := strings.Join(joined, "\n")
			for _, want := range tc.wantStr {
				if !strings.Contains(all, want) {
					t.Errorf("findings missing %q:\n%s", want, all)
				}
			}
		})
	}
}

func TestPreflightResolvableSecretPasses(t *testing.T) {
	t.Setenv("APP_API_KEY", "hunter2") // resolved and discarded; never stored
	app := &config.App{
		ID: "app",
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "app:1"}}},
		},
		Deploy: config.Deploy{
			Mode: config.DeployStandalone,
			Standalone: &config.StandaloneSpec{
				Image: "app:1",
				Name:  "app-1",
				Env:   []config.EnvVar{{Name: "API_KEY", SecretRef: &config.SecretRef{Env: "APP_API_KEY"}}},
			},
		},
	}
	p := &Preflight{
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		Exec:     func(context.Context, string, []string) error { return nil },
	}
	findings := p.Run(context.Background(), app)
	if len(findings) != 0 {
		t.Fatalf("want zero findings, got: %v", findings)
	}
}

func TestPreflightSecretValuesNeverInFindings(t *testing.T) {
	t.Setenv("APP_SECRET_VALUE", "hunter2-super-secret")
	app := &config.App{
		ID: "app",
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "app:1"}}},
		},
		Deploy: config.Deploy{
			Mode: config.DeployStandalone,
			Standalone: &config.StandaloneSpec{
				Image: "app:1",
				Name:  "app-1",
				Env:   []config.EnvVar{{Name: "SECRET", SecretRef: &config.SecretRef{Env: "APP_SECRET_VALUE"}}},
			},
		},
	}
	// The Exec fake fails everything it can; the secret ref still resolves,
	// and the secret value must not appear anywhere in the findings.
	p := &Preflight{
		LookPath: func(string) (string, error) { return "", errors.New("no binaries") },
		Exec:     func(context.Context, string, []string) error { return errors.New("probe failed") },
		DataDir:  filepath.Join(t.TempDir(), "missing"),
	}
	findings := p.Run(context.Background(), app)
	for _, f := range findings {
		if strings.Contains(f.Detail, "hunter2") {
			t.Errorf("secret value leaked into finding %q", f.String())
		}
	}
}
