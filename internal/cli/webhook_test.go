package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/exitcode"
	"github.com/Dragonshorn-Studios/yukariko/internal/learn"
)

var errDeclined = errors.New("declined by test")

func webhookTestEnv(t *testing.T) (cfgPath string) {
	t.Helper()
	cfgPath = filepath.Join(t.TempDir(), "yukariko.yaml")
	body := "schema_version: 1\napps: []\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func runWebhook(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	stdout := &strings.Builder{}
	err := Execute(context.Background(), args, stdout, io.Discard)
	return stdout.String(), Code(err), err
}

// The add→list→remove round trip preserves unrelated configuration and
// never materializes defaults into the file.
func TestWebhookAddListRemoveRoundTrip(t *testing.T) {
	t.Parallel()
	cfg := webhookTestEnv(t)
	base := []string{"--config", cfg, "--data-dir", t.TempDir()}

	out, code, err := runWebhook(t, append([]string{"webhook", "list"}, base...)...)
	if code != exitcode.OK || err != nil {
		t.Fatalf("empty list = %d (%v): %s", code, err, out)
	}
	if !strings.Contains(out, "no webhooks") {
		t.Errorf("empty list output = %q", out)
	}

	out, code, err = runWebhook(t, "webhook", "add", "amadeus", "--config", cfg, "--data-dir", t.TempDir(),
		"--url", "https://amadeus.example.com/hook", "--secret-ref", "env:AMADEUS_HOOK_SECRET",
		"--timeout", "20s", "--header", "X-Tenant=ops")
	if code != exitcode.OK || err != nil {
		t.Fatalf("add = %d (%v): %s", code, err, out)
	}

	// The file carries the hook and the untouched apps key; no server.bind
	// default was materialized.
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"schema_version: 1", "webhooks:", "name: amadeus", "url: https://amadeus.example.com/hook", "env: AMADEUS_HOOK_SECRET", "timeout: 20s", "name: X-Tenant", "value: ops"} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "bind:") {
		t.Errorf("defaults materialized into the file:\n%s", s)
	}

	out, code, err = runWebhook(t, "webhook", "list", "--config", cfg, "--data-dir", t.TempDir())
	if code != exitcode.OK || err != nil || !strings.Contains(out, "amadeus") {
		t.Fatalf("list = %d (%v): %s", code, err, out)
	}

	// A second, differently-shaped target keeps both.
	out, code, err = runWebhook(t, "webhook", "add", "relay", "--config", cfg, "--data-dir", t.TempDir(),
		"--url", "http://127.0.0.1:9090/hook")
	if code != exitcode.OK || err != nil {
		t.Fatalf("add relay = %d (%v): %s", code, err, out)
	}

	out, code, err = runWebhook(t, "webhook", "remove", "amadeus", "--config", cfg, "--data-dir", t.TempDir())
	if code != exitcode.OK || err != nil {
		t.Fatalf("remove = %d (%v): %s", code, err, out)
	}
	raw, _ = os.ReadFile(cfg)
	s = string(raw)
	if strings.Contains(s, "amadeus") || strings.Contains(s, "X-Tenant") {
		t.Errorf("removed webhook survived:\n%s", s)
	}
	if !strings.Contains(s, "relay") {
		t.Errorf("remaining webhook lost:\n%s", s)
	}

	// Removing an unknown name errors and leaves the file untouched.
	before, _ := os.ReadFile(cfg)
	if _, code, err := runWebhook(t, "webhook", "remove", "ghost", "--config", cfg, "--data-dir", t.TempDir()); code != exitcode.Error {
		t.Errorf("remove unknown = %d (%v), want error", code, err)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Error("failed remove changed the file")
	}
}

func TestWebhookAddValidation(t *testing.T) {
	t.Parallel()
	cfg := webhookTestEnv(t)

	cases := []struct {
		name string
		args []string
	}{
		{"missing url", []string{"webhook", "add", "x"}},
		{"bad scheme", []string{"webhook", "add", "x", "--url", "ftp://a.example.com/h"}},
		{"bad secret ref", []string{"webhook", "add", "x", "--url", "https://a.example.com/h", "--secret-ref", "literal-secret"}},
		{"relative secret file", []string{"webhook", "add", "x", "--url", "https://a.example.com/h", "--secret-ref", "file:secrets/hook"}},
		{"bad timeout", []string{"webhook", "add", "x", "--url", "https://a.example.com/h", "--timeout", "soon"}},
		{"bad header", []string{"webhook", "add", "x", "--url", "https://a.example.com/h", "--header", "justname"}},
	}
	for _, c := range cases {
		args := append([]string{"--config", cfg, "--data-dir", t.TempDir()}, c.args...)
		if _, code, err := runWebhook(t, args...); code != exitcode.Usage && code != exitcode.Error {
			t.Errorf("%s: exit = %d (%v), want non-OK classification", c.name, code, err)
		}
	}

	// Duplicate name errors without corrupting the file.
	if _, code, err := runWebhook(t, "webhook", "add", "dup", "--config", cfg, "--data-dir", t.TempDir(), "--url", "https://a.example.com/h"); code != exitcode.OK {
		t.Fatalf("seed add = %d (%v)", code, err)
	}
	before, _ := os.ReadFile(cfg)
	if _, code, err := runWebhook(t, "webhook", "add", "dup", "--config", cfg, "--data-dir", t.TempDir(), "--url", "https://b.example.com/h"); code != exitcode.Error {
		t.Errorf("duplicate add = %d (%v), want error", code, err)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Error("failed duplicate add changed the file")
	}
}

// ApplyConfigEdit directly: failure inside mutate leaves the file
// byte-identical, and a rendered document that fails validation is refused
// before any write.
func TestApplyConfigEditAtomicity(t *testing.T) {
	t.Parallel()
	path := webhookTestEnv(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := learn.ApplyConfigEdit(path, func(cfg *config.Config) error {
		return errDeclined
	}); err == nil {
		t.Fatal("mutate error must surface")
	}
	mid, _ := os.ReadFile(path)
	if string(before) != string(mid) {
		t.Fatal("declined edit changed the file")
	}

	// An edit that produces an invalid document is refused at the parse
	// gate — the write path never sees it.
	if _, err := learn.ApplyConfigEdit(path, func(cfg *config.Config) error {
		cfg.Webhooks = append(cfg.Webhooks, config.Webhook{Name: "bad", URL: "not a url"})
		return nil
	}); err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("invalid edit = %v; want a refused write", err)
	}
	mid, _ = os.ReadFile(path)
	if string(before) != string(mid) {
		t.Fatal("invalid edit changed the file")
	}
}
