package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

const defaultUnitPath = "/etc/systemd/system/yukariko.service"

// daemonReload is the systemctl seam; tests stub it.
var daemonReload = func(ctx context.Context) error {
	_, err := (&runner.Runner{}).Run(ctx, runner.Request{
		Name:    "systemctl daemon-reload",
		Argv:    []string{"systemctl", "daemon-reload"},
		Timeout: 30 * time.Second,
	})
	return err
}

// newExposeCommand grants the strict systemd unit write access to one git
// worktree (or a rootless Docker socket) via the ReadWritePaths drop-in.
func (a *App) newExposeCommand() *cobra.Command {
	var unit string
	cmd := &cobra.Command{
		Use:   "expose [path]",
		Short: "Grant the systemd unit write access to a git worktree or rootless socket",
		Long: `Expose adds the resolved path to the unit's ReadWritePaths drop-in,
idempotently, and reloads systemd.

The shipped unit is deliberately strict (ProtectSystem=strict; only
/var/lib/yukariko writable), but git sources write to their worktrees -
fetch updates .git and the fast-forward merge updates the tree. Run this
from inside the project directory or pass the path explicitly. Targets
under /home or /run/user (rootless Docker sockets) additionally set
ProtectHome=read-only, since ProtectHome=yes blocks those trees entirely.

Requires root; the service user can never grant itself filesystem access.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runExpose(cmd, args, unit)
		},
	}
	cmd.Flags().StringVar(&unit, "unit", defaultUnitPath, "systemd unit file the drop-in belongs to")
	return cmd
}

func (a *App) runExpose(cmd *cobra.Command, args []string, unitPath string) error {
	if a.uid() != 0 {
		return errors.New("expose writes the systemd drop-in; rerun as root (sudo yukariko expose)")
	}
	raw := "."
	if len(args) == 1 {
		raw = args[0]
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", raw, err)
	}
	path, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", raw, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	switch {
	case fi.IsDir():
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			return fmt.Errorf("%s is not a git worktree (no .git); pass the project's worktree directory: %w", path, errUsage)
		}
	case fi.Mode()&os.ModeSocket != 0:
		// Rootless Docker sockets are exposed exactly like worktrees.
	default:
		return fmt.Errorf("%s is neither a git worktree directory nor a socket: %w", path, errUsage)
	}

	dropIn := filepath.Join(unitPath+".d", "10-writables.conf")
	changed, err := writeExposeDropIn(dropIn, path)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if !changed {
		fmt.Fprintf(out, "%s is already exposed in %s\n", path, dropIn)
		return nil
	}
	fmt.Fprintf(out, "exposed %s in %s\n", path, dropIn)
	if err := daemonReload(cmd.Context()); err != nil {
		return fmt.Errorf("drop-in written but systemctl daemon-reload failed (run it manually): %w", err)
	}
	fmt.Fprintln(out, "applied; run: sudo systemctl restart yukariko")
	return nil
}

// writeExposeDropIn adds ReadWritePaths=<path> to the drop-in file,
// creating it (and its directory) as needed, and sets ProtectHome=read-only
// when the path lives under /home or /run/user. Existing lines are always
// preserved; changed is false when the entry (and any needed ProtectHome
// relaxation) is already present. The write is temp-then-rename.
func writeExposeDropIn(dropIn, path string) (changed bool, err error) {
	entry := "ReadWritePaths=" + path
	needHomeRead := strings.HasPrefix(path, "/home/") || strings.HasPrefix(path, "/run/user/")

	var lines []string
	if data, err := os.ReadFile(dropIn); err == nil && len(data) > 0 {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	hasEntry, hasService, hasHomeRead := false, false, false
	for _, l := range lines {
		switch l {
		case entry:
			hasEntry = true
		case "[Service]":
			hasService = true
		case "ProtectHome=read-only":
			hasHomeRead = true
		}
	}
	if hasEntry && (!needHomeRead || hasHomeRead) {
		return false, nil
	}
	if !hasEntry {
		lines = append(lines, entry)
	}
	if needHomeRead && !hasHomeRead {
		lines = append(lines, "ProtectHome=read-only")
	}
	if !hasService {
		lines = append([]string{"[Service]"}, lines...)
	}

	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		return false, err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(dropIn); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(dropIn), "."+filepath.Base(dropIn)+".tmp-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	content := strings.Join(lines, "\n") + "\n"
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), dropIn)
}
