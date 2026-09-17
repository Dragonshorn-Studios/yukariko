// Package ui serves the embedded, read-only Yukariko dashboard: four
// server-rendered sections (Sanctuary overview, Vestments versions,
// Chronicle history, Divination health) built with html/template and
// embedded assets only.
//
// There are no state-changing controls, no forms, and no JavaScript: every
// page renders from the same read-only query layer the CLI and API use,
// and the only interactive element is plain links.
//
// All assets are original to this project (see docs/ASSETS.md): the
// palette — deep navy, ivory, lapis, restrained gold — and the quiet
// archive/chapel motifs are drawn with CSS alone.
package ui

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/state"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

//go:embed templates/*.html static/style.css
var files embed.FS

// Server renders the dashboard from the durable store and configuration.
type Server struct {
	Store  *store.Store
	Config *config.Config
	Now    func() time.Time
}

// Handler builds the UI routes, all GET/HEAD-only.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/static/style.css", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		css, _ := files.ReadFile("static/style.css")
		w.Write(css)
	})
	mux.HandleFunc("GET /ui", s.page("sanctuary"))
	mux.HandleFunc("GET /ui/", s.page("sanctuary"))
	mux.HandleFunc("GET /ui/vestments", s.page("vestments"))
	mux.HandleFunc("GET /ui/chronicle", s.page("chronicle"))
	mux.HandleFunc("GET /ui/divination", s.page("divination"))
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type page struct {
	Title    string
	Section  string
	Nav      []navLink
	Data     any
	Now      string
	HostName string
}

type navLink struct {
	Href, Label, Section string
}

var nav = []navLink{
	{"/ui", "Sanctuary", "sanctuary"},
	{"/ui/vestments", "Vestments", "vestments"},
	{"/ui/chronicle", "Chronicle", "chronicle"},
	{"/ui/divination", "Divination", "divination"},
}

func (s *Server) page(section string) http.HandlerFunc {
	tmpl := template.Must(template.ParseFS(files, "templates/layout.html", "templates/"+section+".html"))
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "only GET is supported", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := s.data(req, section)
		if err := tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
			// Headers may already be written; log-free minimal fallback.
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
	}
}

func (s *Server) data(req *http.Request, section string) page {
	p := page{
		Title:   strings.ToUpper(section[:1]) + section[1:],
		Section: section,
		Nav:     nav,
		Now:     s.now().UTC().Format(time.RFC3339),
	}
	switch section {
	case "sanctuary":
		p.Data = s.sanctuary(req)
	case "vestments":
		p.Data = s.vestments(req)
	case "chronicle":
		p.Data = s.chronicle(req)
	case "divination":
		p.Data = s.divination(req)
	}
	return p
}

// sanctuaryRow is one overview card: running/health/update/deployment are
// deliberately separate columns.
type sanctuaryRow struct {
	ID        string
	Running   string // container presence (standalone) or "—"
	Health    string // http/docker health summary
	Update    string // up-to-date | pending | unknown
	Deploy    string // last deployment outcome
	Detail    string
	BadUpdate string
	BadHealth string
	BadDeploy string
}

func (s *Server) sanctuary(req *http.Request) any {
	ctx := req.Context()
	now := s.now()
	rows := []sanctuaryRow{}
	for i := range s.Config.Apps {
		app := &s.Config.Apps[i]
		row, err := state.StatusRow(ctx, s.Store, app, now)
		if err != nil {
			continue
		}
		r := sanctuaryRow{
			ID:      row.ID,
			Running: "—",
			Health:  orDashText(row.Health),
			Update:  "unknown",
			Deploy:  orDashText(row.LastDeployment),
			Detail:  row.Detail,
		}
		switch {
		case strings.Contains(row.Health, ":unhealthy"):
			r.Health = "unhealthy"
		}
		switch row.State {
		case "up-to-date":
			r.Update, r.BadUpdate = "up-to-date", "ok"
		case "pending":
			r.Update, r.BadUpdate = "pending update", "warn"
		case "failed":
			r.Update, r.BadUpdate, r.Deploy = "failed", "fail", "failed"
		case "stale":
			r.Update, r.BadUpdate = "stale", "stale"
		}
		switch {
		case strings.Contains(row.LastDeployment, "failed"):
			r.Deploy, r.BadDeploy = "failed", "fail"
		case row.LastDeployment != "":
			r.Deploy, r.BadDeploy = "deployed", "ok"
		}
		switch {
		case strings.Contains(row.Health, ":unhealthy"):
			r.BadHealth = "fail"
		case strings.Contains(row.Health, ":healthy"):
			r.BadHealth = "ok"
		default:
			r.BadHealth = "stale"
			r.Health = orDashText(r.Health)
		}
		r.Running = runningText(row)
		rows = append(rows, r)
	}
	return map[string]any{
		"rows":      rows,
		"reporting": reportingCard(ctx, s.Store, now),
		"hosts":     hostRows(ctx, s.Store, now),
	}
}

