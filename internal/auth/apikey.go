package auth

import (
	"net/http"
	"strings"
)

// APIKeyPrefix marks Yukariko-generated bearer tokens. The prefix is an
// operator ergonomics and secret-scanning affordance; validation is by
// SHA-256 lookup, never by shape.
const APIKeyPrefix = "ykr_"

// GenerateAPIKey returns a fresh bearer token and the only form that is
// ever stored: its SHA-256. The token is shown exactly once at creation.
func GenerateAPIKey() (token, hash string) {
	token = APIKeyPrefix + randomToken()
	return token, tokenHash(token)
}

// apiKeyIdentity validates a presented Bearer API key. presented reports
// whether the request carried an Authorization: Bearer credential at all;
// valid is meaningful only then. A store error is returned, never folded
// into "unauthenticated" — store doctrine: a database error is not an auth
// verdict.
func (s *Server) apiKeyIdentity(req *http.Request) (id Identity, valid, presented bool, err error) {
	scheme, cred, ok := strings.Cut(req.Header.Get("Authorization"), " ")
	cred = strings.TrimSpace(cred)
	if !ok || !strings.EqualFold(scheme, "Bearer") || cred == "" {
		return Identity{}, false, false, nil
	}
	presented = true
	if !strings.HasPrefix(cred, APIKeyPrefix) {
		return Identity{}, false, true, nil
	}
	key, found, err := s.Store.APIKeyByHash(req.Context(), tokenHash(cred))
	if err != nil {
		return Identity{}, false, true, err
	}
	if !found || !key.Active(s.now()) {
		return Identity{}, false, true, nil
	}
	if tErr := s.Store.TouchAPIKey(req.Context(), key.ID, s.now()); tErr != nil {
		s.log().Warn("touch api key", "error", tErr)
	}
	return Identity{Subject: "apikey:" + key.Name}, true, true, nil
}
