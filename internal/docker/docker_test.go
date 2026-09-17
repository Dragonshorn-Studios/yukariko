package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// The only argv this package may ever produce are the read verbs ps and
// inspect. These tests encode that contract at the argv-builder level.
func TestListArgvReadOnly(t *testing.T) {
	t.Parallel()
	for _, all := range []bool{true, false} {
		argv := (&CLIClient{Binary: "docker"}).listArgv(all)
		if argv[0] != "docker" {
			t.Errorf("argv[0] = %q, want docker", argv[0])
		}
		if argv[1] != "ps" {
			t.Errorf("argv[1] = %q, want the read verb ps", argv[1])
		}
		if all && !contains(argv, "-a") {
			t.Error("all=true must add -a so stopped containers are returned")
		}
		if !contains(argv, "{{json .}}") {
			t.Error("missing --format {{json .}}")
		}
		assertNoMutationVerb(t, argv)
	}
}

func TestInspectArgvReadOnly(t *testing.T) {
	t.Parallel()
	argv := (&CLIClient{Binary: "docker"}).inspectArgv("abc123")
	if argv[1] != "inspect" {
		t.Errorf("argv[1] = %q, want the read verb inspect", argv[1])
	}
	if !contains(argv, "--type") || !contains(argv, "container") {
		t.Error("inspect must pin --type container to avoid image name collisions")
	}
	if argv[len(argv)-1] != "abc123" {
		t.Errorf("last argv = %q, want the container reference", argv[len(argv)-1])
	}
	assertNoMutationVerb(t, argv)
}

func assertNoMutationVerb(t *testing.T, argv []string) {
	t.Helper()
	mutators := []string{
		"run", "exec", "start", "stop", "restart", "rm", "rmi", "kill",
		"pull", "push", "create", "update", "pause", "unpause", "prune",
		"up", "down", "commit", "cp", "rename", "scale",
	}
	for _, m := range mutators {
		if contains(argv, m) {
			t.Errorf("argv contains mutation verb %q: %v", m, argv)
		}
	}
}

func contains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func TestParsePSOutput(t *testing.T) {
	t.Parallel()
	const ndjson = `{"Command":"\"docker-entrypoint.…\"","CreatedAt":"2026-09-01 10:00:00 +0000 UTC","ID":"aaaa1111aaaa","Image":"nginx:1.27","Labels":"com.docker.compose.project=webapp","Names":"webapp-web-1","Networks":"webapp_default","Ports":"0.0.0.0:8080->80/tcp","State":"running","Status":"Up 2 weeks"}
{"Command":"\"/bin/sh\"","CreatedAt":"2026-07-11 21:30:00 +0000 UTC","ID":"dddd4444dddd","Image":"alpine:3.20","Labels":"","Names":"legacy-name,backup-shell","Networks":"bridge","Ports":"","State":"exited","Status":"Exited (0) 3 days ago"}
{"Command":"\"x\"","CreatedAt":"garbage","ID":"eeee5555eeee","Image":"app:1","Names":"weird","State":"running","Status":"Up"}
`
	rows, err := parsePSOutput([]byte(ndjson))
	if err != nil {
		t.Fatalf("parsePSOutput: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (running and stopped alike)", len(rows))
	}
	web := rows[0]
	if web.ID != "aaaa1111aaaa" || web.Name != "webapp-web-1" || web.Image != "nginx:1.27" ||
		web.State != "running" || web.StatusText != "Up 2 weeks" {
		t.Errorf("web row = %+v", web)
	}
	wantCreated := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if !web.CreatedAt.Equal(wantCreated) {
		t.Errorf("CreatedAt = %v, want %v", web.CreatedAt, wantCreated)
	}
	stopped := rows[1]
	if stopped.State != "exited" {
		t.Errorf("stopped container must be returned, got state %q", stopped.State)
	}
	if stopped.Name != "legacy-name" {
		t.Errorf("Name = %q, want the primary name only", stopped.Name)
	}
	weird := rows[2]
	if !weird.CreatedAt.IsZero() {
		t.Errorf("unparseable timestamp must yield the zero time, got %v", weird.CreatedAt)
	}
	if len(weird.Anomalies) != 1 || !strings.Contains(weird.Anomalies[0], "unparseable created timestamp") {
		t.Errorf("Anomalies = %v, want an explicit unparseable-timestamp anomaly", weird.Anomalies)
	}
}

