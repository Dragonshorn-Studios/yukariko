package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIdP is a minimal OpenID Connect provider for tests: real discovery,
// real RSA-signed ID tokens over a real JWKS endpoint, PKCE verification,
// and hooks to mint misbehaving tokens. No live Authentik required.
type fakeIdP struct {
	ts   *httptest.Server
	priv *rsa.PrivateKey
	// otherKey signs with a key absent from the JWKS (bad-kid path).
	otherKey *rsa.PrivateKey

	ClientID     string
	ClientSecret string

	mu       sync.Mutex
	stateCh  map[string]string // state -> code_challenge
	stateN   map[string]string // state -> nonce
	codes    map[string]string // code -> state
	nextCode int
	verifs   []string // recorded code verifiers

	// tokenHook can rewrite claims per test (bad aud, expired, wrong
	// nonce, groups, ...). nil = the well-behaved default.
	tokenHook func(claims map[string]any, state, nonce string)
	useOther  bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	f := &fakeIdP{
		priv: priv, otherKey: other,
		ClientID: "yukariko-test", ClientSecret: "s3cret",
		stateCh: map[string]string{}, stateN: map[string]string{}, codes: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                f.ts.URL,
			"authorization_endpoint":                f.ts.URL + "/authorize",
			"token_endpoint":                        f.ts.URL + "/token",
			"jwks_uri":                              f.ts.URL + "/jwks",
			"end_session_endpoint":                  f.ts.URL + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &priv.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/authorize", f.authorize)
	mux.HandleFunc("/token", f.token)
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeIdP) authorize(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	state := q.Get("state")
	challenge := q.Get("code_challenge")
	if state == "" || challenge == "" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.stateCh[state] = challenge
	f.stateN[state] = q.Get("nonce")
	f.nextCode++
	code := fmt.Sprintf("code-%d", f.nextCode)
	f.codes[code] = state
	f.mu.Unlock()
	target := q.Get("redirect_uri") + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	http.Redirect(w, req, target, http.StatusFound)
}

func (f *fakeIdP) token(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret := req.PostForm.Get("client_id"), req.PostForm.Get("client_secret")
	if id == "" && secret == "" {
		if u, p, ok := req.BasicAuth(); ok && u != "" {
			id, secret = u, p
		}
	}
	if id != f.ClientID || secret != f.ClientSecret {
		http.Error(w, "bad client", http.StatusUnauthorized)
		return
	}
	code := req.PostForm.Get("code")
	f.mu.Lock()
	state, known := f.codes[code]
	challenge := f.stateCh[state]
	nonce := f.stateN[state]
	verifier := req.PostForm.Get("code_verifier")
	f.verifs = append(f.verifs, verifier)
	f.mu.Unlock()
	if !known || verifier == "" {
		http.Error(w, "unknown code or missing verifier", http.StatusBadRequest)
		return
	}
	// PKCE: BASE64URL(SHA256(verifier)) must equal the stored challenge.
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		http.Error(w, "pkce mismatch", http.StatusBadRequest)
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": f.ts.URL, "sub": "user-1", "aud": f.ClientID,
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": nonce,
		"preferred_username": "ada", "email": "ada@example.com",
		"groups": []string{"ops"},
	}
	if f.tokenHook != nil {
		f.tokenHook(claims, state, nonce)
	}
	key := f.priv
	if f.useOther {
		key = f.otherKey
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		http.Error(w, "signer", http.StatusInternalServerError)
		return
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		http.Error(w, "sign", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": raw,
	})
}

// harness wires a real store, the auth server, and a CLI-shaped root mux
// (ui + api + report + auth behind Protect) exactly like the daemon does.
type harness struct {
	srv   *Server
	ts    *httptest.Server
	idp   *fakeIdP
	store *store.Store
	clock *fakeClock
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newHarness(t *testing.T, mutate func(o *config.OIDCAuth), idp *fakeIdP) *harness {
	t.Helper()
	if idp == nil {
		idp = newFakeIdP(t)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// The client secret resolves through the file branch of SecretRef; env
	// resolution would need t.Setenv, which forbids parallel tests.
	secretPath := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretPath, []byte(idp.ClientSecret), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	clock := &fakeClock{now: time.Now().UTC().Truncate(time.Second)}
	o := config.OIDCAuth{
		Enabled: true, Issuer: idp.ts.URL, ClientID: idp.ClientID,
		ClientSecretRef: &config.SecretRef{File: secretPath},
		SessionTTL:      config.Duration(time.Hour),
		Scopes:          []string{"openid", "profile", "email"},
	}
	if mutate != nil {
		mutate(&o)
	}
	srv := &Server{OIDC: o, Store: st, Now: clock.Now}

	mux := http.NewServeMux()
	mux.HandleFunc("/ui", func(w http.ResponseWriter, req *http.Request) {
		if id, ok := FromContext(req.Context()); ok {
			fmt.Fprintf(w, "ok %s", id.Subject)
			return
		}
		fmt.Fprint(w, "anon")
	})
	mux.HandleFunc("/api/v1/apps", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"apps":[]}`)
	})
	mux.HandleFunc("/report/v1/events", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"accepted":true}`)
	})
	mux.Handle("/auth/", srv.Handler())
	ts := httptest.NewServer(srv.Protect(mux))
	t.Cleanup(ts.Close)
	srv.OIDC.RedirectBase = ts.URL
	return &harness{srv: srv, ts: ts, idp: idp, store: st, clock: clock}
}

