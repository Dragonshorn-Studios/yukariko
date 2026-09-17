package learn

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

// fakeGit is the in-memory GitProber used by classification tests. It
// records probed directories so tests can assert that detection only ever
// looks at the declared compose workdirs.
type fakeGit struct {
	byDir map[string]GitInfo
	err   error
	calls []string
}

func (f *fakeGit) Probe(ctx context.Context, dir string) (GitInfo, error) {
	f.calls = append(f.calls, dir)
	if f.err != nil {
		return GitInfo{}, f.err
	}
	return f.byDir[dir], nil
}

func worktree(branch, url string) GitInfo {
	return GitInfo{InWorkTree: true, Branch: branch, RemoteName: "origin", RemoteURL: url}
}

// --- fixture builders -------------------------------------------------------

func composeProject(name, workdir string, files []string, members ...*docker.Container) *docker.ComposeProject {
	return &docker.ComposeProject{
		Name:        name,
		WorkDir:     workdir,
		ConfigFiles: files,
		Containers:  members,
	}
}

func composeMember(id, name, service, image string, extraLabels map[string]string) *docker.Container {
	labels := map[string]string{
		docker.LabelProject: "proj",
		docker.LabelService: service,
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return &docker.Container{
		ContainerSummary: docker.ContainerSummary{ID: id, Name: name, Image: image, State: "running"},
		Detail:           &docker.ContainerDetail{ID: id, Name: name, ImageRef: image, Labels: labels, State: "running", Running: true},
		Project:          "proj",
		Service:          service,
	}
}

func standaloneContainer(name string, detail *docker.ContainerDetail) *docker.Container {
	return &docker.Container{
		ContainerSummary: docker.ContainerSummary{
			ID:   "dddd4444dddd",
			Name: name,
			Image: func() string {
				if detail != nil {
					return detail.ImageRef
				}
				return "app:1"
			}(),
			State: "running",
		},
		Detail: detail,
	}
}

func baseStandaloneDetail() *docker.ContainerDetail {
	return &docker.ContainerDetail{
		ID:            "dddd4444dddd",
		Name:          "app",
		ImageRef:      "example.com/team/app@sha256:4444444444444444444444444444444444444444444444444444444444444444",
		State:         "running",
		Running:       true,
		RestartPolicy: "unless-stopped",
		NetworkMode:   "bridge",
	}
}

// --- golden classification fixtures ----------------------------------------

func TestGitComposeProjectProposal(t *testing.T) {
	t.Parallel()
	git := &fakeGit{byDir: map[string]GitInfo{
		"/srv/webapp": worktree("main", "https://github.com/example/webapp.git"),
	}}
	report := &docker.Report{
		Projects: []*docker.ComposeProject{
			composeProject("webapp", "/srv/webapp",
				[]string{"/srv/webapp/compose.yaml", "/srv/webapp/compose.override.yaml"},
				composeMember("aaaa1111aaaa", "webapp-web-1", "web", "nginx:1.27", nil),
				composeMember("bbbb2222bbbb", "webapp-cache-1", "cache", "redis:7", nil),
			),
		},
	}
	proposals := Proposals(context.Background(), report, git)
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d, want exactly one for the compose project", len(proposals))
	}
	p := proposals[0]
	if p.ID != "webapp" || p.Mode != config.DeployCompose {
		t.Errorf("ID/mode = %q / %q", p.ID, p.Mode)
	}
	if p.Verdict != VerdictReady {
		t.Errorf("Verdict = %q, want ready; confirmations=%v todos=%v blockers=%v",
			p.Verdict, p.Confirmations, p.TODOs, p.Blockers)
	}
	c := p.Compose
	if c == nil || c.SourceMode != config.SourceGit {
		t.Fatalf("SourceMode = %q, want git", c.SourceMode)
	}
	if c.WorkDir != "/srv/webapp" {
		t.Errorf("WorkDir = %q", c.WorkDir)
	}
	if !slices.Equal(c.ConfigFiles, []string{"/srv/webapp/compose.yaml", "/srv/webapp/compose.override.yaml"}) {
		t.Errorf("ConfigFiles = %v", c.ConfigFiles)
	}
	want := []ServiceImage{{Service: "cache", Image: "redis:7"}, {Service: "web", Image: "nginx:1.27"}}
	if !slices.Equal(c.Services, want) {
		t.Errorf("Services = %v, want %v", c.Services, want)
	}
	g := c.Git
	if g == nil || g.Branch != "main" || g.Remote != "origin" ||
		g.RemoteURL != "https://github.com/example/webapp.git" || g.CredsStripped {
		t.Errorf("GitEvidence = %+v", g)
	}
	if len(git.calls) != 1 || git.calls[0] != "/srv/webapp" {
		t.Errorf("git probes = %v, want exactly the declared workdir", git.calls)
	}
}

