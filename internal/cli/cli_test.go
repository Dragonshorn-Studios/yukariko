package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/exitcode"
	"github.com/Dragonshorn-Studios/yukariko/internal/version"
)

func commandSection(help string) string {
	idx := strings.Index(help, "Available Commands:")
	if idx < 0 {
		return ""
	}
	section := help[idx:]
	if flagIdx := strings.Index(section, "Flags:"); flagIdx >= 0 {
		section = section[:flagIdx]
	}
	return section
}

func TestHelpListsCommands(t *testing.T) {
	t.Parallel()

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if err := Execute(context.Background(), []string{"--help"}, stdout, stderr); err != nil {
		t.Fatalf("help: %v", err)
	}
	out := stdout.String()
	commands := commandSection(out)
	for _, name := range []string{"run", "check", "update", "status", "logs", "learn"} {
		if !strings.Contains(commands, name) {
			t.Errorf("help missing command %q\n%s", name, out)
		}
	}
	for _, alias := range []string{"materialize", "divine", "bless", "observe", "chronicle"} {
		if regexp.MustCompile(`\b` + alias + `\b`).MatchString(commands) {
			t.Errorf("help unexpectedly contains alias %q", alias)
		}
	}
	if !strings.Contains(out, "--config") || !strings.Contains(out, "--data-dir") {
		t.Errorf("help missing global flags\n%s", out)
	}
	if !strings.Contains(out, "source of truth") {
		t.Errorf("help missing architectural boundary\n%s", out)
	}
}

func TestVersionFlag(t *testing.T) {
	orig := version.Version
	version.Version = "1.2.3-test"
	t.Cleanup(func() { version.Version = orig })

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if err := Execute(context.Background(), []string{"--version"}, stdout, stderr); err != nil {
		t.Fatalf("version: %v", err)
	}
	got := stdout.String()
	if !strings.Contains(got, "1.2.3-test") {
		t.Fatalf("version output %q does not contain injected version", got)
	}
	if !strings.Contains(got, "yukariko") {
		t.Fatalf("version output %q does not contain binary name", got)
	}
}

func TestUnknownCommandUsage(t *testing.T) {
	t.Parallel()

	err := Execute(context.Background(), []string{"nope"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	if Code(err) != exitcode.Usage {
		t.Fatalf("exit code %d, want %d (%v)", Code(err), exitcode.Usage, err)
	}
}

func TestUnknownFlagUsage(t *testing.T) {
	t.Parallel()

	err := Execute(context.Background(), []string{"--bogus"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	if Code(err) != exitcode.Usage {
		t.Fatalf("exit code %d, want %d (%v)", Code(err), exitcode.Usage, err)
	}
}

func TestConfigAndDataDirFlags(t *testing.T) {
	t.Parallel()

	app := NewApp()
	// status parses the flags first; the missing config file is its error,
	// which still proves both flags were consumed.
	err := app.Execute(context.Background(), []string{"--config", "c.yaml", "--data-dir", "data", "status"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "c.yaml") {
		t.Fatalf("err = %v, want a config-file error naming the path", err)
	}
	opts := app.Options()
	if opts.Config != "c.yaml" {
		t.Errorf("config = %q", opts.Config)
	}
	if opts.DataDir != "data" {
		t.Errorf("data-dir = %q", opts.DataDir)
	}
}

func TestCancelledContextReachesCommand(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\napps:\n  - id: web\n    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}\n    deploy: {mode: standalone, standalone: {image: nginx:1, name: web}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Execute(ctx, []string{"status", "--config", path, "--data-dir", t.TempDir()}, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if Code(err) != exitcode.Interrupted {
		t.Fatalf("exit code %d, want %d", Code(err), exitcode.Interrupted)
	}
}

func TestRunExitCodes(t *testing.T) {
	t.Parallel()

	t.Run("help", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		code := Run(context.Background(), []string{"--help"}, io.Discard, stderr)
		if code != exitcode.OK {
			t.Fatalf("code %d", code)
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr %q", stderr.String())
		}
	})

	t.Run("missing config", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		code := Run(context.Background(), []string{"check"}, io.Discard, stderr)
		if code != exitcode.Usage {
			t.Fatalf("code %d", code)
		}
		if !strings.Contains(stderr.String(), "--config") {
			t.Fatalf("stderr %q", stderr.String())
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(path, []byte("schema_version: 1\napps:\n  - id: web\n    source: {mode: registry, registry: {images: [{ref: nginx:1}]}}\n    deploy: {mode: standalone, standalone: {image: nginx:1, name: web}}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stderr := &bytes.Buffer{}
		code := Run(ctx, []string{"status", "--config", path, "--data-dir", t.TempDir()}, io.Discard, stderr)
		if code != exitcode.Interrupted {
			t.Fatalf("code %d (stderr %q)", code, stderr.String())
		}
	})
}

func TestRootPrintsHelp(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	code := Run(context.Background(), nil, stdout, io.Discard)
	if code != exitcode.OK {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(stdout.String(), "Available Commands") {
		t.Fatalf("expected help, got %q", stdout.String())
	}
}
