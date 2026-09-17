package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// errorsAs reports whether err carries a *Error.
func errorsAs(err error, target **Error) bool {
	return errors.As(err, target)
}

// --- fake registry ----------------------------------------------------------

type storedManifest struct {
	body        []byte
	contentType string
}

// fakeRegistry implements enough of the Distribution v2 protocol for the
// tests: bearer-token auth, manifests by tag and digest, a multi-arch
// index, redirects, rate limiting, malformed payloads, and delays.
type fakeRegistry struct {
	mu                sync.Mutex
	auth              bool // challenge /v2/ and require a bearer token on manifests
	requireTokenBasic bool // the token endpoint demands HTTP basic credentials
	realmOverride     string
	seenTokens        []string // Authorization headers on manifest requests
	seenBasic         []string // Authorization headers on the token endpoint
	forbidden         map[string]bool
	manifests         map[string]storedManifest
	redirects         map[string]string // reference → real reference (307)
	rateLimit         int               // remaining 429 responses
	malformed         map[string]bool
	delay             time.Duration
	baseURL           string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{
		manifests: map[string]storedManifest{},
		redirects: map[string]string{},
		forbidden: map[string]bool{},
		malformed: map[string]bool{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.baseURL = srv.URL
	return f
}

// urlOf returns the registry host:port form used inside references.
func (f *fakeRegistry) host(t *testing.T) string {
	t.Helper()
	return strings.TrimPrefix(f.baseURL, "http://")
}

func (f *fakeRegistry) set(name, reference string, m storedManifest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifests[name+"|"+reference] = m
}

func digestOf(body []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}

// putManifest stores a manifest by content digest and optional tags,
// returning the digest.
func (f *fakeRegistry) putManifest(t *testing.T, name string, tags []string, body []byte, contentType string) string {
	t.Helper()
	d := digestOf(body)
	f.mu.Lock()
	f.manifests[name+"|"+d] = storedManifest{body: body, contentType: contentType}
	for _, tag := range tags {
		f.manifests[name+"|"+tag] = storedManifest{body: body, contentType: contentType}
	}
	f.mu.Unlock()
	return d
}

func manifestJSON(repo, tag string) []byte {
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"size":      1234,
			"digest":    "sha256:" + fmt.Sprintf("%064x", len(repo)+len(tag)),
		},
		"layers":     []any{},
		"annotation": repo + ":" + tag,
	})
	// Deterministic bytes: re-marshal through a map loses order, so build
	// the JSON from the struct field order Go emits — stable enough because
	// the same bytes are stored and served (the digest is computed from the
	// stored body).
	return body
}

func indexJSON(children map[string]string) []byte {
	type mf struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
		Platform  struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant,omitempty"`
		} `json:"platform"`
	}
	entries := []mf{}
	for plat, d := range children {
		parts := strings.SplitN(plat, "/", 3)
		m := mf{Digest: d, MediaType: "application/vnd.docker.distribution.manifest.v2+json"}
		m.Platform.OS = parts[0]
		m.Platform.Architecture = parts[1]
		if len(parts) > 2 {
			m.Platform.Variant = parts[2]
		}
		entries = append(entries, m)
	}
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.list.v2+json",
		"manifests":     entries,
	})
	return body
}

func (f *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	switch {
	case req.URL.Path == "/v2/":
		if f.auth {
			realm := f.baseURL + "/token"
			if f.realmOverride != "" {
				realm = f.realmOverride
			}
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf("Bearer realm=%q,service=%q,scope=%q",
					realm, "yukariko-test", "repository:app:pull"))
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
		return
	case req.URL.Path == "/token":
		if req.Header.Get("Authorization") != "" {
			f.seenBasic = append(f.seenBasic, req.Header.Get("Authorization"))
		}
		if f.requireTokenBasic && !strings.HasPrefix(req.Header.Get("Authorization"), "Basic ") {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "test-issued-token"})
		return
	}

	if !strings.HasPrefix(req.URL.Path, "/v2/") {
		w.WriteHeader(404)
		return
	}
	rest := strings.TrimPrefix(req.URL.Path, "/v2/")
	if !strings.Contains(rest, "/manifests/") {
		w.WriteHeader(404)
		return
	}
	parts := strings.SplitN(rest, "/manifests/", 2)
	name, reference := parts[0], parts[1]

	if f.auth {
		tok := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		f.seenTokens = append(f.seenTokens, tok)
		if tok != "test-issued-token" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf("Bearer realm=%q,service=%q", f.baseURL+"/token", "yukariko-test"))
			w.WriteHeader(401)
			return
		}
	}
	if f.forbidden[reference] {
		w.WriteHeader(403)
		return
	}
	if f.rateLimit > 0 {
		f.rateLimit--
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
		return
	}
	if f.malformed[name+"|"+reference] {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Write([]byte("{not json"))
		return
	}
	if real, ok := f.redirects[reference]; ok {
		http.Redirect(w, req, "/v2/"+name+"/manifests/"+real, http.StatusTemporaryRedirect)
		return
	}
	m, ok := f.manifests[name+"|"+reference]
	if !ok {
		w.WriteHeader(404)
		return
	}
	w.Header().Set("Content-Type", m.contentType)
	w.Header().Set("Docker-Content-Digest", digestOf(m.body))
	w.Write(m.body)
}

