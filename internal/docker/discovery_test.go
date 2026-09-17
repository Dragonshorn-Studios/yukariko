package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeClient is the in-memory Client used by discovery tests. It records
// every call so tests can assert that discovery issues read operations only.
type fakeClient struct {
	summaries []ContainerSummary
	details   map[string]ContainerDetail
	failIDs   map[string]error
	calls     []string
}

func (f *fakeClient) ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	f.calls = append(f.calls, fmt.Sprintf("ListContainers all=%v", all))
	out := make([]ContainerSummary, len(f.summaries))
	copy(out, f.summaries)
	return out, nil
}

func (f *fakeClient) InspectContainer(ctx context.Context, id string) (ContainerDetail, error) {
	f.calls = append(f.calls, "InspectContainer "+id)
	if err, ok := f.failIDs[id]; ok {
		return ContainerDetail{}, err
	}
	if d, ok := f.details[id]; ok {
		return d, nil
	}
	return ContainerDetail{}, &Error{Code: CodeContainerMissing, Detail: "not faked", cause: ErrContainerMissing}
}

// assertReadVerbsOnly fails if anything but the two read methods was called.
func assertReadVerbsOnly(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		verb, _, _ := strings.Cut(c, " ")
		switch verb {
		case "ListContainers", "InspectContainer":
		default:
			t.Errorf("non-read client call recorded: %s", c)
		}
	}
}

// smallInspect renders a minimal but realistic inspect document.
func smallInspect(id, name, state string, labels map[string]string) string {
	lbl, err := json.Marshal(labels)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(
		`[{"Id":%q,"Name":"/%s","Created":"2026-09-01T10:00:00Z","Image":"sha256:%s",`+
			`"Config":{"Image":"app:1.0","Env":["TZ=UTC"],"Labels":%s},`+
			`"State":{"Status":%q,"Running":%t},`+
			`"HostConfig":{"RestartPolicy":{"Name":"no"}},"Mounts":[],"NetworkSettings":{"Ports":{},"Networks":{}}}]`,
		id, name, strings.Repeat("2", 64), lbl, state, state == "running")
}

// composeInspect renders a Compose member with the standard labels; empty
// workdir/files/number are omitted to exercise partial-label cases.
func composeInspect(id, name, project, service, workdir, files, number, state string) string {
	labels := map[string]string{LabelProject: project, LabelService: service}
	if workdir != "" {
		labels[LabelWorkDir] = workdir
	}
	if files != "" {
		labels[LabelConfigFiles] = files
	}
	if number != "" {
		labels[LabelContainerNumber] = number
	}
	labels[LabelOneOff] = "False"
	return smallInspect(id, name, state, labels)
}

func summary(id, name, image, state string) ContainerSummary {
	return ContainerSummary{ID: id, Name: name, Image: image, State: state}
}

// mainFixture is the shared inventory: a two-service Compose project, a
// system container, and a stopped standalone container.
func mainFixture(t *testing.T) *fakeClient {
	t.Helper()
	return &fakeClient{
		summaries: []ContainerSummary{
			summary("aaaa1111aaaa", "webapp-web-1", "nginx:1.27", "running"),
			summary("bbbb2222bbbb", "webapp-cache-1", "redis:7", "running"),
			summary("cccc3333cccc", "portainer", "portainer/portainer-ce:latest", "running"),
			summary("dddd4444dddd", "backup-shell", "alpine:3.20", "exited"),
		},
		details: map[string]ContainerDetail{
			"aaaa1111aaaa": mustParseInspect(t, webInspectJSON),
			"bbbb2222bbbb": mustParseInspect(t, composeInspect(
				"bbbb2222bbbb", "webapp-cache-1", "webapp", "cache",
				"/srv/webapp", "/srv/webapp/compose.yaml,/srv/webapp/compose.override.yaml", "1", "running")),
			"cccc3333cccc": mustParseInspect(t, smallInspect("cccc3333cccc", "portainer", "running", nil)),
			"dddd4444dddd": mustParseInspect(t, smallInspect("dddd4444dddd", "backup-shell", "exited", nil)),
		},
	}
}

