package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

// --- golden argv per supported option ---------------------------------------

func TestRunArgsGoldenPerOption(t *testing.T) {
	t.Parallel()
	dur := func(d time.Duration) config.Duration { return config.Duration(d) }
	cases := []struct {
		name string
		spec *config.StandaloneSpec
		want []string
	}{
		{
			name: "minimal",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1"},
			want: []string{"docker", "run", "-d", "--name", "app-1", "app:1"},
		},
		{
			name: "restart and identity",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", Restart: "unless-stopped", User: "1000:1000", WorkDir: "/srv"},
			want: []string{"docker", "run", "-d", "--name", "app-1", "--restart", "unless-stopped", "--user", "1000:1000", "--workdir", "/srv", "app:1"},
		},
		{
			name: "restart no is omitted",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", Restart: "no"},
			want: []string{"docker", "run", "-d", "--name", "app-1", "app:1"},
		},
		{
			name: "entrypoint and command",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", Entrypoint: []string{"/docker-entrypoint.sh"}, Command: []string{"nginx", "-g", "daemon off;"}},
			want: []string{"docker", "run", "-d", "--name", "app-1", "--entrypoint", "/docker-entrypoint.sh", "app:1", "nginx", "-g", "daemon off;"},
		},
		{
			name: "mounts",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", Binds: []string{
				"/srv/data:/data",
				"/etc/local:/etc/host:ro",
				"cache_vol:/cache",
			}},
			want: []string{"docker", "run", "-d", "--name", "app-1",
				"--mount", "type=bind,src=/srv/data,dst=/data",
				"--mount", "type=bind,src=/etc/local,dst=/etc/host,readonly",
				"--mount", "type=bind,src=cache_vol,dst=/cache",
				"app:1"},
		},
		{
			name: "ports",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", Ports: []string{
				"127.0.0.1:8080:80/tcp",
				"53:53/udp",
				"9090/tcp",
			}},
			want: []string{"docker", "run", "-d", "--name", "app-1",
				"-p", "127.0.0.1:8080:80/tcp",
				"-p", "53:53/udp",
				"--expose", "9090/tcp",
				"app:1"},
		},
		{
			name: "labels sorted",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", Labels: map[string]string{
				"zeta": "1", "alpha": "2",
			}},
			want: []string{"docker", "run", "-d", "--name", "app-1", "--label", "alpha=2", "--label", "zeta=1", "app:1"},
		},
		{
			name: "healthcheck cmd-shell with timings",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", HealthCheck: &config.ContainerHealthCheck{
				Test:        []string{"CMD-SHELL", "curl -f localhost || exit 1"},
				Interval:    dur(30 * time.Second),
				Timeout:     dur(5 * time.Second),
				Retries:     3,
				StartPeriod: dur(15 * time.Second),
			}},
			want: []string{"docker", "run", "-d", "--name", "app-1",
				"--health-cmd", "curl -f localhost || exit 1",
				"--health-interval", "30s", "--health-timeout", "5s",
				"--health-retries", "3", "--health-start-period", "15s",
				"app:1"},
		},
		{
			name: "healthcheck disabled",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", HealthCheck: &config.ContainerHealthCheck{Test: []string{"NONE"}}},
			want: []string{"docker", "run", "-d", "--name", "app-1", "--no-healthcheck", "app:1"},
		},
		{
			name: "plain env and secret pass-through",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1", Env: []config.EnvVar{
				{Name: "TZ", Value: "Europe/Oslo"},
				{Name: "API_KEY", SecretRef: &config.SecretRef{Env: "APP_API_KEY"}},
			}},
			want: []string{"docker", "run", "-d", "--name", "app-1",
				"--env", "TZ=Europe/Oslo",
				"--env", "API_KEY",
				"app:1"},
		},
		{
			name: "endpoint flags sit after the binary",
			spec: &config.StandaloneSpec{Image: "app:1", Name: "app-1"},
			want: []string{"docker", "--context", "rootless", "run", "-d", "--name", "app-1", "app:1"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Endpoint flags only appear for the flagged case; the rest pin
			// the default-daemon argv byte for byte.
			flags := []string(nil)
			if tc.name == "endpoint flags sit after the binary" {
				flags = []string{"--context", "rootless"}
			}
			argv, _ := runArgs(tc.spec, flags)
			if strings.Join(argv, "|") != strings.Join(tc.want, "|") {
				t.Errorf("argv =\n  %v\nwant\n  %v", argv, tc.want)
			}
		})
	}
}