// noRedirect never follows a redirect, so each hop is assertable.
func noRedirect() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// login walks the full flow and returns the session cookie plus the
// authorize URL it passed through.
func (h *harness) login(t *testing.T) (*http.Cookie, *url.URL) {
	t.Helper()
	client := noRedirect()

	res, err := client.Get(h.ts.URL + "/ui")
	if err != nil {
		t.Fatalf("GET /ui: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("GET /ui = %d, want 302 to login", res.StatusCode)
	}
	loginURL := res.Header.Get("Location")

	res, err = client.Get(h.ts.URL + loginURL)
	if err != nil {
		t.Fatalf("GET login: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("GET login = %d, want 302 to provider", res.StatusCode)
	}
	authURL, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatalf("authorize location: %v", err)
	}

	// Through the provider: authorize redirects straight back to the callback.
	res, err = client.Get(authURL.String())
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("GET authorize = %d, want 302 to callback", res.StatusCode)
	}
	callback := res.Header.Get("Location")

	res, err = client.Get(callback)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	if res.StatusCode != http.StatusFound {
		body := drain(res)
		t.Fatalf("GET callback = %d (%s), want 302 to destination", res.StatusCode, body)
	}
	res.Body.Close()
	if loc := res.Header.Get("Location"); loc != "/ui" {
		t.Errorf("callback redirect = %q, want /ui", loc)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("callback set %d cookies, want 1", len(cookies))
	}
	return cookies[0], authURL
}

func drain(res *http.Response) string {
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return string(body)
}

func TestFullLoginFlowGrantsSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	cookie, authURL := h.login(t)

	q := authURL.Query()
	if q.Get("client_id") != h.idp.ClientID {
		t.Errorf("authorize client_id = %q", q.Get("client_id"))
	}
	if got := q.Get("redirect_uri"); got != h.ts.URL+"/auth/callback" {
		t.Errorf("authorize redirect_uri = %q", got)
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Error("authorize missing S256 PKCE")
	}

	// The session cookie unlocks the dashboard and the API.
	client := h.ts.Client()
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/ui", nil)
	req.AddCookie(cookie)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /ui with cookie: %v", err)
	}
	defer res.Body.Close()
	if body := drain(res); body != "ok ada" {
		t.Errorf("GET /ui body = %q, want %q", body, "ok ada")
	}
	req, _ = http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/apps", nil)
	req.AddCookie(cookie)
	res2, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api with cookie: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Errorf("GET /api with cookie = %d, want 200", res2.StatusCode)
	}

	// The provider enforced PKCE end to end.
	h.idp.mu.Lock()
	defer h.idp.mu.Unlock()
	if len(h.idp.verifs) == 0 || h.idp.verifs[0] == "" {
		t.Error("token exchange carried no PKCE verifier")
	}
}

func TestUnauthenticatedRejectionShapes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	client := noRedirect()

	// API: always a JSON 401.
	res, err := client.Get(h.ts.URL + "/api/v1/apps")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("API unauthenticated = %d, want 401", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("API 401 content type = %q", ct)
	}

	// UI navigation (Sec-Fetch-Mode absent, like curl or a plain link):
	// redirect into the login flow.
	res, err = client.Get(h.ts.URL + "/ui/vestments")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Errorf("UI navigation = %d, want 302", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, "/auth/login?then=") {
		t.Errorf("UI redirect = %q, want /auth/login?then=…", loc)
	} else if !strings.Contains(loc, url.QueryEscape("/ui/vestments")) {
		t.Errorf("UI redirect loses the destination: %q", loc)
	}

	// UI fetch-style request (live.js): 401, not a redirect.
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/ui", nil)
	req.Header.Set("Sec-Fetch-Mode", "cors")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("UI fetch-style = %d, want 401", res.StatusCode)
	}

	// Reports keep their own HMAC channel: never session-gated.
	res, err = client.Post(h.ts.URL+"/report/v1/events", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("report endpoint = %d, want 200 (own auth, not session-gated)", res.StatusCode)
	}
}

func TestStateIsSingleUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	client := noRedirect()

	// Start a login and capture the authorize hop.
	res, _ := client.Get(h.ts.URL + "/ui")
	res.Body.Close()
	res, _ = client.Get(h.ts.URL + res.Header.Get("Location"))
	res.Body.Close()
	authURL := res.Header.Get("Location")

	res, _ = client.Get(authURL) // authorize -> callback
	res.Body.Close()
	callback := res.Header.Get("Location")

	// First use succeeds; the exact same URL must then be dead.
	res, err := client.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("first callback = %d, want 302", res.StatusCode)
	}
	res, err = client.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("replayed callback = %d, want 400", res.StatusCode)
	}
}

func TestUnknownStateRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	client := noRedirect()
	res, err := client.Get(h.ts.URL + "/auth/callback?code=c&state=nope")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown state = %d, want 400", res.StatusCode)
	}
}

func TestTokenVerificationFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		hook func(claims map[string]any, state, nonce string)
	}{
		{
			name: "expired token",
			hook: func(c map[string]any, _, _ string) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		},
		{
			name: "wrong audience",
			hook: func(c map[string]any, _, _ string) { c["aud"] = "some-other-client" },
		},
		{
			name: "wrong issuer",
			hook: func(c map[string]any, _, _ string) { c["iss"] = "https://evil.example.com" },
		},
		{
			name: "wrong nonce",
			hook: func(c map[string]any, _, _ string) { c["nonce"] = "not-this-nonce" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idp := newFakeIdP(t)
			idp.tokenHook = tt.hook
			h := newHarness(t, nil, idp)
			client := noRedirect()

			res, _ := client.Get(h.ts.URL + "/ui")
			res.Body.Close()
			res, _ = client.Get(h.ts.URL + res.Header.Get("Location"))
			res.Body.Close()
			res, _ = client.Get(res.Header.Get("Location"))
			res.Body.Close()

			res, err := client.Get(res.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("%s: callback = %d (%s), want 403", tt.name, res.StatusCode, drain(res))
			} else {
				res.Body.Close()
			}
		})
	}
}

func TestSignatureFromUnknownKeyRejected(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.useOther = true // signs with a key whose kid is not in the JWKS
	h := newHarness(t, nil, idp)
	client := noRedirect()

	res, _ := client.Get(h.ts.URL + "/ui")
	res.Body.Close()
	res, _ = client.Get(h.ts.URL + res.Header.Get("Location"))
	res.Body.Close()
	res, _ = client.Get(res.Header.Get("Location"))
	res.Body.Close()

	res, err := client.Get(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("callback = %d, want 403 for unresolvable key", res.StatusCode)
	}
}

func TestAllowedGroupsEnforced(t *testing.T) {
	t.Parallel()
	// Configured list is default-deny: deny non-members and missing claims,
	// admit members.
	tests := []struct {
		name   string
		groups []string
		hook   func(claims map[string]any, state, nonce string)
		want   int
	}{
		{name: "member allowed", groups: []string{"ops"}, want: http.StatusFound},
		{
			name: "non-member denied", groups: []string{"ops"},
			hook: func(c map[string]any, _, _ string) { c["groups"] = []string{"everyone"} },
			want: http.StatusForbidden,
		},
		{
			name: "missing claim denied", groups: []string{"ops"},
			hook: func(c map[string]any, _, _ string) { delete(c, "groups") },
			want: http.StatusForbidden,
		},
		{
			name: "unset list admits provider-admitted user", groups: nil,
			hook: func(c map[string]any, _, _ string) { c["groups"] = []string{"whoever"} },
			want: http.StatusFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idp := newFakeIdP(t)
			if tt.hook != nil {
				idp.tokenHook = tt.hook
			}
			h := newHarness(t, func(o *config.OIDCAuth) { o.AllowedGroups = tt.groups }, idp)
			client := noRedirect()

			res, _ := client.Get(h.ts.URL + "/ui")
			res.Body.Close()
			res, _ = client.Get(h.ts.URL + res.Header.Get("Location"))
			res.Body.Close()
			res, _ = client.Get(res.Header.Get("Location"))
			res.Body.Close()

			res, err := client.Get(res.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != tt.want {
				t.Errorf("callback = %d (%s), want %d", res.StatusCode, drain(res), tt.want)
			} else {
				res.Body.Close()
			}
		})
	}
}

