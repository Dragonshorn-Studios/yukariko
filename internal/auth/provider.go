package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// idp returns the lazily-built provider, ID-token verifier, and the
// discovered end-session endpoint. Construction happens on first use so an
// unreachable provider never blocks daemon startup; failures surface at
// login time with a clear page. Only successful construction is cached,
// but a hard failure is remembered for a short backoff so a down provider
// (each discovery pays up to the HTTP timeout under the mutex) cannot
// serialize every login attempt behind it.
//
// The provider is built from a background context: the JWKS key set it
// installs keeps refreshing for the process lifetime and must not inherit a
// request's cancellation.
func (s *Server) idp() (*oidc.Provider, *oidc.IDTokenVerifier, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil {
		return s.provider, s.verifier, s.endLogout, nil
	}
	if s.lastFailErr != nil && s.now().Sub(s.lastFail) < discoveryRetryBackoff {
		return nil, nil, "", s.lastFailErr
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, s.httpClient())
	prov, err := oidc.NewProvider(ctx, s.OIDC.Issuer)
	if err != nil {
		err = fmt.Errorf("discover issuer %s: %w", s.OIDC.Issuer, err)
		s.lastFail, s.lastFailErr = s.now(), err
		return nil, nil, "", err
	}
	var disc struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	_ = prov.Claims(&disc)
	s.provider = prov
	s.endLogout = disc.EndSessionEndpoint
	s.lastFailErr = nil
	s.verifier = prov.Verifier(&oidc.Config{
		ClientID: s.OIDC.ClientID,
		// Explicit allowlist: RS256 (the common default) and ES256. Never
		// derived from discovery metadata alone.
		SupportedSigningAlgs: []string{"RS256", "ES256"},
		Now:                  s.now,
	})
	return s.provider, s.verifier, s.endLogout, nil
}

// discoveryRetryBackoff bounds how often a failing provider discovery is
// retried.
const discoveryRetryBackoff = 30 * time.Second

// oauthConfig builds the exchange configuration. The client secret is only
// set on the exchange path (AuthCodeURL never needs it); callers pass "".
func (s *Server) oauthConfig(prov *oidc.Provider, secret string) oauth2.Config {
	return oauth2.Config{
		ClientID:     s.OIDC.ClientID,
		ClientSecret: secret,
		Endpoint:     prov.Endpoint(),
		RedirectURL:  s.OIDC.RedirectURI(),
		Scopes:       s.OIDC.Scopes,
	}
}
