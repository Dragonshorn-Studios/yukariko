package registry

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
)

// Error codes, distinct per the acceptance criteria. Retryable feeds the
// scheduler's backoff: rate limits, timeouts, and unavailability are
// transient; auth/forbidden/not-found are configuration problems.
const (
	CodeAuth             = "auth"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeRateLimited      = "rate_limited"
	CodeTimeout          = "timeout"
	CodeMalformed        = "malformed"
	CodeUnavailable      = "unavailable"
	CodeUnexpectedStatus = "unexpected_status"
)

// Error is a classified, sanitized registry failure.
type Error struct {
	Code       string
	Ref        string
	StatusCode int
	Detail     string
	Retryable  bool
	// RetryAfter surfaces a registry-provided Retry-After hint (429).
	RetryAfter time.Duration
	cause      error
}

func (e *Error) Error() string {
	s := "registry: " + e.Code
	if e.Ref != "" {
		s += " [" + e.Ref + "]"
	}
	if e.StatusCode != 0 {
		s += fmt.Sprintf(" (status %d)", e.StatusCode)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

func (e *Error) Unwrap() error { return e.cause }

// Sentinel errors for errors.Is.
var (
	ErrAuth        = errors.New("registry authentication failed")
	ErrForbidden   = errors.New("registry access forbidden")
	ErrNotFound    = errors.New("image or manifest not found")
	ErrRateLimited = errors.New("registry rate limit reached")
	ErrTimeout     = errors.New("registry request timed out")
	ErrMalformed   = errors.New("registry returned a malformed response")
	ErrUnavailable = errors.New("registry unavailable")
)

// defaultRequestTimeout bounds every HTTP request.
const defaultRequestTimeout = 30 * time.Second

// acceptManifestTypes are the manifest media types this client accepts,
// index/list types first so multi-arch-aware registries serve the index.
var acceptManifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.v1+json",
}, ", ")

// Resolver resolves remote image digests. It never pulls images and never
// treats a lookup as a deployment.
type Resolver struct {
	// HTTPClient is the HTTP client; nil uses one with a 30s timeout.
	HTTPClient *http.Client
	// DockerConfig is the path to Docker's config.json; empty uses the
	// user's home directory.
	DockerConfig string
	// EndpointOverride maps a registry host to an API base URL (tests,
	// mirrors).
	EndpointOverride map[string]string
	// Runner executes docker-credential-* helpers.
	Runner *runner.Runner
	// Platform is the default "os/arch[/variant]" for multi-arch index
	// selection; empty means the running platform.
	Platform string
}

// ImageStatus is one image's change verdict within an app.
type ImageStatus struct {
	Ref          string `json:"ref"`
	RemoteDigest string `json:"remote_digest"`
	Changed      bool   `json:"changed"`
	Detail       string `json:"detail,omitempty"`
}

// CheckResult reports per-image change for one app. It identifies WHICH
// images changed; a successful resolution is never a deployed version.
type CheckResult struct {
	Images        []ImageStatus
	ChangedImages []string
	Changed       bool
}

// Check resolves every image and compares each remote digest with the last
// successfully deployed digest (canonical ref → digest). An image without a
// deployed entry is reported changed ("no deployed digest recorded") so the
// first deploy establishes the baseline. Any per-image lookup failure fails
// the whole check with the image identified — the scheduler backs off.
func (r *Resolver) Check(ctx context.Context, images []config.ImageRef, deployed map[string]string) (CheckResult, error) {
	var out CheckResult
	for _, img := range images {
		ref, err := ParseRef(img.Ref)
		if err != nil {
			return CheckResult{}, &Error{Code: CodeMalformed, Detail: err.Error(), cause: ErrMalformed}
		}
		platform := img.Platform
		if platform == "" {
			platform = r.effectivePlatform()
		}
		digest, err := r.Resolve(ctx, ref, platform)
		if err != nil {
			return CheckResult{}, fmt.Errorf("image %s: %w", ref.String(), err)
		}
		status := ImageStatus{Ref: ref.String(), RemoteDigest: digest}
		last, ok := deployed[ref.String()]
		switch {
		case !ok:
			status.Changed = true
			status.Detail = "no deployed digest recorded"
		case last != digest:
			status.Changed = true
			status.Detail = "remote digest changed"
		default:
			status.Detail = "unchanged"
		}
		if status.Changed {
			out.ChangedImages = append(out.ChangedImages, ref.String())
		}
		out.Images = append(out.Images, status)
	}
	out.Changed = len(out.ChangedImages) > 0
	return out, nil
}

