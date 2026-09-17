package learn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// GitProber answers read-only questions about a Git worktree. It is the
// learn package's only filesystem/Git dependency; production uses
// RunnerGitProber, tests use fakes. Implementations must never mutate
// worktree or repository state.
type GitProber interface {
	// Probe reports whether dir is inside a Git worktree and, if so, its
	// current branch and remote. dir itself must exist; probing a
	// directory without .git is InWorkTree=false with a nil error.
	Probe(ctx context.Context, dir string) (GitInfo, error)
}

// GitInfo is the read-only evidence of one probe.
type GitInfo struct {
	InWorkTree    bool
	Detached      bool // HEAD is detached; Branch is then unusable
	Branch        string
	RemoteName    string // "" when the expected remote does not exist
	RemoteURL     string // credential-stripped; informational
	CredsStripped bool
}

// probeTimeout bounds each git invocation; read-only plumbing answers in
// milliseconds on a healthy machine.
const probeTimeout = 10 * time.Second

// RunnerGitProber implements GitProber with read-only git commands executed
// through the runner: `git rev-parse` for the worktree/branch and
// `git remote get-url` for the remote. It never mutates anything and only
// probes the exact directory it is given.
type RunnerGitProber struct {
	Runner *runner.Runner
	// Remote is the remote name consulted; empty means "origin", matching
	// the config default for git sources.
	Remote string
}

var _ GitProber = (*RunnerGitProber)(nil)

// Probe implements GitProber. All output and error text passes through the
// runner's redaction before it can reach a proposal.
func (p *RunnerGitProber) Probe(ctx context.Context, dir string) (GitInfo, error) {
	if dir == "" {
		return GitInfo{}, errors.New("empty directory")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return GitInfo{}, fmt.Errorf("git probe: %w", err)
	}
	if !fi.IsDir() {
		return GitInfo{}, fmt.Errorf("git probe: %s is not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if os.IsNotExist(err) {
			return GitInfo{}, nil // no worktree here; explicit, not an error
		}
		return GitInfo{}, fmt.Errorf("git probe: %w", err)
	}

	out, err := p.git(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return GitInfo{}, fmt.Errorf("git probe: %w", err)
	}
	if strings.TrimSpace(out) != "true" {
		// e.g. a bare repository: not a usable worktree.
		return GitInfo{}, nil
	}
	info := GitInfo{InWorkTree: true}

	branch, err := p.git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return GitInfo{}, fmt.Errorf("git probe: %w", err)
	}
	branch = strings.TrimSpace(branch)
	if branch == "HEAD" {
		info.Detached = true
	} else {
		info.Branch = branch
	}

	url, err := p.git(ctx, dir, "remote", "get-url", p.remoteName())
	if err != nil {
		return info, nil // a worktree without the remote is normal, not an error
	}
	info.RemoteName = p.remoteName()
	info.RemoteURL, info.CredsStripped = stripURLCredentials(strings.TrimSpace(url))
	return info, nil
}

func (p *RunnerGitProber) remoteName() string {
	if p.Remote != "" {
		return p.Remote
	}
	return "origin"
}

// git runs one read-only git command through the runner. Failure text is
// already redacted by the runner; it is collapsed here for one-line errors.
func (p *RunnerGitProber) git(ctx context.Context, dir string, args ...string) (string, error) {
	r := p.Runner
	if r == nil {
		r = &runner.Runner{}
	}
	res, err := r.Run(ctx, runner.Request{
		Name:    "git " + args[0],
		Argv:    append([]string{"git", "-C", dir}, args...),
		Timeout: probeTimeout,
	})
	if err != nil {
		return "", err
	}
	if res.Status != runner.StatusSuccess {
		detail := strings.Join(strings.Fields(string(res.Stderr)), " ")
		if detail == "" {
			detail = res.Err
		}
		return "", fmt.Errorf("git %s in %s: %s", args[0], dir, detail)
	}
	return string(res.Stdout), nil
}

// credURLPattern matches scheme://user:password@ prefixes. scp-style
// remotes (git@host:path) carry no password and are left untouched, as are
// passwordless userinfo URLs such as ssh://git@host/path.
var credURLPattern = regexp.MustCompile(`^([a-z][a-z0-9+.-]*://)([^/@\s:]+):([^@\s/]+)@`)

// stripURLCredentials removes scheme://user:password@ userinfo from a Git
// remote URL and reports whether anything was stripped. The returned URL is
// safe for proposals, logs, and YAML; the stripped credentials keep working
// through the host's Git credential storage.
func stripURLCredentials(raw string) (string, bool) {
	if m := credURLPattern.FindStringSubmatch(raw); m != nil {
		return m[1] + raw[len(m[0]):], true
	}
	return raw, false
}
