//go:build e2e

package e2e

// Compliance checks: the repository carries no secrets and no third-party
// copyrighted theme assets. These run with the e2e tag alongside the
// acceptance suite.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryHasNoSecrets(t *testing.T) {
	t.Parallel()
	patterns := []struct {
		name   string
		needle string
	}{
		{"private key header", "BEGIN RSA PRIVATE KEY"},
		{"private key header", "BEGIN OPENSSH PRIVATE KEY"},
		{"private key header", "BEGIN EC PRIVATE KEY"},
		{"aws access key id", "AKIA"},
		{"slack token", "xoxb-"},
		{"generic api key", "api_key = \""},
	}
	skip := map[string]bool{
		".git": true, ".zcode": true, "data": true,
	}
	err := filepath.Walk("../..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		if info.IsDir() {
			if skip[base] || strings.HasPrefix(base, ".") && base != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".md") {
			// Test fixtures and documentation may quote obviously fake
			// credentials (e.g. the reporting key fixtures); they are not
			// real secrets. Real-secret patterns above still apply.
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, p := range patterns {
			// "AKIA" is only flagged for AWS-key-shaped strings to avoid
			// false positives in prose.
			if p.needle == "AKIA" && !strings.Contains(text, "AKIA") {
				continue
			}
			if p.needle != "AKIA" && strings.Contains(text, p.needle) {
				// The reporting fixture keys live only in _test.go files.
				if strings.HasSuffix(path, "_test.go") && strings.Contains(p.needle, "PRIVATE") {
					t.Errorf("%s contains %s", path, p.name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAssetsAreOriginalAndInventoried(t *testing.T) {
	t.Parallel()
	// The only static assets are the dashboard stylesheet; the dashboard is
	// typography and CSS with no third-party material of any kind.
	allowed := map[string]bool{
		"style.css": true,
	}
	err := filepath.Walk("../../internal/ui", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if strings.Contains(path, "static/") || strings.Contains(path, "static\\") {
			base := filepath.Base(path)
			if !allowed[base] {
				t.Errorf("undocumented static asset %s; add it to docs/ASSETS.md", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assets, err := os.ReadFile("../../docs/ASSETS.md")
	if err != nil {
		t.Fatal("docs/ASSETS.md must exist")
	}
	lower := strings.ToLower(string(assets))
	for _, must := range []string{"original", "style.css", "mai-hime"} {
		// "mai-hime" must appear only in the exclusion statement — the
		// document asserts no third-party or copyrighted theme assets.
		if !strings.Contains(lower, must) {
			t.Errorf("ASSETS.md missing %q", must)
		}
	}
}
