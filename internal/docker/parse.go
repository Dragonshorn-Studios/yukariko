package docker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// psRow mirrors the fields of `docker ps --format {{json .}}`. The ps
// Labels string is ignored on purpose: its comma-joined form cannot carry
// values containing commas, and discovery obtains authoritative label maps
// from `docker inspect`.
type psRow struct {
	ID        string `json:"ID"`
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	Command   string `json:"Command"`
	CreatedAt string `json:"CreatedAt"`
}

// psCreatedLayout is the fixed format the Docker CLI prints CreatedAt in,
// e.g. "2026-09-01 10:00:00 +0000 UTC".
const psCreatedLayout = "2006-01-02 15:04:05 -0700 MST"

func (r psRow) summary() ContainerSummary {
	name := r.Names
	if i := strings.IndexByte(name, ','); i >= 0 {
		name = name[:i] // legacy additional names follow the primary one
	}
	var anomalies []string
	if r.ID == "" {
		anomalies = append(anomalies, "ps row without an ID")
	}
	createdAt, err := time.Parse(psCreatedLayout, r.CreatedAt)
	if err != nil && r.CreatedAt != "" {
		anomalies = append(anomalies, fmt.Sprintf("unparseable created timestamp %q", r.CreatedAt))
	}
	return ContainerSummary{
		ID:         r.ID,
		Name:       name,
		Image:      r.Image,
		State:      r.State,
		StatusText: r.Status,
		Command:    r.Command,
		CreatedAt:  createdAt,
		Anomalies:  anomalies,
	}
}

// parsePSOutput decodes NDJSON: one JSON object per line. Unparseable output
// is a typed error, never a silently empty inventory.
func parsePSOutput(out []byte) ([]ContainerSummary, error) {
	var rows []ContainerSummary
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var doc psRow
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, &Error{
				Code:   CodeInvalidOutput,
				Detail: fmt.Sprintf("line %d: %v", line, excerpt(err.Error())),
				Hint:   "`docker ps --format {{json .}}` output changed shape; the Docker CLI may be too old",
				cause:  ErrInvalidOutput,
			}
		}
		rows = append(rows, doc.summary())
	}
	if err := sc.Err(); err != nil {
		return nil, &Error{
			Code:   CodeInvalidOutput,
			Detail: excerpt(err.Error()),
			Hint:   "docker ps output could not be read in full",
			cause:  ErrInvalidOutput,
		}
	}
	return rows, nil
}