func TestRegistryComposeProjectProposal(t *testing.T) {
	t.Parallel()
	git := &fakeGit{} // no worktrees anywhere
	report := &docker.Report{
		Projects: []*docker.ComposeProject{
			composeProject("registryapp", "/srv/registryapp",
				[]string{"/srv/registryapp/compose.yaml"},
				composeMember("aaaa1111aaaa", "registryapp-api-1", "api", "ghcr.io/example/api:2.1", nil),
			),
		},
	}
	proposals := Proposals(context.Background(), report, git)
	p := proposals[0]
	if p.Compose.SourceMode != config.SourceRegistry {
		t.Errorf("SourceMode = %q, want registry", p.Compose.SourceMode)
	}
	if !slices.Equal(p.Compose.RegistryImages, []string{"ghcr.io/example/api:2.1"}) {
		t.Errorf("RegistryImages = %v", p.Compose.RegistryImages)
	}
	if p.Verdict != VerdictNeedsConfirmation {
		t.Fatalf("Verdict = %q, want needs_confirmation", p.Verdict)
	}
	found := false
	for _, c := range p.Confirmations {
		if c.Field == "source.mode" && strings.Contains(c.Reason, "not built locally") {
			found = true
		}
	}
	if !found {
		t.Errorf("Confirmations = %+v, want the build-vs-pull confirmation", p.Confirmations)
	}
}

func TestStandaloneReproducibleProposal(t *testing.T) {
	t.Parallel()
	detail := baseStandaloneDetail()
	detail.Mounts = []docker.Mount{
		{Type: "bind", Source: "/srv/app/data", Destination: "/data", RW: true},
		{Type: "volume", Name: "app_cache", Source: "/var/lib/docker/volumes/app_cache/_data", Destination: "/cache", RW: false},
	}
	detail.Ports = []docker.PortMapping{
		{ContainerPort: "8080", Proto: "tcp", Published: true, HostIP: "127.0.0.1", HostPort: "8080"},
		{ContainerPort: "9090", Proto: "tcp"},
	}
	detail.Networks = []docker.NetworkAttachment{{Name: "appnet"}}
	report := &docker.Report{Standalone: []*docker.Container{standaloneContainer("app", detail)}}
	proposals := Proposals(context.Background(), report, &fakeGit{})
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d", len(proposals))
	}
	p := proposals[0]
	if p.Mode != config.DeployStandalone || p.Verdict != VerdictReady {
		t.Fatalf("mode/verdict = %q / %q; confirmations=%v blockers=%v todos=%v",
			p.Mode, p.Verdict, p.Confirmations, p.Blockers, p.TODOs)
	}
	s := p.Standalone
	if !strings.HasPrefix(s.Image, "example.com/team/app@sha256:") {
		t.Errorf("Image = %q", s.Image)
	}
	wantBinds := []string{"/srv/app/data:/data", "app_cache:/cache:ro"}
	if !slices.Equal(s.Binds, wantBinds) {
		t.Errorf("Binds = %v, want %v", s.Binds, wantBinds)
	}
	wantPorts := []string{"127.0.0.1:8080:8080/tcp", "9090/tcp"}
	if !slices.Equal(s.Ports, wantPorts) {
		t.Errorf("Ports = %v, want %v", s.Ports, wantPorts)
	}
	if !slices.Equal(s.Networks, []string{"appnet"}) {
		t.Errorf("Networks = %v", s.Networks)
	}
	if s.Restart != "unless-stopped" || s.ContainerName != "app" {
		t.Errorf("restart/name = %q / %q", s.Restart, s.ContainerName)
	}
}

