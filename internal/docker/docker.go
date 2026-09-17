// Package docker implements read-only discovery of the host's Docker state:
// a container inventory, Compose project grouping, and recreation-relevant
// container detail.
//
// # Docker daemon access is host-equivalent privilege
//
// Any process able to reach the Docker daemon — /var/run/docker.sock, a
// tcp:// endpoint, or a remote context — can bind-mount arbitrary host paths
// and run privileged containers, which is effectively root on the machine.
// Granting a user or binary access to the daemon therefore grants that
// privilege. To minimize exposure, run Yukariko under a dedicated local
// account, prefer the local unix socket over tcp:// or ssh:// endpoints,
// scope socket access to a single group, and prefer rootless Docker where
// feasible. See docs/docker-access.md for the full discussion.
//
// # Read-only guarantee
//
// The CLI client in this package executes only the read verbs `docker ps`
// and `docker inspect`, through internal/runner in argv mode (no shell
// reinterpretation). The Client interface exposes no mutating method, so
// discovery cannot start, stop, pull, create, or remove anything.
//
// # Secret hygiene
//
// `docker inspect` output contains raw environment values. Discovery
// therefore records environment variable NAMES only; values never enter
// results, errors, logs, or storage. For the same reason callers must not
// attach a persistence Sink to the runner driving this package: discovery
// command output is bounded-logged by the runner but never recorded.
package docker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// Classified failure modes. Each Error produced by this package wraps
// exactly one of these sentinels (or none, for unclassified command
// failures), so callers can branch with errors.Is.
var (
	// ErrDaemonUnreachable means the Docker CLI could not reach a daemon.
	ErrDaemonUnreachable = errors.New("docker daemon unreachable")
	// ErrPermissionDenied means the daemon socket rejected the current user.
	ErrPermissionDenied = errors.New("docker daemon socket permission denied")
	// ErrClientMissing means the docker executable was not found.
	ErrClientMissing = errors.New("docker command not found")
	// ErrContainerMissing means the inspected container does not exist,
	// typically because it was removed between listing and inspection.
	ErrContainerMissing = errors.New("container not found")
	// ErrTimeout means a discovery command exceeded its timeout.
	ErrTimeout = errors.New("docker command timed out")
	// ErrInvalidOutput means the CLI answered with unparseable output.
	ErrInvalidOutput = errors.New("unparseable docker command output")
)

// Error codes carried by Error.
const (
	CodeDaemonUnreachable = "daemon_unreachable"
	CodePermissionDenied  = "permission_denied"
	CodeClientMissing     = "client_missing"
	CodeContainerMissing  = "container_missing"
	CodeTimeout           = "timeout"
	CodeInvalidOutput     = "invalid_output"
	CodeCommandFailed     = "command_failed"
)

// Error is a classified, sanitized discovery failure. Detail is a bounded
// excerpt of the Docker CLI's own output (already redacted by the runner);
// Hint is an actionable remediation.
type Error struct {
	Code   string
	Detail string
	Hint   string
	cause  error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("docker: ")
	b.WriteString(e.Code)
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Hint != "" {
		b.WriteString(" (")
		b.WriteString(e.Hint)
		b.WriteString(")")
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.cause }

// Client is the narrow, read-only Docker interface used by discovery and
// later consumers (learn proposals, health checks). Implementations must
// never mutate Docker state.
type Client interface {
	// ListContainers returns the container inventory. When all is true,
	// stopped containers are included.
	ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error)
	// InspectContainer returns recreation-relevant detail for one container,
	// addressed by ID or name.
	InspectContainer(ctx context.Context, idOrName string) (ContainerDetail, error)
}

// CLIClient is the concrete Client backed by the host's Docker CLI. It is
// Yukariko's only Docker access path: no Docker SDK, no socket client.
type CLIClient struct {
	// Binary is the Docker CLI executable; empty means "docker" from PATH.
	Binary string
	// Runner executes the commands. Per the package secret-hygiene rule its
	// Sink must stay nil for discovery: command output can contain secret
	// values and is never persisted.
	Runner *runner.Runner
	// Timeout bounds each CLI invocation; zero means 30s.
	Timeout time.Duration
}

var _ Client = (*CLIClient)(nil)

const defaultTimeout = 30 * time.Second

func (c *CLIClient) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "docker"
}

func (c *CLIClient) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

func (c *CLIClient) runner() *runner.Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return &runner.Runner{}
}

// listArgv builds the read-only argv for the container inventory.
func (c *CLIClient) listArgv(all bool) []string {
	argv := []string{c.binary(), "ps"}
	if all {
		argv = append(argv, "-a")
	}
	return append(argv, "--format", "{{json .}}")
}

