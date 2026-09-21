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
	if _, err := st.RecordEvent(context.Background(), store.Event{
		Time:    now.Add(-30 * time.Second),
		AppID:   "web",
		Level:   store.LevelInfo,
		Kind:    "health",
		Message: "http healthy",
	}); err != nil {
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
	for _, want := range []string{"Synced", "Healthy", "peer-stale", "offline", "Chronicle", "http healthy"} {
		if !strings.Contains(sanctuary, want) {
			t.Errorf("status overview missing %q", want)
		}
	}
	if strings.Contains(sanctuary, "Sanctuary") {
		t.Error("status overview still shows liturgical Sanctuary chrome")
	}
	vestments := get("/ui/vestments")
	for _, want := range []string{"app:1", "sha256:abcd", "Pending update"} {
		if !strings.Contains(vestments, want) {
			t.Errorf("versions missing %q", want)
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
		`aria-current="page"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("accessibility smoke missing %q", want)
		}
	}
	resp2, err := http.Get(srv.URL + "/ui/vestments")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if !strings.Contains(strings.ToLower(readAll(resp2)), `<th scope="col">`) {
		t.Error("versions table missing column headers")
	}
}

func TestLightLapisLock(t *testing.T) {
	t.Parallel()
	css, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	text := string(css)
	for _, tok := range []string{
		"--canvas: #F7F3EB",
		"--surface: #FFFBF5",
		"--ink: #1A1A18",
		"--lapis: #2E5EA8",
		"--mirage: #A8C4E8",
		"--rose-gold: #B8956C",
		"--ok: #2F6F5E",
		"--fail: #9E3B3B",
		"--stale:",
		"prefers-reduced-motion",
	} {
		if !strings.Contains(text, tok) {
			t.Errorf("style.css missing locked token %q", tok)
		}
	}
	lower := strings.ToLower(text)
	for _, forbidden := range []string{"#14213d", "#0d1628", "#b99a45", "--navy"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("style.css still carries retired palette token %q", forbidden)
		}
	}
	assets, err := os.ReadFile(filepath.Join("..", "..", "docs", "ASSETS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(assets), "#14213d") || strings.Contains(strings.ToLower(string(assets)), "chapel") {
		t.Error("docs/ASSETS.md still documents the retired chapel navy/gold lock")
	}
	_, srv := uiServer(t, time.Now())
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(resp)
	for _, want := range []string{
		"Status", "Versions", "Health", "Logs",
		"Observe · Divine · Bless",
		`class="mark"`,
		`class="app-card"`,
		`class="chronicle-panel`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status page missing %q", want)
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