// inspectDoc mirrors the recreation-relevant slice of `docker inspect`.
// Unlisted fields are intentionally not modeled; Config.Env values are
// dropped at parse time and only names survive.
type inspectDoc struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Image   string `json:"Image"`
	Config  struct {
		Image       string            `json:"Image"`
		User        string            `json:"User"`
		WorkingDir  string            `json:"WorkingDir"`
		Cmd         []string          `json:"Cmd"`
		Entrypoint  []string          `json:"Entrypoint"`
		Env         []string          `json:"Env"`
		Labels      map[string]string `json:"Labels"`
		Healthcheck *struct {
			Test        []string `json:"Test"`
			Interval    int64    `json:"Interval"`
			Timeout     int64    `json:"Timeout"`
			Retries     int      `json:"Retries"`
			StartPeriod int64    `json:"StartPeriod"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		Health     *struct {
			Status        string `json:"Status"`
			FailingStreak int    `json:"FailingStreak"`
		} `json:"Health"`
	} `json:"State"`
	HostConfig struct {
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		NetworkMode string `json:"NetworkMode"`
		Privileged  bool   `json:"Privileged"`
		PidMode     string `json:"PidMode"`
		IpcMode     string `json:"IpcMode"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
		Propagation string `json:"Propagation"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIP"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
		Networks map[string]struct {
			NetworkID string   `json:"NetworkID"`
			Aliases   []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// parseInspectOutput decodes the single-container object list printed by
// `docker inspect --type container <ref>`.
func parseInspectOutput(out []byte) (ContainerDetail, error) {
	var docs []inspectDoc
	if err := json.Unmarshal(bytes.TrimSpace(out), &docs); err != nil {
		return ContainerDetail{}, &Error{
			Code:   CodeInvalidOutput,
			Detail: excerpt(err.Error()),
			Hint:   "`docker inspect` did not print a container object list",
			cause:  ErrInvalidOutput,
		}
	}
	if len(docs) == 0 {
		return ContainerDetail{}, &Error{
			Code:   CodeInvalidOutput,
			Detail: "no container object in inspect output",
			cause:  ErrInvalidOutput,
		}
	}
	// One ref was requested, so the daemon answers with one object; the
	// object carries its own ID and name for callers that need to match.
	return docs[0].detail(), nil
}

// detail converts the raw document. Missing fields become anomalies, never
// fabricated values; environment values are reduced to names here.
func (d *inspectDoc) detail() ContainerDetail {
	det := ContainerDetail{
		ID:              d.ID,
		Name:            strings.TrimPrefix(d.Name, "/"),
		ImageRef:        d.Config.Image,
		ImageID:         d.Image,
		Cmd:             copyStrings(d.Config.Cmd),
		Entrypoint:      copyStrings(d.Config.Entrypoint),
		User:            d.Config.User,
		WorkingDir:      d.Config.WorkingDir,
		Labels:          copyLabels(d.Config.Labels),
		RestartPolicy:   d.HostConfig.RestartPolicy.Name,
		RestartMaxRetry: d.HostConfig.RestartPolicy.MaximumRetryCount,
		NetworkMode:     d.HostConfig.NetworkMode,
		Privileged:      d.HostConfig.Privileged,
		PidMode:         d.HostConfig.PidMode,
		IpcMode:         d.HostConfig.IpcMode,
		State:           d.State.Status,
		Running:         d.State.Running,
		Paused:          d.State.Paused,
		Restarting:      d.State.Restarting,
	}
	if det.ID == "" {
		det.Anomalies = append(det.Anomalies, "inspect reports no container ID")
	}
	if det.ImageRef == "" {
		det.Anomalies = append(det.Anomalies, "inspect reports no image reference")
	}
	if det.State == "" {
		det.Anomalies = append(det.Anomalies, "inspect reports no state")
	}
	if d.Created != "" {
		created, err := time.Parse(time.RFC3339, d.Created)
		if err != nil {
			det.Anomalies = append(det.Anomalies, fmt.Sprintf("unparseable created timestamp %q", d.Created))
		} else {
			det.CreatedAt = created
		}
	}
	if h := d.State.Health; h != nil {
		det.Health = HealthState{Configured: true, Status: h.Status, FailingStreak: h.FailingStreak}
		if h.Status == "" {
			det.Anomalies = append(det.Anomalies, "healthcheck configured but status missing")
		}
	}
	if hc := d.Config.Healthcheck; hc != nil {
		def := &HealthCheckDef{
			Test:          copyStrings(hc.Test),
			IntervalNS:    hc.Interval,
			TimeoutNS:     hc.Timeout,
			StartPeriodNS: hc.StartPeriod,
			Retries:       hc.Retries,
		}
		det.HealthCheck = def
	}
	for _, entry := range d.Config.Env {
		name, _, hasValue := strings.Cut(entry, "=")
		if !hasValue || name == "" {
			det.Anomalies = append(det.Anomalies, "env entry without a NAME=value shape (value never captured)")
			continue
		}
		det.EnvKeys = append(det.EnvKeys, name)
	}
	for _, m := range d.Mounts {
		det.Mounts = append(det.Mounts, Mount{
			Type:        m.Type,
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			RW:          m.RW,
			Propagation: m.Propagation,
		})
	}
	for _, key := range sortedKeys(d.NetworkSettings.Ports) {
		port, proto, ok := strings.Cut(key, "/")
		if !ok {
			det.Anomalies = append(det.Anomalies, fmt.Sprintf("malformed port key %q", key))
			continue
		}
		bindings := d.NetworkSettings.Ports[key]
		if len(bindings) == 0 {
			// Exposed but not published: recorded explicitly with Published=false.
			det.Ports = append(det.Ports, PortMapping{ContainerPort: port, Proto: proto})
			continue
		}
		for _, b := range bindings {
			det.Ports = append(det.Ports, PortMapping{
				ContainerPort: port,
				Proto:         proto,
				Published:     true,
				HostIP:        b.HostIP,
				HostPort:      b.HostPort,
			})
		}
	}
	for _, name := range sortedKeys(d.NetworkSettings.Networks) {
		net := d.NetworkSettings.Networks[name]
		aliases := copyStrings(net.Aliases)
		sort.Strings(aliases)
		det.Networks = append(det.Networks, NetworkAttachment{Name: name, Aliases: aliases})
	}
	det.Anomalies = dedupSort(det.Anomalies)
	return det
}

func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copyLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dedupSort(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return slices.Compact(out)
}