func testResolver(f *fakeRegistry) *Resolver {
	host := strings.TrimPrefix(f.baseURL, "http://")
	return &Resolver{
		EndpointOverride: map[string]string{host: f.baseURL},
		Runner:           nil,
	}
}

// --- resolver tests ---------------------------------------------------------

func TestResolvePlainManifest(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	body := manifestJSON("app", "1")
	d := f.putManifest(t, "app", []string{"1"}, body, "application/vnd.docker.distribution.manifest.v2+json")

	ref, _ := ParseRef(f.host(t) + "/app:1")
	got, err := testResolver(f).Resolve(context.Background(), ref, "linux/amd64")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != d {
		t.Errorf("digest = %q, want %q (sha256 of the manifest body)", got, d)
	}
}

func TestDigestPinnedRefIsStable(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	stable := f.putManifest(t, "app", nil, manifestJSON("app", "stable"), "application/vnd.docker.distribution.manifest.v2+json")
	f.set("app", "moving", storedManifest{body: manifestJSON("app", "moved"), contentType: "application/vnd.docker.distribution.manifest.v2+json"})

	pinned, _ := ParseRef(f.host(t) + "/app@" + stable)
	got, err := testResolver(f).Resolve(context.Background(), pinned, "linux/amd64")
	if err != nil || got != stable {
		t.Fatalf("digest-pinned resolve = %q / %v, want %q", got, err, stable)
	}

	// The moving tag changing does not affect the digest-pinned reference.
	f.set("app", "moving", storedManifest{body: []byte(`{"different":true}`), contentType: "application/vnd.docker.distribution.manifest.v2+json"})
	got2, err := testResolver(f).Resolve(context.Background(), pinned, "linux/amd64")
	if err != nil || got2 != stable {
		t.Fatalf("digest-pinned resolve after tag change = %q / %v, want %q", got2, err, stable)
	}
}

func TestMultiArchIndexDeterministic(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	amd64 := f.putManifest(t, "app", nil, manifestJSON("app", "amd64"), "application/vnd.docker.distribution.manifest.v2+json")
	arm64 := f.putManifest(t, "app", nil, manifestJSON("app", "arm64"), "application/vnd.docker.distribution.manifest.v2+json")
	f.putManifest(t, "app", []string{"latest"}, indexJSON(map[string]string{
		"linux/amd64":    amd64,
		"linux/arm64/v8": arm64,
	}), "application/vnd.docker.distribution.manifest.list.v2+json")

	ref, _ := ParseRef(f.host(t) + "/app:latest")
	r := testResolver(f)
	arm, err := r.Resolve(context.Background(), ref, "linux/arm64/v8")
	if err != nil || arm != arm64 {
		t.Fatalf("arm64 digest = %q / %v, want %q", arm, err, arm64)
	}
	arm2, _ := r.Resolve(context.Background(), ref, "linux/arm64/v8")
	if arm2 != arm {
		t.Error("platform resolution must be deterministic")
	}
	amd, err := r.Resolve(context.Background(), ref, "linux/amd64")
	if err != nil || amd != amd64 {
		t.Fatalf("amd64 digest = %q / %v, want %q", amd, err, amd64)
	}
	if _, err := r.Resolve(context.Background(), ref, "linux/riscv64"); !isNotFoundErr(err) {
		t.Errorf("missing platform err = %v, want not_found", err)
	}
}

func isNotFoundErr(err error) bool {
	var e *Error
	if !errorsAs(err, &e) {
		return false
	}
	return e.Code == CodeNotFound
}

