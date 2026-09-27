package auth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// keyHarness builds a gate with API keys enabled and no identity provider:
// keys-only tests never need the OIDC flow, and sessions — when a case
// needs one — are seeded directly into the store.
type keyHarness struct {
	srv   *Server
	ts    *httptest.Server
	store *store.Store
	clock *fakeClock
}

func newKeyHarness(t *testing.T, oidcEnabled bool) *keyHarness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	clock := &fakeClock{now: time.Now().UTC().Truncate(time.Second)}
	srv := &Server{
		OIDC:    oidcConfigFor(oidcEnabled),
		APIKeys: true,
		Store:   st,
		Now:     clock.Now,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ui", func(w http.ResponseWriter, req *http.Request) {
		if id, ok := FromContext(req.Context()); ok {
			fmt.Fprintf(w, "ok %s", id.Subject)
			return
		}
		fmt.Fprint(w, "anon")
	})
	mux.HandleFunc("/api/v1/apps", func(w http.ResponseWriter, req *http.Request) {
		if id, ok := FromContext(req.Context()); ok {
			fmt.Fprintf(w, "api %s", id.Subject)
			return
		}
		fmt.Fprint(w, "api anon")
	})
	mux.HandleFunc("/report/v1/events", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"accepted":true}`)
	})
	ts := httptest.NewServer(srv.Protect(mux))
	t.Cleanup(ts.Close)
	return &keyHarness{srv: srv, ts: ts, store: st, clock: clock}
}

// oidcConfigFor keeps the two harness modes straight: enabled means "OIDC
// gate on" (the issuer is never contacted by key tests), disabled means
// keys-only mode.
func oidcConfigFor(enabled bool) (o config.OIDCAuth) {
	o.Enabled = enabled
	return o
}

// seedKey stores a key and returns its bearer token.
func (h *keyHarness) seedKey(t *testing.T, name string, mutate func(k *store.APIKey)) string {
	t.Helper()
	token, hash := GenerateAPIKey()
	k := store.APIKey{Name: name, TokenHash: hash, CreatedAt: h.clock.now}
	if mutate != nil {
		mutate(&k)
	}
	if err := h.store.CreateAPIKey(context.Background(), k); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return token
}

func (h *keyHarness) get(t *testing.T, path, bearer string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	return res, drain(res)
}

func TestAPIKeyAuthenticatesAPI(t *testing.T) {
	t.Parallel()
	h := newKeyHarness(t, false)
	token := h.seedKey(t, "amadeus", nil)

	res, body := h.get(t, "/api/v1/apps", token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api with key = %d (%s), want 200", res.StatusCode, body)
	}
	if body != "api apikey:amadeus" {
		t.Errorf("GET /api body = %q, want %q", body, "api apikey:amadeus")
	}

	// Successful use stamps last_used_at.
	k, ok, err := h.store.APIKeyByName(context.Background(), "amadeus")
	if err != nil || !ok {
		t.Fatalf("APIKeyByName: ok=%v err=%v", ok, err)
	}
	if k.LastUsedAt == nil {
		t.Error("last_used_at was not touched")
	}
}

func TestAPIKeyRejectionMatrix(t *testing.T) {
	t.Parallel()
	h := newKeyHarness(t, false)
	valid := h.seedKey(t, "amadeus", nil)
	expired := h.seedKey(t, "expired", func(k *store.APIKey) {
		past := h.clock.now.Add(-time.Minute)
		k.ExpiresAt = &past
	})
	revoked := h.seedKey(t, "revoked", nil)
	revokedKey, ok, err := h.store.APIKeyByName(context.Background(), "revoked")
	if err != nil || !ok {
		t.Fatalf("revoke setup lookup: ok=%v err=%v", ok, err)
	}
	if ok, err := h.store.RevokeAPIKey(context.Background(), revokedKey.ID, h.clock.now); err != nil || !ok {
		t.Fatalf("revoke setup: %v, %v", ok, err)
	}

	cases := []struct {
		name   string
		bearer string
	}{
		{"no header", ""},
		{"unknown key", APIKeyPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"foreign bearer token", "some-other-service-token"},
		{"expired key", expired},
		{"revoked key", revoked},
	}
	for _, c := range cases {
		res, body := h.get(t, "/api/v1/apps", c.bearer)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: GET /api = %d (%s), want 401", c.name, res.StatusCode, body)
		}
		if got := res.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: content type = %q, want application/json", c.name, got)
		}
	}

	// The scheme is case-insensitive per RFC 7235: a lowercase bearer
	// prefix must authenticate the same way.
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/apps", nil)
	req.Header.Set("Authorization", "bearer "+valid)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api with lowercase scheme: %v", err)
	}
	resBody := drain(res)
	if res.StatusCode != http.StatusOK || resBody != "api apikey:amadeus" {
		t.Errorf("GET /api with lowercase scheme = %d (%s), want 200", res.StatusCode, resBody)
	}
}

func TestAPIKeyKeysOnlyLeavesUIDashboardAndReportOpen(t *testing.T) {
	t.Parallel()
	h := newKeyHarness(t, false)
	token := h.seedKey(t, "amadeus", nil)

	// /ui is open in keys-only mode; a bearer key neither unlocks an
	// identity there nor breaks the page.
	res, body := h.get(t, "/ui", "")
	if res.StatusCode != http.StatusOK || body != "anon" {
		t.Errorf("GET /ui without key = %d (%s), want 200 anon", res.StatusCode, body)
	}
	res, body = h.get(t, "/ui", token)
	if res.StatusCode != http.StatusOK || body != "anon" {
		t.Errorf("GET /ui with key = %d (%s), want 200 anon", res.StatusCode, body)
	}

	// /report keeps its own HMAC channel and is never session/key gated.
	res, body = h.get(t, "/report/v1/events", "")
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /report = %d (%s), want 200", res.StatusCode, body)
	}
}

func TestAPIKeyCoexistsWithSessions(t *testing.T) {
	t.Parallel()
	h := newKeyHarness(t, true)
	token := h.seedKey(t, "amadeus", nil)

	// Seed a session directly: the IdP flow is covered by the OIDC tests;
	// this only needs a live session row for the coexistence matrix.
	now := h.clock.now
	if err := h.store.CreateWebSession(context.Background(), store.WebSession{
		TokenHash:  tokenHash("sess-token"),
		Subject:    "ada",
		Claims:     "{}",
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Hour),
		LastSeenAt: now,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Cookie unlocks /api.
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/apps", nil)
	req.AddCookie(&http.Cookie{Name: insecureCookieName, Value: "sess-token"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api with cookie: %v", err)
	}
	body := drain(res)
	if res.StatusCode != http.StatusOK || body != "api ada" {
		t.Errorf("GET /api with cookie = %d (%s), want 200 api ada", res.StatusCode, body)
	}

	// A valid key unlocks /api too.
	res2, body2 := h.get(t, "/api/v1/apps", token)
	if res2.StatusCode != http.StatusOK || body2 != "api apikey:amadeus" {
		t.Errorf("GET /api with key = %d (%s), want 200 api apikey:amadeus", res2.StatusCode, body2)
	}

	// A presented-but-invalid key beats a valid session: credentials that
	// fail must not silently fall back.
	req3, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/apps", nil)
	req3.AddCookie(&http.Cookie{Name: insecureCookieName, Value: "sess-token"})
	req3.Header.Set("Authorization", "Bearer "+APIKeyPrefix+"bogus")
	res3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("GET /api with bad key and good cookie: %v", err)
	}
	res3.Body.Close()
	if res3.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api with bad key and good cookie = %d, want 401", res3.StatusCode)
	}
}

// A session cookie that outlives an oidc→keys-only config flip must not
// authenticate /api: in keys-only mode the session flow never runs.
func TestAPIKeyKeysOnlyRejectsStaleSessions(t *testing.T) {
	t.Parallel()
	h := newKeyHarness(t, false)
	now := h.clock.now
	if err := h.store.CreateWebSession(context.Background(), store.WebSession{
		TokenHash:  tokenHash("stale-token"),
		Subject:    "ada",
		Claims:     "{}",
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Hour),
		LastSeenAt: now,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/apps", nil)
	req.AddCookie(&http.Cookie{Name: insecureCookieName, Value: "stale-token"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api with stale cookie: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api with cookie in keys-only mode = %d, want 401", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q, want application/json", got)
	}
}

func TestAPIKeyStoreFailureAnswers503(t *testing.T) {
	t.Parallel()
	h := newKeyHarness(t, false)
	token := h.seedKey(t, "amadeus", nil)

	// A dead key store must answer 503 on a presented key, never a 401
	// (store doctrine: a database error is not an auth verdict).
	if err := h.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	res, _ := h.get(t, "/api/v1/apps", token)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /api with closed store = %d, want 503", res.StatusCode)
	}
	if retry := res.Header.Get("Retry-After"); retry == "" {
		t.Error("503 should carry Retry-After")
	}
}

func TestGenerateAPIKeyFormat(t *testing.T) {
	t.Parallel()
	token, hash := GenerateAPIKey()
	if len(token) < len(APIKeyPrefix)+20 || token[:len(APIKeyPrefix)] != APIKeyPrefix {
		t.Errorf("token %q lacks the %q prefix", token, APIKeyPrefix)
	}
	if hash == token || len(hash) != 64 {
		t.Errorf("hash = %q; want the 64-hex SHA-256, not the token", hash)
	}
	token2, hash2 := GenerateAPIKey()
	if token == token2 || hash == hash2 {
		t.Error("two generated keys collided")
	}
}
