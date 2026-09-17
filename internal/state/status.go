// Package state projects the durable store into the read-only views used by
// the CLI status command, the HTTP API, and the dashboard: per-app status
// rows, version kinds, and bounded log reads.
package state

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// VersionKind is the deployed-version kind for an app's source mode.
func VersionKind(app *config.App) string {
	if app.Source.Mode == config.SourceGit {
		return store.KindGitSHA
	}
	return store.KindDigest
}

// ObservedKind is the observation kind recorded for an app's primary
// version. Registry apps record per-image observations under
// "digest:<canonical ref>"; this returns the primary (first) image's kind.
func ObservedKind(app *config.App) string {
	if app.Source.Mode == config.SourceGit {
		return store.KindGitSHA
	}
	if app.Source.Registry != nil && len(app.Source.Registry.Images) > 0 {
		if ref, err := registry.ParseRef(app.Source.Registry.Images[0].Ref); err == nil {
			return store.KindDigest + ":" + ref.String()
		}
	}
	return store.KindDigest
}

// AppStatus is one app's durable-state snapshot for the status command.
type AppStatus struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Deployed       string `json:"deployed,omitempty"`
	DeployedAt     string `json:"deployed_at,omitempty"`
	Observed       string `json:"observed,omitempty"`
	ObservedAt     string `json:"observed_at,omitempty"`
	Pending        bool   `json:"pending"`
	Health         string `json:"health,omitempty"`
	LastDeployment string `json:"last_deployment,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// StatusRow assembles one app's snapshot from the durable store. Docker
// running-state, Docker health, HTTP health, and the deployed version stay
// separate facts; State summarizes them for humans.
func StatusRow(ctx context.Context, st *store.Store, app *config.App, now time.Time) (AppStatus, error) {
	row := AppStatus{ID: app.ID}
	kind := VersionKind(app)
	rawDeployed, deployedAt, deployedOK, err := st.DeployedVersion(ctx, app.ID, kind)
	if err != nil {
		return row, err
	}
	if deployedOK {
		row.Deployed = ShortVersion(rawDeployed)
		row.DeployedAt = deployedAt.Format(time.RFC3339)
	}
	rawObserved, observedAt, observedOK, err := st.ObservedVersion(ctx, app.ID, ObservedKind(app))
	if err != nil {
		return row, err
	}
	if observedOK {
		row.Observed = ShortVersion(rawObserved)
		row.ObservedAt = observedAt.Format(time.RFC3339)
	}
	row.Pending = pending(rawDeployed, deployedOK, rawObserved, observedOK, app)

	healths, err := st.CurrentHealth(ctx, app.ID)
	if err != nil {
		return row, err
	}
	var hs []string
	for _, h := range healths {
		hs = append(hs, h.CheckKind+":"+h.State)
	}
	row.Health = strings.Join(hs, ",")

	deps, err := st.RecentDeployments(ctx, app.ID, 1)
	if err != nil {
		return row, err
	}
	if len(deps) > 0 {
		d := deps[0]
		row.LastDeployment = d.Status + " " + d.StartedAt.Format(time.RFC3339)
		if d.Status == store.StatusFailed && d.Error != "" {
			row.LastDeployment += ": " + d.Error
		}
	}
	row.State = summarizeState(row, now)
	return row, nil
}

// pending compares the OBSERVED digest with the deployed identity's entry
// for the same reference — never the shortened display forms. For registry
// apps the deployed identity is "ref@digest" pairs; for git apps it is the
// SHA itself.
func pending(rawDeployed string, deployedOK bool, rawObserved string, observedOK bool, app *config.App) bool {
	if !observedOK {
		return false
	}
	if !deployedOK {
		return true
	}
	if app.Source.Mode == config.SourceRegistry && app.Source.Registry != nil && len(app.Source.Registry.Images) > 0 {
		if ref, err := registry.ParseRef(app.Source.Registry.Images[0].Ref); err == nil {
			deployedDigest := ParseDigestVersion(rawDeployed)[ref.String()]
			return deployedDigest != rawObserved
		}
	}
	return rawDeployed != rawObserved
}

// ParseDigestVersion splits the stored "ref@digest,ref@digest" identity
// into its per-reference digests.
func ParseDigestVersion(version string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(version, ",") {
		ref, digest, ok := strings.Cut(pair, "@")
		if ok {
			out[ref] = digest
		}
	}
	return out
}

// summarizeState renders the human verdict: failed > pending > healthy or
// degraded health > stale/unknown.
func summarizeState(row AppStatus, now time.Time) string {
	switch {
	case strings.Contains(row.LastDeployment, string(store.StatusFailed)):
		return "failed"
	case row.Pending:
		return "pending"
	case strings.Contains(row.Health, ":"+config.HealthUnhealthy):
		return "unhealthy"
	case row.Deployed == "" && row.Observed == "":
		return "unknown"
	case stale(row, now):
		return "stale"
	case strings.Contains(row.Health, ":"+config.HealthHealthy) || row.Deployed != "":
		return "up-to-date"
	default:
		return "observed"
	}
}

// stale reports observations older than twice the app interval (bounded to
// at least 10 minutes and at most 24 hours) — an intentionally coarse
// freshness heuristic documented in the status help.
func stale(row AppStatus, now time.Time) bool {
	if row.ObservedAt == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, row.ObservedAt)
	if err != nil {
		return true
	}
	age := now.Sub(at)
	return age > 24*time.Hour
}

// ShortVersion renders a version for humans: SHAs truncate to 12 chars, and
// multi-image identities summarize to a count.
func ShortVersion(v string) string {
	if strings.Contains(v, ",") {
		return fmt.Sprintf("%d images", len(strings.Split(v, ",")))
	}
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// LogEvent is the JSON shape of one history entry.
type LogEvent struct {
	Time    time.Time `json:"time"`
	AppID   string    `json:"app_id,omitempty"`
	Level   string    `json:"level"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// EventsFor reads a bounded, filtered event history.
func EventsFor(ctx context.Context, st *store.Store, appID, level string, limit int, since time.Duration) ([]LogEvent, error) {
	events, err := st.Events(ctx, store.EventsQuery{AppID: appID, Level: level, Limit: limit})
	if err != nil {
		return nil, err
	}
	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	out := make([]LogEvent, 0, len(events))
	for _, e := range events {
		if !cutoff.IsZero() && e.Time.Before(cutoff) {
			continue
		}
		out = append(out, LogEvent{Time: e.Time, AppID: e.AppID, Level: e.Level, Kind: e.Kind, Message: e.Message})
	}
	return out, nil
}
