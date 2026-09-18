package learn

import (
	"context"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

// scanClient fakes one daemon for endpoint-scan tests.
type scanClient struct {
	summaries []docker.ContainerSummary
	details   map[string]docker.ContainerDetail
}

func (c *scanClient) ListContainers(context.Context, bool) ([]docker.ContainerSummary, error) {
	return c.summaries, nil
}

func (c *scanClient) InspectContainer(_ context.Context, id string) (docker.ContainerDetail, error) {
	if d, ok := c.details[id]; ok {
		return d, nil
	}
	return docker.ContainerDetail{Name: id}, nil
}

func running(name, image string) docker.ContainerSummary {
	return docker.ContainerSummary{ID: "id-" + name, Name: name, Image: image, State: "running"}
}

// Non-default daemons stamp their resolved host onto every candidate from
// that daemon, and ID collisions across daemons are disambiguated.
func TestScanStampsEndpointsAndDisambiguates(t *testing.T) {
	t.Parallel()
	def := &scanClient{summaries: []docker.ContainerSummary{running("web", "nginx:1")}}
	rootless := &scanClient{summaries: []docker.ContainerSummary{
		running("web", "nginx:1"),
		running("kuma", "louislam/uptime-kuma:1"),
	}}
	f := &flow{opts: FlowOptions{
		Client: def,
		Stdout: &strings.Builder{},
		Endpoints: []EndpointScan{{
			Endpoint: docker.Endpoint{Name: "rootless", Host: "unix:///run/user/1000/docker.sock"},
			Client:   rootless,
		}},
	}}
	proposals, err := f.scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Proposal{}
	for _, p := range proposals {
		byID[p.ID] = p
	}
	if p := byID["web"]; p == nil || p.Docker != nil {
		t.Fatalf("default-daemon web = %+v, want no endpoint", p)
	}
	rootlessWeb := byID["web-rootless"]
	if rootlessWeb == nil {
		t.Fatalf("collision not disambiguated; proposals = %v", idsOf(proposals))
	}
	if rootlessWeb.Docker == nil || rootlessWeb.Docker.Host != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("web-rootless endpoint = %+v", rootlessWeb.Docker)
	}
	if p := byID["kuma"]; p == nil || p.Docker == nil || p.Docker.Host != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("kuma endpoint = %+v", byID["kuma"])
	}
}

// A failing additional endpoint is skipped without blocking the default
// daemon's scan.
func TestScanSkipsUnreachableEndpoint(t *testing.T) {
	t.Parallel()
	def := &scanClient{summaries: []docker.ContainerSummary{running("web", "nginx:1")}}
	out := &strings.Builder{}
	f := &flow{opts: FlowOptions{
		Client: def,
		Stdout: out,
		Endpoints: []EndpointScan{{
			Endpoint: docker.Endpoint{Name: "down", Host: "unix:///run/user/42/docker.sock"},
			Client:   &errClient{},
		}},
	}}
	proposals, err := f.scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || proposals[0].ID != "web" {
		t.Fatalf("proposals = %v", idsOf(proposals))
	}
	if !strings.Contains(out.String(), `skipping docker endpoint "down"`) {
		t.Fatalf("no skip note; output = %q", out.String())
	}
}

// The endpoint is learn-owned discovered reality: a re-learn replaces a
// hand-set endpoint on an imported app rather than preserving it.
func TestMergeReplacesEndpoint(t *testing.T) {
	t.Parallel()
	original := []byte(`schema_version: 1
apps:
  - id: web
    docker: {context: hand-set}
    source:
      mode: registry
      registry: {images: [{ref: nginx:1}]}
    deploy:
      mode: standalone
      standalone: {image: nginx:1, name: web, health_check: {test: [NONE]}}
`)
	f := &flow{opts: FlowOptions{ConfigPath: "/tmp/unused"}}
	apps := []config.App{{
		ID:     "web",
		Docker: &config.DockerEndpoint{Host: "unix:///run/user/1000/docker.sock"},
		Source: config.Source{Mode: config.SourceRegistry, Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "nginx:1"}}}},
		Deploy: config.Deploy{Mode: config.DeployStandalone, Standalone: &config.StandaloneSpec{
			Image: "nginx:1", Name: "web", HealthCheck: &config.ContainerHealthCheck{Test: []string{"NONE"}},
		}},
	}}
	merged, err := f.merge(original, true, apps)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(merged), "hand-set") {
		t.Fatal("hand-set endpoint survived the merge; docker is learn-owned")
	}
	if !strings.Contains(string(merged), "unix:///run/user/1000/docker.sock") {
		t.Fatalf("discovered endpoint missing from merge:\n%s", merged)
	}
	if _, err := config.Parse(merged); err != nil {
		t.Fatalf("merged document invalid: %v", err)
	}
}

type errClient struct{}

func (*errClient) ListContainers(context.Context, bool) ([]docker.ContainerSummary, error) {
	return nil, docker.ErrDaemonUnreachable
}

func (*errClient) InspectContainer(context.Context, string) (docker.ContainerDetail, error) {
	return docker.ContainerDetail{}, docker.ErrDaemonUnreachable
}

func idsOf(ps []*Proposal) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}