// Resolve returns the remote digest for one reference: the manifest digest
// for single-platform images, or the digest of the child manifest matching
// the platform when the reference is a multi-architecture index. Digest-
// pinned references resolve to their own (immutable) digest.
func (r *Resolver) Resolve(ctx context.Context, ref Ref, platform string) (string, error) {
	if platform == "" {
		platform = r.effectivePlatform()
	}
	token, basic, err := r.authenticate(ctx, ref)
	if err != nil {
		return "", err
	}
	digest, mediaType, body, err := r.fetchManifest(ctx, ref, ref.reference(), token, basic)
	if err != nil {
		return "", err
	}
	if isIndex(mediaType) {
		child, err := selectPlatform(body, platform, ref)
		if err != nil {
			return "", err
		}
		childDigest, _, _, err := r.fetchManifest(ctx, ref, child, token, basic)
		if err != nil {
			return "", err
		}
		return childDigest, nil
	}
	if err := validateManifest(body, ref); err != nil {
		return "", err
	}
	return digest, nil
}

// validateManifest rejects bodies that are not plausibly an image manifest.
func validateManifest(body []byte, ref Ref) error {
	var m struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &m); err != nil || m.SchemaVersion == 0 {
		return &Error{Code: CodeMalformed, Ref: ref.String(),
			Detail: "manifest is not a valid image manifest", cause: ErrMalformed}
	}
	return nil
}

// --- HTTP plumbing ----------------------------------------------------------

func (r *Resolver) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: defaultRequestTimeout}
}

func (r *Resolver) effectivePlatform() string {
	if r.Platform != "" {
		return r.Platform
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}

// authenticate performs the Distribution token dance: ping /v2/, parse the
// challenge, and fetch a bearer token when required. Basic-challenge
// registries get credentials used directly.
func (r *Resolver) authenticate(ctx context.Context, ref Ref) (token string, basic bool, err error) {
	creds, err := r.credentialsFor(ctx, ref.Registry)
	if err != nil {
		return "", false, &Error{Code: CodeAuth, Ref: ref.String(), Detail: err.Error(), cause: ErrAuth}
	}
	base := ref.apiEndpoint(r.EndpointOverride)
	req, err := newRequest(ctx, "GET", base+"/v2/", nil)
	if err != nil {
		return "", false, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", false, classifyTransport(ref.String(), err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		return "", false, nil
	}
	if resp.StatusCode != 401 {
		return "", false, statusError(ref.String(), resp)
	}
	ch, err := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	if err != nil {
		return "", false, &Error{Code: CodeMalformed, Ref: ref.String(), Detail: err.Error(), cause: ErrMalformed}
	}
	if ch.Basic {
		return "", !creds.empty(), nil
	}
	tok, err := r.token(ctx, ch, creds)
	if err != nil {
		return "", false, err
	}
	return tok, false, nil
}

// fetchManifest GETs one manifest and returns its verified digest.
func (r *Resolver) fetchManifest(ctx context.Context, ref Ref, reference, token string, basic bool) (digest, mediaType string, body []byte, err error) {
	base := ref.apiEndpoint(r.EndpointOverride)
	req, err := newRequest(ctx, "GET", base+"/v2/"+ref.Repository+"/manifests/"+reference, strings.NewReader(""))
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("Accept", acceptManifestTypes)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if basic {
		creds, cErr := r.credentialsFor(ctx, ref.Registry)
		if cErr != nil {
			return "", "", nil, &Error{Code: CodeAuth, Ref: ref.String(), Detail: cErr.Error(), cause: ErrAuth}
		}
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", "", nil, classifyTransport(ref.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", nil, statusError(ref.String(), resp)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", "", nil, &Error{Code: CodeUnavailable, Ref: ref.String(), Detail: err.Error(), Retryable: true}
	}
	digest = "sha256:" + hexSum(body)
	if header := resp.Header.Get("Docker-Content-Digest"); header != "" && header != digest {
		// The registry claims one digest but served other bytes: the
		// response cannot be trusted.
		return "", "", nil, &Error{Code: CodeMalformed, Ref: ref.String(),
			Detail: fmt.Sprintf("content digest mismatch: header %s, body %s", header, digest),
			cause:  ErrMalformed}
	}
	mediaType = resp.Header.Get("Content-Type")
	return digest, mediaType, body, nil
}

// selectPlatform picks the index child matching the platform.
func selectPlatform(body []byte, platform string, ref Ref) (string, error) {
	var index struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform *struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return "", &Error{Code: CodeMalformed, Ref: ref.String(), Detail: "index is not valid JSON: " + err.Error(), cause: ErrMalformed}
	}
	osName, arch, variant := splitPlatform(platform)
	for _, m := range index.Manifests {
		if m.Digest == "" || m.Platform == nil {
			continue // attestation/unknown entries
		}
		if m.Platform.OS == osName && m.Platform.Architecture == arch && m.Platform.Variant == variant {
			return m.Digest, nil
		}
	}
	return "", &Error{Code: CodeNotFound, Ref: ref.String(),
		Detail: fmt.Sprintf("index has no manifest for platform %q", platform), cause: ErrNotFound}
}

