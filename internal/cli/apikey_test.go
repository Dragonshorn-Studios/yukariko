package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/exitcode"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

func apiKeyTestEnv(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	cfgPath = filepath.Join(t.TempDir(), "yukariko.yaml")
	if err := os.WriteFile(cfgPath, []byte("schema_version: 1\napps: []\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	return cfgPath, t.TempDir()
}

// runAPIKey executes an apikey command and returns stdout and the exit code.
func runAPIKey(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	stdout := &strings.Builder{}
	err := Execute(context.Background(), args, stdout, io.Discard)
	return stdout.String(), Code(err), err
}

func TestAPIKeyCreatePrintsTokenOnce(t *testing.T) {
	t.Parallel()
	cfg, dataDir := apiKeyTestEnv(t)

	out, code, err := runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir, "--name", "amadeus", "--json")
	if code != exitcode.OK || err != nil {
		t.Fatalf("apikey create = %d (%v); out: %s", code, err, out)
	}
	var created struct {
		Name      string     `json:"name"`
		Token     string     `json:"token"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("decode create output %q: %v", out, err)
	}
	if created.Name != "amadeus" || !strings.HasPrefix(created.Token, "ykr_") {
		t.Errorf("create output = %+v; want named token with ykr_ prefix", created)
	}
	if created.ExpiresAt != nil {
		t.Errorf("expires_at = %v; want unset", created.ExpiresAt)
	}

	// The store holds only the SHA-256 of the token.
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	k, ok, err := st.APIKeyByName(context.Background(), "amadeus")
	if err != nil || !ok {
		t.Fatalf("APIKeyByName: ok=%v err=%v", ok, err)
	}
	sum := sha256.Sum256([]byte(created.Token))
	if k.TokenHash != hex.EncodeToString(sum[:]) {
		t.Error("stored token_hash is not the SHA-256 of the printed token")
	}
}

func TestAPIKeyCreateWithExpiry(t *testing.T) {
	t.Parallel()
	cfg, dataDir := apiKeyTestEnv(t)
	before := time.Now().UTC().Add(-time.Minute)
	out, code, err := runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir, "--name", "temp", "--expires", "720h", "--json")
	if code != exitcode.OK || err != nil {
		t.Fatalf("apikey create = %d (%v); out: %s", code, err, out)
	}
	var created struct {
		Token     string     `json:"token"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("decode create output %q: %v", out, err)
	}
	// The expiry must be roughly now+720h and in the future — a sign flip
	// or wrong epoch would leave the key born expired.
	if created.ExpiresAt == nil {
		t.Fatal("expires_at missing")
	}
	if want := before.Add(720 * time.Hour); created.ExpiresAt.Before(want) {
		t.Errorf("expires_at = %v, want at least %v (now+720h)", created.ExpiresAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if !created.ExpiresAt.After(time.Now().UTC()) {
		t.Errorf("expires_at = %v is not in the future", created.ExpiresAt.Format(time.RFC3339))
	}

	// The stored row carries the same future expiry.
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	k, ok, err := st.APIKeyByName(context.Background(), "temp")
	if err != nil || !ok {
		t.Fatalf("APIKeyByName: ok=%v err=%v", ok, err)
	}
	if k.ExpiresAt == nil || !k.ExpiresAt.Equal(*created.ExpiresAt) {
		t.Errorf("stored expires_at = %v, want %v", k.ExpiresAt, created.ExpiresAt)
	}
}

func TestAPIKeyCreateRejectsBadNameAndDuplicates(t *testing.T) {
	t.Parallel()
	cfg, dataDir := apiKeyTestEnv(t)

	if _, code, err := runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir, "--name", "bad name!"); code != exitcode.Usage {
		t.Errorf("invalid name exit = %d (%v), want %d", code, err, exitcode.Usage)
	}
	if _, code, err := runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir, "--name", "ok", "--expires", "nope"); code != exitcode.Usage {
		t.Errorf("invalid expires exit = %d (%v), want %d", code, err, exitcode.Usage)
	}
	// A missing required flag is cobra-side validation: still Usage.
	if _, code, err := runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir); code != exitcode.Usage {
		t.Errorf("missing --name exit = %d (%v), want %d", code, err, exitcode.Usage)
	}
	if _, code, err := runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir, "--name", "amadeus"); code != exitcode.OK {
		t.Fatalf("first create = %d (%v), want OK", code, err)
	}
	if _, code, err := runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir, "--name", "amadeus"); code != exitcode.Error {
		t.Errorf("duplicate create = %d (%v), want error", code, err)
	}
}

