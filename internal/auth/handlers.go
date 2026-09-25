package auth

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"

	"github.com/Dragonshorn-Studios/yukariko/internal/store"

	"golang.org/x/oauth2"
)

// errorPage renders a minimal, self-contained page: no stylesheet (the
// dashboard assets sit behind the gate this package enforces), no scripts,
// no inline styles — browser default typography is the entire design.
// Messages are static strings from this package or the provider's error
// code; html/template escapes everything.
var errorPageTmpl = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Yukariko · {{.Title}}</title>
</head>
<body>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
<p><a href="/ui">Return to the dashboard</a></p>
</body>
</html>
`))

func (s *Server) errorPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = errorPageTmpl.Execute(w, map[string]string{"Title": title, "Message": message})
}

// handleLogin starts the authorization-code flow: single-use state, nonce,
// and PKCE verifier are recorded server-side, then the browser is pointed
// at the provider.
func (s *Server) handleLogin(w http.ResponseWriter, req *http.Request) {
	if !s.rateLimit(clientIP(req)) {
		s.errorPage(w, http.StatusTooManyRequests, "Too many sign-in attempts",
			"Sign-in initiation is rate-limited per source address; try again shortly.")
		return
	}
	// Opportunistic sweep keeps the pending table bounded.
	_, _ = s.Store.SweepAuth(req.Context(), s.now())

	prov, _, _, err := s.idp()
	if err != nil {
		s.errorPage(w, http.StatusServiceUnavailable, "Sign-in is unavailable",
			"The identity provider could not be reached. This is a Yukariko-side display; try again in a moment.")
		return
	}
	then := sanitizeThen(req.URL.Query().Get("then"))
	state := randomToken()
	nonce := randomToken()
	verifier := oauth2.GenerateVerifier()
	now := s.now()
	if err := s.Store.CreatePendingAuth(req.Context(), store.AuthPending{
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
		RedirectTo:   then,
		CreatedAt:    now,
		ExpiresAt:    now.Add(s.loginTTL()),
	}); err != nil {
		s.errorPage(w, http.StatusInternalServerError, "Sign-in could not start",
			"The sign-in state could not be recorded. Try again.")
		return
	}
	cfg := s.oauthConfig(prov, "")
	authURL := cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce),
	)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, req, authURL, http.StatusFound)
}

// idClaims is the display subset carried out of a verified ID token.
type idClaims struct {
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	Groups            []string `json:"groups"`
}

// handleCallback finishes the flow: consume the single-use state, exchange
// the code (the only place the client secret is resolved), verify the ID
// token, enforce the group policy, and open the session.
func (s *Server) handleCallback(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	if e := q.Get("error"); e != "" {
		s.errorPage(w, http.StatusForbidden, "Sign-in was refused",
			"The identity provider reported: "+e+".")
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" {
		s.errorPage(w, http.StatusBadRequest, "Sign-in failed",
			"The sign-in response is incomplete; start again from the dashboard.")
		return
	}
	pending, ok, err := s.Store.ConsumePendingAuth(req.Context(), state, s.now())
	if err != nil {
		s.errorPage(w, http.StatusInternalServerError, "Sign-in failed",
			"The sign-in state could not be read. Try again.")
		return
	}
	if !ok {
		// Unknown, expired, or replayed: deliberately indistinguishable.
		s.errorPage(w, http.StatusBadRequest, "Sign-in expired",
			"The sign-in request is unknown, expired, or already used; start again from the dashboard.")
		return
	}

	secret, err := s.OIDC.ClientSecretRef.Resolve()
	if err != nil {
		s.errorPage(w, http.StatusServiceUnavailable, "Sign-in is unavailable",
			"The OIDC client secret could not be resolved from its reference. This is a server configuration problem.")
		return
	}
	prov, verifier, _, err := s.idp()
	if err != nil {
		s.errorPage(w, http.StatusServiceUnavailable, "Sign-in is unavailable",
			"The identity provider could not be reached.")
		return
	}
	ctx := context.WithValue(req.Context(), oauth2.HTTPClient, s.httpClient())
	cfg := s.oauthConfig(prov, secret)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(pending.CodeVerifier))
	if err != nil {
		s.errorPage(w, http.StatusForbidden, "Sign-in failed",
			"The identity provider rejected the sign-in exchange.")
		return
	}
	rawIDToken, hasIDToken := tok.Extra("id_token").(string)
	if !hasIDToken {
		s.errorPage(w, http.StatusForbidden, "Sign-in failed",
			"The identity provider returned no identity token.")
		return
	}
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		s.errorPage(w, http.StatusForbidden, "Sign-in failed",
			"The identity token did not verify (signature, issuer, audience, or expiry).")
		return
	}
	if idToken.Nonce != pending.Nonce {
		s.errorPage(w, http.StatusForbidden, "Sign-in failed",
			"The identity token nonce did not match this sign-in attempt.")
		return
	}
	var cl idClaims
	if err := idToken.Claims(&cl); err != nil {
		s.errorPage(w, http.StatusForbidden, "Sign-in failed",
			"The identity token's claims could not be read.")
		return
	}
	if !s.groupsAllowed(cl.Groups) {
		s.errorPage(w, http.StatusForbidden, "Not authorized",
			"Membership in one of the configured allowed groups is required.")
		return
	}

	subject := cl.PreferredUsername
	if subject == "" {
		subject = idToken.Subject
	}
	id := Identity{Subject: subject, Email: cl.Email, Name: cl.Name, Groups: cl.Groups}
	claimsJSON, err := json.Marshal(id)
	if err != nil {
		s.errorPage(w, http.StatusInternalServerError, "Sign-in failed",
			"The session could not be recorded. Try again.")
		return
	}
	now := s.now()
	token := randomToken()
	if err := s.Store.CreateWebSession(req.Context(), store.WebSession{
		TokenHash:  tokenHash(token),
		Subject:    subject,
		Claims:     string(claimsJSON),
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.sessionTTL()),
		LastSeenAt: now,
	}); err != nil {
		s.errorPage(w, http.StatusInternalServerError, "Sign-in failed",
			"The session could not be recorded. Try again.")
		return
	}
	s.setCookie(w, token)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, req, pending.RedirectTo, http.StatusFound)
}

// groupsAllowed enforces the optional allow-list against the verified
// groups claim: an empty configuration admits anyone the provider's own
// application policy admitted; a configured list is default-deny, and a
// missing claim satisfies nothing.
func (s *Server) groupsAllowed(groups []string) bool {
	if len(s.OIDC.AllowedGroups) == 0 {
		return true
	}
	for _, want := range s.OIDC.AllowedGroups {
		for _, got := range groups {
			if got == want {
				return true
			}
		}
	}
	return false
}

// handleLogout drops the server-side session and redirects to the
// provider's RP-initiated logout when discovery advertised one. GET logout
// trades a theoretical cross-site "forced logout" for the dashboard's
// form-free contract; the worst case is an annoyance, not exposure.
func (s *Server) handleLogout(w http.ResponseWriter, req *http.Request) {
	if c, err := req.Cookie(s.cookieName()); err == nil && c.Value != "" {
		_ = s.Store.DeleteWebSession(req.Context(), tokenHash(c.Value))
	}
	s.clearCookie(w)
	w.Header().Set("Cache-Control", "no-store")
	if _, _, end, err := s.idp(); err == nil && end != "" {
		u := end + "?" + url.Values{
			"post_logout_redirect_uri": {s.OIDC.RedirectBase + "/ui"},
		}.Encode()
		http.Redirect(w, req, u, http.StatusFound)
		return
	}
	http.Redirect(w, req, "/ui", http.StatusFound)
}