func TestResolveRunEnv(t *testing.T) {
	t.Setenv("APP_SECRET_RUN", "hunter2")
	spec := &config.StandaloneSpec{Env: []config.EnvVar{
		{Name: "PLAIN", Value: "x"},
		{Name: "SECRET", SecretRef: &config.SecretRef{Env: "APP_SECRET_RUN"}},
	}}
	env, err := resolveRunEnv(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(env, ",") != "APP_SECRET_RUN=hunter2" {
		t.Errorf("env = %v", env)
	}
	// An unresolvable secret is an error, never a silent omission.
	spec.Env[1].SecretRef = &config.SecretRef{Env: "DEFINITELY_MISSING"}
	if _, err := resolveRunEnv(spec); err == nil {
		t.Error("expected an error for the unresolvable secret")
	}
}

// --- fake engine lifecycle --------------------------------------------------

type fakeEngine struct {
	mu      sync.Mutex
	steps   []string
	detail  docker.ContainerDetail
	errs    map[string]error
	removed string
}

func (f *fakeEngine) record(step string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, step)
	return nil
}

func (f *fakeEngine) Inspect(_ context.Context, name string) (docker.ContainerDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "inspect "+name)
	if f.errs != nil {
		if err, ok := f.errs[name]; ok {
			return docker.ContainerDetail{}, err
		}
	}
	return f.detail, nil
}

func (f *fakeEngine) Pull(_ context.Context, image, digest string) error {
	return f.record("pull " + image + "@" + digest)
}

func (f *fakeEngine) Stop(_ context.Context, name string) error { return f.record("stop " + name) }

func (f *fakeEngine) Rename(_ context.Context, oldName, newName string) error {
	return f.record(fmt.Sprintf("rename %s %s", oldName, newName))
}

func (f *fakeEngine) Run(_ context.Context, argv []string, env []string) error {
	return f.record("run " + strings.Join(argv, " ") + " env[" + strings.Join(env, ",") + "]")
}

func (f *fakeEngine) ConnectNetwork(_ context.Context, network, container string) error {
	return f.record(fmt.Sprintf("connect %s %s", network, container))
}

func (f *fakeEngine) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, "remove "+name)
	f.removed = name
	return nil
}

type fixedLookup struct{ version string }

func (l fixedLookup) DeployedVersion(_ context.Context, _ string) (string, bool) {
	return l.version, l.version != ""
}

func standalonePipeline(t *testing.T, engine *fakeEngine, ck *countingCheckpoint, lookup DeployedLookup) *StandalonePipeline {
	t.Helper()
	resolver := &fakeResolver{digests: map[string]string{"docker.io/library/app:1": "sha256:fedc"}}
	return &StandalonePipeline{
		Engine:     engine,
		Registry:   resolver,
		Health:     &fakeHealth{},
		Checkpoint: ck,
		Deployed:   lookup,
		Now:        func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func standaloneApp() *config.App {
	return &config.App{
		ID: "app",
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "app:1"}}},
		},
		Deploy: config.Deploy{
			Mode:       config.DeployStandalone,
			Standalone: &config.StandaloneSpec{Image: "app:1", Name: "app-1"},
		},
	}
}

func TestStandaloneSuccessLifecycle(t *testing.T) {
	t.Parallel()
	engine := &fakeEngine{detail: docker.ContainerDetail{Name: "app-1", EnvKeys: []string{}}}
	ck := &countingCheckpoint{}
	p := standalonePipeline(t, engine, ck, fixedLookup{version: "sha256:old"})

	res, err := p.Deploy(context.Background(), standaloneApp())
	if err != nil || !res.Success {
		t.Fatalf("deploy = %v / %v (%s)", err, res.Success, res.Detail)
	}
	want := []string{
		"inspect app-1", // early compose-management guard
		"pull app:1@sha256:fedc",
		"inspect app-1",
		"stop app-1",
		"rename app-1 app-1-yukariko-old-1700000000",
		"run docker run -d --name app-1 app:1 env[]",
		"remove app-1-yukariko-old-1700000000",
	}
	got := engine.recordedCallsStr()
	if strings.Join(got, " >> ") != strings.Join(want, " >> ") {
		t.Errorf("lifecycle =\n  %s\nwant\n  %s", strings.Join(got, " >> "), strings.Join(want, " >> "))
	}
	if ck.count() != 1 || !strings.Contains(ck.versions[0], "sha256:fedc") {
		t.Errorf("checkpoints = %v, want the resolved digest once", ck.versions)
	}
}

func TestStandaloneUnchangedDigestIsNoOp(t *testing.T) {
	t.Parallel()
	engine := &fakeEngine{detail: docker.ContainerDetail{Name: "app-1"}}
	ck := &countingCheckpoint{}
	p := standalonePipeline(t, engine, ck, fixedLookup{version: "sha256:fedc"})

	res, err := p.Deploy(context.Background(), standaloneApp())
	if err != nil || !res.Success {
		t.Fatalf("deploy = %v / %v", err, res.Success)
	}
	if !strings.Contains(res.Detail, "unchanged") {
		t.Errorf("detail = %q, want the no-op note", res.Detail)
	}
	got := engine.recordedCallsStr()
	for _, s := range got {
		if strings.HasPrefix(s, "stop") || strings.HasPrefix(s, "run") || strings.HasPrefix(s, "remove") {
			t.Errorf("unchanged digest mutated the container: %s", s)
		}
	}
	if ck.count() != 0 {
		t.Errorf("checkpoints = %d, want zero for a no-op", ck.count())
	}
}