func TestStatusCasesAreDistinct(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")
	f.putManifest(t, "app", []string{"ok"}, manifestJSON("app", "ok"), "application/vnd.docker.distribution.manifest.v2+json")
	f.manifests["app|bad"] = storedManifest{body: []byte("{not json"), contentType: "application/vnd.docker.distribution.manifest.v2+json"}
	f.redirects["via-redirect"] = "ok"
	f.forbidden["forbidden"] = true

	cases := []struct {
		tag        string
		wantCode   string
		retryable  bool
		wantSubstr string
	}{
		{tag: "missing", wantCode: CodeNotFound},
		{tag: "forbidden", wantCode: CodeForbidden},
		{tag: "bad", wantCode: CodeMalformed},
		{tag: "via-redirect", wantCode: "", wantSubstr: "sha256:"},
	}
	for _, tc := range cases {
		ref, _ := ParseRef(f.host(t) + "/app:" + tc.tag)
		got, err := testResolver(f).Resolve(context.Background(), ref, "linux/amd64")
		if tc.wantCode == "" {
			if err != nil || !strings.HasPrefix(got, "sha256:") {
				t.Errorf("%s: resolve = %q / %v, want a digest via redirect", tc.tag, got, err)
			}
			continue
		}
		var e *Error
		if !errorsAs(err, &e) || e.Code != tc.wantCode {
			t.Errorf("%s: err = %v, want code %q", tc.tag, err, tc.wantCode)
		}
		_ = got
	}
}

func TestRateLimitAndRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.rateLimit = 1
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")

	ref, _ := ParseRef(f.host(t) + "/app:1")
	_, err := testResolver(f).Resolve(context.Background(), ref, "linux/amd64")
	var e *Error
	if !errorsAs(err, &e) || e.Code != CodeRateLimited {
		t.Fatalf("err = %v, want rate_limited", err)
	}
	if !e.Retryable || e.RetryAfter != 3*time.Second {
		t.Errorf("retryable=%v retryAfter=%s, want true / 3s", e.Retryable, e.RetryAfter)
	}
	// The budget is consumed; the next resolve succeeds.
	got, err := testResolver(f).Resolve(context.Background(), ref, "linux/amd64")
	if err != nil || got == "" {
		t.Fatalf("resolve after rate limit = %q / %v", got, err)
	}
}

func TestTimeoutIsTyped(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.delay = 300 * time.Millisecond
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")

	host := strings.TrimPrefix(f.baseURL, "http://")
	r := testResolver(f)
	r.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	ref, _ := ParseRef(host + "/app:1")
	_, err := r.Resolve(context.Background(), ref, "linux/amd64")
	var e *Error
	if !errorsAs(err, &e) || e.Code != CodeTimeout || !e.Retryable {
		t.Fatalf("err = %v, want retryable timeout", err)
	}
}

func TestAuthBearerFlow(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.auth = true
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")

	ref, _ := ParseRef(f.host(t) + "/app:1")
	got, err := testResolver(f).Resolve(context.Background(), ref, "linux/amd64")
	if err != nil || got == "" {
		t.Fatalf("resolve with auth = %q / %v", got, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seenTokens) == 0 || f.seenTokens[len(f.seenTokens)-1] != "test-issued-token" {
		t.Errorf("registry never saw the issued bearer token: %v", f.seenTokens)
	}
}

func TestUnencryptedRealmRefused(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.auth = true
	f.realmOverride = "http://evil.example.com/token"
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")

	ref, _ := ParseRef(f.host(t) + "/app:1")
	_, err := testResolver(f).Resolve(context.Background(), ref, "linux/amd64")
	var e *Error
	if !errorsAs(err, &e) || e.Code != CodeAuth {
		t.Fatalf("err = %v, want auth error for an unencrypted realm", err)
	}
	if strings.Contains(err.Error(), "evil.example.com") == false && err.Error() == "" {
		t.Error("unreachable")
	}
}

func TestAuthRequiredWithoutCredentialsFailsTyped(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.auth = true
	f.requireTokenBasic = true
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")

	ref, _ := ParseRef(f.host(t) + "/app:1")
	_, err := testResolver(f).Resolve(context.Background(), ref, "linux/amd64")
	var e *Error
	if !errorsAs(err, &e) || e.Code != CodeAuth {
		t.Fatalf("err = %v, want auth error", err)
	}
}

// --- credentials ------------------------------------------------------------

func TestCredentialsFromDockerConfig(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.auth = true
	f.requireTokenBasic = true
	host := f.host(t)
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")

	cfgDir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("reguser:s3cretpass"))
	cfg := fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, host, auth)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	r := testResolver(f)
	r.DockerConfig = filepath.Join(cfgDir, "config.json")
	ref, _ := ParseRef(host + "/app:1")
	got, err := r.Resolve(context.Background(), ref, "linux/amd64")
	if err != nil || got == "" {
		t.Fatalf("resolve with config credentials = %q / %v", got, err)
	}
	f.mu.Lock()
	sent := strings.Join(f.seenBasic, " ")
	f.mu.Unlock()
	if !strings.Contains(sent, "Basic ") {
		t.Error("token endpoint never received basic credentials")
	}
}

