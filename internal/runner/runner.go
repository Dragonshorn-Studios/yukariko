// Package runner is the only component allowed to execute local Git, Docker,
// Compose, and explicitly configured hook commands.
//
// Guarantees:
//
//   - Requests are argv-based and passed to the OS without shell
//     reinterpretation unless the caller explicitly opts into shell mode.
//   - Output is captured with hard size limits and marked when truncated.
//   - Timeout and cancellation terminate the child process tree as safely as
//     the platform supports and are reported as distinct statuses.
//   - A non-zero exit is never reported as success.
//   - Configured secret values and common credential patterns are redacted
//     from captured output, error text, log fields, and summaries.
//   - No network payload can ever select or provide a command: requests are
//     constructed by Yukariko from its own configuration.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Command outcome statuses.
const (
	StatusSuccess   = "success"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
)

// DefaultMaxOutputBytes bounds captured output per stream when a request
// does not specify a limit.
const DefaultMaxOutputBytes = 64 << 10

// waitDelay bounds how long Wait waits for output pipes to drain after a
// cancelled process tree has been signalled.
const waitDelay = 3 * time.Second

// Request describes one command to execute. Argv is mandatory; the first
// element is the executable. Shell opts into shell interpretation of the
// joined argv, which is a deliberate, documented risk.
type Request struct {
	Name           string
	Argv           []string
	Dir            string
	Env            []string // KEY=VALUE additions on top of the controlled allowlist
	Timeout        time.Duration
	Shell          bool
	MaxOutputBytes int64
	Redactions     *Redactor
	AppID          string
	DeploymentID   string
	SkipRedactArgv bool
}

// Result is the bounded outcome of one command.
type Result struct {
	Status          string
	ExitCode        int // only meaningful when Status is success or failed
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	StdoutBytes     int64 // full pre-truncation size
	StderrBytes     int64
	StartedAt       time.Time
	EndedAt         time.Time
	Err             string // redacted failure description, empty on success
}

// Summary is the safe, persistable form of one command run.
type Summary struct {
	Request    Request
	Result     Result
	ArgvJoined string // redacted display form
}

// Sink persists safe command summaries. It is implemented by adapters over
// the durable store; a nil sink means summaries are only logged.
type Sink interface {
	RecordCommandSummary(ctx context.Context, s Summary) error
}

// Runner executes commands under the guarantees of this package.
type Runner struct {
	Log  *slog.Logger
	Sink Sink
}

// envAllowlist names the variables children inherit from Yukariko's own
// environment. Everything else is dropped so secrets present in the parent
// environment never leak into command output. Additions from Request.Env are
// appended explicitly.
var envAllowlist = []string{
	"PATH", "HOME", "USERPROFILE",
	"TEMP", "TMP", "SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC",
	"LANG", "LC_ALL", "TERM",
	"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG",
	"SSH_AUTH_SOCK", "GNUPGHOME",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// Run executes the command and returns its bounded, redacted result. The
// returned error is reserved for request validation; process outcomes are
// carried by Result.Status so callers can never conflate "command failed"
// with "runner broke".
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return Result{}, fmt.Errorf("runner: request %q has an empty argv", req.Name)
	}
	if ctx.Err() != nil {
		return Result{Status: StatusCancelled, Err: ctx.Err().Error()}, nil
	}

	argv := req.Argv
	if req.Shell {
		argv = shellArgv(req.Argv)
	}

	limit := req.MaxOutputBytes
	if limit <= 0 {
		limit = DefaultMaxOutputBytes
	}
	redactor := req.Redactions
	if redactor == nil {
		redactor = NewRedactor(nil)
	}

	var timedOut atomic.Bool
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = buildEnv(req.Env)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = killTree(cmd.Process)
		}
		return nil // classification uses the context error
	}
	cmd.WaitDelay = waitDelay
	setSysProcAttr(cmd)

	stdout := newLimitedWriter(limit)
	stderr := newLimitedWriter(limit)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if r.Log != nil {
		r.Log.InfoContext(ctx, "command started",
			"name", req.Name, "app", req.AppID,
			"argv", redactor.String(displayArgv(req)), "dir", req.Dir,
			"deployment", req.DeploymentID)
	}

	result := Result{StartedAt: time.Now()}
	var waitErr error
	if startErr := cmd.Start(); startErr != nil {
		result.EndedAt = time.Now()
		result.Status = StatusFailed
		result.Err = redactor.String(startErr.Error())
		r.finish(ctx, req, result)
		return result, nil
	}
	var timer *time.Timer
	if req.Timeout > 0 {
		timer = time.AfterFunc(req.Timeout, func() {
			timedOut.Store(true)
			if cmd.Process != nil {
				_ = killTree(cmd.Process)
			}
		})
		defer timer.Stop()
	}
	waitErr = cmd.Wait()
	result.EndedAt = time.Now()
	result.Stdout, result.StdoutTruncated, result.StdoutBytes = stdout.snapshot()
	result.Stderr, result.StderrTruncated, result.StderrBytes = stderr.snapshot()
	result.Status, result.ExitCode, result.Err = classify(waitErr, ctx, timedOut.Load(), req)

	result.Stdout = redactor.Bytes(result.Stdout)
	result.Stderr = redactor.Bytes(result.Stderr)
	result.Err = redactor.String(result.Err)
	r.finish(ctx, req, result)
	return result, nil
}

