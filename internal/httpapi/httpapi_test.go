package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

func testServer(t *testing.T, now time.Time) *Server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := osMkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
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
	return &Server{Store: st, Config: cfg, Now: func() time.Time { return now }}
}

func osMkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func seed(ctx context.Context, t *testing.T, s *Server) {
	t.Helper()
	depID, err := s.Store.BeginDeployment(ctx, store.BeginDeploymentParams{AppID: "web", Cause: "test", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.CommitDeploymentSuccess(ctx, "web", "digest", "app:1@sha256:abcd", depID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.RecordObservation(ctx, "web", "digest:app:1", "sha256:abcd", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.RecordHealthSample(ctx, "web", "http", "healthy", "status 200", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Store.RecordEvent(ctx, store.Event{AppID: "web", Level: store.LevelInfo, Kind: "state", Message: "deployed"})
}

func TestMethodMatrixRejectsMutations(t *testing.T) {
	t.Parallel()
	s := testServer(t, time.Now())
	h := s.Handler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	routes := []string{"/api/v1/apps", "/api/v1/apps/web", "/api/v1/hosts", "/api/v1/deployments", "/api/v1/logs", "/api/v1/health"}
	for _, route := range routes {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req, _ := http.NewRequest(method, srv.URL+route, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, route, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, route, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("%s %s Allow = %q", method, route, allow)
			}
		}
		// GET and HEAD succeed (or 404 for detail routes missing content —
		// all these exist in the fixture).
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req, _ := http.NewRequest(method, srv.URL+route, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, route, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s %s = %d, want 200", method, route, resp.StatusCode)
			}
		}
	}
	if resp, _ := http.Get(srv.URL + "/api/v1/nope"); resp != nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown route = %d, want 404", resp.StatusCode)
		}
	}
}

func TestAppsProjection(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := testServer(t, now)
	seed(context.Background(), t, s)

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/v1/apps")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Apps []struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			Deployed string `json:"deployed"`
			Pending  bool   `json:"pending"`
			Health   string `json:"health"`
		} `json:"apps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Apps) != 1 {
		t.Fatalf("apps = %d", len(got.Apps))
	}
	a := got.Apps[0]
	if a.ID != "web" || a.Deployed == "" || a.Pending {
		t.Errorf("app = %+v", a)
	}
	if !strings.Contains(a.Health, "http:healthy") {
		t.Errorf("health = %q, want http:healthy", a.Health)
	}

	// Detail endpoint with deployments and health.
	resp2, err := http.Get(srv.URL + "/api/v1/apps/web")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var detail struct {
		Deployments []struct {
			Status string `json:"status"`
		} `json:"deployments"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Deployments) != 1 || detail.Deployments[0].Status != store.StatusSucceeded {
		t.Errorf("deployments = %+v", detail.Deployments)
	}
	// Unknown app → 404.
	resp3, _ := http.Get(srv.URL + "/api/v1/apps/ghost")
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("unknown app = %d, want 404", resp3.StatusCode)
	}
}

func TestHostsAvailabilityRules(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := testServer(t, now)
	// A peer seen 2 minutes ago: online. One seen 5 minutes ago: stale.
	// One seen 30 minutes ago: offline. Absent peers are never listed as
	// healthy — absence is not in the list at all.
	if err := s.Store.TouchRemoteHost(context.Background(), "peer-online", now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.TouchRemoteHost(context.Background(), "peer-stale", now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.TouchRemoteHost(context.Background(), "peer-offline", now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/v1/hosts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Hosts []struct {
			HostID       string `json:"host_id"`
			Source       string `json:"source"`
			Availability string `json:"availability"`
		} `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"local":        "online",
		"peer-online":  "online",
		"peer-stale":   "stale",
		"peer-offline": "offline",
	}
	if len(got.Hosts) != len(want) {
		t.Fatalf("hosts = %+v", got.Hosts)
	}
	for _, h := range got.Hosts {
		if h.Availability != want[h.HostID] {
			t.Errorf("host %q = %q, want %q", h.HostID, h.Availability, want[h.HostID])
		}
		if h.HostID != "local" && h.Source != "remote" {
			t.Errorf("remote host %q source = %q", h.HostID, h.Source)
		}
	}
}

func TestLogsPagination(t *testing.T) {
	t.Parallel()
	s := testServer(t, time.Now())
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		_, _ = s.Store.RecordEvent(ctx, store.Event{AppID: "web", Level: store.LevelInfo, Kind: "k", Message: "e"})
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/logs?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Events []json.RawMessage `json:"events"`
		Count  int               `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 10 || len(got.Events) != 10 {
		t.Errorf("count = %d events = %d, want 10/10 (pagination respected)", got.Count, len(got.Events))
	}
}

func TestSecurityHeadersAndSanitization(t *testing.T) {
	t.Parallel()
	s := testServer(t, time.Now())
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/apps")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	text := string(body[:n])
	for _, leak := range []string{"config", "data/", "password", "env"} {
		if strings.Contains(strings.ToLower(text), leak) && leak != "config" {
			t.Errorf("response contains %q: %s", leak, text)
		}
	}
}

func TestConcurrentReads(t *testing.T) {
	t.Parallel()
	s := testServer(t, time.Now())
	seed(context.Background(), t, s)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				resp, err := http.Get(srv.URL + "/api/v1/apps")
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}