func runningText(row state.AppStatus) string {
	switch {
	case row.Deployed == "" && row.Observed == "":
		return "no data"
	case row.State == "failed":
		return "failed deploy"
	default:
		return "deployed"
	}
}

func reportingCard(ctx context.Context, st *store.Store, now time.Time) map[string]any {
	backlog, _ := st.PendingOutboundCount(ctx)
	last, ok, _ := st.LastDelivery(ctx)
	card := map[string]any{
		"backlog": backlog,
		"last":    "—",
	}
	if ok {
		card["last"] = last.UTC().Format(time.RFC3339)
		if now.Sub(last) > 10*time.Minute {
			card["degraded"] = true
		}
	}
	return card
}

func hostRows(ctx context.Context, st *store.Store, now time.Time) []map[string]any {
	hosts := []map[string]any{{
		"id":     "local",
		"source": "local",
		"state":  "online",
		"cls":    "ok",
	}}
	peers, _ := st.RemoteHosts(ctx)
	for _, h := range peers {
		age := now.Sub(h.LastSeenAt)
		avail, cls := "offline", "offline"
		switch {
		case age <= 3*time.Minute:
			avail, cls = "online", "ok"
		case age <= 10*time.Minute:
			avail, cls = "stale", "stale"
		}
		hosts = append(hosts, map[string]any{
			"id":     h.HostID,
			"source": "remote report",
			"state":  avail + " · seen " + h.LastSeenAt.UTC().Format(time.RFC3339),
			"cls":    cls,
		})
	}
	return hosts
}

// vestmentRow pairs configured images with observed/deployed digests.
type vestmentRow struct {
	App      string
	Image    string
	Observed string
	Deployed string
	Pending  bool
	Note     string
}

func (s *Server) vestments(req *http.Request) any {
	ctx := req.Context()
	rows := []vestmentRow{}
	for i := range s.Config.Apps {
		app := &s.Config.Apps[i]
		kind := state.VersionKind(app)
		deployed, _, depOK, _ := s.Store.DeployedVersion(ctx, app.ID, kind)
		observed, _, obsOK, _ := s.Store.ObservedVersion(ctx, app.ID, state.ObservedKind(app))
		images := []string{}
		if app.Source.Mode == config.SourceRegistry && app.Source.Registry != nil {
			for _, img := range app.Source.Registry.Images {
				images = append(images, img.Ref)
			}
		}
		if len(images) == 0 {
			images = append(images, "git worktree: "+orDashText(app.Source.Git.Dir))
		}
		rows = append(rows, vestmentRow{
			App:      app.ID,
			Image:    strings.Join(images, ", "),
			Observed: orDashOK(short(observed), obsOK),
			Deployed: orDashOK(short(deployed), depOK),
			Pending:  obsOK && depOK && observed != deployed,
			Note:     pendingNote(obsOK, depOK),
		})
	}
	return map[string]any{"rows": rows}
}

func orDashOK(v string, ok bool) string {
	if !ok || v == "" {
		return "—"
	}
	return v
}

func pendingNote(obsOK, depOK bool) string {
	switch {
	case !obsOK:
		return "nothing observed yet"
	case !depOK:
		return "never deployed by Yukariko"
	default:
		return ""
	}
}

func short(v string) string {
	if len(v) > 12 {
		return v[:12] + "…"
	}
	return v
}

func orDashText(v string) string {
	if v == "" {
		return "—"
	}
	return v
}

// chronicleData is the bounded, filterable history.
func (s *Server) chronicle(req *http.Request) any {
	ctx := req.Context()
	appID := req.URL.Query().Get("app")
	limit := 50
	events, _ := state.EventsFor(ctx, s.Store, appID, req.URL.Query().Get("level"), limit, 0)
	var deployments []store.Deployment
	if appID != "" {
		deployments, _ = s.Store.RecentDeployments(ctx, appID, 20)
	} else {
		deployments, _ = s.Store.RecentDeployments(ctx, "", 20)
	}
	return map[string]any{
		"events":      events,
		"deployments": deployments,
		"app":         appID,
	}
}

// divinationData is the current health per app and check.
func (s *Server) divination(req *http.Request) any {
	healths, _ := s.Store.CurrentHealthAll(req.Context())
	rows := make([]map[string]any, 0, len(healths))
	for _, h := range healths {
		cls := "stale"
		switch h.State {
		case config.HealthHealthy:
			cls = "ok"
		case config.HealthUnhealthy:
			cls = "fail"
		case config.HealthChecking:
			cls = "warn"
		}
		rows = append(rows, map[string]any{
			"app":    h.AppID,
			"check":  h.CheckKind,
			"state":  h.State,
			"cls":    cls,
			"reason": h.Reason,
			"when":   h.CheckedAt.UTC().Format(time.RFC3339),
		})
	}
	return map[string]any{"rows": rows}
}