func TestSessionExpiry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	cookie, _ := h.login(t)

	// Advance beyond the TTL: the session is gone and the old cookie
	// authenticates nothing.
	h.clock.now = h.clock.now.Add(2 * time.Hour)
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/ui", nil)
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.AddCookie(cookie)
	res, err := noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired session = %d, want 401", res.StatusCode)
	}
}

func TestLogoutDropsSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	cookie, _ := h.login(t)

	client := noRedirect()
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/auth/logout", nil)
	req.AddCookie(cookie)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("logout = %d, want 302", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, h.idp.ts.URL+"/logout") {
		t.Errorf("logout redirect = %q, want the provider's end-session endpoint", loc)
	}
	if !strings.Contains(loc, "post_logout_redirect_uri=") {
		t.Errorf("logout redirect lacks post_logout_redirect_uri: %q", loc)
	}
	// The session cookie is cleared and the row is gone.
	var cleared bool
	for _, c := range res.Cookies() {
		if c.Name == h.srv.cookieName() && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not clear the session cookie")
	}
	if _, ok, _ := h.store.WebSession(t.Context(), tokenHash(cookie.Value), time.Now()); ok {
		t.Error("logout left the session row in the store")
	}

	// The old cookie no longer authenticates: the dashboard bounces an
	// unauthenticated navigation into the login flow.
	req, _ = http.NewRequest(http.MethodGet, h.ts.URL+"/ui", nil)
	req.AddCookie(cookie)
	res, err = noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Errorf("old cookie after logout = %d, want 302 to login", res.StatusCode)
	} else if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, "/auth/login?") {
		t.Errorf("old cookie after logout redirects to %q, want the login flow", loc)
	}
}

func TestProviderDownSurfacesAtLogin(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	url := idp.ts.URL
	idp.ts.Close() // provider unreachable from the start
	h := newHarness(t, nil, idp)
	_ = url
	client := noRedirect()

	res, err := client.Get(h.ts.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("login with dead provider = %d, want 503", res.StatusCode)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("auth page Cache-Control = %q, want no-store", cc)
	}
}

func TestLoginRateLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	client := noRedirect()
	for i := 0; i < loginRateEvents; i++ {
		res, err := client.Get(h.ts.URL + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusFound {
			t.Fatalf("login %d = %d, want 302 (provider is healthy)", i+1, res.StatusCode)
		}
	}
	res, err := client.Get(h.ts.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("login over limit = %d, want 429", res.StatusCode)
	}
}

func TestCookieFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		redirectBase string
		wantName     string
		wantSecure   bool
	}{
		{name: "https origin uses __Host- and Secure", redirectBase: "https://yukariko.example.com", wantName: "__Host-yukariko_session", wantSecure: true},
		{name: "loopback http keeps a plain cookie", redirectBase: "http://127.0.0.1:8484", wantName: "yukariko_session", wantSecure: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{OIDC: config.OIDCAuth{RedirectBase: tt.redirectBase, SessionTTL: config.Duration(time.Hour)}}
			rec := httptest.NewRecorder()
			s.setCookie(rec, "token")
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("set %d cookies", len(cookies))
			}
			c := cookies[0]
			if c.Name != tt.wantName || c.Secure != tt.wantSecure || !c.HttpOnly ||
				c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 3600 {
				t.Errorf("cookie = %+v", c)
			}
		})
	}
}

func TestSanitizeThen(t *testing.T) {
	t.Parallel()
	tests := []struct{ raw, want string }{
		{"", "/ui"},
		{"/ui/vestments", "/ui/vestments"},
		{"/ui?app=web", "/ui?app=web"},
		{"https://evil.example.com", "/ui"},
		{"//evil.example.com", "/ui"},
		{"/auth/callback", "/ui"},
		{"/auth", "/ui"},
		{"relative", "/ui"},
	}
	for _, tt := range tests {
		if got := sanitizeThen(tt.raw); got != tt.want {
			t.Errorf("sanitizeThen(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestErrorPageIsEscapedAndStrict(t *testing.T) {
	t.Parallel()
	s := &Server{OIDC: config.OIDCAuth{}}
	rec := httptest.NewRecorder()
	s.errorPage(rec, http.StatusForbidden, "Not authorized", "provider reported: <script>alert(1)</script>")
	res := rec.Result()
	res.Body.Close()
	if res.Header.Get("Content-Security-Policy") != "default-src 'none'; style-src 'none'" {
		t.Errorf("CSP = %q", res.Header.Get("Content-Security-Policy"))
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("error page reflects unescaped script: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("error page does not escape payload: %s", body)
	}
}
