// Package registry resolves remote image digests over the OCI/Distribution
// v2 HTTP protocol so the scheduler can detect changed images without
// pulling anything.
//
// Lookup results are informational: a successful resolution or a changed
// digest is never a deployed version — only the deploy pipeline's success
// checkpoint (#11) records one. Credentials come from Docker's own
// configuration and credential helpers, are used in-memory only, and are
// never logged, stored, or embedded in errors.
package registry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Ref is a parsed, normalized image reference.
type Ref struct {
	Registry   string // as configured, e.g. "docker.io", "ghcr.io", "127.0.0.1:5000"
	Repository string // "library/nginx", "team/app"
	Tag        string // "latest" when omitted (unless digest-pinned)
	Digest     string // "sha256:…" when the reference is digest-pinned
}

var (
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$|^sha512:[a-f0-9]{128}$`)
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	repoPattern   = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
)

// ParseRef parses an image reference with Docker's normalization rules:
// a missing registry means Docker Hub, a single-segment Docker Hub
// repository gains the "library/" prefix, and a missing tag means "latest".
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, errors.New("empty image reference")
	}
	ref := Ref{}
	if i := strings.Index(s, "@"); i >= 0 {
		ref.Digest = s[i+1:]
		s = s[:i]
		if !digestPattern.MatchString(ref.Digest) {
			return Ref{}, fmt.Errorf("unsupported digest %q", ref.Digest)
		}
	}

	registry := ""
	rest := s
	if i := strings.Index(s, "/"); i >= 0 {
		first := s[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry, rest = first, s[i+1:]
		}
	}
	if registry == "" {
		registry = "docker.io"
	}

	repo := rest
	tag := ""
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		repo, tag = rest[:i], rest[i+1:]
	}
	if repo == "" {
		return Ref{}, fmt.Errorf("image reference %q has no repository", s)
	}
	if registry == "docker.io" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if !repoPattern.MatchString(repo) {
		return Ref{}, fmt.Errorf("invalid repository %q", repo)
	}
	if tag == "" {
		if ref.Digest == "" {
			tag = "latest"
		}
	} else if !tagPattern.MatchString(tag) {
		return Ref{}, fmt.Errorf("invalid tag %q", tag)
	}

	ref.Registry, ref.Repository, ref.Tag = registry, repo, tag
	return ref, nil
}

// String renders the canonical reference form.
func (r Ref) String() string {
	s := r.Registry + "/" + r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// IsDigestPinned reports whether the reference is immutable by digest.
func (r Ref) IsDigestPinned() bool { return r.Digest != "" }

// reference returns the manifest reference this resolution fetches: the
// digest when pinned, otherwise the tag.
func (r Ref) reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// apiEndpoint maps the registry to its Distribution API base URL. Docker
// Hub aliases collapse onto registry-1.docker.io; loopback registries use
// plain http (the documented local-development convention). Other
// insecure-plain registries are unsupported in this build.
func (r Ref) apiEndpoint(overrides map[string]string) string {
	if u, ok := overrides[strings.ToLower(r.Registry)]; ok {
		return strings.TrimRight(u, "/")
	}
	reg := strings.ToLower(r.Registry)
	switch reg {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		reg = "registry-1.docker.io"
	}
	scheme := "https"
	if reg == "localhost" || strings.HasPrefix(reg, "127.0.0.1") || strings.HasPrefix(reg, "[::1]") {
		scheme = "http"
	}
	return scheme + "://" + reg
}
