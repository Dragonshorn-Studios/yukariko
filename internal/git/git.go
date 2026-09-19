// Package git implements Git branch change detection and safe worktree
// preparation for git-source apps.
//
// # Update policy
//
// The deployed SHA never changes here. This package observes the remote
// branch and, when asked to prepare a deployment, moves the worktree with a
// fast-forward-only merge after the remote state is fetched and verified.
// Anything not provably safe — dirty worktree, diverged local commits,
// detached HEAD, missing remote or branch — is blocked with an actionable
// reason and zero data loss. `git reset --hard` and every other lossy or
// history-rewriting command are never used.
//
// # Credentials and sanitization
//
// All host Git credentials (credential helpers, ssh agents, .git/config)
// apply untouched; Yukariko never persists or logs them. Remote URLs never
// enter results: details name remotes by their configured NAME only, and
// command output passes through the runner's redaction before it can reach
// an error or log.
//
// The observed SHA is informational; the deployed SHA is advanced by the
// deployment pipeline (#11) only after the whole deploy and required
// post-deploy checks succeed.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// Status classifies one check or preparation outcome.
type Status string

const (
	// StatusUpToDate: the remote branch matches the local HEAD; nothing to
	// do and no worktree mutation.
	StatusUpToDate Status = "up_to_date"
	// StatusChanged: the remote branch advanced and the worktree can
	// fast-forward; the deploy pipeline may Prepare and deploy.
	StatusChanged Status = "changed"
	// StatusReady: the worktree is at the requested remote commit.
	StatusReady Status = "ready"
	// StatusBlocked: a local or configuration condition refuses the update;
	// nothing was touched. Requires user action.
	StatusBlocked Status = "blocked"
	// StatusError: a transient failure (network, auth, host); retry with
	// backoff.
	StatusError Status = "error"
)

// Result is the typed outcome fed into the scheduler and the deploy
// pipeline.
type Result struct {
	Status      Status
	ObservedSHA string // remote SHA seen this pass; never a deploy checkpoint
	PreparedSHA string // local HEAD after a successful Prepare
	Detail      string
}

// Client runs read-only Git inspection and fast-forward-only worktree
// preparation through the runner.
type Client struct {
	// Runner executes git commands. Its Sink must stay nil: git command
	// output is never persisted (it can carry paths and remote URLs).
	Runner *runner.Runner
	// Run overrides command execution (tests inject a recorder). When nil,
	// Runner.Run is used.
	Run func(ctx context.Context, req runner.Request) (runner.Result, error)
	// FetchTimeout bounds network commands (ls-remote, fetch); zero means
	// 2 minutes. Local commands get a shorter fixed budget.
	FetchTimeout time.Duration
}

const (
	fetchDefaultTimeout = 2 * time.Minute
	localTimeout        = 30 * time.Second
)

