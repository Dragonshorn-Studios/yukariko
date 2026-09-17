package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Dragonshorn-Studios/yukariko/internal/exitcode"
	"github.com/Dragonshorn-Studios/yukariko/internal/version"
)

// ErrNotImplemented is returned by command stubs until their owning issue lands.
var ErrNotImplemented = errors.New("not implemented")

// Options holds global flags. Paths are stored only; YAML is not loaded until issue #2.
type Options struct {
	Config  string
	DataDir string
}

// App is the Yukariko CLI.
type App struct {
	opts Options
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

	root.PersistentFlags().StringVar(&a.opts.Config, "config", "", "path to YAML configuration file")
	root.PersistentFlags().StringVar(&a.opts.DataDir, "data-dir", "", "path to Yukariko data directory")

	root.AddCommand(newStubCommand("run", "Run the daemon, scheduler, health monitor, and optional HTTP server"))
	root.AddCommand(newStubCommand("check", "Observe sources and health without deploying"))
	root.AddCommand(newStubCommand("update", "Request an update through preflight and per-app locks"))
	root.AddCommand(newStubCommand("status", "Show process, health, and deployed-version status"))
	root.AddCommand(newStubCommand("logs", "Show bounded structured event history"))
	root.AddCommand(newStubCommand("learn", "Import local Docker/Compose apps into configuration"))

	return root
}

func newStubCommand(name, short string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			return fmt.Errorf("%s: %w", name, ErrNotImplemented)
		},
	}
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
