package docker

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Compose label keys used for grouping. Compose v1 and v2 set the same
// prefixes on every container they manage.
const (
	LabelProject         = "com.docker.compose.project"
	LabelService         = "com.docker.compose.service"
	LabelWorkDir         = "com.docker.compose.project.working_dir"
	LabelConfigFiles     = "com.docker.compose.project.config_files"
	LabelContainerNumber = "com.docker.compose.container-number"
	LabelOneOff          = "com.docker.compose.oneoff"
)

// composeLabelKeys are the labels that mark a container as Compose-managed
// even when project/service are absent.
var composeLabelKeys = []string{
	LabelProject, LabelService, LabelWorkDir, LabelConfigFiles,
	LabelContainerNumber, LabelOneOff,
}

// ContainerSummary is the lightweight `docker ps` inventory record.
type ContainerSummary struct {
	ID         string
	Name       string
	Image      string // reference as the daemon reports it
	State      string // created|running|paused|restarting|removing|exited|dead
	StatusText string // human-readable status line from docker ps
	Command    string // truncated display command from docker ps
	CreatedAt  time.Time
	Anomalies  []string // missing or unparseable summary fields
}

// HealthState distinguishes "no healthcheck configured" (Configured=false)
// from any configured status; an empty Status with Configured=true is an
// explicit unknown surfaced in Anomalies.
type HealthState struct {
	Configured    bool
	Status        string // starting|healthy|unhealthy
	FailingStreak int
}

// Mount is one entry of the inspect Mounts list.
type Mount struct {
	Type        string // bind|volume|tmpfs|npipe|cluster
	Name        string // volume name when Type is volume
	Source      string
	Destination string
	Mode        string
	RW          bool
	Propagation string
}

// PortMapping is one resolved port binding. Published=false marks an exposed
// port with no host binding; it is still listed rather than dropped.
type PortMapping struct {
	ContainerPort string
	Proto         string
	Published     bool
	HostIP        string
	HostPort      string
}

// NetworkAttachment is one network the container joined.
type NetworkAttachment struct {
	Name    string
	Aliases []string
}

// ContainerDetail is the recreation-relevant slice of `docker inspect`.
// Environment variables are captured as NAMES only: inspect carries raw
// values, and values must never enter results, logs, or storage.
type ContainerDetail struct {
	ID              string
	Name            string
	ImageRef        string
	ImageID         string
	CreatedAt       time.Time
	State           string
	Running         bool
	Paused          bool
	Restarting      bool
	Health          HealthState
	Cmd             []string
	Entrypoint      []string
	User            string
	WorkingDir      string
	EnvKeys         []string
	Labels          map[string]string
	RestartPolicy   string // no|always|unless-stopped|on-failure
	RestartMaxRetry int
	NetworkMode     string
	Mounts          []Mount
	Ports           []PortMapping
	Networks        []NetworkAttachment
	Anomalies       []string // missing or unparseable inspect fields
}

// Container is one discovered container: the summary plus, when inspection
// succeeded, its detail and Compose classification. Every anomaly — summary
// parse issues, failed inspection, missing or malformed Compose labels —
// accumulates in Anomalies; empty fields are never fabricated.
type Container struct {
	ContainerSummary
	Detail *ContainerDetail // nil when inspection failed; see Anomalies

	Project         string // com.docker.compose.project; "" = not grouped
	Service         string // com.docker.compose.service; "" = unknown
	ContainerNumber int    // compose replica index; 0 = unknown (see Anomalies)
	OneOff          bool   // compose oneoff (leftover `compose run` container)

	SystemCandidate bool     // likely Yukariko itself or system/infrastructure
	SystemReasons   []string // why it was flagged; sorted, deduplicated
}

// ComposeProject groups every discovered container that claims membership in
// one Compose project. It is one candidate, never N independent apps.
// WorkDir and ConfigFiles are set only when every member carries the same
// label; absences, partial coverage, and conflicts leave them empty and are
// explained in Anomalies.
type ComposeProject struct {
	Name            string
	WorkDir         string
	ConfigFiles     []string // compose's own order preserved; overrides matter
	Services        []string // distinct, sorted
	Containers      []*Container
	SystemCandidate bool
	SystemReasons   []string
	Anomalies       []string
}

// Report is the result of one read-only discovery pass. Projects and
// Standalone are sorted deterministically; system candidates are included
// and flagged so callers can exclude them by default.
type Report struct {
	GeneratedAt time.Time
	Projects    []*ComposeProject // sorted by Name
	Standalone  []*Container      // sorted by Name, then ID
}

