package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Dragonshorn-Studios/yukariko/internal/daemon"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/exitcode"
	"github.com/Dragonshorn-Studios/yukariko/internal/learn"
	"github.com/Dragonshorn-Studios/yukariko/internal/version"
)

// errUsage marks flag/argument problems so Code maps them to the usage exit
// code; command implementations wrap it with %w.
var errUsage = errors.New("usage error")

// Options holds global flags. Paths are stored only; YAML is not loaded until
// a command needs it.
type Options struct {
	Config  string
	DataDir string
}

// App is the Yukariko CLI. The unexported dependency fields exist so tests
// can inject fakes; production builds use the real implementations.
type App struct {
	opts         Options
	euid         func() int
	stdin        io.Reader
	dockerClient docker.Client
	gitProber    learn.GitProber
	daemonHook   func(*daemon.Options)
}

// NewApp constructs a CLI with empty config and data-dir paths.
func NewApp() *App {
	return &App{}
}

// Options returns the flags parsed by the last Execute/Run call.
func (a *App) Options() Options {
	return a.opts
}

// Command builds the root cobra command.
func (a *App) Command() *cobra.Command {
	root := &cobra.Command{
		Use:   "yukariko",
		Short: "Local Docker/Compose/Git deploy agent",
		Long: `Yukariko is a local single-binary agent for Docker Compose and standalone Docker deploys.

Host Docker, Compose, and Git remain the source of truth. Yukariko observes, records,
and runs controlled local commands; it does not replace Compose, Coolify, or Portainer.`,
		Version:          version.String(),
		SilenceUsage:     true,
		SilenceErrors:    true,
		TraverseChildren: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true

	root.PersistentFlags().StringVar(&a.opts.Config, "config", "", "path to YAML configuration (default /etc/yukariko/yukariko.yaml when run as root on Linux)")
	root.PersistentFlags().StringVar(&a.opts.DataDir, "data-dir", "", "path to Yukariko data directory (default /var/lib/yukariko as root on Linux, else ./data)")

	root.AddCommand(a.newRunCommand())
	root.AddCommand(a.newCheckCommand())
	root.AddCommand(a.newUpdateCommand())
	root.AddCommand(a.newStatusCommand())
	root.AddCommand(a.newLogsCommand())
	root.AddCommand(a.newLearnCommand())

	return root
}

// Execute runs the CLI with the given arguments.
func (a *App) Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root := a.Command()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.ExecuteContext(ctx)
}

// Execute is a package-level helper around a fresh App.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return NewApp().Execute(ctx, args, stdout, stderr)
}

// Run executes the CLI and maps errors to process exit codes.
func (a *App) Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	err := a.Execute(ctx, args, stdout, stderr)
	if err == nil {
		return exitcode.OK
	}
	code := Code(err)
	fmt.Fprintln(stderr, err.Error())
	return code
}

// Run is a package-level helper around a fresh App.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return NewApp().Run(ctx, args, stdout, stderr)
}

// Code maps an error from Execute to a process exit code.
func Code(err error) int {
	if err == nil {
		return exitcode.OK
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return exitcode.Interrupted
	}
	if errors.Is(err, learn.ErrAborted) {
		return exitcode.Interrupted
	}
	if errors.Is(err, errUsage) {
		return exitcode.Usage
	}
	if isUsageError(err) {
		return exitcode.Usage
	}
	return exitcode.Error
}

func isUsageError(err error) bool {
	s := err.Error()
	switch {
	case strings.Contains(s, "unknown command"):
		return true
	case strings.Contains(s, "unknown flag"):
		return true
	case strings.Contains(s, "unknown shorthand flag"):
		return true
	case strings.Contains(s, "flag needs an argument"):
		return true
	case strings.Contains(s, "accepts"):
		return true
	default:
		return false
	}
}
