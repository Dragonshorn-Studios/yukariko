//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/daemon"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// --- generic fixture helpers ------------------------------------------------

func sha256Bytes(b []byte) [32]byte { return sha256.Sum256(b) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out.String())
}

func parseOrFail(t *testing.T, text string) *config.Config {
	t.Helper()
	cfg, err := parseConfig([]byte(text))
	if err != nil {
		t.Fatalf("parse config: %v\n%s", err, text)
	}
	return cfg
}

func appsOf(cfg *config.Config) []*config.App {
	out := make([]*config.App, 0, len(cfg.Apps))
	for i := range cfg.Apps {
		out = append(out, &cfg.Apps[i])
	}
	return out
}

// openStore opens a store under a temp directory.
func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// assemble wires the real daemon with the process edges faked: preflight
// always passes, pipeline commands are recorded, and the standalone engine
// is a recording fake.
// assemble wires the daemon; the optional mutate hook adjusts the options
// (registry endpoint overrides, engine fakes) before assembly.
func assemble(t *testing.T, cfg *config.Config, dataDir string, commands *recorder, mutate func(*daemon.Options)) *daemon.Assembled {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := daemon.Options{
		Config:  cfg,
		DataDir: dataDir,
		Preflight: &schedule.Preflight{
			LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
			Exec:     func(context.Context, string, []string) error { return nil },
		},
		PipelineRun: commands.run,
		Engine:      &recordingEngine{},
	}
	if mutate != nil {
		mutate(&opts)
	}
	asm, err := daemon.Assemble(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { asm.Store.Close() })
	return asm
}

// parseConfig bridges to the strict config loader.
func parseConfig(data []byte) (*config.Config, error) { return config.Parse(data) }

// recordingEngine is a fake mutation engine recording each stage.
type recordingEngine struct {
	mu    sync.Mutex
	steps []string
}

func (e *recordingEngine) note(stage string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.steps = append(e.steps, stage)
}

// Snapshot returns the recorded stages, joined for substring assertions.
func (e *recordingEngine) Snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.steps...)
}

func (e *recordingEngine) Inspect(_ context.Context, name string) (docker.ContainerDetail, error) {
	e.note("inspect " + name)
	return docker.ContainerDetail{}, fmt.Errorf("no such container: %w", docker.ErrContainerMissing)
}

func (e *recordingEngine) Pull(_ context.Context, image, digest string) error {
	e.note(fmt.Sprintf("pull %s@%s", image, digest))
	return nil
}

func (e *recordingEngine) Stop(_ context.Context, name string) error {
	e.note("stop " + name)
	return nil
}

func (e *recordingEngine) Rename(_ context.Context, oldName, newName string) error {
	e.note(fmt.Sprintf("rename %s %s", oldName, newName))
	return nil
}

func (e *recordingEngine) Run(_ context.Context, argv []string, _ []string) error {
	e.note(strings.Join(argv, " "))
	return nil
}

func (e *recordingEngine) ConnectNetwork(_ context.Context, network, container string) error {
	e.note(fmt.Sprintf("connect %s %s", network, container))
	return nil
}

func (e *recordingEngine) Remove(_ context.Context, name string) error {
	e.note("remove " + name)
	return nil
}