func TestStandaloneRunFailureLeavesManualRecoveryPath(t *testing.T) {
	t.Parallel()
	engine := &fakeEngine{detail: docker.ContainerDetail{Name: "app-1"}}
	ck := &countingCheckpoint{}
	p := standalonePipeline(t, engine, ck, fixedLookup{version: "sha256:old"})
	// Fail the Run stage: wrap via a failing engine method.
	p.Engine = &runFailingEngine{fakeEngine: engine, failMsg: "docker: run failed"}

	_, err := p.Deploy(context.Background(), standaloneApp())
	if err == nil || !strings.Contains(err.Error(), "manual recovery") || !strings.Contains(err.Error(), "docker start app-1") {
		t.Fatalf("err = %v, want the documented manual recovery", err)
	}
	got := engine.recordedCallsStr()
	last := got[len(got)-1]
	if !strings.HasPrefix(last, "run ") {
		t.Errorf("last step = %q, want the failed run", last)
	}
	if engine.removed != "" {
		t.Error("the old container must never be removed after a failed run")
	}
	if ck.count() != 0 {
		t.Errorf("checkpoints = %d, want zero", ck.count())
	}
}

type runFailingEngine struct {
	*fakeEngine
	failMsg string
}

func (f *runFailingEngine) Run(_ context.Context, _ []string, _ []string) error {
	_ = f.record("run <failed>")
	return errors.New(f.failMsg)
}

func TestStandalonePreflightRefusalsBeforeStop(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		detail  docker.ContainerDetail
		wantSub string
	}{
		{
			name:    "compose managed",
			detail:  docker.ContainerDetail{Name: "app-1", Labels: map[string]string{"com.docker.compose.project": "stack"}},
			wantSub: "compose-managed",
		},
		{
			name:    "privileged",
			detail:  docker.ContainerDetail{Name: "app-1", Privileged: true},
			wantSub: "privileged",
		},
		{
			name:    "env names missing from spec",
			detail:  docker.ContainerDetail{Name: "app-1", EnvKeys: []string{"TZ", "SPECIAL"}},
			wantSub: "SPECIAL",
		},
		{
			name:    "healthcheck missing from spec",
			detail:  docker.ContainerDetail{Name: "app-1", Health: docker.HealthState{Configured: true, Status: "healthy"}},
			wantSub: "healthcheck",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := &fakeEngine{detail: tc.detail}
			ck := &countingCheckpoint{}
			p := standalonePipeline(t, engine, ck, fixedLookup{version: "sha256:old"})

			_, err := p.Deploy(context.Background(), standaloneApp())
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want refusal containing %q", err, tc.wantSub)
			}
			if !strings.Contains(err.Error(), "nothing was stopped") {
				t.Errorf("refusal must state nothing was stopped: %v", err)
			}
			got := engine.recordedCallsStr()
			for _, s := range got {
				if strings.HasPrefix(s, "stop") || strings.HasPrefix(s, "run") || strings.HasPrefix(s, "remove") {
					t.Errorf("refused deployment mutated: %s", s)
				}
			}
			if ck.count() != 0 {
				t.Errorf("checkpoints = %d, want zero", ck.count())
			}
		})
	}
}

func TestStandaloneMissingContainerStillCreates(t *testing.T) {
	t.Parallel()
	engine := &fakeEngine{errs: map[string]error{"app-1": fmt.Errorf("no such container: %w", docker.ErrContainerMissing)}}
	ck := &countingCheckpoint{}
	p := standalonePipeline(t, engine, ck, fixedLookup{version: ""})

	res, err := p.Deploy(context.Background(), standaloneApp())
	if err != nil || !res.Success {
		t.Fatalf("first-time creation = %v / %v", err, res.Success)
	}
	got := engine.recordedCallsStr()
	for _, s := range got {
		if strings.HasPrefix(s, "rename") || strings.HasPrefix(s, "remove") || strings.HasPrefix(s, "stop") {
			t.Errorf("creation must not stop/rename/remove: %s", s)
		}
	}
	if ck.count() != 1 {
		t.Errorf("checkpoints = %d, want one", ck.count())
	}
}

// recordedCalls exposes the staged step strings.
func (f *fakeEngine) recordedCallsStr() []string { return f.steps }
