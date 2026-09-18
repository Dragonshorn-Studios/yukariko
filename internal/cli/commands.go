package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/daemon"
	"github.com/Dragonshorn-Studios/yukariko/internal/state"
)

// daemonOptions builds the assembly options from the global flags plus the
// test hook. Configuration comes only from validated configuration files;
// a root invocation on Linux falls back to the installer's system layout
// (see defaults.go) when the flags are omitted.
func (a *App) daemonOptions() (daemon.Options, error) {
	configPath, err := a.configPathFor()
	if err != nil {
		return daemon.Options{}, err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return daemon.Options{}, err
	}
	opts := daemon.Options{Config: cfg, DataDir: a.dataDirFor()}
	if a.daemonHook != nil {
		a.daemonHook(&opts)
	}
	return opts, nil
}

func appsSlice(cfg *config.Config) []*config.App {
	out := make([]*config.App, 0, len(cfg.Apps))
	for i := range cfg.Apps {
		out = append(out, &cfg.Apps[i])
	}
	return out
}

// --- run / materialize ------------------------------------------------------

func (a *App) newRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "run",
		Aliases: []string{"materialize"},
		Short:   "Run the daemon, scheduler, health monitor, and optional HTTP server",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := a.daemonOptions()
			if err != nil {
				return err
			}
			asm, err := daemon.Assemble(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer asm.Store.Close()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "yukariko daemon running (data dir %s); Docker and Compose directories remain the source of truth.\n", opts.DataDir)
			if asm.APIOn || asm.ReportHandler != nil {
				if asm.APIBind == "" {
					// Inbound reporting without the server section: there is
					// no configured address. Refuse rather than bind a random
					// port on every interface.
					return fmt.Errorf("inbound reporting is enabled but server.enabled is false; set server.enabled and server.bind so the report listener has an address: %w", errUsage)
				}
				mux := http.NewServeMux()
				mux.Handle("/ui", asm.UI)
				mux.Handle("/ui/", asm.UI)
				if asm.APIOn {
					mux.Handle("/api/", asm.API.Handler())
					mux.Handle("/api", asm.API.Handler())
				}
				if asm.ReportHandler != nil {
					mux.Handle("/report/v1/events", asm.ReportHandler)
				}
				ln, err := net.Listen("tcp", asm.APIBind)
				if err != nil {
					return fmt.Errorf("bind %s: %w", asm.APIBind, err)
				}
				srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
				go func() { _ = srv.Serve(ln) }()
				defer srv.Close()
				fmt.Fprintf(out, "listening on %s (dashboard /ui/, API /api/, reports /report/v1/events).\n", asm.APIBind)
			}
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				asm.Monitor.Run(ctx, appsSlice(asm.Config))
			}()
			if asm.Reporter != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					asm.Reporter.Run(ctx)
				}()
			}
			runErr := asm.Scheduler.Run(ctx)
			wg.Wait()
			if runErr == nil {
				fmt.Fprintln(out, "daemon stopped cleanly.")
			}
			return runErr
		},
	}
}

// --- check / divine ---------------------------------------------------------

func (a *App) newCheckCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "check",
		Aliases: []string{"divine"},
		Short:   "Observe sources and health without deploying",
		Long: `Check observes every configured app's source (Git branch or registry digest)
and records what it saw. It never deploys, restarts, or mutates anything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := a.daemonOptions()
			if err != nil {
				return err
			}
			asm, err := daemon.Assemble(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer asm.Store.Close()

			type checkLine struct {
				ID       string `json:"id"`
				Changed  bool   `json:"changed"`
				Observed string `json:"observed,omitempty"`
				Detail   string `json:"detail,omitempty"`
				Error    string `json:"error,omitempty"`
			}
			var lines []checkLine
			failed := false
			apps := appsSlice(asm.Config)
			for _, app := range apps {
				res, err := asm.Checker.Check(cmd.Context(), app)
				line := checkLine{ID: app.ID}
				if err != nil {
					line.Error = err.Error()
					failed = true
				} else {
					line.Changed = res.Changed
					line.Observed = res.Observed
					line.Detail = res.Detail
				}
				lines = append(lines, line)
				if jsonOut {
					continue
				}
				switch {
				case err != nil:
					fmt.Fprintf(cmd.OutOrStdout(), "%-16s ERROR: %v\n", app.ID, err)
				case res.Changed:
					fmt.Fprintf(cmd.OutOrStdout(), "%-16s CHANGED %s\n", app.ID, res.Detail)
				default:
					fmt.Fprintf(cmd.OutOrStdout(), "%-16s up-to-date %s\n", app.ID, res.Detail)
				}
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(lines); err != nil {
					return err
				}
			}
			if failed {
				return errors.New("check: one or more apps failed observation")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	return cmd
}

// --- update / bless ---------------------------------------------------------

func (a *App) newUpdateCommand() *cobra.Command {
	var appID string
	var allApps, dryRun bool
	cmd := &cobra.Command{
		Use:     "update",
		Aliases: []string{"bless"},
		Short:   "Request one app/all apps update through the same preflight/lock/state machine",
		Long: `Update runs one deployment pass for the selected app(s) through the same