// Discover performs one read-only inventory pass: list all containers
// (running and stopped), inspect each one, group Compose members by project,
// and flag likely system/infrastructure candidates. Only reads are issued —
// ListContainers and InspectContainer are the sole calls, and the Client
// interface exposes no mutation path. Failure to list or context
// cancellation aborts the pass; failures to inspect individual containers
// become anomalies so the rest of the inventory is still returned.
func Discover(ctx context.Context, client Client) (*Report, error) {
	summaries, err := client.ListContainers(ctx, true)
	if err != nil {
		return nil, err
	}
	report := &Report{GeneratedAt: time.Now()}
	byProject := map[string]*ComposeProject{}
	for _, s := range summaries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := &Container{ContainerSummary: s}
		detail, err := client.InspectContainer(ctx, s.ID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.Anomalies = append(c.Anomalies, fmt.Sprintf("inspect failed: %v", err))
		} else {
			c.Detail = &detail
		}
		classifyCompose(c)
		flagSystem(c)
		dedupSortInto(&c.ContainerSummary.Anomalies)
		if c.Project != "" && c.Service != "" {
			p := byProject[c.Project]
			if p == nil {
				p = &ComposeProject{Name: c.Project}
				byProject[c.Project] = p
			}
			p.Containers = append(p.Containers, c)
		} else {
			report.Standalone = append(report.Standalone, c)
		}
	}
	for _, p := range byProject {
		p.finalize()
		report.Projects = append(report.Projects, p)
	}
	sort.Slice(report.Projects, func(i, j int) bool {
		return report.Projects[i].Name < report.Projects[j].Name
	})
	sortContainerSlice(report.Standalone)
	return report, nil
}

// classifyCompose reads authoritative Compose labels from the detail and
// decides membership. Containers carrying some Compose labels but missing
// project or service stay standalone with an explicit anomaly — they are
// never guessed into a project.
func classifyCompose(c *Container) {
	if c.Detail == nil {
		return // grouping needs authoritative labels; anomaly already recorded
	}
	labels := c.Detail.Labels
	c.Project = labels[LabelProject]
	c.Service = labels[LabelService]
	switch {
	case c.Project != "" && c.Service != "":
		if raw := labels[LabelContainerNumber]; raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				c.Anomalies = append(c.Anomalies, fmt.Sprintf("malformed %s value %q", LabelContainerNumber, raw))
			} else {
				c.ContainerNumber = n
			}
		}
		raw, ok := labels[LabelOneOff]
		switch {
		case !ok:
		case strings.EqualFold(raw, "true"):
			c.OneOff = true
		case strings.EqualFold(raw, "false"):
		default:
			c.Anomalies = append(c.Anomalies, fmt.Sprintf("malformed %s value %q", LabelOneOff, raw))
		}
	case c.Project == "" && c.Service != "":
		c.Anomalies = append(c.Anomalies,
			fmt.Sprintf("compose labels present but %s missing or empty; not grouped", LabelProject))
	case c.Project != "" && c.Service == "":
		c.Anomalies = append(c.Anomalies,
			fmt.Sprintf("compose labels present but %s missing or empty; not grouped", LabelService))
	default:
		for _, k := range composeLabelKeys {
			if _, ok := labels[k]; ok {
				c.Anomalies = append(c.Anomalies,
					"compose labels present but project and service are both missing or empty; not grouped")
				return
			}
		}
	}
}

// finalize derives project-level fields from member containers: the service
// list, workdir/config-files consensus, system aggregation, and stable
// ordering. Consensus means every member carries the same non-empty label;
// anything else stays empty with an anomaly naming what was observed.
func (p *ComposeProject) finalize() {
	services := map[string]bool{}
	workdirs := map[string]bool{}
	configs := map[string]bool{}
	wdHave, cfHave := 0, 0
	for _, c := range p.Containers {
		if c.Service != "" {
			services[c.Service] = true
		}
		if c.Detail == nil {
			continue
		}
		if wd := c.Detail.Labels[LabelWorkDir]; wd != "" {
			workdirs[wd] = true
			wdHave++
		}
		if cf := c.Detail.Labels[LabelConfigFiles]; cf != "" {
			configs[cf] = true
			cfHave++
		}
	}
	p.Services = sortedKeys(services)
	p.WorkDir = consensus(workdirs, wdHave, len(p.Containers), LabelWorkDir, "working dir", &p.Anomalies)
	p.ConfigFiles = splitFiles(consensus(configs, cfHave, len(p.Containers), LabelConfigFiles, "config files", &p.Anomalies))
	for _, c := range p.Containers {
		if c.SystemCandidate {
			p.SystemCandidate = true
			p.SystemReasons = mergeUnique(p.SystemReasons, c.SystemReasons...)
		}
	}
	sort.Strings(p.SystemReasons)
	sortContainerSlice(p.Containers)
	dedupSortInto(&p.Anomalies)
}