func TestComposeServiceWithoutImageReferenceIsConfirmed(t *testing.T) {
	t.Parallel()
	// A compose member whose inspect lost the image reference must surface
	// as a confirmation on its service — never silently disappear.
	member := composeMember("aaaa1111aaaa", "webapp-web-1", "web", "nginx:1.27", nil)
	member.Detail.ImageRef = ""
	report := &docker.Report{
		Projects: []*docker.ComposeProject{
			composeProject("webapp", "/srv/webapp", []string{"/srv/webapp/compose.yaml"}, member),
		},
	}
	p := Proposals(context.Background(), report, &fakeGit{})[0]
	if slices.ContainsFunc(p.Compose.Services, func(s ServiceImage) bool { return s.Service == "web" }) {
		t.Errorf("Services = %v, want no fabricated image entry", p.Compose.Services)
	}
	if !slices.ContainsFunc(p.Confirmations, func(c Confirmation) bool {
		return c.Field == "deploy.services" && strings.Contains(c.Reason, "service web has no image reference")
	}) {
		t.Errorf("Confirmations = %+v, want the missing-image confirmation", p.Confirmations)
	}
}

func TestStandaloneMissingRestartPolicyIsImplicitNo(t *testing.T) {
	t.Parallel()
	detail := baseStandaloneDetail()
	detail.RestartPolicy = ""
	report := &docker.Report{Standalone: []*docker.Container{standaloneContainer("app", detail)}}
	p := Proposals(context.Background(), report, &fakeGit{})[0]
	if p.Standalone.Restart != "no" {
		t.Errorf("Restart = %q, want docker's implicit default \"no\"", p.Standalone.Restart)
	}
}

func TestStandaloneNeedsConfirmationForMutabilityAndEnv(t *testing.T) {
	t.Parallel()
	detail := baseStandaloneDetail()
	detail.ImageRef = "example.com/team/app:latest"
	detail.EnvKeys = []string{"TZ", "PGPASSWORD"}
	report := &docker.Report{Standalone: []*docker.Container{standaloneContainer("app", detail)}}
	p := Proposals(context.Background(), report, &fakeGit{})[0]
	if p.Verdict != VerdictNeedsConfirmation {
		t.Fatalf("Verdict = %q, want needs_confirmation", p.Verdict)
	}
	if !slices.ContainsFunc(p.Confirmations, func(c Confirmation) bool {
		return c.Field == "standalone.image" && strings.Contains(c.Reason, "latest")
	}) {
		t.Errorf("Confirmations = %+v, want the mutable-tag confirmation", p.Confirmations)
	}
	wantTodos := []string{
		"environment variable PGPASSWORD: name suggests a secret; provide a secret_ref",
		"environment variable TZ: provide a value or secret_ref",
	}
	if !slices.Equal(p.TODOs, wantTodos) {
		t.Errorf("TODOs = %v, want %v", p.TODOs, wantTodos)
	}
	if !slices.Equal(p.Standalone.EnvKeys, []string{"TZ", "PGPASSWORD"}) {
		t.Errorf("EnvKeys = %v (names only)", p.Standalone.EnvKeys)
	}
}

