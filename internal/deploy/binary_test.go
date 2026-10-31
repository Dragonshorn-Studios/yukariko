package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/health"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
)

type memCheckpoint struct {
	version string
	calls   int
}

func (m *memCheckpoint) MarkDeployed(_ context.Context, _, version string) error {
	m.calls++
	m.version = version
	return nil
}

type fakeInstaller struct {
	tag string
	err error
}

func (f fakeInstaller) Install(context.Context, *config.ReleaseSource, string, string) (string, error) {
	return f.tag, f.err
}

func TestBinaryDeployRestartsThenCheckpoints(t *testing.T) {
	t.Parallel()
	var argv []string
	cp := &memCheckpoint{}
	p := &BinaryPipeline{
		Releases:   fakeInstaller{tag: "v2"},
		Health:     &health.Service{},
		Checkpoint: cp,
		Run: func(_ context.Context, req runner.Request) (runner.Result, error) {
			argv = append(argv, strings.Join(req.Argv, " "))
			return runner.Result{Status: runner.StatusSuccess}, nil
		},
	}
	app := binaryApp(t)
	res, err := p.Deploy(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || cp.calls != 1 || cp.version != "v2" {
		t.Fatalf("res=%+v checkpoint=%+v", res, cp)
	}
	if len(argv) != 1 || argv[0] != "systemctl --user restart marller" {
		t.Fatalf("argv = %v", argv)
	}
}

func TestBinaryDeployDoesNotCheckpointWhenRestartFails(t *testing.T) {
	t.Parallel()
	cp := &memCheckpoint{}
	p := &BinaryPipeline{
		Releases:   fakeInstaller{tag: "v2"},
		Health:     &health.Service{},
		Checkpoint: cp,
		Run: func(context.Context, runner.Request) (runner.Result, error) {
			return runner.Result{Status: runner.StatusFailed, Err: "unit not found"}, nil
		},
	}
	_, err := p.Deploy(context.Background(), binaryApp(t))
	if err == nil || !strings.Contains(err.Error(), "unit not found") {
		t.Fatalf("err = %v", err)
	}
	if cp.calls != 0 {
		t.Fatal("checkpoint advanced after a failed restart")
	}
}

func TestBinaryDeployRefusesWithoutCheckpoint(t *testing.T) {
	t.Parallel()
	p := &BinaryPipeline{Releases: fakeInstaller{tag: "v2"}, Health: &health.Service{}}
	_, err := p.Deploy(context.Background(), binaryApp(t))
	if err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("err = %v", err)
	}
}

func TestBinaryDeployInstallError(t *testing.T) {
	t.Parallel()
	p := &BinaryPipeline{
		Releases:   fakeInstaller{err: errors.New("checksum mismatch for asset")},
		Health:     &health.Service{},
		Checkpoint: &memCheckpoint{},
	}
	_, err := p.Deploy(context.Background(), binaryApp(t))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
}

func binaryApp(t *testing.T) *config.App {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.App{
		ID: "marller",
		Source: config.Source{
			Mode: config.SourceRelease,
			Release: &config.ReleaseSource{
				Repo: "Dragonshorn-Studios/marller", Asset: "marller-{tag}.tar.gz", ArchiveBinary: "marller",
			},
		},
		Deploy: config.Deploy{
			Mode:   config.DeployBinary,
			Binary: &config.BinaryDeploy{Dir: dir, Bin: "bin/marller", Unit: "marller"},
		},
	}
}

// DeployResult is referenced so a signature drift fails this file's build
// alongside the pipeline.
var _ schedule.Deployer = (*BinaryPipeline)(nil)
