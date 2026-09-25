package config

import "strings"

// Auth configures optional authentication for the read-only HTTP surface
// (dashboard and API, issue #61). Sessions are server-side rows in the
// store; the OIDC protocol layer is coreos/go-oidc + x/oauth2 — hand-rolled
// verification is a non-goal. When the section is absent or disabled, the
// HTTP surface behaves exactly as before (loopback-bind recommended).
type Auth struct {
	OIDC OIDCAuth `yaml:"oidc,omitempty"`
}

// OIDCAuth is the OpenID Connect relying-party configuration. Any compliant
// provider works; Authentik is the documented instance. The client secret is
// a reference only, resolved at token-exchange time and never persisted.
type OIDCAuth struct {
	Enabled         bool       `yaml:"enabled,omitempty"`
	Issuer          string     `yaml:"issuer,omitempty"`
	ClientID        string     `yaml:"client_id,omitempty"`
	ClientSecretRef *SecretRef `yaml:"client_secret_ref,omitempty"`
	// RedirectBase is the external origin browsers use to reach Yukariko
	// (for example https://yukariko.example.com). The authorization-code
	// redirect URI is RedirectBase + /auth/callback.
	RedirectBase  string   `yaml:"redirect_base,omitempty"`
	Scopes        []string `yaml:"scopes,omitempty"`
	AllowedGroups []string `yaml:"allowed_groups,omitempty"`
	SessionTTL    Duration `yaml:"session_ttl,omitempty"`
}

// ApplyDefaults fills unset optional auth fields.
func (a *Auth) ApplyDefaults() {
	o := &a.OIDC
	if len(o.Scopes) == 0 {
		o.Scopes = DefaultOIDCScopes
	}
	if o.SessionTTL == 0 {
		o.SessionTTL = DefaultOIDCSessionTTL
	}
}

// RedirectURI returns the exact authorization-code redirect URI registered
// with the provider.
func (o *OIDCAuth) RedirectURI() string {
	return strings.TrimRight(o.RedirectBase, "/") + "/auth/callback"
}