preflight, per-app lock, and state machine the daemon uses. Without
--dry-run this deploys for real when a change is detected. --all selects
every configured app.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if (appID == "") == !allApps {
				return fmt.Errorf("use exactly one of --app <id> or --all: %w", errUsage)
			}
			opts, err := a.daemonOptions()
			if err != nil {
				return err
			}
			asm, err := daemon.Assemble(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer asm.Store.Close()

			var targets []*config.App
			for _, app := range appsSlice(asm.Config) {
				if allApps || app.ID == appID {
					targets = append(targets, app)
				}
			}
			if len(targets) == 0 {
				return fmt.Errorf("no app %q in the configuration: %w", appID, errUsage)
			}
			sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })

			failed := false
			for _, app := range targets {
				if dryRun {
					if err := updateDryRun(cmd, asm, app); err != nil {
						failed = true
					}
					continue
				}
				// Trigger enters the same preflight/lock/state machine the
				// scheduler uses and queues on the per-app lock.
				if err := asm.Scheduler.Trigger(cmd.Context(), app.ID); err != nil {
					failed = true
					fmt.Fprintf(cmd.OutOrStdout(), "%-16s FAILED: %v\n", app.ID, err)
					continue
				}
				st, _ := asm.Scheduler.Status(app.ID)
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %s: %s\n", app.ID, st.State, st.Detail)
			}
			if failed {
				return errors.New("update: one or more apps failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appID, "app", "", "update this app")
	cmd.Flags().BoolVar(&allApps, "all", false, "update every configured app")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check and preflight without deploying")
	return cmd
}

// updateDryRun checks the source and runs preflight without any mutation.
func updateDryRun(cmd *cobra.Command, asm *daemon.Assembled, app *config.App) error {
	res, err := asm.Checker.Check(cmd.Context(), app)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "%-16s ERROR: %v\n", app.ID, err)
		return err
	}
	if !res.Changed {
		fmt.Fprintf(cmd.OutOrStdout(), "%-16s up-to-date; nothing to deploy\n", app.ID)
		return nil
	}
	findings := asm.Preflight.Run(cmd.Context(), app)
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(cmd.OutOrStdout(), "%-16s preflight: %s\n", app.ID, f.String())
		}
		return fmt.Errorf("preflight refused %s", app.ID)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%-16s would deploy: %s (preflight passed)\n", app.ID, res.Detail)
	return nil
}

// --- status / observe -------------------------------------------------------

func (a *App) newStatusCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "status",
		Aliases: []string{"observe"},
		Short:   "Show process, health, observed version, deployed version, pending update, and last success/failure",
		Long: `Status reads the durable store: deployed and observed versions, current
health, and the last deployment outcome, with the human verdict per app.
Docker/Compose directories remain the source of truth; these are Yukariko's
recorded observations.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := a.daemonOptions()
			if err != nil {
				return err
			}
			asm, err := daemon.Assemble(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer asm.Store.Close()
			ctx := cmd.Context()

			var rows []state.AppStatus
			for _, app := range appsSlice(asm.Config) {
				row, err := state.StatusRow(ctx, asm.Store, app, time.Now())
				if err != nil {
					return err
				}
				rows = append(rows, row)
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			for _, row := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-10s deployed=%s observed=%s pending=%s health=%s last=%s\n",
					row.ID, row.State,
					orDefault(row.Deployed, "none"), orDefault(row.Observed, "none"),
					yesNo(row.Pending), orDefault(row.Health, "unknown"),
					orDefault(row.LastDeployment, "none"))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	return cmd
}

// --- logs / chronicle -------------------------------------------------------

func (a *App) newLogsCommand() *cobra.Command {
	var appID, level string
	var limit int
	var since time.Duration
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "logs",
		Aliases: []string{"chronicle"},
		Short:   "Read bounded structured event history with app/time/level filters",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := a.daemonOptions()
			if err != nil {
				return err
			}
			asm, err := daemon.Assemble(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer asm.Store.Close()

			events, err := state.EventsFor(cmd.Context(), asm.Store, appID, level, limit, since)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(events)
			}
			for _, e := range events {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %-5s %-16s %s: %s\n",
					e.Time.Format(time.RFC3339), e.Level, orDefault(e.AppID, "-"), e.Kind, e.Message)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appID, "app", "", "filter by app ID")
	cmd.Flags().StringVar(&level, "level", "", "filter by level (debug|info|warn|error)")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum events to show (hard cap 1000)")
	cmd.Flags().DurationVar(&since, "since", 0, "only events newer than this duration (e.g. 24h)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	return cmd
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
