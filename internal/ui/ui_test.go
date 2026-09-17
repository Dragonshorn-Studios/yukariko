package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

func uiServer(t *testing.T, now time.Time) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{SchemaVersion: 1, Apps: []config.App{{
		ID: "web",
		Source: config.Source{
			Mode:     config.SourceRegistry,
			Registry: &config.RegistrySource{Images: []config.ImageRef{{Ref: "app:1"}}},
		},
		Deploy: config.Deploy{
			Mode:       config.DeployStandalone,
			Standalone: &config.StandaloneSpec{Image: "app:1", Name: "web-1"},
		},
	}}}
	// Seed: one succeeded deployment, one observation, healthy HTTP, and two
	// remote peers whose report age makes them stale and offline.
	depID, err := st.BeginDeployment(context.Background(), store.BeginDeploymentParams{AppID: "web", Cause: "test", At: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitDeploymentSuccess(context.Background(), "web", "digest", "docker.io/library/app:1@sha256:abcd", depID, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(context.Background(), "web", "digest:docker.io/library/app:1", "sha256:abcd", now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHealthSample(context.Background(), "web", "http", "healthy", "status 200", nil, now); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchRemoteHost(context.Background(), "peer-stale", now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchRemoteHost(context.Background(), "peer-offline", now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Config: cfg, Now: func() time.Time { return now }}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func readAll(resp *http.Response) string {
	var b strings.Builder
	buf := make([]byte, 1<<16)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

func TestDashboardRoutesRender(t *testing.T) {
	t.Parallel()
	_, srv := uiServer(t, time.Now())
	for _, route := range []string{"/ui", "/ui/vestments", "/ui/chronicle", "/ui/divination"} {
		resp, err := http.Get(srv.URL + route)
		if err != nil {
			t.Fatalf("%s: %v", route, err)
		}
		body := readAll(resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", route, resp.StatusCode)
		}
		if !strings.Contains(body, "Yukariko") {
			t.Errorf("%s body missing the layout", route)
		}
	}
}

func TestDashboardGoldenFragments(t *testing.T) {
	t.Parallel()
	_, srv := uiServer(t, time.Now())
	get := func(path string) string {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return readAll(resp)
	}
	sanctuary := get("/ui")
	for _, want := range []string{"up-to-date", "peer-stale", "offline"} {
		if !strings.Contains(sanctuary, want) {
			t.Errorf("sanctuary missing %q", want)
		}
	}
	vestments := get("/ui/vestments")
	for _, want := range []string{"app:1", "sha256:abcd", "current"} {
		if !strings.Contains(vestments, want) {
			t.Errorf("vestments missing %q", want)
		}
	}
	if !strings.Contains(get("/ui/divination"), "healthy") {
		t.Error("divination missing the health reading")
	}
	if !strings.Contains(get("/ui/chronicle"), "succeeded") {
		t.Error("chronicle missing the deployment record")
	}
}

func TestDashboardNoScriptAndReadOnly(t *testing.T) {
	t.Parallel()
	_, srv := uiServer(t, time.Now())
	for _, route := range []string{"/ui", "/ui/vestments", "/ui/chronicle", "/ui/divination"} {
		resp, err := http.Get(srv.URL + route)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.ToLower(readAll(resp))
		resp.Body.Close()
		if strings.Contains(body, "<script") {
			t.Errorf("%s contains JavaScript; the dashboard must work with JS disabled", route)
		}
		if strings.Contains(body, "<form") {
			t.Errorf("%s contains a form; the dashboard is read-only", route)
		}
	}
	resp, err := http.Post(srv.URL+"/ui", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /ui = %d, want 405", resp.StatusCode)
	}
}

func TestDashboardAccessibilitySmoke(t *testing.T) {
	t.Parallel()
	_, srv := uiServer(t, time.Now())
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	html := strings.ToLower(readAll(resp))
	for _, want := range []string{
		`<html lang="en">`,
		`<main id="main">`,
		`aria-label="sections"`,
		`class="skip"`,
		`<th scope="col">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("accessibility smoke missing %q", want)
		}
	}
}

func TestAssetInventoryDocumented(t *testing.T) {
	t.Parallel()
	entries, err := filepath.Glob("static/*")
	if err != nil || len(entries) == 0 {
		t.Fatalf("static assets = %v", entries)
	}
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "docs/ASSETS.md") {
			t.Errorf("%s lacks the provenance reference", path)
		}
	}
	if _, err := os.Stat(filepath.Join("..", "..", "docs", "ASSETS.md")); err != nil {
		t.Error("docs/ASSETS.md must exist and inventory the assets")
	}
}
