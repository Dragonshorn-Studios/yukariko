package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
	"github.com/Dragonshorn-Studios/yukariko/internal/learn"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// newLearnCommand wires the interactive onboarding flow (#7). Scanning is
// read-only; the configuration is written only after an explicit
// confirmation, and learn never deploys anything.
func (a *App) newLearnCommand() *cobra.Command {
	var dryRun, jsonOut, includeSystem bool
	cmd := &cobra.Command{
		Use:   "learn",
		Short: "Import local Docker/Compose apps into configuration",
		Long: `Scan the local Docker state (read-only), present importable candidates,
resolve required fields interactively, and preview a unified YAML diff.
The configuration is written only after explicit confirmation: a timestamped
backup is created and the file is replaced atomically after validation.
learn never deploys, restarts, or changes Docker state.

--json prints machine-readable proposals without prompts and never writes.
--dry-run previews the merged configuration without writing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath, err := a.configPathFor()
			if err != nil {
				return err
			}
			runner := &runner.Runner{}
			client := a.dockerClient
			if client == nil {
				client = &docker.CLIClient{Runner: runner}
			}
			git := a.gitProber
			if git == nil {
				git = &learn.RunnerGitProber{Runner: runner}
			}
			stdin := a.stdin
			if stdin == nil {
				stdin = os.Stdin
			}
			_, err = learn.RunFlow(cmd.Context(), learn.FlowOptions{
				ConfigPath:    configPath,
				DryRun:        dryRun,
				JSONOut:       jsonOut,
				IncludeSystem: includeSystem,
				Client:        client,
				Git:           git,
				Stdin:         stdin,
				Stdout:        cmd.OutOrStdout(),
			})
			if errors.Is(err, learn.ErrDeclined) {
				return nil // a clean decline; the flow already explained it
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview the merged configuration without writing it")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print machine-readable proposals; no prompts, no writes")
	cmd.Flags().BoolVar(&includeSystem, "include-system", false, "offer system/infrastructure candidates (excluded by default)")
	return cmd
}
