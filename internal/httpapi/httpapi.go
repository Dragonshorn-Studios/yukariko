// Package httpapi serves the read-only query layer behind the dashboard:
// JSON projections of the durable state with explicit source and age, GET
// and HEAD only. No route in this package can deploy, restart, execute
// commands, or modify configuration — the handlers read the store and
// nothing else.
//
// # Exposure
//
// The default bind is loopback (127.0.0.1:8484). Exposing the API beyond
// localhost is an operator decision: put it behind existing access controls
// or an authenticating tunnel; this service implements no authentication.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/state"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// Heartbeat thresholds for remote-host availability. Absence of a
// heartbeat is offline — it is never healthy.
const (
	defaultStaleAfter   = 3 * time.Minute
	defaultOfflineAfter = 10 * time.Minute
)

// maxListLimit caps every list endpoint regardless of the query.
const maxListLimit = 1000

// Server carries the read dependencies. It is safe for concurrent use.
type Server struct {
	Store        *store.Store
	Config       *config.Config
	StaleAfter   time.Duration // remote heartbeat age → stale
	OfflineAfter time.Duration // remote heartbeat age → offline
	Now          func() time.Time
}

// Handler builds the routed, middleware-wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", s.method("health", s.handleLiveness))
	mux.HandleFunc("/api/v1/apps", s.method("apps", s.handleApps))
	mux.HandleFunc("/api/v1/apps/", s.method("app", s.handleApp))
	mux.HandleFunc("/api/v1/hosts", s.method("hosts", s.handleHosts))
	mux.HandleFunc("/api/v1/deployments", s.method("deployments", s.handleDeployments))
	mux.HandleFunc("/api/v1/logs", s.method("logs", s.handleLogs))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, req *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return s.secure(mux)
}

// method enforces GET/HEAD-only and JSON responses for one route.
func (s *Server) method(name string, h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "only GET and HEAD are supported")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		h(w, req)
	}
}

// secure applies the response security headers to every response.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, req)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// --- handlers ---------------------------------------------------------------

func (s *Server) handleLiveness(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   s.now().UTC().Format(time.RFC3339),
	})
}

// handleApps lists every configured app with its durable projections.
func (s *Server) handleApps(w http.ResponseWriter, req *http.Request) {
	if s.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "no configuration loaded")
		return
	}
	now := s.now()
	out := make([]state.AppStatus, 0, len(s.Config.Apps))
	for i := range s.Config.Apps {
		row, err := state.StatusRow(req.Context(), s.Store, &s.Config.Apps[i], now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store read failed")
			return
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

// handleApp serves one app's detail: projections plus recent deployments.
func (s *Server) handleApp(w http.ResponseWriter, req *http.Request) {
	id := strings.TrimPrefix(req.URL.Path, "/api/v1/apps/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if s.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "no configuration loaded")
		return
	}
	for i := range s.Config.Apps {
		if s.Config.Apps[i].ID == id {
			row, err := state.StatusRow(req.Context(), s.Store, &s.Config.Apps[i], s.now())
			if err != nil {
				writeError(w, http.StatusInternalServerError, "store read failed")
				return
			}
			deployments, _ := s.Store.RecentDeployments(req.Context(), id, listLimit(req, 20))
			healths, _ := s.Store.CurrentHealth(req.Context(), id)
			writeJSON(w, http.StatusOK, map[string]any{
				"app":         row,
				"deployments": deployments,
				"health":      healths,
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "unknown app")
}

// handleHosts lists the local host and known remote peers with their
// availability derived from heartbeat age.
func (s *Server) handleHosts(w http.ResponseWriter, req *http.Request) {
	now := s.now()
	remoteHosts, err := s.Store.RemoteHosts(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store read failed")
		return
	}
	type host struct {
		HostID       string `json:"host_id"`
		Source       string `json:"source"`
		Availability string `json:"availability"`
		LastSeenAt   string `json:"last_seen_at,omitempty"`
	}
	hosts := []host{{
		HostID:       "local",
		Source:       "local",
		Availability: "online",
		LastSeenAt:   now.UTC().Format(time.RFC3339),
	}}
	for _, rh := range remoteHosts {
		hosts = append(hosts, host{
			HostID:       rh.HostID,
			Source:       "remote",
			Availability: availability(now.Sub(rh.LastSeenAt), s.staleAfter(), s.offlineAfter()),
			LastSeenAt:   rh.LastSeenAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hosts":         hosts,
		"stale_after":   s.staleAfter().String(),
		"offline_after": s.offlineAfter().String(),
	})
}

func (s *Server) staleAfter() time.Duration {
	if s.StaleAfter > 0 {
		return s.StaleAfter
	}
	return defaultStaleAfter
}

func (s *Server) offlineAfter() time.Duration {
	if s.OfflineAfter > 0 {
		return s.OfflineAfter
	}
	return defaultOfflineAfter
}

// availability derives online/stale/offline deterministically from the
// heartbeat age. A host with no heartbeat never appears here as healthy.
func availability(age, staleAfter, offlineAfter time.Duration) string {
	switch {
	case age <= staleAfter:
		return "online"
	case age <= offlineAfter:
		return "stale"
	default:
		return "offline"
	}
}

// handleDeployments lists recent deployment records, newest first.
func (s *Server) handleDeployments(w http.ResponseWriter, req *http.Request) {
	appID := req.URL.Query().Get("app")
	limit := listLimit(req, 50)
	deployments, err := s.Store.RecentDeployments(req.Context(), appID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store read failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": deployments, "count": len(deployments)})
}

// handleLogs serves the bounded, filtered event history.
func (s *Server) handleLogs(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	appID := q.Get("app")
	level := q.Get("level")
	limit := listLimit(req, 100)
	events, err := s.Store.Events(req.Context(), store.EventsQuery{AppID: appID, Level: level, Limit: limit})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store read failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

// listLimit parses ?limit with a default and a hard cap.
func listLimit(req *http.Request, def int) int {
	n := def
	if raw := req.URL.Query().Get("limit"); raw != "" {
		if parsed, err := parseInt(raw); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > maxListLimit {
		n = maxListLimit
	}
	return n
}

func parseInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
