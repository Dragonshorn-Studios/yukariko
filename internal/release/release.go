// Package release downloads one GitHub release asset, verifies it against
// the release's SHA256SUMS, and extracts a single archive member.
//
// The token, when configured, is a SecretRef resolved at call time. It is
// sent only to the GitHub API host and is never logged, stored, or written
// into an error. Downloaded bytes are installed by the caller; this package
// does not execute them.
package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

const (
	defaultAPI = "https://api.github.com"
	maxBytes   = 256 << 20
	userAgent  = "yukariko"
)

// tagPattern matches the release tags Marller and Yukariko publish.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Client talks to the GitHub releases API.
type Client struct {
	// HTTP overrides the client. Nil uses a client that refuses redirects
	// off the allowed hosts and strips Authorization when leaving the API.
	HTTP *http.Client
	// APIBase overrides https://api.github.com. Tests point it at httptest.
	APIBase string
	// OS and Arch override runtime.GOOS / runtime.GOARCH when expanding
	// {os} and {arch} in the asset template.
	OS   string
	Arch string
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Latest reports the newest release tag for the configured repository.
func (c *Client) Latest(ctx context.Context, src *config.ReleaseSource) (string, error) {
	rel, err := c.lookup(ctx, src)
	if err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// Install downloads the asset for the newest release, verifies SHA256SUMS,
// extracts ArchiveBinary, and atomically replaces dir/bin. It returns the
// tag that was installed.
func (c *Client) Install(ctx context.Context, src *config.ReleaseSource, dir, bin string) (string, error) {
	if src == nil {
		return "", errors.New("release: source is required")
	}
	dest, err := Dest(dir, bin)
	if err != nil {
		return "", err
	}
	rel, err := c.lookup(ctx, src)
	if err != nil {
		return "", err
	}
	assetName, err := expandAsset(src.Asset, rel.TagName, c.goos(), c.goarch())
	if err != nil {
		return "", err
	}
	asset, sums, err := findAssets(rel.Assets, assetName)
	if err != nil {
		return "", err
	}
	token, err := tokenOf(src)
	if err != nil {
		return "", err
	}
	archive, err := c.download(ctx, asset.URL, token)
	if err != nil {
		return "", err
	}
	sumBytes, err := c.download(ctx, sums.URL, token)
	if err != nil {
		return "", err
	}
	want, err := checksumFor(string(sumBytes), assetName)
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("release: checksum mismatch for %s", assetName)
	}
	member, err := extractMember(assetName, archive, src.ArchiveBinary)
	if err != nil {
		return "", err
	}
	if err := place(dest, member); err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// Dest joins a relative binary path onto an app home and rejects paths that
// leave the home.
func Dest(dir, rel string) (string, error) {
	if dir == "" {
		return "", errors.New("release: app directory is required")
	}
	rel = path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
		return "", fmt.Errorf("release: binary path %q must stay inside the app directory", rel)
	}
	dest := filepath.Join(dir, filepath.FromSlash(rel))
	parent := filepath.Clean(dir)
	out, err := filepath.Rel(parent, dest)
	if err != nil || out == ".." || strings.HasPrefix(out, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("release: binary path %q escapes the app directory", rel)
	}
	return dest, nil
}

func (c *Client) lookup(ctx context.Context, src *config.ReleaseSource) (ghRelease, error) {
	if src == nil || src.Repo == "" {
		return ghRelease{}, errors.New("release: repository is required")
	}
	token, err := tokenOf(src)
	if err != nil {
		return ghRelease{}, err
	}
	endpoint := c.apiBase() + "/repos/" + src.Repo + "/releases/latest"
	body, err := c.get(ctx, endpoint, "application/vnd.github+json", token)
	if err != nil {
		return ghRelease{}, err
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return ghRelease{}, fmt.Errorf("release: parse latest release: %w", err)
	}
	if !tagPattern.MatchString(rel.TagName) {
		return ghRelease{}, fmt.Errorf("release: refusing tag %q", rel.TagName)
	}
	return rel, nil
}

func (c *Client) download(ctx context.Context, rawURL, token string) ([]byte, error) {
	return c.get(ctx, rawURL, "application/octet-stream", token)
}

func (c *Client) get(ctx context.Context, rawURL, accept, token string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("release: refusing URL %q", rawURL)
	}
	if err := c.allowURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("release: request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: download: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("release: read response: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("release: response exceeds 256MiB")
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release: %s returned status %d", u.Host, res.StatusCode)
	}
	return body, nil
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: c.checkRedirect}
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("release: too many redirects")
	}
	if err := c.allowURL(req.URL); err != nil {
		return err
	}
	base, _ := url.Parse(c.apiBase())
	if req.URL.Host != base.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

