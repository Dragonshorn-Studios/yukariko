package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Endpoint is one local Docker daemon addressable by the CLI. The implicit
// default daemon (Name and Host empty) carries no flags; context-derived
// entries select their daemon by --context name, socket-derived entries by
// -H host URL.
type Endpoint struct {
	// Name labels the endpoint in scan output and ID disambiguation: the
	// Docker context name, "rootless-<uid>", or "" for the implicit default.
	Name string
	// Host is the daemon's endpoint URL (e.g.
	// unix:///run/user/1000/docker.sock); empty for the implicit default.
	Host string
	// Flags are the docker CLI global flags selecting this endpoint; nil
	// for the implicit default.
	Flags []string
}

// LocalEndpoints lists every local Docker daemon reachable through the
// invoking user's Docker contexts: the default daemon first, then one
// entry per additional local context, deduplicated by host URL. Remote
// endpoints (ssh://, tcp://) are excluded — Yukariko is a local agent.
//
// Contexts are per-user (~/.docker/contexts): a rootless daemon usually has
// NO context entry for the invoking root or service user. Pair with
// RootlessSockets, which finds such daemons by their sockets instead.
func (c *CLIClient) LocalEndpoints(ctx context.Context) ([]Endpoint, error) {
	res, err := c.run(ctx, "docker context ls", append([]string{c.binary(), "context", "ls"}, "--format", "{{json .}}"))
	if err != nil {
		return nil, err
	}
	return parseContexts(res.Stdout), nil
}

// RootlessSockets lists rootless Docker daemon sockets under /run/user,
// addressed directly by host URL — no Docker context required. This is the
// same ground truth the installer's rootless hint uses.
func RootlessSockets() []Endpoint {
	return socketsUnder("/run/user")
}

// socketsUnder lists <dir>/<uid>/docker.sock entries that are actual
// sockets. Best effort: unreadable or non-socket entries are skipped.
func socketsUnder(runUserDir string) []Endpoint {
	matches, err := filepath.Glob(filepath.Join(runUserDir, "*", "docker.sock"))
	if err != nil {
		return nil
	}
	var out []Endpoint
	for _, sock := range matches {
		fi, err := os.Stat(sock)
		if err != nil || fi.Mode()&os.ModeSocket == 0 {
			continue
		}
		uid := filepath.Base(filepath.Dir(sock))
		host := "unix://" + sock
		out = append(out, Endpoint{
			Name:  "rootless-" + uid,
			Host:  host,
			Flags: []string{"-H", host},
		})
	}
	return out
}

// parseContexts extracts local, distinct endpoints from `docker context ls
// --format {{json .}}` output (one JSON object per line). The "default"
// context is folded into the implicit first entry; contexts pointing at the
// same host as an already-seen endpoint are skipped; unparsable lines are
// ignored — listing is advisory for discovery, never a gate.
func parseContexts(stdout []byte) []Endpoint {
	out := []Endpoint{{}} // implicit default daemon, no flags
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(stdout)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row struct {
			Name      string `json:"Name"`
			Endpoints struct {
				Docker struct {
					Host string `json:"Host"`
				} `json:"docker"`
			} `json:"Endpoints"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.Name == "" {
			continue
		}
		// The default context IS the implicit first entry; mark its host as
		// seen so aliases of the default daemon are collapsed too.
		if row.Name == "default" {
			if row.Endpoints.Docker.Host != "" {
				seen[row.Endpoints.Docker.Host] = true
			}
			continue
		}
		if seen[row.Endpoints.Docker.Host] {
			continue
		}
		if !isLocalHost(row.Endpoints.Docker.Host) {
			continue
		}
		seen[row.Endpoints.Docker.Host] = true
		out = append(out, Endpoint{
			Name:  row.Name,
			Host:  row.Endpoints.Docker.Host,
			Flags: []string{"--context", row.Name},
		})
	}
	return out
}

// isLocalHost reports whether the endpoint URL addresses a local daemon.
func isLocalHost(host string) bool {
	return strings.HasPrefix(host, "unix://") && len(host) > len("unix://") ||
		strings.HasPrefix(host, "npipe://") && len(host) > len("npipe://")
}
