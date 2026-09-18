package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
)

// Endpoint is one local Docker daemon addressable by the CLI. The implicit
// default daemon (Name and Host empty) carries no flags; every other entry
// selects its daemon by context name.
type Endpoint struct {
	// Name is the Docker context name; empty for the implicit default.
	Name string
	// Host is the daemon's endpoint URL (e.g.
	// unix:///run/user/1000/docker.sock); empty for the implicit default.
	Host string
}

// Flags returns the docker CLI global flags selecting this endpoint, or nil
// for the implicit default.
func (e Endpoint) Flags() []string {
	if e.Name == "" {
		return nil
	}
	return []string{"--context", e.Name}
}

// LocalEndpoints lists every local Docker daemon addressable from this
// machine: the invoking user's default daemon first, then one entry per
// additional local Docker context, deduplicated by host URL. Remote
// endpoints (ssh://, tcp://) are excluded — Yukariko is a local agent.
func (c *CLIClient) LocalEndpoints(ctx context.Context) ([]Endpoint, error) {
	res, err := c.run(ctx, "docker context ls", append([]string{c.binary(), "context", "ls"}, "--format", "{{json .}}"))
	if err != nil {
		return nil, err
	}
	return parseContexts(res.Stdout), nil
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
		out = append(out, Endpoint{Name: row.Name, Host: row.Endpoints.Docker.Host})
	}
	return out
}

// isLocalHost reports whether the endpoint URL addresses a local daemon.
func isLocalHost(host string) bool {
	return strings.HasPrefix(host, "unix://") && len(host) > len("unix://") ||
		strings.HasPrefix(host, "npipe://") && len(host) > len("npipe://")
}