// inspectArgv builds the read-only argv for one container inspection.
// --type pins resolution to containers so a name collision with an image
// cannot return the wrong object.
func (c *CLIClient) inspectArgv(idOrName string) []string {
	return []string{c.binary(), "inspect", "--type", "container", idOrName}
}

// ListContainers implements Client via `docker ps --format {{json .}}`.
func (c *CLIClient) ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	res, err := c.run(ctx, "docker ps", c.listArgv(all))
	if err != nil {
		return nil, err
	}
	return parsePSOutput(res.Stdout)
}

// InspectContainer implements Client via `docker inspect --type container`.
func (c *CLIClient) InspectContainer(ctx context.Context, idOrName string) (ContainerDetail, error) {
	if idOrName == "" {
		return ContainerDetail{}, &Error{Code: CodeCommandFailed, Detail: "empty container id"}
	}
	res, err := c.run(ctx, "docker inspect", c.inspectArgv(idOrName))
	if err != nil {
		return ContainerDetail{}, err
	}
	return parseInspectOutput(res.Stdout)
}

// run executes one docker invocation through the runner and classifies any
// failure. The runner performs redaction; classification adds actionable,
// sanitized context on top. The returned error is non-nil only for
// unclassifiable runner-level problems.
func (c *CLIClient) run(ctx context.Context, name string, argv []string) (runner.Result, error) {
	res, err := c.runner().Run(ctx, runner.Request{
		Name:    name,
		Argv:    argv,
		Timeout: c.timeout(),
	})
	if err != nil {
		return res, err
	}
	if cerr := classify(ctx, res); cerr != nil {
		return res, cerr
	}
	return res, nil
}

// classify maps a runner result onto this package's typed errors. A
// successful result yields nil; cancellation surfaces the context error.
func classify(ctx context.Context, res runner.Result) error {
	switch res.Status {
	case runner.StatusSuccess:
		return nil
	case runner.StatusTimeout:
		return &Error{
			Code:   CodeTimeout,
			Detail: collapse(res.Err),
			Hint:   "raise the timeout if the daemon is slow to answer",
			cause:  ErrTimeout,
		}
	case runner.StatusCancelled:
		if ctx.Err() != nil {
			return ctx.Err()
		}
	case runner.StatusFailed:
		return classifyFailure(res)
	}
	return &Error{Code: CodeCommandFailed, Detail: collapse(res.Err)}
}

// classifyFailure recognizes the Docker CLI's common failure texts. The
// detail is bounded and was already redacted by the runner.
func classifyFailure(res runner.Result) error {
	detail := failureDetail(res)
	low := strings.ToLower(string(res.Stderr) + "\n" + res.Err)
	switch {
	case strings.Contains(low, "cannot connect to the docker daemon"),
		strings.Contains(low, "is the docker daemon running"),
		strings.Contains(low, "error during connect"):
		return &Error{
			Code:   CodeDaemonUnreachable,
			Detail: detail,
			Hint:   "start the daemon (e.g. `systemctl start docker`) or point DOCKER_HOST at a reachable endpoint",
			cause:  ErrDaemonUnreachable,
		}
	case strings.Contains(low, "permission denied"):
		return &Error{
			Code:   CodePermissionDenied,
			Detail: detail,
			Hint:   "grant this user read access to the Docker socket (e.g. docker group membership); socket access is root-equivalent, so scope it narrowly",
			cause:  ErrPermissionDenied,
		}
	case strings.Contains(low, "no such container"):
		return &Error{
			Code:   CodeContainerMissing,
			Detail: detail,
			Hint:   "the container was removed between listing and inspection",
			cause:  ErrContainerMissing,
		}
	case strings.Contains(low, "executable file not found"),
		strings.Contains(low, "no such file or directory"):
		return &Error{
			Code:   CodeClientMissing,
			Detail: detail,
			Hint:   "install the Docker CLI or correct PATH",
			cause:  ErrClientMissing,
		}
	default:
		return &Error{Code: CodeCommandFailed, Detail: detail}
	}
}

// maxDetailLen bounds the failure detail carried by Error.
const maxDetailLen = 512

func failureDetail(res runner.Result) string {
	if s := collapse(string(res.Stderr)); s != "" {
		return excerpt(s)
	}
	return excerpt(collapse(res.Err))
}

// collapse turns multi-line CLI output into a single log-friendly line.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// excerpt bounds a detail string on rune boundaries.
func excerpt(s string) string {
	runes := []rune(s)
	if len(runes) <= maxDetailLen {
		return s
	}
	return string(runes[:maxDetailLen]) + "…"
}
