package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// CLIEngine implements Engine with the host Docker CLI through the runner:
// every mutation is an explicit, argv-based command.
type CLIEngine struct {
	Docker docker.Client // Inspect; nil uses a CLI client over Runner
	Runner *runner.Runner
}

func (e *CLIEngine) runner() *runner.Runner {
	if e.Runner != nil {
		return e.Runner
	}
	return &runner.Runner{}
}

func (e *CLIEngine) dockerClient() docker.Client {
	if e.Docker != nil {
		return e.Docker
	}
	return &docker.CLIClient{Runner: e.runner()}
}

// Inspect reads the current container through the read-only discovery
// client.
func (e *CLIEngine) Inspect(ctx context.Context, name string) (docker.ContainerDetail, error) {
	return e.dockerClient().InspectContainer(ctx, name)
}

// Pull pulls the image and verifies the expected manifest digest arrived.
func (e *CLIEngine) Pull(ctx context.Context, image, expectedDigest string) error {
	if expectedDigest == "" {
		return errors.New("no expected digest resolved; refusing to pull unverified content")
	}
	if _, err := e.run(ctx, 10*time.Minute, "pull", image); err != nil {
		return err
	}
	res, err := e.run(ctx, defaultVerifyTimeout, "image", "inspect", image, "--format", "{{json .RepoDigests}}")
	if err != nil {
		return err
	}
	if !strings.Contains(string(res.Stdout), expectedDigest) {
		return fmt.Errorf("local RepoDigests do not contain the expected %s; the pull did not deliver the verified manifest", expectedDigest)
	}
	return nil
}

func (e *CLIEngine) Stop(ctx context.Context, name string) error {
	_, err := e.run(ctx, 2*time.Minute, "stop", name)
	return err
}

func (e *CLIEngine) Rename(ctx context.Context, oldName, newName string) error {
	_, err := e.run(ctx, defaultVerifyTimeout, "rename", oldName, newName)
	return err
}

func (e *CLIEngine) Run(ctx context.Context, argv []string, env []string) error {
	_, err := e.runEnv(ctx, 10*time.Minute, argv, env)
	return err
}

func (e *CLIEngine) ConnectNetwork(ctx context.Context, network, container string) error {
	_, err := e.run(ctx, defaultVerifyTimeout, "network", "connect", network, container)
	return err
}

func (e *CLIEngine) Remove(ctx context.Context, name string) error {
	_, err := e.run(ctx, defaultVerifyTimeout, "rm", name)
	return err
}

func (e *CLIEngine) run(ctx context.Context, timeout time.Duration, args ...string) (runner.Result, error) {
	return e.runEnv(ctx, timeout, args, nil)
}

func (e *CLIEngine) runEnv(ctx context.Context, timeout time.Duration, args, env []string) (runner.Result, error) {
	r := e.runner()
	return r.Run(ctx, runner.Request{
		Name:    "docker " + args[0],
		Argv:    args,
		Timeout: timeout,
		Env:     env,
	})
}