func TestStandaloneUnsafeSpecsRefuseAutoImport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(d *docker.ContainerDetail)
		reason string
	}{
		{
			name:   "privileged",
			mutate: func(d *docker.ContainerDetail) { d.Privileged = true },
			reason: "privileged",
		},
		{
			name:   "host pid namespace",
			mutate: func(d *docker.ContainerDetail) { d.PidMode = "host" },
			reason: "host PID namespace",
		},
		{
			name:   "shared pid namespace",
			mutate: func(d *docker.ContainerDetail) { d.PidMode = "container:abc111" },
			reason: "shares the PID namespace of another container",
		},
		{
			name:   "host ipc namespace",
			mutate: func(d *docker.ContainerDetail) { d.IpcMode = "host" },
			reason: "host IPC namespace",
		},
		{
			name:   "shared network namespace",
			mutate: func(d *docker.ContainerDetail) { d.NetworkMode = "container:abc111" },
			reason: "network namespace",
		},
		{
			name: "anonymous volume",
			mutate: func(d *docker.ContainerDetail) {
				d.Mounts = []docker.Mount{{Type: "volume", Destination: "/data"}}
			},
			reason: "anonymous volume",
		},
		{
			name: "tmpfs mount",
			mutate: func(d *docker.ContainerDetail) {
				d.Mounts = []docker.Mount{{Type: "tmpfs", Destination: "/tmp"}}
			},
			reason: "tmpfs",
		},
		{
			name:   "unknown restart policy",
			mutate: func(d *docker.ContainerDetail) { d.RestartPolicy = "sometimes" },
			reason: "unknown restart policy",
		},
		{
			name:   "no image reference",
			mutate: func(d *docker.ContainerDetail) { d.ImageRef = "" },
			reason: "no image reference",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			detail := baseStandaloneDetail()
			tc.mutate(detail)
			report := &docker.Report{Standalone: []*docker.Container{standaloneContainer("app", detail)}}
			p := Proposals(context.Background(), report, &fakeGit{})[0]
			if p.Verdict != VerdictUnsupported {
				t.Fatalf("Verdict = %q, want unsupported", p.Verdict)
			}
			if !strings.Contains(strings.Join(p.Blockers, "\n"), tc.reason) {
				t.Errorf("Blockers = %v, want a reason containing %q", p.Blockers, tc.reason)
			}
		})
	}
}

func TestComposeManagedAndOneOffAndUnknownRefuseImport(t *testing.T) {
	t.Parallel()
	labels := map[string]string{docker.LabelProject: "proj", docker.LabelService: "web"}
	composeLabeled := standaloneContainer("stray", &docker.ContainerDetail{
		ID: "eeee5555eeee", ImageRef: "app:1", Labels: labels,
	})
	oneOff := standaloneContainer("leftover", baseStandaloneDetail())
	oneOff.OneOff = true
	unknown := standaloneContainer("ghost", nil)
	report := &docker.Report{Standalone: []*docker.Container{composeLabeled, oneOff, unknown}}
	proposals := Proposals(context.Background(), report, &fakeGit{})
	if len(proposals) != 3 {
		t.Fatalf("proposals = %d, want 3", len(proposals))
	}
	byID := map[string]*Proposal{}
	for _, p := range proposals {
		byID[p.ID] = p
	}
	for id, wantReason := range map[string]string{
		"stray":    "Compose labels",
		"leftover": "one-off",
		"ghost":    "not inspectable",
	} {
		p := byID[id]
		if p == nil || p.Verdict != VerdictUnsupported ||
			!strings.Contains(strings.Join(p.Blockers, "\n"), wantReason) {
			t.Errorf("proposal %q = %+v, want unsupported with %q", id, p, wantReason)
		}
	}
}

