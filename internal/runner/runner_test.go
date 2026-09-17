package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// The tests exercise real child processes via a re-exec helper: when
// YUKARIKO_RUNNER_CHILD is set, the test binary behaves as the requested
// helper program instead of running tests.
func TestMain(m *testing.M) {
	if cmd := os.Getenv("YUKARIKO_RUNNER_CHILD"); cmd != "" {
		os.Exit(childMain(cmd))
	}
	os.Exit(m.Run())
}

func childMain(cmd string) int {
	switch {
	case cmd == "exit0":
		return 0
	case cmd == "exit3":
		fmt.Fprintln(os.Stderr, "child failure text")
		return 3
	case strings.HasPrefix(cmd, "echo:"):
		fmt.Println(strings.TrimPrefix(cmd, "echo:"))
		return 0
	case strings.HasPrefix(cmd, "env:"):
		name := strings.TrimPrefix(cmd, "env:")
		if val, ok := os.LookupEnv(name); ok {
			fmt.Printf("%s=%s\n", name, val)
		} else {
			fmt.Printf("%s=(unset)\n", name)
		}
		return 0
	case cmd == "cwd":
		wd, _ := os.Getwd()
		fmt.Println(wd)
		return 0
	case strings.HasPrefix(cmd, "sleep:"):
		d, _ := time.ParseDuration(strings.TrimPrefix(cmd, "sleep:"))
		time.Sleep(d)
		return 0
	case strings.HasPrefix(cmd, "bigout:"):
		var n int
		fmt.Sscanf(strings.TrimPrefix(cmd, "bigout:"), "%d", &n)
		chunk := bytes.Repeat([]byte("yukariko"), 128) // 1 KiB
		for i := 0; i < n; i++ {
			os.Stdout.Write(chunk)
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown helper %q\n", cmd)
		return 2
	}
}

// helperRequest builds a Request that re-executes the test binary as the
// given helper command. The child switch is passed through the controlled
// environment, proving that explicit additions reach the child.
func helperRequest(t *testing.T, name, helper string) Request {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot resolve test binary: %v", err)
	}
	return Request{
		Name: name,
		Argv: []string{exe},
		Env:  []string{"YUKARIKO_RUNNER_CHILD=" + helper},
	}
}

func TestSuccessExitZero(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), helperRequest(t, "ok", "exit0"))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Status != StatusSuccess || res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("result = %+v", res)
	}
	if res.EndedAt.Before(res.StartedAt) {
		t.Error("timestamps must be ordered")
	}
}

func TestNonZeroExitIsNeverSuccess(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), helperRequest(t, "fail", "exit3"))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}
	if res.Err == "" {
		t.Fatal("failure must carry a reason")
	}
}

