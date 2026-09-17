package learn

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

// secretKeyPattern flags environment variable names that look like secrets.
// It only shapes the TODO hint; every env key requires a manual value or
// reference regardless, because values were never captured.
var secretKeyPattern = regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key|access_?key|private_?key|credential)`)

// standaloneCandidate classifies one standalone container: the observed
// launch spec plus a reproducibility verdict. Anything the standalone spec
// cannot represent faithfully, or that is ambiguous, refuses auto-import.
func standaloneCandidate(c *docker.Container) candidate {
	pr := &Proposal{
		Mode:            config.DeployStandalone,
		SystemCandidate: c.SystemCandidate,
		SystemReasons:   copyStrings(c.SystemReasons),
		Standalone: &StandaloneProposal{
			ContainerName: c.Name,
			Labels:        map[string]string{},
		},
	}
	if c.Detail == nil {
		pr.block("container was not inspectable during discovery; no reproducible spec exists")
		pr.ID = slug(c.Name, appIDMaxLen)
		pr.finalize()
		return candidate{proposal: pr, base: pr.ID, key: "standalone/" + c.Name}
	}
	d := c.Detail
	sp := pr.Standalone

	// Compose management is never a standalone app.
	if c.OneOff {
		pr.block("compose one-off container (leftover of `docker compose run`); not a deployable app")
	}
	if d.Labels[docker.LabelProject] != "" || d.Labels[docker.LabelService] != "" {
		pr.block("container carries Compose labels; import the Compose project instead of this container")
	}

	// Image evidence and pinning. Unlike Compose (where the compose file
	// owns the refs), the imported spec is what will be re-run verbatim, so
	// mutable references are explicit confirmations.
	if d.ImageRef == "" {
		pr.block("inspect reports no image reference")
	} else {
		sp.Image = d.ImageRef
		switch pinState(d.ImageRef) {
		case pinNone:
			pr.confirm("standalone.image",
				fmt.Sprintf("image %s has no tag or digest; docker would assume :latest", d.ImageRef))
		case pinLatest:
			pr.confirm("standalone.image",
				fmt.Sprintf("image tag of %s is 'latest' (mutable); pin a version or digest for reproducible deploys", d.ImageRef))
		}
	}

	// Namespace and privilege semantics the spec must preserve; anything
	// shared beyond the container refuses auto-import.
	if d.Privileged {
		pr.block("container runs privileged; recreating it would silently drop host-level access")
	}
	switch sharedMode(d.PidMode) {
	case sharedHost:
		pr.block("shares the host PID namespace (pid: host)")
	case sharedContainer:
		pr.block(fmt.Sprintf("shares the PID namespace of another container (pid: %s)", d.PidMode))
	}
	switch sharedMode(d.IpcMode) {
	case sharedHost:
		pr.block("shares the host IPC namespace (ipc: host)")
	case sharedContainer:
		pr.block(fmt.Sprintf("shares the IPC namespace of another container (ipc: %s)", d.IpcMode))
	}
	if strings.HasPrefix(d.NetworkMode, "container:") || strings.HasPrefix(d.NetworkMode, "service:") {
		pr.block(fmt.Sprintf("shares another container's network namespace (network_mode: %s)", d.NetworkMode))
	}

	// Restart policy. An absent policy is docker's implicit "no" — the
	// semantic equivalent, not a fabricated value.
	switch d.RestartPolicy {
	case "":
		sp.Restart = "no"
	case "no", "always", "unless-stopped", "on-failure":
		sp.Restart = d.RestartPolicy
	default:
		pr.block(fmt.Sprintf("unknown restart policy %q", d.RestartPolicy))
	}

	// Mounts become binds; anything not representable refuses import.
	for _, m := range d.Mounts {
		switch m.Type {
		case "bind":
			sp.Binds = append(sp.Binds, bindString(m.Source, m.Destination, m.RW))
		case "volume":
			if m.Name == "" {
				pr.block(fmt.Sprintf("anonymous volume at %s cannot be reattached by name; use named volumes or binds", m.Destination))
				continue
			}
			sp.Binds = append(sp.Binds, bindString(m.Name, m.Destination, m.RW))
		case "tmpfs":
			pr.block(fmt.Sprintf("tmpfs mount at %s is not representable in the standalone spec", m.Destination))
		default:
			pr.block(fmt.Sprintf("unsupported mount type %q at %s", m.Type, m.Destination))
		}
	}

	// Ports: published as [hostip:]hostport:container/proto, exposed-only
	// as container/proto.
	for _, pm := range d.Ports {
		if !pm.Published {
			sp.Ports = append(sp.Ports, pm.ContainerPort+"/"+pm.Proto)
			continue
		}
		s := pm.HostPort + ":" + pm.ContainerPort + "/" + pm.Proto
		if pm.HostIP != "" && pm.HostIP != "0.0.0.0" && pm.HostIP != "::" {
			s = pm.HostIP + ":" + s
		}
		sp.Ports = append(sp.Ports, s)
	}

	// Networks: an explicit host network mode is reproducible; otherwise the
	// joined networks are proposed.
	if d.NetworkMode == "host" {
		sp.Networks = []string{"host"}
	} else {
		for _, n := range d.Networks {
			sp.Networks = append(sp.Networks, n.Name)
		}
		sp.Networks = dedupSorted(sp.Networks)
	}

	// Environment: names only; every key needs a manual value or reference.
	sp.EnvKeys = copyStrings(d.EnvKeys)
	for _, k := range d.EnvKeys {
		if secretKeyPattern.MatchString(k) {
			pr.TODOs = append(pr.TODOs, fmt.Sprintf("environment variable %s: name suggests a secret; provide a secret_ref", k))
		} else {
			pr.TODOs = append(pr.TODOs, fmt.Sprintf("environment variable %s: provide a value or secret_ref", k))
		}
	}

	sp.Entrypoint = copyStrings(d.Entrypoint)
	sp.Command = copyStrings(d.Cmd)
	sp.User = d.User
	sp.WorkDir = d.WorkingDir
	for k, v := range d.Labels {
		sp.Labels[k] = v
	}

	// Health: a configured healthcheck definition is reproducible; observed
	// health without a captured definition is not.
	if hc := d.HealthCheck; hc != nil && !(len(hc.Test) == 1 && hc.Test[0] == "NONE") {
		sp.HealthCheck = hc
	}
	if d.Health.Configured && sp.HealthCheck == nil {
		pr.confirm("standalone.health_check",
			"docker health status was observed but the healthcheck definition is missing; health cannot be reproduced")
	}

	pr.ID = slug(c.Name, appIDMaxLen)
	pr.finalize()
	return candidate{proposal: pr, base: pr.ID, key: "standalone/" + c.Name}
}

// bindString renders one mount as the spec's bind form: source:destination
// with :ro appended for read-only mounts.
func bindString(source, destination string, rw bool) string {
	s := source + ":" + destination
	if !rw {
		s += ":ro"
	}
	return s
}

type pinKind int

const (
	pinOK     pinKind = iota // digest- or version-pinned
	pinNone                  // no tag and no digest
	pinLatest                // explicitly :latest
)

// pinState classifies an image reference's mutability. Any digest form
// counts as pinned; the tag after the last path segment decides otherwise.
func pinState(ref string) pinKind {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return pinOK
	}
	seg := ref
	if i := strings.LastIndexByte(seg, '/'); i >= 0 {
		seg = seg[i+1:]
	}
	i := strings.IndexByte(seg, ':')
	if i < 0 {
		return pinNone
	}
	if strings.EqualFold(seg[i+1:], "latest") {
		return pinLatest
	}
	return pinOK
}

type sharedKind int

const (
	sharedNone sharedKind = iota
	sharedHost
	sharedContainer
)

// sharedMode classifies pid/ipc namespace modes. Empty, private, and
// shareable keep the namespace inside the container; host and
// container:<id> reach outside it.
func sharedMode(mode string) sharedKind {
	switch {
	case mode == "host":
		return sharedHost
	case strings.HasPrefix(mode, "container:"):
		return sharedContainer
	default:
		return sharedNone
	}
}
