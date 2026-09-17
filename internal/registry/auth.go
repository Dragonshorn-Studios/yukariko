package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// credentials are used in-memory only: never stored, logged, or embedded in
// errors.
type credentials struct {
	Username string
	Password string
}

func (c credentials) empty() bool { return c.Username == "" && c.Password == "" }

// dockerConfig mirrors the parts of ~/.docker/config.json Yukariko reads.
type dockerConfig struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

func (r *Resolver) dockerConfigPath() string {
	if r.DockerConfig != "" {
		return r.DockerConfig
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

// credentialsFor resolves the credentials Docker itself would use for a
// registry: a per-registry helper, then the config file's auth entries,
// then the default credential store. Missing configuration yields anonymous
// access, not an error. A failing helper is an error — guessing would be
// worse.
func (r *Resolver) credentialsFor(ctx context.Context, registry string) (credentials, error) {
	path := r.dockerConfigPath()
	if path == "" {
		return credentials{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentials{}, nil
		}
		return credentials{}, fmt.Errorf("read docker config: %w", err)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return credentials{}, fmt.Errorf("parse docker config: %w", err)
	}

	// 1. Per-registry helper.
	if name := lookupHelper(cfg.CredHelpers, registry); name != "" {
		return r.helperCredentials(ctx, name, registry)
	}

	// 2. Config file auth entries (base64 user:pass, or legacy plain).
	key := dockerConfigKey(cfg, registry)
	if key != "" {
		entry := cfg.Auths[key]
		if entry.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return credentials{}, fmt.Errorf("decode docker config auth entry: %w", err)
			}
			if u, p, ok := strings.Cut(string(raw), ":"); ok {
				return credentials{Username: u, Password: p}, nil
			}
			return credentials{}, errors.New("docker config auth entry is not user:password")
		}
		if entry.Username != "" || entry.Password != "" {
			return credentials{Username: entry.Username, Password: entry.Password}, nil
		}
	}

	// 3. Default credential store.
	if cfg.CredsStore != "" {
		return r.helperCredentials(ctx, cfg.CredsStore, registry)
	}
	return credentials{}, nil
}

// dockerConfigKey finds the config key Docker would match for a registry.
// Keys are hosts with optional scheme prefixes; Docker Hub's legacy key is
// the index URL.
func dockerConfigKey(cfg dockerConfig, registry string) string {
	normalized := strings.ToLower(registry)
	if normalized == "docker.io" {
		legacy := "https://index.docker.io/v1/"
		if _, ok := cfg.Auths[legacy]; ok {
			return legacy
		}
	}
	candidates := []string{
		registry,
		strings.ToLower(registry),
		"https://" + registry,
		"https://" + normalized,
	}
	for _, c := range candidates {
		if _, ok := cfg.Auths[c]; ok {
			return c
		}
	}
	return ""
}

func lookupHelper(helpers map[string]string, registry string) string {
	if v, ok := helpers[registry]; ok {
		return v
	}
	if v, ok := helpers[strings.ToLower(registry)]; ok {
		return v
	}
	if v, ok := helpers["https://"+registry]; ok {
		return v
	}
	return ""
}

// helperCredentials asks docker-credential-<name> for the registry's
// credentials. The helper protocol reads a JSON server description from
// stdin and prints {"Username":…,"Secret":…}. The exchange runs through the
// runner and stays in memory.
func (r *Resolver) helperCredentials(ctx context.Context, helper, registry string) (credentials, error) {
	req := strings.Join([]string{"docker-credential-" + helper, "get"}, " ")
	rn := r.Runner
	if rn == nil {
		rn = &runner.Runner{}
	}
	res, err := rn.Run(ctx, runner.Request{
		Name:    req,
		Argv:    []string{"docker-credential-" + helper, "get"},
		Timeout: 30 * time.Second,
		Stdin:   []byte(`{"ServerURL":"` + registry + `"}`),
	})
	if err != nil {
		return credentials{}, err
	}
	if res.Status != runner.StatusSuccess {
		detail := strings.TrimSpace(string(res.Stderr))
		if detail == "" {
			detail = res.Err
		}
		return credentials{}, fmt.Errorf("credential helper %s failed: %s", helper, detail)
	}
	out := strings.TrimSpace(string(res.Stdout))
	if out == "" || out == "{}" || strings.Contains(out, `"Username": ""`) {
		return credentials{}, nil // helper has no entry for this registry
	}
	var got struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		return credentials{}, fmt.Errorf("parse credential helper output: %w", err)
	}
	return credentials{Username: got.Username, Password: got.Secret}, nil
}

// --- registry token flow ----------------------------------------------------

// authChallenge is a parsed WWW-Authenticate: Bearer header.
type authChallenge struct {
	Realm   string
	Service string
	Scope   string
	Basic   bool // Basic realm — the registry wants plain HTTP basic auth
}

// parseChallenge handles the subset of RFC 6750/7235 challenges registries
// actually issue.
func parseChallenge(header string) (authChallenge, error) {
	if header == "" {
		return authChallenge{}, errors.New("missing WWW-Authenticate header")
	}
	lower := strings.ToLower(header)
	if strings.HasPrefix(lower, "basic") {
		return authChallenge{Basic: true}, nil
	}
	if !strings.HasPrefix(lower, "bearer") {
		return authChallenge{}, fmt.Errorf("unsupported auth scheme in %q", header)
	}
	var ch authChallenge
	for _, part := range splitQuoted(header[len("Bearer"):], ',') {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "realm":
			ch.Realm = v
		case "service":
			ch.Service = v
		case "scope":
			ch.Scope = v
		}
	}
	if ch.Realm == "" {
		return authChallenge{}, fmt.Errorf("bearer challenge without realm in %q", header)
	}
	return ch, nil
}

// validateRealm refuses token realms that could leak credentials.
func validateRealm(realm string) error {
	u, err := url.Parse(realm)
	if err != nil || u.Host == "" {
		return &Error{Code: CodeMalformed, Detail: "token realm is not a valid URL", cause: ErrMalformed}
	}
	host := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if host == "localhost" || strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "[::1]") {
			return nil
		}
	}
	return &Error{Code: CodeAuth, Detail: "token realm must be https (or loopback http); refusing to send credentials", cause: ErrAuth}
}

// splitQuoted splits on sep, honoring double-quoted values.
func splitQuoted(s string, sep byte) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case ch == sep && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// token obtains a bearer token from the challenge's realm, optionally
// authenticating with the registry credentials. The realm must be https
// (or plain http on a loopback host): credentials are never sent to an
// unencrypted endpoint an attacker could inject via the challenge header.
func (r *Resolver) token(ctx context.Context, ch authChallenge, creds credentials) (string, error) {
	if err := validateRealm(ch.Realm); err != nil {
		return "", err
	}
	u := ch.Realm + "?"
	if ch.Service != "" {
		u += "service=" + ch.Service + "&"
	}
	if ch.Scope != "" {
		u += "scope=" + ch.Scope
	}
	req, err := newRequest(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	if !creds.empty() {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	client := r.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", &Error{Code: CodeUnavailable, Detail: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", &Error{Code: CodeAuth, Detail: fmt.Sprintf("token endpoint returned %d", resp.StatusCode)}
	}
	var got struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return "", &Error{Code: CodeMalformed, Detail: "token response is not valid JSON: " + err.Error()}
	}
	if got.Token != "" {
		return got.Token, nil
	}
	if got.AccessToken != "" {
		return got.AccessToken, nil
	}
	return "", &Error{Code: CodeMalformed, Detail: "token response carries no token"}
}
