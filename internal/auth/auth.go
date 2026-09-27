// Package auth implements Yukariko's optional OIDC authentication gate
// (issue #61). The dashboard and the read-only API sit behind a server-side
// session; the OpenID Connect protocol layer is coreos/go-oidc plus
// x/oauth2 — hand-rolled token verification is a non-goal by doctrine.
//
// Trust model: Yukariko verifies ID tokens itself (signature via the
// provider's JWKS, issuer, audience, expiry, nonce) and never trusts
// headers from a reverse proxy. Machine peers keep their own HMAC channel:
// /report is never session-gated. The client secret is a config.SecretRef
// resolved only inside the token exchange and never logged or stored.
//
// The gate is fail-closed opt-in: without auth.oidc.enabled the HTTP
// surface behaves exactly as before.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Identity is the verified sign-in state exposed to downstream handlers
// (the dashboard footer). It carries a safe display subset only; raw tokens
// never leave this package.
type Identity struct {
	Subject string   `json:"subject"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
}

type identityKey struct{}

// WithIdentity attaches a verified identity to a request context.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the identity established by Protect, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Server is the OIDC gate: session middleware plus the /auth handlers.
type Server struct {
	// OIDC is the validated gate configuration. It is read at request
	// time; do not mutate it after the first use — the cached verifier
	// snapshots ClientID at discovery.
	OIDC config.OIDCAuth
	// APIKeys enables bearer-key authentication on /api paths (issue #64).
	// Keys live in the store; when OIDC is disabled the gate runs in a
	// keys-only mode that leaves /ui open and requires a key for /api.
	APIKeys bool
	// Store persists sessions and single-use login state.
	Store *store.Store
	// HTTPClient is used for discovery, token exchange, and JWKS key
	// refresh (the provider's key set fetches ride this client for the
	// process lifetime); nil = a client with a 30s timeout.
	HTTPClient *http.Client
	// Log receives failure diagnostics (never secret-adjacent content);
	// nil falls back to slog.Default(). Client-facing messages stay
	// generic on purpose — this is the operator's only trail.
	Log *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time

	// pendingTTL bounds an in-flight login; tests may shorten it.
	pendingTTL time.Duration

	mu        sync.Mutex
	provider  *oidc.Provider
	verifier  *oidc.IDTokenVerifier
	endLogout string
	// Negative cache for discovery: while the provider is unreachable,
	// retrying at most once per backoff keeps a hanging discovery from
	// serializing every login behind the mutex.
	lastFail    time.Time
	lastFailErr error

	winMu   sync.Mutex
	windows map[string]*rateWindow
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// pendingWindow is how long a login attempt stays valid between the
// redirect to the provider and the callback.
const pendingWindow = 10 * time.Minute

// Login-initiation rate limit, per source IP. A backstop against slamming
// the provider and flooding the pending table, not a primary control.
const (
	loginRateEvents = 10
	loginRatePer    = time.Minute
)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) loginTTL() time.Duration {
	if s.pendingTTL > 0 {
		return s.pendingTTL
	}
	return pendingWindow
}

func (s *Server) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Handler builds the /auth routes. Everything it serves is no-store.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("GET /auth/logout", s.handleLogout)
	mux.HandleFunc("/auth", http.NotFound)
	mux.HandleFunc("/auth/", http.NotFound)
	return noStore(mux)
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, req)
	})
}

// Protect gates the dashboard and API behind the session and, when API
// keys are enabled, bearer keys. Only /ui and /api are protected: /auth
// carries its own handlers and /report keeps its independent HMAC
// authentication (machine peers, not browsers). A failing session or key
// store answers 503 instead of a login redirect — the login cannot succeed
// against the same broken store, so the redirect would be a lie.
func (s *Server) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if isProtected(req.URL.Path) {
			if s.APIKeys && isAPIPath(req.URL.Path) {
				id, ok, presented, err := s.apiKeyIdentity(req)
				if err != nil {
					s.log().Error("api key store unavailable", "error", err)
					s.backendUnavailable(w)
					return
				}
				if presented {
					// A presented key is the request's credential claim: it
					// must validate, or the request is rejected — no silent
					// fallback to a session cookie.
					if ok {
						next.ServeHTTP(w, req.WithContext(WithIdentity(req.Context(), id)))
						return
					}
					s.reject(w, req)
					return
				}
				if !s.OIDC.Enabled {
					// Keys-only mode: no sessions exist, so an unkeyed API
					// request cannot authenticate.
					s.reject(w, req)
					return
				}
				// OIDC is on and no key was presented: the session flow below
				// decides (a browser session still unlocks /api).
			}
			if s.OIDC.Enabled {
				id, ok, err := s.identity(req)
				if err != nil {
					s.log().Error("web session store unavailable", "error", err)
					s.backendUnavailable(w)
					return
				}
				if ok {
					next.ServeHTTP(w, req.WithContext(WithIdentity(req.Context(), id)))
					return
				}
				s.reject(w, req)
				return
			}
			// Keys-only mode reaching this point is /ui: it stays open,
			// exactly as when no authentication is configured.
		}
		next.ServeHTTP(w, req)
	})
}

// backendUnavailable answers a failing auth store: fail closed with a
// retryable 503, never a redirect or a pass-through.
func (s *Server) backendUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "30")
	http.Error(w, "authentication backend unavailable", http.StatusServiceUnavailable)
}

func isProtected(path string) bool {
	return path == "/ui" || strings.HasPrefix(path, "/ui/") ||
		path == "/api" || strings.HasPrefix(path, "/api/")
}

// reject answers an unauthenticated request. API callers always get a JSON
// 401; dashboard navigations are redirected into the login flow, while
// fetch-style requests (live patching) get a 401 so live.js falls back to a
// full navigation, which then lands on the provider.
func (s *Server) reject(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if isAPIPath(req.URL.Path) || !isNavigation(req) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"authentication required"}`)
		return
	}
	then := url.QueryEscape(req.URL.RequestURI())
	http.Redirect(w, req, "/auth/login?then="+then, http.StatusFound)
}

func isAPIPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

// isNavigation reports whether the request looks like a document load.
// Sec-Fetch-Mode is absent on non-browser agents (curl, no-JS agents) and
// older browsers; those are treated as navigations so a human always gets
// the redirect, and scripted fetches (mode "cors") get the 401.
func isNavigation(req *http.Request) bool {
	mode := req.Header.Get("Sec-Fetch-Mode")
	return mode == "" || mode == "navigate"
}

// identity resolves the request's session cookie to a verified identity.
// A store error is returned to the caller (store doctrine: never treat a
// database error as "no session"); unreadable claims fail closed as
// unauthenticated with a warning — that is a store-health signal, not an
// auth verdict.
func (s *Server) identity(req *http.Request) (Identity, bool, error) {
	c, err := req.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return Identity{}, false, nil
	}
	hash := tokenHash(c.Value)
	sess, ok, err := s.Store.WebSession(req.Context(), hash, s.now())
	if err != nil {
		return Identity{}, false, err
	}
	if !ok {
		return Identity{}, false, nil
	}
	if tErr := s.Store.TouchWebSession(req.Context(), hash, s.now()); tErr != nil {
		s.log().Warn("touch web session", "error", tErr)
	}
	var id Identity
	if err := json.Unmarshal([]byte(sess.Claims), &id); err != nil {
		s.log().Warn("web session claims unreadable; failing closed", "error", err)
		return Identity{}, false, nil
	}
	if id.Subject == "" {
		id.Subject = sess.Subject
	}
	return id, true, nil
}