func (c *Client) allowURL(u *url.URL) error {
	if u == nil || u.Host == "" {
		return errors.New("release: refusing an empty URL")
	}
	if u.User != nil {
		return errors.New("release: refusing a URL with userinfo")
	}
	if !c.allowHost(u.Host) {
		return fmt.Errorf("release: refusing host %s", u.Host)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		base, err := url.Parse(c.apiBase())
		if err != nil {
			return fmt.Errorf("release: api base: %w", err)
		}
		if u.Host == base.Host || isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("release: refusing plain http for %s", u.Host)
	default:
		return fmt.Errorf("release: refusing scheme %s", u.Scheme)
	}
}

func (c *Client) allowHost(host string) bool {
	base, err := url.Parse(c.apiBase())
	if err != nil {
		return false
	}
	if host == base.Host {
		return true
	}
	if base.Host == "api.github.com" {
		switch host {
		case "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com", "codeload.github.com":
			return true
		}
	}
	return false
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return defaultAPI
}

func (c *Client) goos() string {
	if c.OS != "" {
		return c.OS
	}
	return runtime.GOOS
}

func (c *Client) goarch() string {
	if c.Arch != "" {
		return c.Arch
	}
	return runtime.GOARCH
}

func tokenOf(src *config.ReleaseSource) (string, error) {
	if src == nil || src.TokenRef == nil {
		return "", nil
	}
	value, err := src.TokenRef.Resolve()
	if err != nil {
		return "", fmt.Errorf("release: token: %w", err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("release: token reference resolved empty")
	}
	return value, nil
}

func findAssets(assets []ghAsset, name string) (asset, sums ghAsset, err error) {
	var found, foundSums bool
	for _, a := range assets {
		switch a.Name {
		case name:
			asset = a
			found = true
		case "SHA256SUMS":
			sums = a
			foundSums = true
		}
	}
	if !found {
		return ghAsset{}, ghAsset{}, fmt.Errorf("release: release has no asset %s", name)
	}
	if !foundSums {
		return ghAsset{}, ghAsset{}, errors.New("release: release has no SHA256SUMS asset; refusing to install unverified")
	}
	if asset.URL == "" || sums.URL == "" {
		return ghAsset{}, ghAsset{}, errors.New("release: asset is missing a download URL")
	}
	return asset, sums, nil
}

func expandAsset(tmpl, tag, goos, goarch string) (string, error) {
	if !tagPattern.MatchString(tag) {
		return "", fmt.Errorf("release: refusing tag %q", tag)
	}
	if strings.Contains(tmpl, "/") || strings.Contains(tmpl, `\`) || strings.Contains(tmpl, "..") {
		return "", fmt.Errorf("release: asset template %q must be a filename", tmpl)
	}
	out := strings.NewReplacer("{tag}", tag, "{os}", goos, "{arch}", goarch).Replace(tmpl)
	if strings.ContainsAny(out, "{}") || out == "" || strings.Contains(out, "/") || strings.Contains(out, "..") {
		return "", fmt.Errorf("release: asset template %q expanded to %q", tmpl, out)
	}
	return out, nil
}

func checksumFor(sums, filename string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == filename {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("release: SHA256SUMS has no entry for %s", filename)
}

func extractMember(assetName string, archive []byte, member string) ([]byte, error) {
	if member == "" || strings.Contains(member, "/") || strings.Contains(member, `\`) || member == "." || member == ".." {
		return nil, fmt.Errorf("release: refusing archive member %q", member)
	}
	switch {
	case strings.HasSuffix(assetName, ".tar.gz"), strings.HasSuffix(assetName, ".tgz"):
		return extractTar(archive, member)
	case strings.HasSuffix(assetName, ".zip"):
		return extractZip(archive, member)
	default:
		return nil, fmt.Errorf("release: unsupported archive %q (want .tar.gz or .zip)", assetName)
	}
}

func extractTar(archive []byte, member string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("release: read gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("release: archive has no %s", member)
		}
		if err != nil {
			return nil, fmt.Errorf("release: read archive: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name != member {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("release: %s is not a regular file", member)
		}
		return readLimited(tr, member)
	}
}

func extractZip(archive []byte, member string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("release: read zip: %w", err)
	}
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		if name != member {
			continue
		}
		if f.FileInfo().IsDir() {
			return nil, fmt.Errorf("release: %s is not a regular file", member)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("release: open %s: %w", member, err)
		}
		defer rc.Close()
		return readLimited(rc, member)
	}
	return nil, fmt.Errorf("release: archive has no %s", member)
}

func readLimited(r io.Reader, member string) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("release: read %s: %w", member, err)
	}
	if int64(len(buf)) > maxBytes {
		return nil, fmt.Errorf("release: %s exceeds 256MiB", member)
	}
	return buf, nil
}

func place(dest string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("release: mkdir: %w", err)
	}
	staged := dest + ".new"
	if err := os.WriteFile(staged, data, 0o755); err != nil {
		return fmt.Errorf("release: stage %s: %w", filepath.Base(staged), err)
	}
	if err := os.Rename(staged, dest); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("release: replace %s: %w", filepath.Base(dest), err)
	}
	return nil
}

func isLoopback(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}