// consensus resolves one project-wide label. Full coverage with a single
// distinct value wins; absence, partial coverage, and conflicts are all
// reported and yield "" — never a fabricated or majority-voted value.
func consensus(values map[string]bool, have, total int, label, what string, anomalies *[]string) string {
	switch {
	case len(values) == 0:
		*anomalies = append(*anomalies, fmt.Sprintf("%s (%s) missing from every container", what, label))
		return ""
	case len(values) == 1 && have == total:
		for v := range values {
			return v
		}
	case have < total:
		*anomalies = append(*anomalies,
			fmt.Sprintf("%s (%s) missing on %d of %d containers", what, label, total-have, total))
	}
	if len(values) > 1 {
		*anomalies = append(*anomalies,
			fmt.Sprintf("%s (%s) conflicts across containers: %s", what, label, strings.Join(sortedKeys(values), " vs ")))
	}
	return ""
}

// splitFiles splits compose's comma-joined config_files label, preserving
// compose's own order (later files override earlier ones) and dropping
// duplicates and empties.
func splitFiles(joined string) []string {
	if joined == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Split(joined, ",") {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// sortContainerSlice orders containers by Name then ID for determinism.
func sortContainerSlice(cs []*Container) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Name != cs[j].Name {
			return cs[i].Name < cs[j].Name
		}
		return cs[i].ID < cs[j].ID
	})
}

// infraImages maps the final path segment of well-known management and
// infrastructure image repositories to a human-readable reason. The list is
// deliberately small and only drives a default-exclusion hint; it never
// triggers a destructive action, and callers may ignore the flag.
var infraImages = map[string]string{
	"portainer":           "portainer management UI",
	"portainer-ce":        "portainer management UI",
	"portainer-ee":        "portainer management UI",
	"watchtower":          "watchtower auto-updater",
	"traefik":             "traefik reverse proxy",
	"coolify":             "coolify PaaS",
	"dokploy":             "dokploy PaaS",
	"easypanel":           "easypanel PaaS",
	"caprover":            "caprover PaaS",
	"uptime-kuma":         "uptime-kuma monitor",
	"diun":                "diun image notifier",
	"docker-socket-proxy": "docker socket proxy",
	"socket-proxy":        "docker socket proxy",
}

// selfID returns the identity used to recognize Yukariko's own container:
// the host name, which inside a container defaults to the 12-hex short
// container ID. Overridable in tests.
var selfID = func() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.ToLower(h)
}

// flagSystem marks Yukariko itself and likely system/infrastructure
// containers so callers can exclude them by default. Heuristics, in order:
//
//  1. The container name or its Compose project is yukariko or yukariko-*
//     prefixed: the agent must never manage the manager.
//  2. The container ID starts with the current host name when that host
//     name is a 12-hex short ID — covers Yukariko running in a container
//     under an arbitrary name.
//  3. The image repository's final path segment appears in the documented
//     infrastructure list.
func flagSystem(c *Container) {
	name := strings.ToLower(strings.TrimPrefix(c.Name, "/"))
	project := strings.ToLower(c.Project)
	switch {
	case name == "yukariko" || strings.HasPrefix(name, "yukariko-"):
		c.SystemCandidate = true
		c.SystemReasons = append(c.SystemReasons, "container name is yukariko or yukariko-prefixed")
	case project == "yukariko" || strings.HasPrefix(project, "yukariko-"):
		c.SystemCandidate = true
		c.SystemReasons = append(c.SystemReasons, "compose project is yukariko or yukariko-prefixed")
	}
	if id := selfID(); isHex12(id) && strings.HasPrefix(strings.ToLower(c.ID), id) {
		c.SystemCandidate = true
		c.SystemReasons = append(c.SystemReasons, "host name matches this container's ID: yukariko runs inside it")
	}
	if reason, ok := infraImages[imageRepository(c.Image)]; ok {
		c.SystemCandidate = true
		c.SystemReasons = append(c.SystemReasons, "infrastructure image: "+reason)
	}
	c.SystemReasons = dedupSort(c.SystemReasons)
}

// imageRepository reduces an image reference to the final path segment of
// its repository, lowercased, tag and digest stripped. Registry ports never
// appear in the final segment, so cutting at the first colon is safe.
func imageRepository(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, '/'); i >= 0 {
		image = image[i+1:]
	}
	if i := strings.IndexByte(image, ':'); i >= 0 {
		image = image[:i]
	}
	return strings.ToLower(image)
}

func isHex12(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func mergeUnique(base []string, add ...string) []string {
	for _, v := range add {
		if !slices.Contains(base, v) {
			base = append(base, v)
		}
	}
	return base
}

// dedupSortInto normalizes an anomaly/reason slice in place for determinism.
func dedupSortInto(dst *[]string) {
	*dst = dedupSort(*dst)
}