func TestParsePSOutputInvalid(t *testing.T) {
	t.Parallel()
	for name, out := range map[string]string{
		"not json":       "this is not json\n",
		"truncated json": `{"ID":`,
		"wrong shape":    `["not","an","object"]`,
	} {
		_, err := parsePSOutput([]byte(out))
		if !errors.Is(err, ErrInvalidOutput) {
			t.Errorf("%s: err = %v, want ErrInvalidOutput", name, err)
		}
	}
	// An object with no fields parses but must be flagged, never dropped.
	rows, err := parsePSOutput([]byte("{}\n"))
	if err != nil || len(rows) != 1 {
		t.Fatalf("empty object: rows=%v err=%v, want one flagged row", rows, err)
	}
	if rows[0].ID != "" || len(rows[0].Anomalies) != 1 ||
		!strings.Contains(rows[0].Anomalies[0], "ps row without an ID") {
		t.Errorf("empty row = %+v, want an explicit missing-ID anomaly", rows[0])
	}
	// An empty inventory is valid: a host without containers.
	rows, err = parsePSOutput(nil)
	if err != nil || len(rows) != 0 {
		t.Errorf("empty output: rows=%v err=%v, want empty success", rows, err)
	}
}

// webInspectJSON is a realistic `docker inspect` document for a Compose web
// service: health configured, bind+volume mounts, a published and an exposed
// port, one network, and environment entries that include a secret value.
const webInspectJSON = `[
  {
    "Id": "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111",
    "Name": "/webapp-web-1",
    "Created": "2026-09-01T10:00:00.123456789Z",
    "Image": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "Config": {
      "Image": "nginx:1.27",
      "User": "101",
      "WorkingDir": "/usr/share/nginx/html",
      "Cmd": ["nginx", "-g", "daemon off;"],
      "Entrypoint": ["/docker-entrypoint.sh"],
      "Env": [
        "PATH=/usr/local/sbin:/usr/local/bin",
        "NGINX_VERSION=1.27.0",
        "TZ=Europe/Oslo",
        "DATABASE_URL=postgres://app:hunter2@db/app",
        "BROKEN_ENTRY"
      ],
      "Labels": {
        "com.docker.compose.project": "webapp",
        "com.docker.compose.service": "web",
        "com.docker.compose.project.working_dir": "/srv/webapp",
        "com.docker.compose.project.config_files": "/srv/webapp/compose.yaml,/srv/webapp/compose.override.yaml",
        "com.docker.compose.container-number": "1",
        "com.docker.compose.oneoff": "False"
      }
    },
    "State": {
      "Status": "running",
      "Running": true,
      "Paused": false,
      "Restarting": false,
      "Health": {"Status": "healthy", "FailingStreak": 0}
    },
    "HostConfig": {
      "RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
      "NetworkMode": "webapp_default"
    },
    "Mounts": [
      {"Type": "bind", "Source": "/srv/webapp/site", "Destination": "/usr/share/nginx/html", "Mode": "", "RW": true, "Propagation": "rprivate"},
      {"Type": "volume", "Name": "webapp_cache", "Source": "/var/lib/docker/volumes/webapp_cache/_data", "Destination": "/var/cache/nginx", "Mode": "z", "RW": true, "Propagation": ""}
    ],
    "NetworkSettings": {
      "Ports": {
        "443/tcp": null,
        "80/tcp": [{"HostIP": "0.0.0.0", "HostPort": "8080"}]
      },
      "Networks": {
        "webapp_default": {"NetworkID": "net1111aaaa1111", "Aliases": ["web", "aaaa1111aaaa"]}
      }
    }
  }
]`

func mustParseInspect(t *testing.T, doc string) ContainerDetail {
	t.Helper()
	d, err := parseInspectOutput([]byte(doc))
	if err != nil {
		t.Fatalf("parseInspectOutput: %v", err)
	}
	return d
}

