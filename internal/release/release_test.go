package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

func TestInstallVerifiesChecksumAndReplaces(t *testing.T) {
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	t.Setenv("TEST_RELEASE_TOKEN", token)

	payload := []byte("#!/bin/sh\necho new\n")
	archive := tarGz(t, "marller", payload)
	sum := sha256.Sum256(archive)
	sums := hex.EncodeToString(sum[:]) + "  marller-v1.2.3-linux-amd64.tar.gz\n"

	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		sawAuth = true
		switch r.URL.Path {
		case "/repos/Dragonshorn-Studios/marller/releases/latest":
			_ = json.NewEncoder(w).Encode(ghRelease{
				TagName: "v1.2.3",
				Assets: []ghAsset{
					{Name: "marller-v1.2.3-linux-amd64.tar.gz", URL: "http://" + r.Host + "/asset"},
					{Name: "SHA256SUMS", URL: "http://" + r.Host + "/sums"},
				},
			})
		case "/asset":
			_, _ = w.Write(archive)
		case "/sums":
			_, _ = w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	dest := filepath.Join(dir, "bin", "marller")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{APIBase: srv.URL, OS: "linux", Arch: "amd64"}
	src := &config.ReleaseSource{
		Repo:          "Dragonshorn-Studios/marller",
		Asset:         "marller-{tag}-{os}-{arch}.tar.gz",
		ArchiveBinary: "marller",
		TokenRef:      &config.SecretRef{Env: "TEST_RELEASE_TOKEN"},
	}
	tag, err := c.Install(context.Background(), src, dir, "bin/marller")
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v1.2.3" {
		t.Fatalf("tag = %q", tag)
	}
	if !sawAuth {
		t.Fatal("token was not presented to the release API")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("installed %q, want the archive member", got)
	}
	if _, err := os.Stat(dest + ".new"); !os.IsNotExist(err) {
		t.Fatal("staged file was left behind")
	}
}

func TestInstallKeepsBinaryOnChecksumMismatch(t *testing.T) {
	t.Parallel()
	archive := tarGz(t, "marller", []byte("new"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/app/releases/latest":
			_ = json.NewEncoder(w).Encode(ghRelease{
				TagName: "v9",
				Assets: []ghAsset{
					{Name: "app-v9-linux-amd64.tar.gz", URL: "http://" + r.Host + "/asset"},
					{Name: "SHA256SUMS", URL: "http://" + r.Host + "/sums"},
				},
			})
		case "/asset":
			_, _ = w.Write(archive)
		case "/sums":
			_, _ = w.Write([]byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  app-v9-linux-amd64.tar.gz\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	dest := filepath.Join(dir, "bin", "app")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{APIBase: srv.URL, OS: "linux", Arch: "amd64"}
	_, err := c.Install(context.Background(), &config.ReleaseSource{
		Repo: "acme/app", Asset: "app-{tag}-{os}-{arch}.tar.gz", ArchiveBinary: "app",
	}, dir, "bin/app")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "old" {
		t.Fatalf("binary changed to %q on a failed checksum", got)
	}
}

func TestLatestDoesNotEchoToken(t *testing.T) {
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	t.Setenv("TEST_RELEASE_TOKEN_LEAK", token)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied "+token, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := &Client{APIBase: srv.URL}
	_, err := c.Latest(context.Background(), &config.ReleaseSource{
		Repo: "acme/app", TokenRef: &config.SecretRef{Env: "TEST_RELEASE_TOKEN_LEAK"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked into error: %s", err.Error())
	}
}

func TestDestRejectsEscape(t *testing.T) {
	t.Parallel()
	if _, err := Dest("/home/op/.marller", "../marller"); err == nil {
		t.Fatal("path escape was accepted")
	}
}

func tarGz(t *testing.T, name string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