func TestTimeoutClassifiedAndEnforced(t *testing.T) {
	r := &Runner{}
	req := helperRequest(t, "hang", "sleep:30s")
	req.Timeout = 300 * time.Millisecond
	start := time.Now()
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Status != StatusTimeout {
		t.Fatalf("status = %q (%s), want timeout", res.Status, res.Err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took %s; child was not terminated", elapsed)
	}
}

func TestCancellationClassified(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{}
	req := helperRequest(t, "hang", "sleep:30s")

	done := make(chan Result, 1)
	go func() {
		res, _ := r.Run(ctx, req)
		done <- res
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		if res.Status != StatusCancelled {
			t.Fatalf("status = %q, want cancelled", res.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not terminate the run")
	}
}

func TestOutputCapturedAndTruncated(t *testing.T) {
	r := &Runner{}
	req := helperRequest(t, "big", "bigout:200") // 200 KiB
	req.MaxOutputBytes = 4096
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSuccess {
		t.Fatalf("status = %q", res.Status)
	}
	if len(res.Stdout) != 4096 {
		t.Fatalf("stdout len = %d, want the 4096 limit", len(res.Stdout))
	}
	if !res.StdoutTruncated {
		t.Error("truncation must be flagged")
	}
	if res.StdoutBytes <= 4096 {
		t.Errorf("full size = %d, want > limit", res.StdoutBytes)
	}
}

func TestOutputExactlyAtLimitIsNotTruncated(t *testing.T) {
	r := &Runner{}
	req := helperRequest(t, "exact", "bigout:4") // exactly 4 KiB
	req.MaxOutputBytes = 4096
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StdoutTruncated || res.StdoutBytes != 4096 {
		t.Fatalf("truncated=%v bytes=%d; exact-size output must not be flagged", res.StdoutTruncated, res.StdoutBytes)
	}
}

func TestWorkingDirectoryRespected(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{}
	req := helperRequest(t, "cwd", "cwd")
	req.Dir = dir
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != dir {
		t.Fatalf("child cwd = %q, want %q", got, dir)
	}
}

func TestControlledEnvironment(t *testing.T) {
	t.Setenv("YUKARIKO_PARENT_ONLY", "leak-me-not")
	r := &Runner{}

	// Parent-only variables never reach the child. The variable name avoids
	// credential-shaped keywords so the output survives pattern redaction
	// and the absence is directly assertable.
	req := helperRequest(t, "env", "env:YUKARIKO_PARENT_ONLY")
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out := string(res.Stdout); !strings.Contains(out, "YUKARIKO_PARENT_ONLY=(unset)") {
		t.Fatalf("parent env leaked to child: %q", out)
	}
	if strings.Contains(string(res.Stdout), "leak-me-not") {
		t.Fatalf("parent env value leaked to child: %q", res.Stdout)
	}

	// Explicit additions are visible, including to the child switch itself.
	req2 := helperRequest(t, "env", "env:YUKARIKO_ADDED")
	req2.Env = append(req2.Env, "YUKARIKO_ADDED=visible")
	res2, err := r.Run(context.Background(), req2)
	if err != nil {
		t.Fatal(err)
	}
	if out := string(res2.Stdout); !strings.Contains(out, "YUKARIKO_ADDED=visible") {
		t.Fatalf("explicit addition missing: %q", out)
	}
}

func TestShellModeIsExplicit(t *testing.T) {
	r := &Runner{}
	req := Request{Name: "sh", Argv: []string{os.Args[0]}}
	req.Env = []string{"YUKARIKO_RUNNER_CHILD=echo:shell-ran"}
	req.Shell = true
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSuccess {
		t.Fatalf("shell mode status = %q (%s)", res.Status, res.Err)
	}
	if !strings.Contains(string(res.Stdout), "shell-ran") {
		t.Fatalf("shell output = %q", res.Stdout)
	}
}

func TestRedactionAcrossOutputs(t *testing.T) {
	const secret = "supersecret-value-1234"
	r := &Runner{}
	req := helperRequest(t, "echo", "echo:token=supersecret-value-1234 https://deploy:hunter42@example.com/deploy.git Authorization: Basic dXNlcjpwYXNz supersecret-value-1234")
	req.Redactions = NewRedactor([]string{secret})
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	if strings.Contains(out, secret) {
		t.Errorf("literal secret leaked: %q", out)
	}
	if strings.Contains(out, "hunter42") {
		t.Errorf("URL credentials leaked: %q", out)
	}
	if strings.Contains(out, "dXNlcjpwYXNz") {
		t.Errorf("authorization header leaked: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction markers: %q", out)
	}
}

func TestRedactorPatterns(t *testing.T) {
	r := NewRedactor([]string{"hunter2"})
	cases := []struct{ in, want string }{
		{"https://user:hunter2@host/path", "https://user:[REDACTED]@host/path"},
		{"git@github.com", "git@github.com"}, // scp-style syntax without a password is untouched
		{"Authorization: Bearer abc.def", "Authorization: [REDACTED]"},
		{"authorization:   xyz", "authorization: [REDACTED]"},
		{"X-API-Key: abcd1234", "X-API-Key: [REDACTED]"},
		{"?token=hunter2&x=1", "?token=[REDACTED]&x=1"},
		{"password=hunter2", "password=[REDACTED]"},
		{"normal text", "normal text"},
		{"hunter2", "[REDACTED]"},
		{"pw", "pw"}, // short values are never treated as secrets
	}
	for _, c := range cases {
		if got := r.String(c.in); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSinkReceivesRedactedSummary(t *testing.T) {
	const secret = "supersecret-value-1234"
	fake := &fakeSink{}
	r := &Runner{Sink: fake}
	req := helperRequest(t, "echo", "echo:sending supersecret-value-1234")
	req.Redactions = NewRedactor([]string{secret})
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(fake.summaries) != 1 {
		t.Fatalf("sink got %d summaries", len(fake.summaries))
	}
	s := fake.summaries[0]
	if s.ArgvJoined == "" || s.Result.Status != StatusSuccess {
		t.Fatalf("summary = %+v", s)
	}
	if strings.Contains(s.ArgvJoined, secret) ||
		strings.Contains(string(s.Result.Stdout), secret) {
		t.Fatal("summary carried the secret")
	}
}

func TestSlogOutputRedacted(t *testing.T) {
	const secret = "supersecret-value-1234"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := &Runner{Log: logger}
	req := helperRequest(t, "echo", "echo:logging supersecret-value-1234")
	req.Redactions = NewRedactor([]string{secret})
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if strings.Contains(logs, secret) {
		t.Fatalf("logs leaked secret: %s", logs)
	}
	for _, want := range []string{"command started", "command finished", "echo"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q: %s", want, logs)
		}
	}
}

func TestEmptyArgvRejected(t *testing.T) {
	r := &Runner{}
	if _, err := r.Run(context.Background(), Request{Name: "empty"}); err == nil {
		t.Fatal("empty argv must be a request error")
	}
}

func TestCancelledContextFailsFast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Runner{}
	res, err := r.Run(ctx, helperRequest(t, "x", "exit0"))
	if err != nil {
		t.Fatalf("runner error: %v", err)
	}
	if res.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", res.Status)
	}
}

func TestMissingExecutableIsFailedNotCrash(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), Request{
		Name: "nope",
		Argv: []string{"yukariko-definitely-not-a-binary-1234"},
	})
	if err != nil {
		t.Fatalf("runner error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
}

type fakeSink struct {
	summaries []Summary
}

func (f *fakeSink) RecordCommandSummary(_ context.Context, s Summary) error {
	f.summaries = append(f.summaries, s)
	return nil
}