func TestParseInspectOutput(t *testing.T) {
	t.Parallel()
	det := mustParseInspect(t, webInspectJSON)
	if det.ID != "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111" {
		t.Errorf("ID = %q", det.ID)
	}
	if det.Name != "webapp-web-1" {
		t.Errorf("Name = %q, want the leading slash stripped", det.Name)
	}
	if det.ImageRef != "nginx:1.27" || det.ImageID != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Errorf("image ref/id = %q / %q", det.ImageRef, det.ImageID)
	}
	wantCreated := time.Date(2026, 9, 1, 10, 0, 0, 123456789, time.UTC)
	if !det.CreatedAt.Equal(wantCreated) {
		t.Errorf("CreatedAt = %v, want %v", det.CreatedAt, wantCreated)
	}
	if det.State != "running" || !det.Running || det.Paused || det.Restarting {
		t.Errorf("state = %+v", det)
	}
	if !det.Health.Configured || det.Health.Status != "healthy" || det.Health.FailingStreak != 0 {
		t.Errorf("health = %+v", det.Health)
	}
	if det.RestartPolicy != "unless-stopped" || det.NetworkMode != "webapp_default" {
		t.Errorf("restart/network = %q / %q", det.RestartPolicy, det.NetworkMode)
	}
	if det.User != "101" || det.WorkingDir != "/usr/share/nginx/html" {
		t.Errorf("user/workdir = %q / %q", det.User, det.WorkingDir)
	}
	if strings.Join(det.Cmd, "|") != "nginx|-g|daemon off;" {
		t.Errorf("Cmd = %v", det.Cmd)
	}
	if strings.Join(det.Entrypoint, "|") != "/docker-entrypoint.sh" {
		t.Errorf("Entrypoint = %v", det.Entrypoint)
	}
	wantEnv := []string{"PATH", "NGINX_VERSION", "TZ", "DATABASE_URL"}
	if strings.Join(det.EnvKeys, ",") != strings.Join(wantEnv, ",") {
		t.Errorf("EnvKeys = %v, want %v (docker order, names only)", det.EnvKeys, wantEnv)
	}
	if det.Labels[LabelProject] != "webapp" || det.Labels[LabelService] != "web" {
		t.Errorf("Labels = %v", det.Labels)
	}
	if len(det.Mounts) != 2 {
		t.Fatalf("Mounts = %+v, want bind + volume", det.Mounts)
	}
	bind, vol := det.Mounts[0], det.Mounts[1]
	if bind.Type != "bind" || bind.Source != "/srv/webapp/site" ||
		bind.Destination != "/usr/share/nginx/html" || !bind.RW {
		t.Errorf("bind mount = %+v", bind)
	}
	if vol.Type != "volume" || vol.Name != "webapp_cache" || !vol.RW {
		t.Errorf("volume mount = %+v", vol)
	}
	if len(det.Ports) != 2 {
		t.Fatalf("Ports = %+v, want published + exposed", det.Ports)
	}
	// Keys are processed sorted: 443 before 80.
	if p := det.Ports[0]; p.ContainerPort != "443" || p.Proto != "tcp" || p.Published {
		t.Errorf("exposed port = %+v, want Published=false, not dropped", p)
	}
	if p := det.Ports[1]; p.ContainerPort != "80" || !p.Published ||
		p.HostIP != "0.0.0.0" || p.HostPort != "8080" {
		t.Errorf("published port = %+v", p)
	}
	if len(det.Networks) != 1 {
		t.Fatalf("Networks = %+v", det.Networks)
	}
	net := det.Networks[0]
	if net.Name != "webapp_default" || strings.Join(net.Aliases, ",") != "aaaa1111aaaa,web" {
		t.Errorf("network = %+v, want aliases sorted", net)
	}
	// The malformed env entry is an explicit anomaly, never a fabricated key.
	if len(det.Anomalies) != 1 || !strings.Contains(det.Anomalies[0], "env entry without a NAME=value shape") {
		t.Errorf("Anomalies = %v, want the malformed-env anomaly", det.Anomalies)
	}
}

// TestInspectNeverCarriesEnvValues pins the secret-hygiene rule: docker
// inspect carries raw environment values, discovery results must not.
func TestInspectNeverCarriesEnvValues(t *testing.T) {
	t.Parallel()
	det := mustParseInspect(t, webInspectJSON)
	c := &Container{ContainerSummary: ContainerSummary{ID: "aaaa1111aaaa"}, Detail: &det}
	for _, blob := range []string{fmt.Sprintf("%+v", det), fmt.Sprintf("%+v", *c)} {
		if strings.Contains(blob, "hunter2") || strings.Contains(blob, "postgres://app:") {
			t.Error("environment values leaked into the discovery record")
		}
	}
}