func TestDiscoverGroupsComposeProject(t *testing.T) {
	t.Parallel()
	client := mainFixture(t)
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	assertReadVerbsOnly(t, client.calls)
	if client.calls[0] != "ListContainers all=true" {
		t.Errorf("first call = %q, want the full inventory (running and stopped)", client.calls[0])
	}
	if len(report.Projects) != 1 {
		t.Fatalf("Projects = %d, want exactly one group for the Compose project", len(report.Projects))
	}
	p := report.Projects[0]
	if p.Name != "webapp" {
		t.Errorf("project name = %q", p.Name)
	}
	if p.WorkDir != "/srv/webapp" {
		t.Errorf("WorkDir = %q", p.WorkDir)
	}
	wantFiles := []string{"/srv/webapp/compose.yaml", "/srv/webapp/compose.override.yaml"}
	if !slices.Equal(p.ConfigFiles, wantFiles) {
		t.Errorf("ConfigFiles = %v, want %v in compose's own order", p.ConfigFiles, wantFiles)
	}
	if !slices.Equal(p.Services, []string{"cache", "web"}) {
		t.Errorf("Services = %v, want [cache web]", p.Services)
	}
	if len(p.Containers) != 2 {
		t.Fatalf("project members = %d, want 2", len(p.Containers))
	}
	if len(report.Standalone) != 2 {
		t.Fatalf("Standalone = %d, want 2 (compose members must not leak in)", len(report.Standalone))
	}
	if got := standaloneNames(report); !slices.Equal(got, []string{"backup-shell", "portainer"}) {
		t.Errorf("Standalone names = %v", got)
	}
	// Both running and stopped containers are returned (acceptance criterion).
	shell := report.Standalone[0]
	if shell.Name != "backup-shell" || shell.State != "exited" {
		t.Errorf("stopped container = %+v, want present with its state", shell)
	}
	web := p.Containers[1]
	if web.Service != "web" || web.Project != "webapp" || web.ContainerNumber != 1 || web.OneOff {
		t.Errorf("web member = %+v", web.ContainerSummary)
	}
	if web.Detail == nil || !web.Detail.Health.Configured || web.Detail.Health.Status != "healthy" {
		t.Errorf("web detail = %+v", web.Detail)
	}
	if len(p.Anomalies) != 0 {
		t.Errorf("clean project must carry no anomalies, got %v", p.Anomalies)
	}
}

func TestDiscoverComposeReplicas(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		summaries: []ContainerSummary{
			summary("aaaa1111aaaa", "webapp-web-1", "nginx:1.27", "running"),
			summary("aaaa1111bbbb", "webapp-web-2", "nginx:1.27", "running"),
			summary("bbbb2222bbbb", "webapp-cache-1", "redis:7", "running"),
		},
		details: map[string]ContainerDetail{
			"aaaa1111aaaa": mustParseInspect(t, composeInspect("aaaa1111aaaa", "webapp-web-1", "webapp", "web", "/srv/webapp", "/srv/webapp/compose.yaml", "1", "running")),
			"aaaa1111bbbb": mustParseInspect(t, composeInspect("aaaa1111bbbb", "webapp-web-2", "webapp", "web", "/srv/webapp", "/srv/webapp/compose.yaml", "2", "running")),
			"bbbb2222bbbb": mustParseInspect(t, composeInspect("bbbb2222bbbb", "webapp-cache-1", "webapp", "cache", "/srv/webapp", "/srv/webapp/compose.yaml", "1", "running")),
		},
	}
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(report.Projects) != 1 {
		t.Fatalf("Projects = %d, want one group regardless of replica count", len(report.Projects))
	}
	p := report.Projects[0]
	if len(p.Containers) != 3 {
		t.Errorf("members = %d, want 3", len(p.Containers))
	}
	nums := []int{}
	for _, c := range p.Containers {
		if c.Service == "web" {
			nums = append(nums, c.ContainerNumber)
		}
	}
	if !slices.Equal(nums, []int{1, 2}) {
		t.Errorf("web replica numbers = %v, want [1 2]", nums)
	}
}