func TestComposeUnknownWorkdirProducesConfirmations(t *testing.T) {
	t.Parallel()
	report := &docker.Report{
		Projects: []*docker.ComposeProject{
			composeProject("mystery", "", nil,
				composeMember("aaaa1111aaaa", "mystery-web-1", "web", "nginx:1.27", nil)),
		},
	}
	p := Proposals(context.Background(), report, &fakeGit{})[0]
	if p.Verdict != VerdictNeedsConfirmation {
		t.Fatalf("Verdict = %q, want needs_confirmation", p.Verdict)
	}
	fields := map[string]bool{}
	for _, c := range p.Confirmations {
		fields[c.Field] = true
	}
	for _, want := range []string{"source.mode", "deploy.compose.work_dir", "deploy.compose.files"} {
		if !fields[want] {
			t.Errorf("missing confirmation for %s in %+v", want, p.Confirmations)
		}
	}
	if p.Compose.SourceMode != "" {
		t.Errorf("SourceMode = %q, want empty (undetermined, never guessed)", p.Compose.SourceMode)
	}
}

func TestNilProberAndProbeFailureAreExplicit(t *testing.T) {
	t.Parallel()
	mkReport := func() *docker.Report {
		return &docker.Report{
			Projects: []*docker.ComposeProject{
				composeProject("webapp", "/srv/webapp",
					[]string{"/srv/webapp/compose.yaml"},
					composeMember("aaaa1111aaaa", "webapp-web-1", "web", "nginx:1.27", nil)),
			},
		}
	}
	p := Proposals(context.Background(), mkReport(), nil)[0]
	if !slices.ContainsFunc(p.Confirmations, func(c Confirmation) bool {
		return c.Field == "source.mode" && strings.Contains(c.Reason, "git detection unavailable")
	}) {
		t.Errorf("nil prober: Confirmations = %+v", p.Confirmations)
	}
	p = Proposals(context.Background(), mkReport(), &fakeGit{err: errors.New("git exploded")})[0]
	if !slices.ContainsFunc(p.Confirmations, func(c Confirmation) bool {
		return c.Field == "source.mode" && strings.Contains(c.Reason, "git detection failed")
	}) {
		t.Errorf("failing prober: Confirmations = %+v", p.Confirmations)
	}
}

func TestDetachedHeadConfirmsBranch(t *testing.T) {
	t.Parallel()
	git := &fakeGit{byDir: map[string]GitInfo{
		"/srv/webapp": {InWorkTree: true, Detached: true},
	}}
	report := &docker.Report{
		Projects: []*docker.ComposeProject{
			composeProject("webapp", "/srv/webapp",
				[]string{"/srv/webapp/compose.yaml"},
				composeMember("aaaa1111aaaa", "webapp-web-1", "web", "nginx:1.27", nil)),
		},
	}
	p := Proposals(context.Background(), report, git)[0]
	if !slices.ContainsFunc(p.Confirmations, func(c Confirmation) bool {
		return c.Field == "source.git.branch" && strings.Contains(c.Reason, "detached")
	}) {
		t.Errorf("Confirmations = %+v, want detached-branch confirmation", p.Confirmations)
	}
	if p.Compose.Git == nil || !p.Compose.Git.Detached {
		t.Errorf("GitEvidence = %+v, want Detached", p.Compose.Git)
	}
}

// --- determinism, IDs, system candidates -----------------------------------

func TestProposalsDeterministicAcrossShuffledInput(t *testing.T) {
	t.Parallel()
	build := func(order int) *docker.Report {
		projects := []*docker.ComposeProject{
			composeProject("webapp", "/srv/webapp",
				[]string{"/srv/webapp/compose.yaml"},
				composeMember("aaaa1111aaaa", "webapp-web-1", "web", "nginx:1.27", nil)),
			composeProject("registryapp", "/srv/registryapp",
				[]string{"/srv/registryapp/compose.yaml"},
				composeMember("bbbb2222bbbb", "registryapp-api-1", "api", "ghcr.io/example/api:2.1", nil)),
		}
		standalone := []*docker.Container{
			standaloneContainer("app", baseStandaloneDetail()),
			standaloneContainer("My_Old-App", baseStandaloneDetail()),
		}
		if order == 1 {
			projects[0], projects[1] = projects[1], projects[0]
			standalone[0], standalone[1] = standalone[1], standalone[0]
		}
		return &docker.Report{Projects: projects, Standalone: standalone}
	}
	git := func() *fakeGit {
		return &fakeGit{byDir: map[string]GitInfo{
			"/srv/webapp": worktree("main", "https://github.com/example/webapp.git"),
		}}
	}
	a := Proposals(context.Background(), build(0), git())
	b := Proposals(context.Background(), build(1), git())
	if !reflect.DeepEqual(a, b) {
		t.Error("classification must be deterministic regardless of input order")
	}
	ids := []string{}
	for _, p := range a {
		ids = append(ids, p.ID)
	}
	if !slices.IsSorted(ids) {
		t.Errorf("proposal IDs = %v, want sorted output", ids)
	}
}