func splitPlatform(p string) (osName, arch, variant string) {
	parts := strings.Split(p, "/")
	switch len(parts) {
	case 1:
		return runtime.GOOS, parts[0], ""
	case 2:
		return parts[0], parts[1], ""
	default:
		return parts[0], parts[1], strings.Join(parts[2:], "/")
	}
}

func isIndex(mediaType string) bool {
	return mediaType == "application/vnd.docker.distribution.manifest.list.v2+json" ||
		mediaType == "application/vnd.oci.image.index.v1+json"
}

func statusError(ref string, resp *http.Response) error {
	e := &Error{Ref: ref, StatusCode: resp.StatusCode}
	switch {
	case resp.StatusCode == 401:
		e.Code, e.cause, e.Detail = CodeAuth, ErrAuth, "authentication required and token flow exhausted"
	case resp.StatusCode == 403:
		e.Code, e.cause, e.Detail = CodeForbidden, ErrForbidden, "access denied"
	case resp.StatusCode == 404:
		e.Code, e.cause, e.Detail = CodeNotFound, ErrNotFound, "repository or manifest not found"
	case resp.StatusCode == 429:
		e.Code, e.cause, e.Detail, e.Retryable = CodeRateLimited, ErrRateLimited, "rate limited by the registry", true
		if ra, err := time.ParseDuration(strings.TrimSpace(resp.Header.Get("Retry-After")) + "s"); err == nil {
			e.RetryAfter = ra
			e.Detail = fmt.Sprintf("rate limited; Retry-After %s", e.RetryAfter)
		}
	case resp.StatusCode >= 500:
		e.Code, e.cause, e.Detail, e.Retryable = CodeUnavailable, ErrUnavailable, "registry server error", true
	default:
		e.Code, e.Detail = CodeUnexpectedStatus, fmt.Sprintf("unexpected response status")
	}
	return e
}

func classifyTransport(ref string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Ref: ref, Detail: err.Error(), Retryable: true, cause: ErrTimeout}
	}
	var netErr netError
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Code: CodeTimeout, Ref: ref, Detail: err.Error(), Retryable: true, cause: ErrTimeout}
	}
	return &Error{Code: CodeUnavailable, Ref: ref, Detail: err.Error(), Retryable: true, cause: ErrUnavailable}
}

type netError interface{ Timeout() bool }

func newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return http.NewRequestWithContext(ctx, method, url, body)
}

func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}