func TestDiscoverConflictingWorkDir(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		summaries: []ContainerSummary{
			summary("aaaa1111aaaa", "mix-a-1", "app:1", "running"),
			summary("bbbb2222bbbb", "mix-b-1", "app:1", "running"),
		},
		details: map[string]ContainerDetail{
			"aaaa1111aaaa": mustParseInspect(t, composeInspect("aaaa1111aaaa", "mix-a-1", "mixed", "a", "/srv/one", "/srv/one/compose.yaml", "1", "running")),
			"bbbb2222bbbb": mustParseInspect(t, composeInspect("bbbb2222bbbb", "mix-b-1", "mixed", "b", "/srv/two", "/srv/two/compose.yaml", "1", "running")),
		},
	}
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	p := report.Projects[0]
	if p.WorkDir != "" {
		t.Errorf("WorkDir = %q, want empty on conflict (never a fabricated majority value)", p.WorkDir)
	}
	if p.ConfigFiles != nil {
		t.Errorf("ConfigFiles = %v, want nil on conflict", p.ConfigFiles)
	}
	joined := strings.Join(p.Anomalies, "\n")
	if !strings.Contains(joined, "/srv/one") || !strings.Contains(joined, "/srv/two") {
		t.Errorf("Anomalies = %v, want both conflicting values named", p.Anomalies)
	}
}

func TestDiscoverPartialLabelCoverage(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		summaries: []ContainerSummary{
			summary("aaaa1111aaaa", "part-a-1", "app:1", "running"),
			summary("bbbb2222bbbb", "part-b-1", "app:1", "running"),
			summary("cccc3333cccc", "part-c-1", "app:1", "running"),
		},
		details: map[string]ContainerDetail{
			"aaaa1111aaaa": mustParseInspect(t, composeInspect("aaaa1111aaaa", "part-a-1", "partial", "a", "/srv/x", "", "", "running")),
			"bbbb2222bbbb": mustParseInspect(t, composeInspect("bbbb2222bbbb", "part-b-1", "partial", "b", "/srv/x", "", "", "running")),
			"cccc3333cccc": mustParseInspect(t, composeInspect("cccc3333cccc", "part-c-1", "partial", "c", "", "", "", "running")),
		},
	}
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	p := report.Projects[0]
	if p.WorkDir != "" {
		t.Errorf("WorkDir = %q, want empty under partial coverage", p.WorkDir)
	}
	found := false
	for _, a := range p.Anomalies {
		if strings.Contains(a, "missing on 1 of 3 containers") {
			found = true
		}
	}
	if !found {
		t.Errorf("Anomalies = %v, want explicit partial-coverage anomaly", p.Anomalies)
	}
}

func TestDiscoverMissingLabelsEverywhere(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		summaries: []ContainerSummary{
			summary("aaaa1111aaaa", "nc-a-1", "app:1", "running"),
			summary("bbbb2222bbbb", "nc-b-1", "app:1", "running"),
		},
		details: map[string]ContainerDetail{
			"aaaa1111aaaa": mustParseInspect(t, composeInspect("aaaa1111aaaa", "nc-a-1", "nocfg", "a", "/srv/y", "", "", "running")),
			"bbbb2222bbbb": mustParseInspect(t, composeInspect("bbbb2222bbbb", "nc-b-1", "nocfg", "b", "/srv/y", "", "", "running")),
		},
	}
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	p := report.Projects[0]
	if p.WorkDir != "/srv/y" {
		t.Errorf("WorkDir = %q, want the consistent value", p.WorkDir)
	}
	if p.ConfigFiles != nil {
		t.Errorf("ConfigFiles = %v, want nil", p.ConfigFiles)
	}
	found := false
	for _, a := range p.Anomalies {
		if strings.Contains(a, "config files") && strings.Contains(a, "missing from every container") {
			found = true
		}
	}
	if !found {
		t.Errorf("Anomalies = %v, want a missing-everywhere anomaly", p.Anomalies)
	}
}