func TestCredentialsFromHelper(t *testing.T) {
	f := newFakeRegistry(t) // no t.Parallel: mutates PATH
	f.auth = true
	f.requireTokenBasic = true
	host := f.host(t)
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")

	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		`if [ "$1" = get ]; then echo '{"Username":"helperuser","Secret":"helperpass"}'; fi` + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker-credential-teststore"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()
	cfg := fmt.Sprintf(`{"auths":{},"credHelpers":{"%s":"teststore"}}`, host)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+string(os.PathListSeparator)+oldPath)
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })

	r := testResolver(f)
	r.DockerConfig = filepath.Join(cfgDir, "config.json")
	ref, _ := ParseRef(host + "/app:1")
	got, err := r.Resolve(context.Background(), ref, "linux/amd64")
	if err != nil || got == "" {
		t.Fatalf("resolve with helper credentials = %q / %v", got, err)
	}
	f.mu.Lock()
	sent := strings.Join(f.seenBasic, " ")
	f.mu.Unlock()
	if !strings.Contains(sent, basicUserPass("helperuser", "helperpass")) {
		t.Errorf("helper credentials not used for the token request: %q", sent)
	}
}

// basicUserPass renders the expected Basic auth value.
func basicUserPass(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestCheckIdentifiesChangedImages(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	d1 := f.putManifest(t, "app", []string{"one"}, manifestJSON("app", "one"), "application/vnd.docker.distribution.manifest.v2+json")
	f.putManifest(t, "other", []string{"two"}, manifestJSON("other", "two"), "application/vnd.docker.distribution.manifest.v2+json")
	f.putManifest(t, "app", []string{"never-deployed"}, manifestJSON("app", "never-deployed"), "application/vnd.docker.distribution.manifest.v2+json")

	host := f.host(t)
	images := []config.ImageRef{
		{Ref: host + "/app:one"},
		{Ref: host + "/other:two"},
		{Ref: host + "/app:never-deployed"},
	}
	r := testResolver(f)
	// First image unchanged, second unchanged, third has no deployed entry.
	res, err := r.Check(context.Background(), images[:2], map[string]string{
		host + "/app:one":   d1,
		host + "/other:two": digestOf(f.manifests["other|two"].body),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Changed {
		t.Errorf("unchanged images reported changed: %+v", res)
	}

	// The registry serves a new manifest for app:one; other:two stays put.
	f.putManifest(t, "app", []string{"one"}, manifestJSON("app", "one-v2"), "application/vnd.docker.distribution.manifest.v2+json")
	res, err = r.Check(context.Background(), images, map[string]string{
		host + "/app:one":   d1,
		host + "/other:two": digestOf(f.manifests["other|two"].body),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Changed || len(res.ChangedImages) != 2 {
		t.Fatalf("changed = %v images = %v, want exactly the changed ones", res.Changed, res.ChangedImages)
	}
	found := strings.Join(res.ChangedImages, " ")
	if !strings.Contains(found, "/app:one") || !strings.Contains(found, "/app:never-deployed") {
		t.Errorf("changed images = %q, want app:one (digest changed) and app:never-deployed (no baseline)", found)
	}
	if strings.Contains(found, "/other:two") {
		t.Errorf("unchanged image wrongly reported: %q", found)
	}
}

func TestErrorsNeverCarryCredentials(t *testing.T) {
	f := newFakeRegistry(t)
	f.auth = true
	f.requireTokenBasic = true
	host := f.host(t)
	f.putManifest(t, "app", []string{"1"}, manifestJSON("app", "1"), "application/vnd.docker.distribution.manifest.v2+json")

	cfgDir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("reguser:s3cretpass"))
	cfg := fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, host, auth)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	r := testResolver(f)
	r.DockerConfig = filepath.Join(cfgDir, "config.json")
	ref, _ := ParseRef(host + "/app:missing")
	_, err := r.Resolve(context.Background(), ref, "linux/amd64")
	if err == nil {
		t.Fatal("expected the not-found error")
	}
	if strings.Contains(err.Error(), "s3cretpass") || strings.Contains(err.Error(), "reguser") {
		t.Errorf("credentials leaked into error: %v", err)
	}
}
