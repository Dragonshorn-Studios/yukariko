package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/git"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// fakeGitRunner answers git.Check's two queries so the worktree always sits
// at the remote SHA — the post-cancel state where prepare merged but the
// deployment never checkpointed.
type fakeGitRunner struct{ sha string }

func (f *fakeGitRunner) run(_ context.Context, req runner.Request) (runner.Result, error) {
	joined := strings.Join(req.Argv, " ")
	switch {
	case strings.Contains(joined, "ls-remote"):
		return runner.Result{Status: runner.StatusSuccess, Stdout: []byte(f.sha + " refs/heads/main\n")}, nil
	case strings.Contains(joined, "rev-parse"):
		return runner.Result{Status: runner.StatusSuccess, Stdout: []byte(f.sha + "\n")}, nil
	}
	return runner.Result{Status: runner.StatusSuccess}, nil
}

// A cancelled-after-merge pass must not silently forget the deployment:
// the check owes one until the deployed-version checkpoint catches up.
func TestGitCheckOwesDeploymentUntilCheckpointed(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sha := "0123456789abcdef0123456789abcdef01234567"
	fake := &fakeGitRunner{sha: sha}
	checker := &SourceChecker{
		store: st,
		git:   &git.Client{Run: fake.run},
	}

	// Never checkpointed: deployment owed even though the worktree matches.
	workDir := t.TempDir()
	app := &config.App{
		ID:     "web",
		Source: config.Source{Mode: config.SourceGit, Git: &config.GitSource{Dir: workDir, Branch: "main", Remote: "origin"}},
	}
	res, err := checker.Check(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || !strings.Contains(res.Detail, "deployment owed") {
		t.Fatalf("res = changed=%v detail=%q, want owed", res.Changed, res.Detail)
	}

	// Checkpoint the observed SHA: nothing owed anymore.
	depID, err := st.BeginDeployment(context.Background(), store.BeginDeploymentParams{
		AppID: "web", Cause: "test", At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitDeploymentSuccess(context.Background(), "web", store.KindGitSHA, sha, depID, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err = checker.Check(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatalf("res = changed with matching checkpoint (detail %q)", res.Detail)
	}

	// Remote moves again: changed via the normal path.
	fake.sha = "89abcdef0123456789abcdef0123456789abcdef"
	res, err = checker.Check(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatalf("res = unchanged after remote advanced (detail %q)", res.Detail)
	}
}