func TestAPIKeyListAndRevoke(t *testing.T) {
	t.Parallel()
	cfg, dataDir := apiKeyTestEnv(t)
	base := []string{"--config", cfg, "--data-dir", dataDir}

	if out, code, err := runAPIKey(t, append([]string{"apikey", "list"}, base...)...); code != exitcode.OK || err != nil {
		t.Fatalf("empty list = %d (%v); out: %s", code, err, out)
	} else if !strings.Contains(out, "no api keys") {
		t.Errorf("empty list output = %q", out)
	}

	out, code, err := runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir, "--name", "amadeus")
	if code != exitcode.OK || err != nil {
		t.Fatalf("create amadeus = %d (%v): %s", code, err, out)
	}
	out, code, err = runAPIKey(t, "apikey", "create", "--config", cfg, "--data-dir", dataDir, "--name", "ci")
	if code != exitcode.OK || err != nil {
		t.Fatalf("create ci = %d (%v): %s", code, err, out)
	}

	out, code, err = runAPIKey(t, "apikey", "list", "--config", cfg, "--data-dir", dataDir, "--json")
	if code != exitcode.OK || err != nil {
		t.Fatalf("list = %d (%v): %s", code, err, out)
	}
	var views []apiKeyView
	if err := json.Unmarshal([]byte(out), &views); err != nil {
		t.Fatalf("decode list %q: %v", out, err)
	}
	if len(views) != 2 || views[0].Name != "amadeus" || views[1].Name != "ci" {
		t.Fatalf("list = %+v; want amadeus then ci", views)
	}
	if views[0].Status != "active" {
		t.Errorf("status = %q, want active", views[0].Status)
	}

	// Revoke by name; the status flips and a second revoke is idempotent.
	out, code, err = runAPIKey(t, "apikey", "revoke", "amadeus", "--config", cfg, "--data-dir", dataDir)
	if code != exitcode.OK || err != nil {
		t.Fatalf("revoke = %d (%v): %s", code, err, out)
	}
	out, code, err = runAPIKey(t, "apikey", "list", "--config", cfg, "--data-dir", dataDir, "--json")
	if code != exitcode.OK || err != nil {
		t.Fatalf("list after revoke = %d (%v): %s", code, err, out)
	}
	if err := json.Unmarshal([]byte(out), &views); err != nil {
		t.Fatalf("decode list %q: %v", out, err)
	}
	if views[0].Status != "revoked" || views[0].RevokedAt == nil {
		t.Errorf("status after revoke = %+v; want revoked with stamp", views[0])
	}
	out, code, err = runAPIKey(t, "apikey", "revoke", "amadeus", "--config", cfg, "--data-dir", dataDir)
	if code != exitcode.OK || err != nil || !strings.Contains(out, "already revoked") {
		t.Errorf("second revoke = %d (%v): %s; want idempotent OK", code, err, out)
	}

	// Revoke by id exercises the APIKeyByID fallback (the common flow after
	// copying an id from `apikey list`).
	out, code, err = runAPIKey(t, "apikey", "revoke", views[1].ID, "--config", cfg, "--data-dir", dataDir)
	if code != exitcode.OK || err != nil || !strings.Contains(out, "revoked") {
		t.Errorf("revoke by id = %d (%v): %s; want OK", code, err, out)
	}

	// Unknown reference errors.
	if _, code, err := runAPIKey(t, "apikey", "revoke", "ghost", "--config", cfg, "--data-dir", dataDir); code != exitcode.Error {
		t.Errorf("revoke unknown = %d (%v), want error", code, err)
	}
}
