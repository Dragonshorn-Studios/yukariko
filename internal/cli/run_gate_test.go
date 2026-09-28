package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is an output buffer readable while the run goroutine writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freeBind reserves an ephemeral port for a config bind. Port 0 is not a
// valid configured bind, so the test must pick a real one.
func freeBind(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// In keys-only mode the run listener must serve /api behind the key gate
// (401 without a key) and mount no /auth routes — the documented
// "no /auth routes in keys-only mode" guarantee, pinned at the HTTP level.
func TestRunKeysOnlyGateWiring(t *testing.T) {
	t.Parallel()
	bind := freeBind(t)
	cfg := filepath.Join(t.TempDir(), "yukariko.yaml")
	body := "schema_version: 1\nserver: {enabled: true, bind: " + bind + "}\n" +
		"auth:\n  api_keys:\n    enabled: true\napps: []\n"
	if err := os.WriteFile(cfg, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	app.stdin = strings.NewReader("")
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.Execute(ctx, []string{"run", "--config", cfg, "--data-dir", t.TempDir()}, out, io.Discard) }()

	// Wait for the listener to come up.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if strings.Contains(out.String(), "listening on "+bind) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never listened; output:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// /api without a key is 401 JSON; /auth/login does not exist; /ui is open.
	res, err := http.Get("http://" + bind + "/api/v1/apps")
	if err != nil {
		t.Fatalf("GET /api: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api without key = %d, want 401", res.StatusCode)
	}
	res, err = http.Get("http://" + bind + "/auth/login")
	if err != nil {
		t.Fatalf("GET /auth/login: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /auth/login in keys-only mode = %d, want 404 (no /auth routes)", res.StatusCode)
	}
	res, err = http.Get("http://" + bind + "/ui")
	if err != nil {
		t.Fatalf("GET /ui: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /ui in keys-only mode = %d, want 200", res.StatusCode)
	}

	// The startup lines distinguish the two gate modes.
	if !strings.Contains(out.String(), "auth: api keys enabled") {
		t.Errorf("output missing api-keys line:\n%s", out.String())
	}
	if strings.Contains(out.String(), "auth: oidc enabled") {
		t.Errorf("output must not claim oidc in keys-only mode:\n%s", out.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop after cancel")
	}
}