// finish logs and (optionally) persists the outcome. Everything handed to
// logging or persistence has already been redacted.
func (r *Runner) finish(ctx context.Context, req Request, result Result) {
	if r.Log != nil {
		r.logResult(ctx, req, result)
	}
	if r.Sink != nil {
		redactor := req.Redactions
		if redactor == nil {
			redactor = NewRedactor(nil)
		}
		if err := r.Sink.RecordCommandSummary(ctx, Summary{
			Request:    req,
			Result:     result,
			ArgvJoined: redactor.String(displayArgv(req)),
		}); err != nil && r.Log != nil {
			r.Log.ErrorContext(ctx, "record command summary",
				"name", req.Name, "error", err)
		}
	}
}

func (r *Runner) logResult(ctx context.Context, req Request, res Result) {
	level := slog.LevelInfo
	if res.Status != StatusSuccess {
		level = slog.LevelWarn
	}
	attrs := []any{
		"name", req.Name, "app", req.AppID, "status", res.Status,
		"exit_code", res.ExitCode,
		"duration_ms", res.EndedAt.Sub(res.StartedAt).Milliseconds(),
		"stdout_bytes", res.StdoutBytes, "stderr_bytes", res.StderrBytes,
		"deployment", req.DeploymentID,
	}
	if res.Err != "" {
		attrs = append(attrs, "error", res.Err)
	}
	r.Log.Log(ctx, level, "command finished", attrs...)
}

// classify maps a wait error onto a runner status. A fired timeout timer
// wins over exit-code interpretation; cancellation from the caller is
// distinct from the request's own timeout.
func classify(waitErr error, ctx context.Context, timedOut bool, req Request) (status string, exitCode int, errMsg string) {
	switch {
	case waitErr == nil:
		return StatusSuccess, 0, ""
	case timedOut:
		return StatusTimeout, -1, fmt.Sprintf("timed out after %s", req.Timeout)
	case ctx.Err() != nil:
		return StatusCancelled, -1, "cancelled: " + ctx.Err().Error()
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		return StatusFailed, code, fmt.Sprintf("exit status %d", code)
	}
	return StatusFailed, -1, waitErr.Error()
}

// buildEnv assembles the child environment: the documented allowlist plus
// explicit additions. Additions must look like KEY=VALUE; they override
// allowlisted entries with the same name.
func buildEnv(additions []string) []string {
	seen := make(map[string]bool)
	env := make([]string, 0, len(envAllowlist)+len(additions))
	for _, entry := range os.Environ() {
		name, ok := envName(entry)
		if !ok || !slices.Contains(envAllowlist, name) || seen[name] {
			continue
		}
		seen[name] = true
		env = append(env, entry)
	}
	for _, entry := range additions {
		name, ok := envName(entry)
		if !ok || name == "" {
			continue // silently drop malformed entries; they are never user input
		}
		if !seen[name] {
			env = append(env, entry)
			seen[name] = true
			continue
		}
		// override: replace the allowlisted value
		for i, existing := range env {
			if existingName, _ := envName(existing); existingName == name {
				env[i] = entry
				break
			}
		}
	}
	return env
}

func envName(entry string) (string, bool) {
	if entry == "" {
		return "", false
	}
	if i := strings.IndexByte(entry, '='); i > 0 {
		return entry[:i], true
	}
	return "", false
}

// shellArgv wraps argv for explicit shell interpretation. The joined string
// is passed as one shell program; this is opt-in because it reintroduces
// shell metacharacter semantics (injection risk) that argv mode removes.
func shellArgv(argv []string) []string {
	joined := strings.Join(argv, " ")
	if runtime.GOOS == "windows" {
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = "cmd"
		}
		return []string{comspec, "/C", joined}
	}
	return []string{"/bin/sh", "-c", joined}
}

// displayArgv renders the request argv for logs and summaries. It is redacted
// by callers.
func displayArgv(req Request) string {
	if req.Shell {
		return "sh -c " + strconv.Quote(strings.Join(req.Argv, " "))
	}
	return strings.Join(req.Argv, " ")
}