// Check resolves the configured remote branch and classifies the worktree
// against it. It is read-only unless the remote is actually ahead, in which
// case a fetch updates remote-tracking refs only — working tree, local
// branches, and the deployed SHA are never touched.
func (c *Client) Check(ctx context.Context, app *config.App) (Result, error) {
	src, err := gitSource(app)
	if err != nil {
		return Result{}, err
	}
	dir, branch, remote, blockedRes := worktreeParams(src)
	if blockedRes != "" {
		return Result{Status: StatusBlocked, Detail: blockedRes}, nil
	}
	if err := checkDir(dir); err != nil {
		return Result{Status: StatusBlocked, Detail: err.Error()}, nil
	}

	remoteSHA, err := c.lsRemote(ctx, dir, remote, branch)
	if err != nil {
		if isMissingRemote(err) {
			return blocked(fmt.Sprintf("remote %q not found in this repository; check the remote name and the repository's configuration", remote)), nil
		}
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	if remoteSHA == "" {
		return blocked(fmt.Sprintf("branch %q not found on remote %q; the branch may have been deleted or renamed", branch, remote)), nil
	}

	head, err := c.git(ctx, dir, localTimeout, "rev-parse", "HEAD")
	if err != nil {
		return Result{Status: StatusBlocked, Detail: "not a usable git repository: " + err.Error()}, nil
	}
	branchName, err := c.git(ctx, dir, localTimeout, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	if branchName == "HEAD" {
		return blocked(fmt.Sprintf("HEAD is detached; check out the configured branch %q first", branch)), nil
	}
	if remoteSHA == head {
		return Result{Status: StatusUpToDate, ObservedSHA: remoteSHA, Detail: "remote matches local HEAD"}, nil
	}
	dirty, err := c.isDirty(ctx, dir)
	if err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	if dirty {
		return blocked("worktree has uncommitted changes; commit or stash them before updating"), nil
	}

	// The remote is ahead: fetch so the remote commit exists locally and the
	// fast-forward question becomes decidable. This mutates remote-tracking
	// refs only.
	if err := c.fetch(ctx, dir, remote, branch); err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	ff, err := c.isAncestor(ctx, dir, head, remoteSHA)
	if err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	if !ff {
		return blocked(fmt.Sprintf("local branch %q has commits not on remote %q; resolve the divergence manually (Yukariko never rewrites history)", branchName, remote)), nil
	}
	return Result{
		Status:      StatusChanged,
		ObservedSHA: remoteSHA,
		Detail:      fmt.Sprintf("remote %q branch %q advanced; fast-forward possible", remote, branch),
	}, nil
}

// Prepare brings the worktree to the remote branch's current commit with a
// fast-forward-only merge. It re-validates every safety condition at run
// time; on success the worktree is exactly at the fetched commit. The
// deployed SHA is advanced by the deployment pipeline, never here.
func (c *Client) Prepare(ctx context.Context, app *config.App) (Result, error) {
	src, err := gitSource(app)
	if err != nil {
		return Result{}, err
	}
	dir, branch, remote, blockedRes := worktreeParams(src)
	if blockedRes != "" {
		return Result{Status: StatusBlocked, Detail: blockedRes}, nil
	}
	if err := checkDir(dir); err != nil {
		return Result{Status: StatusBlocked, Detail: err.Error()}, nil
	}

	if err := c.fetch(ctx, dir, remote, branch); err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	target, err := c.git(ctx, dir, localTimeout, "rev-parse", "FETCH_HEAD")
	if err != nil || target == "" {
		return Result{Status: StatusError, Detail: orDetail(errDetail(err), "fetch produced no commit")}, nil
	}

	head, err := c.git(ctx, dir, localTimeout, "rev-parse", "HEAD")
	if err != nil {
		return Result{Status: StatusBlocked, Detail: "not a usable git repository: " + err.Error()}, nil
	}
	branchName, err := c.git(ctx, dir, localTimeout, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	if branchName == "HEAD" {
		return blocked(fmt.Sprintf("HEAD is detached; check out the configured branch %q first", branch)), nil
	}
	if target == head {
		return Result{Status: StatusReady, ObservedSHA: target, PreparedSHA: head,
			Detail: "worktree already at the requested commit"}, nil
	}
	dirty, err := c.isDirty(ctx, dir)
	if err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	if dirty {
		return blocked("worktree has uncommitted changes; commit or stash them before updating"), nil
	}
	ff, err := c.isAncestor(ctx, dir, head, target)
	if err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	if !ff {
		return blocked(fmt.Sprintf("local branch %q has commits not on remote %q; resolve the divergence manually (Yukariko never rewrites history)", branchName, remote)), nil
	}

	if _, err := c.git(ctx, dir, localTimeout, "merge", "--ff-only", target); err != nil {
		return Result{Status: StatusError, Detail: "fast-forward merge failed: " + err.Error()}, nil
	}
	newHead, err := c.git(ctx, dir, localTimeout, "rev-parse", "HEAD")
	if err != nil {
		return Result{Status: StatusError, Detail: err.Error()}, nil
	}
	if newHead != target {
		return Result{Status: StatusError, Detail: fmt.Sprintf("worktree did not reach the requested commit: at %s, want %s", newHead, target)}, nil
	}
	return Result{
		Status:      StatusReady,
		ObservedSHA: target,
		PreparedSHA: newHead,
		Detail:      "worktree fast-forwarded to " + target,
	}, nil
}

// --- git plumbing -----------------------------------------------------------

func gitSource(app *config.App) (*config.GitSource, error) {
	if app == nil || app.Source.Mode != config.SourceGit || app.Source.Git == nil {
		return nil, errors.New("app is not configured as a git source")
	}
	return app.Source.Git, nil
}

// worktreeParams extracts and sanity-checks the static parameters. The
// third return is a blocked detail when the configuration itself is
// insufficient.
func worktreeParams(src *config.GitSource) (dir, branch, remote, blockedDetail string) {
	dir = src.Dir
	if dir == "" {
		return "", "", "", "git worktree directory is unset"
	}
	branch = src.Branch
	if branch == "" {
		return "", "", "", "git branch is unset"
	}
	remote = src.Remote
	if remote == "" {
		remote = "origin" // matches the config default
	}
	return dir, branch, remote, ""
}

func checkDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("git worktree %s: %v", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("git worktree %s is not a directory", dir)
	}
	return nil
}

func (c *Client) lsRemote(ctx context.Context, dir, remote, branch string) (string, error) {
	out, err := c.git(ctx, dir, c.fetchBudget(), "ls-remote", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

func (c *Client) fetch(ctx context.Context, dir, remote, branch string) error {
	_, err := c.git(ctx, dir, c.fetchBudget(), "fetch", remote, branch)
	return err
}

// isDirty reports modifications to tracked files (staged or unstaged).
// Untracked files are common in real worktrees (compose overrides, local
// config) and cannot conflict with a fast-forward that does not touch them;
// if one ever would, the merge itself refuses safely and the pass fails
// without data loss.
func (c *Client) isDirty(ctx context.Context, dir string) (bool, error) {
	out, err := c.git(ctx, dir, localTimeout, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// isAncestor reports whether ancestor is contained in descendant. The
// descendant must exist locally (fetch first).
func (c *Client) isAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	res, err := c.run(ctx, dir, localTimeout, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return false, err
	}
	switch {
	case res.Status == runner.StatusSuccess:
		return true, nil
	case res.Status == runner.StatusFailed && res.ExitCode == 1:
		return false, nil // documented exit code: not an ancestor
	default:
		return false, errors.New(failureText(res))
	}
}

func (c *Client) git(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	res, err := c.run(ctx, dir, timeout, args...)
	if err != nil {
		return "", err
	}
	if res.Status != runner.StatusSuccess {
		return "", errors.New(failureText(res))
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

func (c *Client) run(ctx context.Context, dir string, timeout time.Duration, args ...string) (runner.Result, error) {
	r := c.Runner
	if r == nil {
		r = &runner.Runner{}
	}
	req := runner.Request{
		Name:    "git " + args[0],
		Argv:    append([]string{"git", "-c", "safe.directory=" + dir, "-C", dir}, args...),
		Timeout: timeout,
	}
	if c.Run != nil {
		return c.Run(ctx, req)
	}
	return r.Run(ctx, req)
}

// failureText renders one command failure. Output was already redacted by
// the runner (credential URLs and tokens scrubbed) before it reaches here.
func failureText(res runner.Result) string {
	if s := strings.Join(strings.Fields(string(res.Stderr)), " "); s != "" {
		return s
	}
	return res.Err
}

func (c *Client) fetchBudget() time.Duration {
	if c.FetchTimeout > 0 {
		return c.FetchTimeout
	}
	return fetchDefaultTimeout
}

// isMissingRemote recognizes a remote name that is not configured in the
// worktree — a local configuration problem (blocked), not a transient
// network failure (error).
func isMissingRemote(err error) bool {
	s := err.Error()
	return strings.Contains(s, "does not appear to be a git repository") ||
		strings.Contains(s, "No such remote")
}

func blocked(detail string) Result {
	return Result{Status: StatusBlocked, Detail: detail}
}

func errDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func orDetail(detail, fallback string) string {
	if detail == "" {
		return fallback
	}
	return detail
}
