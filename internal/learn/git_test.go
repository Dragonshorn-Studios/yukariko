package learn

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

func TestStripURLCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in        string
		want      string
		wantStrip bool
	}{
		{"https://user:pass@example.com/o/r.git", "https://example.com/o/r.git", true},
		{"https://deploy:token123@example.com/o/r.git", "https://example.com/o/r.git", true},
		{"http://u:p@host:8080/o/r.git", "http://host:8080/o/r.git", true},
		{"ssh://git@github.com/o/r.git", "ssh://git@github.com/o/r.git", false},
		{"git@github.com:o/r.git", "git@github.com:o/r.git", false},
		{"https://github.com/o/r.git", "https://github.com/o/r.git", false},
		{"file:///srv/repo", "file:///srv/repo", false},
	}
	for _, tc := range tests {
		got, stripped := stripURLCredentials(tc.in)
		if got != tc.want || stripped != tc.wantStrip {
			t.Errorf("stripURLCredentials(%q) = %q, %v; want %q, %v",
				tc.in, got, stripped, tc.want, tc.wantStrip)
		}
	}
}

// requireGit skips the real-git tests when no git binary is available.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// gitRun runs one git command in dir; only used to build fixtures.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// newRepo creates a temp worktree on branch main with two commits and the
// given remotes (name=url, credential-bearing URLs allowed).
func newRepo(t *testing.T, remotes ...[2]string) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	gitRun(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "first")
	gitRun(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "second")
	for _, r := range remotes {
		gitRun(t, dir, "remote", "add", r[0], r[1])
	}
	return dir
}

func TestRunnerGitProberRealRepo(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, [2]string{"origin", "https://deploy:hunter2@example.com/example/repo.git"})
	p := &RunnerGitProber{}
	info, err := p.Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !info.InWorkTree || info.Detached || info.Branch != "main" {
		t.Errorf("info = %+v, want a worktree on main", info)
	}
	if info.RemoteName != "origin" {
		t.Errorf("RemoteName = %q", info.RemoteName)
	}
	if info.RemoteURL != "https://example.com/example/repo.git" || !info.CredsStripped {
		t.Errorf("RemoteURL = %q stripped=%v, want credentials stripped", info.RemoteURL, info.CredsStripped)
	}
}

func TestRunnerGitProberDetachedHead(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	gitRun(t, dir, "checkout", "--detach", "HEAD")
	info, err := (&RunnerGitProber{}).Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !info.Detached || info.Branch != "" {
		t.Errorf("info = %+v, want detached with no branch", info)
	}
}

func TestRunnerGitProberNotAWorktree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	info, err := (&RunnerGitProber{}).Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.InWorkTree {
		t.Errorf("info = %+v, want InWorkTree=false for a plain directory", info)
	}
}

func TestRunnerGitProberNoRemote(t *testing.T) {
	t.Parallel()
	dir := newRepo(t) // no remotes
	info, err := (&RunnerGitProber{}).Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.RemoteName != "" || info.RemoteURL != "" {
		t.Errorf("info = %+v, want no remote", info)
	}
}

func TestRunnerGitProberCustomRemoteName(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, [2]string{"upstream", "https://example.com/example/repo.git"})
	info, err := (&RunnerGitProber{Remote: "upstream"}).Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.RemoteName != "upstream" || info.RemoteURL != "https://example.com/example/repo.git" {
		t.Errorf("info = %+v", info)
	}
}

func TestRunnerGitProberMissingDir(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := (&RunnerGitProber{}).Probe(context.Background(), missing); err == nil {
		t.Error("probing a missing directory must error")
	}
	if _, err := (&RunnerGitProber{}).Probe(context.Background(), ""); err == nil {
		t.Error("probing an empty directory must error")
	}
}

// TestProposalsNeverContainRemoteCredentials is the end-to-end secret-leak
// regression: a credential-bearing remote URL must never reach a proposal.
func TestProposalsNeverContainRemoteCredentials(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, [2]string{"origin", "https://deploy:hunter2@example.com/example/repo.git"})
	report := &docker.Report{
		Projects: []*docker.ComposeProject{
			composeProject("webapp", dir, []string{filepath.Join(dir, "compose.yaml")},
				composeMember("aaaa1111aaaa", "webapp-web-1", "web", "nginx:1.27", nil)),
		},
	}
	proposals := Proposals(context.Background(), report, &RunnerGitProber{})
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d", len(proposals))
	}
	p := proposals[0]
	if p.Compose.Git == nil || p.Compose.Git.RemoteURL != "https://example.com/example/repo.git" {
		t.Fatalf("GitEvidence = %+v, want a credential-stripped URL", p.Compose.Git)
	}
	blob := fmt.Sprintf("%+v", p)
	if strings.Contains(blob, "hunter2") || strings.Contains(blob, "deploy:") {
		t.Error("credential leaked into the proposal's string form")
	}
}
