package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/exitcode"
)

// The HTTP listener must honor server.enabled: off means no listener at
// all (it used to bind a random port on every interface via a stray
// `|| true`), and inbound reporting without a configured bind fails
// closed instead of guessing an address.
func TestRunListenerGating(t *testing.T) {
	writeCfg := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "yukariko.yaml")
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// A pre-cancelled context makes run return promptly; whether the
	// listener started is visible in the output.
	runRun := func(t *testing.T, cfg string) (string, int, error) {
		t.Helper()
		app := NewApp()
		app.stdin = strings.NewReader("")
		out := &strings.Builder{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := app.Execute(ctx, []string{"run", "--config", cfg, "--data-dir", t.TempDir()}, out, io.Discard)
		return out.String(), Code(err), err
	}

	t.Run("server disabled means no listener", func(t *testing.T) {
		cfg := writeCfg(t, "schema_version: 1\napps: []\n")
		out, _, err := runRun(t, cfg)
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("run = %v", err)
		}
		if strings.Contains(out, "listening on") {
			t.Fatalf("listener started with the server disabled:\n%s", out)
		}
	})

	t.Run("server enabled listens on the configured bind", func(t *testing.T) {
		cfg := writeCfg(t, "schema_version: 1\nserver:\n  enabled: true\napps: []\n")
		out, _, err := runRun(t, cfg)
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("run = %v", err)
		}
		if !strings.Contains(out, "listening on 127.0.0.1:8484") {
			t.Fatalf("expected the default loopback listener:\n%s", out)
		}
	})

	t.Run("oidc gate enabled wraps the listener", func(t *testing.T) {
		secretFile := filepath.Join(t.TempDir(), "oidc.secret")
		if err := os.WriteFile(secretFile, []byte("test-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := writeCfg(t, "schema_version: 1\nserver:\n  enabled: true\nauth:\n  oidc:\n    enabled: true\n    issuer: https://auth.example.com/application/o/yukariko/\n    client_id: yukariko\n    client_secret_ref: {file: "+secretFile+"}\n    redirect_base: https://yukariko.example.com\napps: []\n")
		out, _, err := runRun(t, cfg)
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("run = %v", err)
		}
		// Pinning the Protect wiring: without this branch the dashboard and
		// API would ship ungated while every other test still passes.
		if !strings.Contains(out, "auth: oidc enabled (issuer https://auth.example.com/application/o/yukariko/") {
			t.Fatalf("expected the auth wiring line in run output:\n%s", out)
		}
	})

	t.Run("inbound reporting without the server section fails closed", func(t *testing.T) {
		keyFile := filepath.Join(t.TempDir(), "report.key")
		if err := os.WriteFile(keyFile, []byte("shared-reporting-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := writeCfg(t, "schema_version: 1\nreporting:\n  inbound:\n    enabled: true\n    require_tls: false\n    hosts:\n      - id: alpha\n        keys: [{key_id: k1, secret_ref: {file: "+keyFile+"}}]\napps: []\n")
		out, code, err := runRun(t, cfg)
		if err == nil || !strings.Contains(err.Error(), "server.enabled") {
			t.Fatalf("err = %v, want the bind guidance", err)
		}
		if code != exitcode.Usage {
			t.Fatalf("code = %d, want usage", code)
		}
		if strings.Contains(out, "listening on") {
			t.Fatalf("listener started without a configured bind:\n%s", out)
		}
	})
}