func TestProposalIDCollisionIsDisambiguatedDeterministically(t *testing.T) {
	t.Parallel()
	// "My_Old-App" and "my-old-app" sanitize to the same slug.
	report := &docker.Report{Standalone: []*docker.Container{
		standaloneContainer("My_Old-App", baseStandaloneDetail()),
		standaloneContainer("my-old-app", baseStandaloneDetail()),
	}}
	p := Proposals(context.Background(), report, &fakeGit{})
	if len(p) != 2 || p[0].ID == p[1].ID {
		t.Fatalf("IDs = %v / %v, want unique", p[0].ID, p[1].ID)
	}
	if p[0].ID != "my-old-app" || p[1].ID != "my-old-app-2" {
		t.Errorf("IDs = %q, %q; want deterministic base + -2 suffix", p[0].ID, p[1].ID)
	}
}

func TestSlugRules(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"webapp", "webapp"},
		{"My App.Great", "my-app-great"},
		{"_weird--name__", "weird-name"},
		{"42 numbers!", "42-numbers"},
		{"", "app"},
		{"---", "app"},
		// Non-ASCII runes become dashes; surviving ASCII letters remain.
		{"Ünïcode", "n-code"},
		{strings.Repeat("a", 100) + "-tail", strings.Repeat("a", 63)},
	}
	for _, tc := range tests {
		if got := slug(tc.in, appIDMaxLen); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSystemCandidatesFlaggedNotFiltered(t *testing.T) {
	t.Parallel()
	infra := standaloneContainer("portainer", baseStandaloneDetail())
	infra.SystemCandidate = true
	infra.SystemReasons = []string{"infrastructure image: portainer management UI"}
	report := &docker.Report{Standalone: []*docker.Container{infra}}
	p := Proposals(context.Background(), report, &fakeGit{})
	if len(p) != 1 {
		t.Fatalf("proposals = %d, want the flagged candidate to still be present", len(p))
	}
	if !p[0].SystemCandidate || len(p[0].SystemReasons) == 0 {
		t.Errorf("SystemCandidate/Reasons = %v / %v", p[0].SystemCandidate, p[0].SystemReasons)
	}
}

func TestNilReportYieldsNil(t *testing.T) {
	t.Parallel()
	if got := Proposals(context.Background(), nil, &fakeGit{}); got != nil {
		t.Errorf("Proposals(nil) = %v, want nil", got)
	}
}

// Compile-time shape guard: proposals never carry env values because learn
// never receives them; assert the string form of a fully-populated proposal
// stays free of planted values from the discovery layer.
func TestProposalStringFormsCarryNoEnvValues(t *testing.T) {
	t.Parallel()
	detail := baseStandaloneDetail()
	detail.EnvKeys = []string{"DATABASE_PASSWORD"}
	report := &docker.Report{Standalone: []*docker.Container{standaloneContainer("app", detail)}}
	p := Proposals(context.Background(), report, &fakeGit{})[0]
	blob := fmt.Sprintf("%+v", p)
	if strings.Contains(blob, "=") && strings.Contains(blob, "DATABASE_PASSWORD=") {
		t.Error("an environment value shape leaked into the proposal")
	}
}