func TestParseInspectEmptyOutput(t *testing.T) {
	t.Parallel()
	_, err := parseInspectOutput([]byte("[]"))
	if !errors.Is(err, ErrInvalidOutput) {
		t.Errorf("err = %v, want ErrInvalidOutput for an empty object list", err)
	}
	_, err = parseInspectOutput(nil)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Errorf("err = %v, want ErrInvalidOutput for empty output", err)
	}
}

func TestClassifyFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		res      runner.Result
		wantErr  error // nil = must not match any sentinel
		wantCode string
	}{
		{
			name:    "success",
			res:     runner.Result{Status: runner.StatusSuccess},
			wantErr: nil,
		},
		{
			name: "daemon unreachable",
			res: runner.Result{Status: runner.StatusFailed, Stderr: []byte(
				"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")},
			wantErr:  ErrDaemonUnreachable,
			wantCode: CodeDaemonUnreachable,
		},
		{
			name: "permission denied",
			res: runner.Result{Status: runner.StatusFailed, Stderr: []byte(
				"permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock")},
			wantErr:  ErrPermissionDenied,
			wantCode: CodePermissionDenied,
		},
		{
			name: "container missing",
			res: runner.Result{Status: runner.StatusFailed, Stderr: []byte(
				"Error: No such container: ghost")},
			wantErr:  ErrContainerMissing,
			wantCode: CodeContainerMissing,
		},
		{
			name:     "client missing",
			res:      runner.Result{Status: runner.StatusFailed, Err: `exec: "docker": executable file not found in $PATH`},
			wantErr:  ErrClientMissing,
			wantCode: CodeClientMissing,
		},
		{
			name:     "unclassified failure",
			res:      runner.Result{Status: runner.StatusFailed, ExitCode: 1, Stderr: []byte("something odd happened")},
			wantErr:  nil,
			wantCode: CodeCommandFailed,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classify(context.Background(), tc.res)
			if tc.name == "success" {
				if err != nil {
					t.Fatalf("classify(success) = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("classify returned nil for a failure result")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				for _, sentinel := range []error{ErrDaemonUnreachable, ErrPermissionDenied, ErrContainerMissing, ErrClientMissing, ErrTimeout} {
					if errors.Is(err, sentinel) {
						t.Errorf("unclassified failure matched sentinel %v", sentinel)
					}
				}
			}
			var derr *Error
			if !errors.As(err, &derr) {
				t.Fatalf("classify error is not *Error: %T", err)
			}
			if derr.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", derr.Code, tc.wantCode)
			}
			if derr.Hint == "" && tc.wantErr != nil {
				t.Error("classified failures must carry an actionable hint")
			}
			if len(derr.Detail) > maxDetailLen*4+4 {
				t.Errorf("Detail unreasonably long: %d bytes", len(derr.Detail))
			}
		})
	}
}

func TestClassifyTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	err := classify(context.Background(), runner.Result{Status: runner.StatusTimeout, Err: "timed out after 30s"})
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("timeout err = %v, want ErrTimeout", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = classify(ctx, runner.Result{Status: runner.StatusCancelled, Err: "cancelled: context canceled"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation err = %v, want context.Canceled", err)
	}
}

func TestFailureDetailBounded(t *testing.T) {
	t.Parallel()
	res := runner.Result{
		Status: runner.StatusFailed,
		Stderr: []byte(strings.Repeat("x", 4*maxDetailLen) + "\nsecond line"),
	}
	e, ok := classify(context.Background(), res).(*Error)
	if !ok {
		t.Fatalf("want *Error, got %T", classify(context.Background(), res))
	}
	if runes := len([]rune(e.Detail)); runes > maxDetailLen+1 {
		t.Errorf("Detail = %d runes, want ≤ %d", runes, maxDetailLen+1)
	}
	if strings.Contains(e.Detail, "\n") {
		t.Error("Detail must be collapsed to one line for logs")
	}
}
