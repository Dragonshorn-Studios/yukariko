package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// Cookie names. The __Host- prefix (no domain, path /, Secure-only) is used
// whenever the external origin is https; plain-http loopback testing keeps
// an ordinary name because __Host- cookies require a secure connection.
const (
	secureCookieName   = "__Host-yukariko_session"
	insecureCookieName = "yukariko_session"
)

// randomToken returns a 256-bit random value in cookie-safe encoding. It
// carries no meaning; only its SHA-256 is ever stored.
func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) secureCookies() bool {
	return strings.HasPrefix(s.OIDC.RedirectBase, "https:")
}

func (s *Server) cookieName() string {
	if s.secureCookies() {
		return secureCookieName
	}
	return insecureCookieName
}

func (s *Server) sessionTTL() time.Duration {
	if ttl := s.OIDC.SessionTTL.D(); ttl > 0 {
		return ttl
	}
	return config.DefaultOIDCSessionTTL.D()
}

func (s *Server) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.sessionTTL().Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

// sanitizeThen keeps the post-login landing local: an absolute URL, a
// protocol-relative //host, or an /auth path would turn the redirect into
// an open redirect or a loop.
func sanitizeThen(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/auth") {
		return "/ui"
	}
	return raw
}

// rateWindow is a fixed-window counter (receiver pattern).
type rateWindow struct {
	start time.Time
	count int
}

func (s *Server) rateLimit(key string) bool {
	if s.windows == nil {
		s.windows = map[string]*rateWindow{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	win := s.windows[key]
	if win == nil || now.Sub(win.start) >= loginRatePer {
		win = &rateWindow{start: now}
		s.windows[key] = win
	}
	win.count++
	return win.count <= loginRateEvents
}

// clientIP is the transport peer. Behind a reverse proxy every browser
// shares the proxy's address, which still bounds total login-initiation
// volume; trusting X-Forwarded-For is deliberately out of scope.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}