func TestDiscoverMalformedAndUnpairableLabels(t *testing.T) {
	t.Parallel()
	// Containers with some Compose labels but no project/service pair stay
	// standalone with explicit anomalies — never guessed into a group.
	client := &fakeClient{
		summaries: []ContainerSummary{
			summary("bbbb2222bbbb", "svc-only", "app:1", "running"),
			summary("cccc3333cccc", "wd-only", "app:1", "running"),
		},
		details: map[string]ContainerDetail{
			"bbbb2222bbbb": mustParseInspect(t, smallInspect("bbbb2222bbbb", "svc-only", "running", map[string]string{LabelService: "web"})),
			"cccc3333cccc": mustParseInspect(t, smallInspect("cccc3333cccc", "wd-only", "running", map[string]string{LabelWorkDir: "/srv/z"})),
		},
	}
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(report.Projects) != 0 {
		t.Fatalf("Projects = %d, want 0: unpairable containers are never guessed into groups", len(report.Projects))
	}
	got := standaloneNames(report)
	if !slices.Equal(got, []string{"svc-only", "wd-only"}) {
		t.Fatalf("Standalone names = %v, want the two unpairable containers", got)
	}
	byName := map[string]*Container{}
	for _, c := range report.Standalone {
		byName[c.Name] = c
	}
	svcOnly := byName["svc-only"]
	if !strings.Contains(strings.Join(svcOnly.Anomalies, "\n"), LabelProject) {
		t.Errorf("svc-only anomalies = %v, want missing %s named", svcOnly.Anomalies, LabelProject)
	}
	wdOnly := byName["wd-only"]
	if !strings.Contains(strings.Join(wdOnly.Anomalies, "\n"), "project and service are both missing") {
		t.Errorf("wd-only anomalies = %v, want the both-missing anomaly", wdOnly.Anomalies)
	}
	// A malformed container-number label does not block grouping: the member
	// is grouped and the malformed value is recorded as an anomaly.
	client2 := &fakeClient{
		summaries: []ContainerSummary{summary("aaaa1111aaaa", "odd-one-1", "app:1", "running")},
		details: map[string]ContainerDetail{
			"aaaa1111aaaa": mustParseInspect(t, composeInspect("aaaa1111aaaa", "odd-one-1", "odd", "one", "/srv/odd", "/srv/odd/compose.yaml", "seven", "running")),
		},
	}
	report2, err := Discover(context.Background(), client2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(report2.Projects) != 1 {
		t.Fatalf("Projects = %d, want the single odd project", len(report2.Projects))
	}
	member := report2.Projects[0].Containers[0]
	if member.ContainerNumber != 0 {
		t.Errorf("ContainerNumber = %d, want 0 for a malformed label", member.ContainerNumber)
	}
	if !strings.Contains(strings.Join(member.Anomalies, "\n"), "malformed "+LabelContainerNumber) {
		t.Errorf("member anomalies = %v, want malformed-number anomaly", member.Anomalies)
	}
}

func TestDiscoverInspectFailureIsNotFatal(t *testing.T) {
	t.Parallel()
	client := mainFixture(t)
	client.failIDs = map[string]error{
		"dddd4444dddd": &Error{Code: CodeDaemonUnreachable, Detail: "transient", cause: ErrDaemonUnreachable},
	}
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v — one failed inspection must not abort the pass", err)
	}
	var shell *Container
	for _, c := range report.Standalone {
		if c.Name == "backup-shell" {
			shell = c
		}
	}
	if shell == nil {
		t.Fatal("backup-shell missing from the report")
	}
	if shell.Detail != nil {
		t.Error("Detail must stay nil when inspection failed")
	}
	if !strings.Contains(strings.Join(shell.Anomalies, "\n"), "inspect failed") {
		t.Errorf("Anomalies = %v, want an inspect-failed anomaly", shell.Anomalies)
	}
	// The rest of the inventory is unaffected.
	if len(report.Projects) != 1 || len(report.Standalone) != 2 {
		t.Errorf("projects=%d standalone=%d, want the full inventory", len(report.Projects), len(report.Standalone))
	}
}

