package runner

import (
	"regexp"
	"strings"
)

// replacement is the marker substituted for every redacted region. It
// reveals nothing about the length or shape of the original value.
const replacement = "[REDACTED]"

// Redactor removes configured secret values and common credential-bearing
// patterns from anything Yukariko logs, stores, or returns in errors.
// Values are matched literally; the redactor never logs or renders the
// values themselves.
type Redactor struct {
	secrets  []string
	urlCred  *regexp.Regexp
	header   *regexp.Regexp
	query    *regexp.Regexp
	apiToken *regexp.Regexp
}

// credentialPatterns covers the common ways commands echo credentials:
// URLs with userinfo, authorization headers, token-style query or body
// parameters, and bare Yukariko API-key tokens (#64) wherever they appear.
// The api-token pattern has no trailing \b on purpose: base64url tokens
// can end in '-', which is not a word character, and a backtracking match
// would leave the last few characters visible.
var (
	urlCredPattern  = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*)://([^:/@\s]+):([^@\s/]+)@`)
	headerPattern   = regexp.MustCompile(`(?i)((?:proxy-)?authorization|x-api-key|x-auth-token)\s*:[^\r\n]*`)
	queryPattern    = regexp.MustCompile(`(?i)((?:api_?key|access_?token|auth_?token|token|secret|password|passwd|pwd)[_=])([^&\s"']+)`)
	apiTokenPattern = regexp.MustCompile(`\bykr_[a-zA-Z0-9_-]{20,}`)
)

// NewRedactor builds a redactor for the given literal secret values. Values
// that are empty or shorter than four characters are ignored: redacting
// short strings corrupts output without ever providing protection.
func NewRedactor(secrets []string) *Redactor {
	kept := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if len(s) >= 4 {
			kept = append(kept, s)
		}
	}
	return &Redactor{
		secrets:  kept,
		urlCred:  urlCredPattern,
		header:   headerPattern,
		query:    queryPattern,
		apiToken: apiTokenPattern,
	}
}

// String redacts a value intended for logs, errors, or display.
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, replacement)
	}
	s = r.urlCred.ReplaceAllString(s, "$1://$2:"+replacement+"@")
	s = r.header.ReplaceAllString(s, "$1: "+replacement)
	s = r.query.ReplaceAllString(s, "$1"+replacement)
	s = r.apiToken.ReplaceAllString(s, replacement)
	return s
}

// Bytes redacts captured command output.
func (r *Redactor) Bytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return []byte(r.String(string(b)))
}
