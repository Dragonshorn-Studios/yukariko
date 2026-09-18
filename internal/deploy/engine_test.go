package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// argvRecorder captures every request the engine would execute.
type argvRecorder struct {
	reqs []runner.Request
}

func (r *argvRecorder) record(_ context.Context, req runner.Request) (runner.Result, error) {
	r.reqs = append(r.reqs, req)
	return runner.Result{Status: runner.StatusSuccess}, nil
}

func (r *argvRecorder) joined() string {
	var lines []string
	for _, req := range r.reqs {
		lines = append(lines, strings.Join(req.Argv, " "))
	}
	return strings.Join(lines, "\n")
}

// TestCLIEngineVerbArgv pins the engine's argv contract: every verb leads
// with the docker binary (a #12 regression — the verbs used to execute
// argv[0]="pull" directly), endpoint flags sit between binary and verb, and
// Run passes the pipeline's argv through untouched.
func TestCLIEngineVerbArgv(t *testing.T) {
	t.Parallel()
	rec := &argvRecorder{}
	e := &CLIEngine{
		Exec:        rec.record,
		GlobalFlags: []string{"--context", "rootless"},
	}
	ctx := context.Background()

	if err := e.Stop(ctx, "app-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Rename(ctx, "app-1", "app-1-old"); err != nil {
		t.Fatal(err)
	}
	if err := e.ConnectNetwork(ctx, "net1", "app-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(ctx, "app-1-old"); err != nil {
		t.Fatal(err)
	}
	runArgv := []string{"docker", "--context", "rootless", "run", "-d", "--name", "app-1", "app:1"}
	if err := e.Run(ctx, runArgv, []string{"SECRET=x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, 2*time.Minute, "pull", "app:1"); err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"docker --context rootless stop app-1",
		"docker --context rootless rename app-1 app-1-old",
		"docker --context rootless network connect net1 app-1",
		"docker --context rootless rm app-1-old",
		"docker --context rootless run -d --name app-1 app:1",
		"docker --context rootless pull app:1",
	}, "\n")
	if got := rec.joined(); got != want {
		t.Fatalf("argv =\n%s\nwant\n%s", got, want)
	}
	// The secret env travels only with the run invocation.
	last := rec.reqs[len(rec.reqs)-2] // the Run request, before the pull
	if len(last.Env) != 1 || last.Env[0] != "SECRET=x" {
		t.Fatalf("run env = %v, want the secret pass-through", last.Env)
	}
}

// Without endpoint flags the verbs address the default daemon byte-for-byte
// as before this feature existed.
func TestCLIEngineVerbArgvDefaultDaemon(t *testing.T) {
	t.Parallel()
	rec := &argvRecorder{}
	e := &CLIEngine{Exec: rec.record}
	if err := e.Stop(context.Background(), "app-1"); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.joined(), "docker stop app-1"; got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}