func TestDiscoverSystemCandidates(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		summaries: []ContainerSummary{
			summary("aaaa1111aaaa", "portainer", "portainer/portainer-ce:latest", "running"),
			summary("bbbb2222bbbb", "watchtower", "containrrr/watchtower:latest", "running"),
			summary("cccc3333cccc", "yukariko-yukariko-1", "ghcr.io/dragonshorn-studios/yukariko:dev", "running"),
			summary("dddd4444dddd", "site", "nginx:1.27", "running"),
		},
		details: map[string]ContainerDetail{
			"aaaa1111aaaa": mustParseInspect(t, smallInspect("aaaa1111aaaa", "portainer", "running", nil)),
			"bbbb2222bbbb": mustParseInspect(t, smallInspect("bbbb2222bbbb", "watchtower", "running", nil)),
			"cccc3333cccc": mustParseInspect(t, composeInspect("cccc3333cccc", "yukariko-yukariko-1", "yukariko", "yukariko", "/srv/yukariko", "/srv/yukariko/compose.yaml", "1", "running")),
			"dddd4444dddd": mustParseInspect(t, smallInspect("dddd4444dddd", "site", "running", nil)),
		},
	}
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byName := map[string]*Container{}
	for _, c := range report.Standalone {
		byName[c.Name] = c
	}
	portainer := byName["portainer"]
	if !portainer.SystemCandidate ||
		!slices.Contains(portainer.SystemReasons, "infrastructure image: portainer management UI") {
		t.Errorf("portainer flags = %v %v", portainer.SystemCandidate, portainer.SystemReasons)
	}
	if !byName["watchtower"].SystemCandidate {
		t.Error("watchtower must be flagged as infrastructure")
	}
	if byName["site"].SystemCandidate {
		t.Errorf("plain user app flagged: %+v", byName["site"].SystemReasons)
	}
	var yuk *ComposeProject
	for _, p := range report.Projects {
		if p.Name == "yukariko" {
			yuk = p
		}
	}
	if yuk == nil || !yuk.SystemCandidate {
		t.Fatal("the yukariko project must be flagged as a system candidate")
	}
	if !slices.Contains(yuk.SystemReasons, "container name is yukariko or yukariko-prefixed") {
		t.Errorf("project reasons = %v", yuk.SystemReasons)
	}
	if !yuk.Containers[0].SystemCandidate {
		t.Error("member containers of a system project are flagged too")
	}
}

// TestDiscoverSelfIDFlag covers the heuristic that recognizes Yukariko
// running inside its own container (host name = short container ID).
func TestDiscoverSelfIDFlag(t *testing.T) {
	orig := selfID
	selfID = func() string { return "aaaa1111aaaa" }
	t.Cleanup(func() { selfID = orig })

	client := mainFixture(t)
	report, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var web *Container
	for _, p := range report.Projects {
		for _, c := range p.Containers {
			if c.ID == "aaaa1111aaaa" {
				web = c
			}
		}
	}
	if web == nil || !web.SystemCandidate {
		t.Fatal("the container whose ID matches the host name must be flagged")
	}
	if !slices.Contains(web.SystemReasons, "host name matches this container's ID: yukariko runs inside it") {
		t.Errorf("reasons = %v", web.SystemReasons)
	}
}

func TestDiscoverDeterministic(t *testing.T) {
	t.Parallel()
	forward := mainFixture(t)
	backward := &fakeClient{
		summaries: reverse(forward.summaries),
		details:   forward.details,
	}
	ra, err := Discover(context.Background(), forward)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	rb, err := Discover(context.Background(), backward)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	ra.GeneratedAt, rb.GeneratedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(ra, rb) {
		t.Error("discovery must be deterministic regardless of daemon listing order")
	}
}

func TestDiscoverCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := mainFixture(t)
	report, err := Discover(ctx, client)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if report != nil {
		t.Error("a cancelled pass must not return a partial report")
	}
}

func standaloneNames(r *Report) []string {
	out := make([]string, 0, len(r.Standalone))
	for _, c := range r.Standalone {
		out = append(out, c.Name)
	}
	return out
}

func reverse(in []ContainerSummary) []ContainerSummary {
	out := make([]ContainerSummary, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}
