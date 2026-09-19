package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

const defaultUnitPath = "/etc/systemd/system/yukariko.service"

// daemonReload is the systemctl seam; tests stub it. The runner reserves
// its error for request validation — process outcomes arrive on
// Result.Status and are surfaced here, never swallowed.
var daemonReload = func(ctx context.Context) error {
	res, err := (&runner.Runner{}).Run(ctx, runner.Request{
		Name:    "systemctl daemon-reload",
		Argv:    []string{"systemctl", "daemon-reload"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if res.Status != runner.StatusSuccess {
		detail := strings.TrimSpace(string(res.Stderr))
		if detail == "" {
			detail = res.Err
		}
		return fmt.Errorf("systemctl daemon-reload %s: %s", res.Status, detail)
	}
	return nil
}

// newExposeCommand grants the strict systemd unit write access to one git
// worktree (or a rootless Docker socket) via the ReadWritePaths drop-in.
func (a *App) newExposeCommand() *cobra.Command {
	var unit string
	cmd := &cobra.Command{
		Use:   "expose [path]",
		Short: "Grant the systemd unit write access to a git worktree or rootless socket",
		Long: `Expose adds the resolved path to the unit's ReadWritePaths drop-in,
idempotently, and reloads systemd (even when the entry already existed,
so a previously failed reload always heals on rerun).

The shipped unit is deliberately strict (ProtectSystem=strict; only
/var/lib/yukariko writable), but git sources write to their worktrees -
fetch updates .git and the fast-forward merge updates the tree. Run this
from inside the project directory or pass the path explicitly. Targets
under /home, /root, or /run/user (rootless Docker sockets) additionally
set ProtectHome=read-only, since ProtectHome=yes blocks those trees
entirely and a ReadWritePaths grant alone would stay inert.

expose also applies the matching filesystem ACLs for the unit's user
(rwX on worktrees, rw on sockets, traverse on the owning /home/<user> or
/run/user/<uid> tree), reapplied on every run because rootless dockerd
recreates its socket; skipped when the unit runs as root.

Linux only; requires root - the service user can never grant itself
filesystem access.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runExpose(cmd, args, unit)
		},
	}
	cmd.Flags().StringVar(&unit, "unit", defaultUnitPath, "systemd unit file the drop-in belongs to")
	return cmd
}

func (a *App) runExpose(cmd *cobra.Command, args []string, unitPath string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("expose manages the Linux systemd unit layout (this is %s): %w", runtime.GOOS, errUsage)
	}
	if a.uid() != 0 {
		return errors.New("expose writes the systemd drop-in; rerun as root (sudo yukariko expose)")
	}
	// A drop-in for a missing or mistyped unit is silently ignored by
	// systemd; fail closed with an installer pointer instead.
	if _, err := os.Stat(unitPath); err != nil {
		return fmt.Errorf("unit %s not found (install it first: scripts/install.sh): %w", unitPath, err)
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
	isSocket := fi.Mode()&os.ModeSocket != 0
	switch {
	case fi.IsDir():
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			return fmt.Errorf("%s is not a git worktree (no .git); pass the project's worktree directory: %w", path, errUsage)
		}
	case isSocket:
		// Rootless Docker sockets; a /run/user target still receives the
		// ProtectHome relaxation in writeExposeDropIn.
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
	}
	// Filesystem ACLs for the unit's user — reapplied on every run, because
	// rootless dockerd recreates its socket (dropping ACLs) and /run/user is
	// recreated at login. The sandbox grant alone does not cross Unix
	// ownership.
	if err := applyACLs(cmd.Context(), out, path, isSocket, unitPath); err != nil {
		return err
	}
	// Always reload: a previous run may have written the drop-in but failed
	// its reload, and the only healing path back through this command is a
	// rerun. daemon-reload is idempotent and cheap.
	if err := daemonReload(cmd.Context()); err != nil {
		return fmt.Errorf("drop-in present but systemctl daemon-reload failed (run it manually): %w", err)
	}
	if changed {
		fmt.Fprintf(out, "exposed %s in %s\n", path, dropIn)
	}
	fmt.Fprintln(out, "applied; run: sudo systemctl restart yukariko")
	return nil
}

// aclAvailable/unitUser/aclRun are the ACL seams; tests stub them.
var (
	aclAvailable = func() bool {
		_, err := exec.LookPath("setfacl")
		return err == nil
	}
	unitUser = func(ctx context.Context, unitName string) (string, error) {
		res, err := (&runner.Runner{}).Run(ctx, runner.Request{
			Name:    "systemctl show " + unitName,
			Argv:    []string{"systemctl", "show", unitName, "--value", "-p", "User"},
			Timeout: 15 * time.Second,
		})
		if err != nil {
			return "", err
		}
		if res.Status != runner.StatusSuccess {
			return "", fmt.Errorf("systemctl show %s %s: %s", unitName, res.Status, orACLDetail(res))
		}
		return strings.TrimSpace(string(res.Stdout)), nil
	}
	aclRun = func(ctx context.Context, argv []string) error {
		res, err := (&runner.Runner{}).Run(ctx, runner.Request{
			Name:    "setfacl",
			Argv:    argv,
			Timeout: time.Minute,
		})
		if err != nil {
			return err
		}
		if res.Status != runner.StatusSuccess {
			return fmt.Errorf("setfacl %s: %s", strings.Join(argv[2:], " "), orACLDetail(res))
		}
		return nil
	}
)

func orACLDetail(res runner.Result) string {
	if s := strings.TrimSpace(string(res.Stderr)); s != "" {
		return s
	}
	return res.Err
}

// applyACLs grants the unit's user the Unix permissions the sandbox grant
// cannot provide: rwX (+defaults) on a worktree, rw on a socket, and
// traverse on the owning /home/<user> or /run/user/<uid> parent. A unit
// running as root needs none of this. Best effort per command, with the
// overall failure text actionable.
func applyACLs(ctx context.Context, out io.Writer, target string, isSocket bool, unitPath string) error {
	if !aclAvailable() {
		return errors.New("setfacl not found; install the acl package (rerunning scripts/install.sh does it) so expose can grant the daemon user filesystem access")
	}
	user, err := unitUser(ctx, filepath.Base(unitPath))
	if err != nil || user == "" {
		user = defaultServiceUser // the shipped unit's user; best effort
		fmt.Fprintf(out, "note: could not resolve the unit's User= (%v); assuming %s\n", err, user)
	}
	if user == "root" {
		fmt.Fprintln(out, "unit runs as root; no filesystem ACLs needed")
		return nil
	}
	for _, argv := range aclCommands(target, isSocket, user) {
		if err := aclRun(ctx, argv); err != nil {
			return fmt.Errorf("%v (granting %s access to %s): %w", argv, user, target, err)
		}
	}
	return nil
}

// aclCommands builds the setfacl invocations for one target: traverse on
// the owning user-tree parent, then the target grant itself.
func aclCommands(target string, isSocket bool, user string) [][]string {
	var cmds [][]string
	if parent := owningUserTree(target); parent != "" {
		cmds = append(cmds, []string{"setfacl", "-m", "u:" + user + ":x", parent})
	}
	if isSocket {
		cmds = append(cmds, []string{"setfacl", "-m", "u:" + user + ":rw", target})
	} else {
		cmds = append(cmds, []string{"setfacl", "-R", "-m", "u:" + user + ":rwX", "-m", "d:u:" + user + ":rwX", target})
	}
	return cmds
}

// owningUserTree returns the /home/<user> or /run/user/<uid> prefix the
// target lives under, or "" for system trees like /srv.
func owningUserTree(target string) string {
	for _, prefix := range []string{"/home/", "/run/user/"} {
		if !strings.HasPrefix(target, prefix) {
			continue
		}
		rest := strings.TrimPrefix(target, prefix)
		if seg := strings.Split(rest, "/"); seg[0] != "" {
			return prefix + seg[0]
		}
	}
	return ""
}

const defaultServiceUser = "yukariko"

// protectHomeBlocks reports whether the path lives in a tree the unit's
// ProtectHome=yes makes inaccessible (/home, /root, /run/user). A
// ReadWritePaths grant for such a path stays inert without the relaxation.
func protectHomeBlocks(path string) bool {
	for _, prefix := range []string{"/home/", "/root/", "/run/user/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// writeExposeDropIn adds ReadWritePaths=<path> to the drop-in file,
// creating it (and its directory) as needed, and sets ProtectHome=read-only
// when the path lives in a tree ProtectHome=yes blocks. Existing lines are
// always preserved; new lines are inserted inside the [Service] section (a
// plain append could land under a later section header, where systemd
// ignores them); changed is false when the entry (and any needed
// ProtectHome relaxation) is already present. The write is
// temp-then-rename, preserving an existing file's mode.
func writeExposeDropIn(dropIn, path string) (changed bool, err error) {
	entry := "ReadWritePaths=" + path
	relaxHome := protectHomeBlocks(path)

	data, err := os.ReadFile(dropIn)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		// genuinely new file
	default:
		return false, fmt.Errorf("read existing drop-in %s: %w", dropIn, err)
	}
	var lines []string
	if len(data) > 0 {
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
	if hasEntry && (!relaxHome || hasHomeRead) {
		return false, nil
	}
	var add []string
	if !hasEntry {
		add = append(add, entry)
	}
	if relaxHome && !hasHomeRead {
		add = append(add, "ProtectHome=read-only")
	}
	lines = insertAfterService(lines, hasService, add...)

	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		return false, fmt.Errorf("write drop-in %s: %w", dropIn, err)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(dropIn); err == nil {
		mode = fi.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat existing drop-in %s: %w", dropIn, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dropIn), "."+filepath.Base(dropIn)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("write drop-in %s: %w", dropIn, err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	content := strings.Join(lines, "\n") + "\n"
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, fmt.Errorf("write drop-in %s: %w", dropIn, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("write drop-in %s: %w", dropIn, err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return false, fmt.Errorf("write drop-in %s: %w", dropIn, err)
	}
	return true, os.Rename(tmp.Name(), dropIn)
}

// insertAfterService places new lines inside the [Service] section, after
// its last occurrence; without a header the lines prepend together with
// one, so they are never emitted sectionless.
func insertAfterService(lines []string, hasService bool, add ...string) []string {
	out := make([]string, 0, len(lines)+len(add)+1)
	if !hasService {
		out = append(out, "[Service]")
		out = append(out, add...)
		return append(out, lines...)
	}
	at := 0
	for i, l := range lines {
		if l == "[Service]" {
			at = i
		}
	}
	out = append(out, lines[:at+1]...)
	out = append(out, add...)
	return append(out, lines[at+1:]...)
}
