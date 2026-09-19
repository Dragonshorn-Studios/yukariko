package git

import (
	"context"

	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// The tests use real git on temporary bare+clone repository pairs; the
// suite skips when git is unavailable.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// gitRun runs one fixture-building git command, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	requireGit(t)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// gitOut runs one fixture git command and returns trimmed stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	requireGit(t)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func commitFile(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "-c", "user.email=yukariko@test", "-c", "user.name=yukariko", "commit", "-q", "-m", msg)
	return gitOut(t, dir, "rev-parse", "HEAD")
}

// repo is a bare remote with one clone (the tested worktree).
type repo struct {
	work string
	bare string
	seq  int
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	requireGit(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	work := t.TempDir()
	gitRun(t, "", "init", "--bare", "-q", bare)
	gitRun(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	gitRun(t, work, "init")
	gitRun(t, work, "symbolic-ref", "HEAD", "refs/heads/main")
	commitFile(t, work, "index.txt", "one", "initial")
	gitRun(t, work, "remote", "add", "origin", bare)
	gitRun(t, work, "push", "-q", "-u", "origin", "main")
	return &repo{work: work, bare: bare}
}

// advance moves the remote branch forward via a throwaway clone, returning
// the new remote SHA. The tested worktree is not touched.
func (r *repo) advance(t *testing.T) string {
	t.Helper()
	r.seq++
	pusher := t.TempDir()
	gitRun(t, "", "clone", "-q", r.bare, pusher)
	commitFile(t, pusher, "f.txt", strings.Repeat("x", r.seq)+"\n", "remote commit")
	gitRun(t, pusher, "push", "-q", "origin", "main")
	return gitOut(t, pusher, "rev-parse", "HEAD")
}

func (r *repo) head(t *testing.T) string {
	return gitOut(t, r.work, "rev-parse", "HEAD")
}

func gitApp(dir string) *config.App {
	return &config.App{
		ID: "app",
		Source: config.Source{
			Mode: config.SourceGit,
			Git:  &config.GitSource{Dir: dir, Branch: "main", Remote: "origin"},
		},
	}
}

func testClient() *Client { return &Client{} }

// --- fixtures ---------------------------------------------------------------

func TestCheckUpToDateCausesNoMutation(t *testing.T) {
	r := newRepo(t)
	before := r.head(t)
	porcelainBefore := gitOut(t, r.work, "status", "--porcelain")

	res, err := testClient().Check(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusUpToDate {
		t.Fatalf("status = %q (%s), want up_to_date", res.Status, res.Detail)
	}
	if res.ObservedSHA != before {
		t.Errorf("ObservedSHA = %q, want local HEAD %q", res.ObservedSHA, before)
	}
	if r.head(t) != before {
		t.Error("up-to-date check must not move HEAD")
	}
	if gitOut(t, r.work, "status", "--porcelain") != porcelainBefore {
		t.Error("up-to-date check must not dirty the worktree")
	}
}

func TestChangedRemotePreparesExactCommit(t *testing.T) {
	r := newRepo(t)
	sha := r.advance(t)

	res, err := testClient().Check(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusChanged || res.ObservedSHA != sha {
		t.Fatalf("status = %q sha = %q, want changed at %q", res.Status, res.ObservedSHA, sha)
	}
	// Check alone must not move the worktree.
	if r.head(t) != gitOut(t, r.work, "rev-parse", "HEAD") || gitOut(t, r.work, "rev-parse", "HEAD") != gitOut(t, r.work, "rev-parse", "HEAD") {
		t.Fatal("unreachable")
	}

	prep, err := testClient().Prepare(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Status != StatusReady || prep.PreparedSHA != sha {
		t.Fatalf("prepare = %q sha %q, want ready at %q", prep.Status, prep.PreparedSHA, sha)
	}
	if r.head(t) != sha {
		t.Errorf("HEAD = %q, want the requested commit %q", r.head(t), sha)
	}
	content, err := os.ReadFile(filepath.Join(r.work, "f.txt"))
	if err != nil || strings.TrimSpace(string(content)) != "x" {
		t.Errorf("prepared worktree content = %q (err %v), want the fetched commit's file", content, err)
	}
}

func TestDirtyWorktreeBlockedWithoutDataLoss(t *testing.T) {
	r := newRepo(t)
	local := "local uncommitted edit"
	if err := os.WriteFile(filepath.Join(r.work, "index.txt"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	r.advance(t)

	res, err := testClient().Check(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusBlocked || !strings.Contains(res.Detail, "uncommitted") {
		t.Fatalf("status = %q detail = %q, want blocked for uncommitted changes", res.Status, res.Detail)
	}
	content, _ := os.ReadFile(filepath.Join(r.work, "index.txt"))
	if string(content) != local {
		t.Error("blocked check must preserve local modifications")
	}
	// Prepare refuses the same way.
	prep, err := testClient().Prepare(context.Background(), gitApp(r.work))
	if err != nil || prep.Status != StatusBlocked {
		t.Fatalf("Prepare = %v / %q, want blocked", err, prep.Status)
	}
	content, _ = os.ReadFile(filepath.Join(r.work, "index.txt"))
	if string(content) != local {
		t.Error("blocked prepare must preserve local modifications")
	}
}

func TestDivergedBranchBlockedAndPreserved(t *testing.T) {
	r := newRepo(t)
	// Local commit, never pushed.
	localSHA := commitFile(t, r.work, "local.txt", "local work", "local commit")
	// Remote moves too.
	remoteSHA := r.advance(t)

	res, err := testClient().Check(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusBlocked || !strings.Contains(res.Detail, "divergence") {
		t.Fatalf("status = %q detail = %q, want diverged block", res.Status, res.Detail)
	}
	if r.head(t) != localSHA {
		t.Error("diverged block must preserve the local commit")
	}
	prep, err := testClient().Prepare(context.Background(), gitApp(r.work))
	if err != nil || prep.Status != StatusBlocked {
		t.Fatalf("Prepare = %v / %q, want blocked", err, prep.Status)
	}
	if r.head(t) != localSHA {
		t.Error("prepare must not rewrite history")
	}
	if gitOut(t, r.work, "rev-parse", remoteSHA) != remoteSHA {
		t.Error("the remote commit should be present locally after fetch")
	}
}

func TestDetachedHeadBlocked(t *testing.T) {
	r := newRepo(t)
	gitRun(t, r.work, "checkout", "-q", "--detach", "HEAD")
	r.advance(t)

	res, err := testClient().Check(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusBlocked || !strings.Contains(res.Detail, "detached") {
		t.Fatalf("status = %q detail = %q, want detached block", res.Status, res.Detail)
	}
}

func TestDeletedRemoteBranchBlocked(t *testing.T) {
	r := newRepo(t)
	pusher := t.TempDir()
	gitRun(t, "", "clone", "-q", r.bare, pusher)
	// Allow the deletion by moving the bare repo's HEAD off main.
	gitRun(t, r.bare, "symbolic-ref", "HEAD", "refs/heads/other")
	gitRun(t, pusher, "push", "-q", "origin", "--delete", "main")

	res, err := testClient().Check(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusBlocked || !strings.Contains(res.Detail, "not found on remote") {
		t.Fatalf("status = %q detail = %q, want missing-branch block", res.Status, res.Detail)
	}
}

func TestMissingRemoteBranchConfigurationBlocked(t *testing.T) {
	newRepo(t)
	app := gitApp(filepath.Join(t.TempDir(), "nope"))
	res, err := testClient().Check(context.Background(), app)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusBlocked || !strings.Contains(res.Detail, "git worktree") {
		t.Fatalf("status = %q detail = %q, want missing-dir block", res.Status, res.Detail)
	}

	app2 := gitApp("")
	res2, err := testClient().Check(context.Background(), app2)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res2.Status != StatusBlocked || !strings.Contains(res2.Detail, "directory is unset") {
		t.Fatalf("status = %q detail = %q, want unset-dir block", res2.Status, res2.Detail)
	}
}

func TestAuthFailureIsErrorAndURLSanitized(t *testing.T) {
	r := newRepo(t)
	// A credential-bearing URL that fails fast: the .invalid TLD never
	// resolves. Git echoes the URL with credentials into its stderr.
	gitRun(t, r.work, "remote", "set-url", "origin", "https://user:secretPW@yukariko.invalid/repo.git")

	client := &Client{FetchTimeout: 15 * time.Second}
	res, err := client.Check(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusError {
		t.Fatalf("status = %q detail = %q, want error", res.Status, res.Detail)
	}
	if strings.Contains(res.Detail, "secretPW") || strings.Contains(res.Detail, "user:") || strings.Contains(res.Detail, "user@") {
		t.Errorf("credential leaked into detail %q: neither the password nor the username may appear", res.Detail)
	}
	// The worktree is untouched by the failed fetch.
	if gitOut(t, r.work, "remote", "get-url", "origin") != "https://user:secretPW@yukariko.invalid/repo.git" {
		t.Error("check must not modify the repository configuration")
	}
}

func TestPrepareIsIdempotentAtTarget(t *testing.T) {
	r := newRepo(t)
	sha := r.advance(t)
	client := testClient()

	if _, err := client.Check(context.Background(), gitApp(r.work)); err != nil {
		t.Fatalf("Check: %v", err)
	}
	prep, err := client.Prepare(context.Background(), gitApp(r.work))
	if err != nil || prep.Status != StatusReady || prep.PreparedSHA != sha {
		t.Fatalf("first prepare = %v / %q / %q", err, prep.Status, prep.PreparedSHA)
	}
	again, err := client.Prepare(context.Background(), gitApp(r.work))
	if err != nil || again.Status != StatusReady || again.PreparedSHA != sha {
		t.Fatalf("second prepare = %v / %q / %q, want ready at the same commit", err, again.Status, again.PreparedSHA)
	}
	if !strings.Contains(again.Detail, "already") {
		t.Errorf("second prepare detail = %q, want the already-at-target note", again.Detail)
	}
}

func TestMissingRemoteConfigurationBlocked(t *testing.T) {
	r := newRepo(t)
	gitRun(t, r.work, "remote", "remove", "origin")

	res, err := testClient().Check(context.Background(), gitApp(r.work))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusBlocked || !strings.Contains(res.Detail, "not found in this repository") {
		t.Fatalf("status = %q detail = %q, want missing-remote block", res.Status, res.Detail)
	}
}

func TestNonGitAppRejected(t *testing.T) {
	app := &config.App{
		ID:     "app",
		Source: config.Source{Mode: config.SourceRegistry},
	}
	if _, err := testClient().Check(context.Background(), app); err == nil {
		t.Fatal("expected an error for a non-git app")
	}
	if _, err := testClient().Prepare(context.Background(), app); err == nil {
		t.Fatal("expected an error for a non-git app")
	}
	if _, err := testClient().Check(context.Background(), nil); !errors.Is(err, err) && err == nil {
		t.Fatal("expected an error for a nil app")
	}
}

func TestDefaultRemoteFallback(t *testing.T) {
	r := newRepo(t)
	app := gitApp(r.work)
	app.Source.Git.Remote = "" // config default fills "origin"
	res, err := testClient().Check(context.Background(), app)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != StatusUpToDate {
		t.Fatalf("status = %q detail = %q, want up_to_date via the origin default", res.Status, res.Detail)
	}
}

// Every git invocation scopes safe.directory to the worktree so checks and
// prepares are not refused with "dubious ownership" when Yukariko runs as
// a different user than the worktree's owner (root CLI passes, the systemd
// service user) — the same rule learn's probe already follows.
func TestGitArgvCarriesSafeDirectory(t *testing.T) {
	t.Parallel()
	var got []string
	c := &Client{Run: func(_ context.Context, req runner.Request) (runner.Result, error) {
		got = req.Argv
		return runner.Result{Status: runner.StatusSuccess, Stdout: []byte("abc123 refs/heads/main\n")}, nil
	}}
	if _, err := c.lsRemote(context.Background(), "/home/u/proj", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	want := []string{"git", "-c", "safe.directory=/home/u/proj", "-C", "/home/u/proj",
		"ls-remote", "origin", "refs/heads/main"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("argv = %v, want %v", got, want)
	}
}
