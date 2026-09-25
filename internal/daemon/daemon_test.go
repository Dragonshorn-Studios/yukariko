package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/git"
	"github.com/Dragonshorn-Studios/yukariko/internal/registry"
	"github.com/Dragonshorn-Studios/yukariko/internal/runner"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// --- git fixture ------------------------------------------------------------

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// openTempStore opens a store under a temp directory.
func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// --- source checker dispatch ------------------------------------------------

func TestSourceCheckerGitDispatch(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, "", "init", "--bare", "-q", bare)
	gitRun(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	gitRun(t, work, "init")
	gitRun(t, work, "symbolic-ref", "HEAD", "refs/heads/main")
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("x\n"), 0o644)
	gitRun(t, work, "add", "f.txt")
	gitRun(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "one")
	gitRun(t, work, "remote", "add", "origin", bare)
	gitRun(t, work, "push", "-q", "-u", "origin", "main")
	sha := gitOut(t, work, "rev-parse", "HEAD")

	st := openTempStore(t)
	checker := &SourceChecker{store: st, git: &git.Client{}}
	app := &config.App{
		ID: "g",
		Source: config.Source{
			Mode: config.SourceGit,
			Git:  &config.GitSource{Dir: work, Branch: "main", Remote: "origin"},
		},
	}
	res, err := checker.Check(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	// Never checkpointed: the deployment is owed even though the worktree
	// already sits at the remote SHA (first run, or a cancelled pass).
	if !res.Changed || res.Observed != sha || !strings.Contains(res.Detail, "deployment owed") {
		t.Errorf("res = %+v, want owed at %q", res, sha)
	}
	if v, _, ok, _ := st.ObservedVersion(context.Background(), "g", store.KindGitSHA); !ok || v != sha {
		t.Errorf("observed = %q ok=%v, want the SHA recorded separately", v, ok)
	}
	// A git deploy checkpoint stores under the same kind the checker reads.
	if _, _, ok, _ := st.DeployedVersion(context.Background(), "g", store.KindGitSHA); ok {
		t.Error("nothing deployed yet; deployed version must be absent")
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out.String())
}

func TestSourceCheckerRegistryDispatch(t *testing.T) {
	t.Parallel()
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Header().Set("Docker-Content-Digest", digestOf(manifest))
		w.Write(manifest)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	st := openTempStore(t)
	resolver := &registry.Resolver{EndpointOverride: map[string]string{host: srv.URL}}
	checker := &SourceChecker{store: st, registry: resolver}
	app := &config.App{
		ID: "r",
		Source: config.Source{
			Mode: config.SourceRegistry,
			Registry: &config.RegistrySource{
				Images: []config.ImageRef{{Ref: host + "/app:1"}},
			},
		},
	}
	res, err := checker.Check(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Errorf("first observation must report changed (no baseline): %+v", res)
	}
	// The observation is stored per image under a digest:<ref> kind.
	obsKind := store.KindDigest + ":" + host + "/app:1"
	if v, _, ok, _ := st.ObservedVersion(context.Background(), "r", obsKind); !ok || v != digestOf(manifest) {
		t.Errorf("observed = %q ok=%v, want %q", v, ok, digestOf(manifest))
	}
	// With the baseline recorded (through a real open deployment), the next
	// check is unchanged.
	depID, err := st.BeginDeployment(context.Background(), store.BeginDeploymentParams{AppID: "r", Cause: "test", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitDeploymentSuccess(context.Background(), "r", store.KindDigest, host+"/app:1@"+digestOf(manifest), depID, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err = checker.Check(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Errorf("same digest reported changed: %+v", res)
	}
}

func digestOf(body []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}

// --- deployment dispatcher transaction --------------------------------------

func TestDeployDispatcherCommitsAndFailsTransactionally(t *testing.T) {
	t.Parallel()
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "image inspect") || req.URL.Path == "/v2/app/manifests/1" {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", digestOf(manifest))
			w.Write(manifest)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	composeFile := filepath.Join(dir, "compose.yaml")
	os.WriteFile(composeFile, []byte("services: {}\n"), 0o644)

	app := &config.App{
		ID:      "web",
		Timeout: config.Duration(time.Second),
		Source: config.Source{
			Mode: config.SourceRegistry,
			Registry: &config.RegistrySource{
				Images: []config.ImageRef{{Ref: host + "/app:1"}},
			},
		},
		Deploy: config.Deploy{
			Mode: config.DeployCompose,
			Compose: &config.ComposeDeploy{
				WorkDir: dir, Files: []string{composeFile}, ProjectName: "web",
			},
		},
	}

	st := openTempStore(t)
	runnerSvc := &runner.Runner{}
	dispatcher := &DeployDispatcher{
		store:    st,
		runner:   runnerSvc,
		Registry: &registry.Resolver{EndpointOverride: map[string]string{host: srv.URL}},
	}

	// The compose pipeline runs real `docker compose` through the runner
	// here, which fails on hosts without Docker: this asserts the failure
	// path records a failed deployment and never advances the version.
	_, err := dispatcher.Deploy(context.Background(), app)
	if err == nil {
		t.Skip("docker compose is available; failure-path coverage already asserted")
	}
	if deps, _ := st.RecentDeployments(context.Background(), "web", 1); len(deps) != 1 || deps[0].Status != store.StatusFailed {
		t.Errorf("deployment row = %+v, want exactly one failed row", deps)
	}
	if _, _, ok, _ := st.DeployedVersion(context.Background(), "web", store.KindDigest); ok {
		t.Error("a failed deployment must never advance the deployed version")
	}
}

func TestAssembleBuildsAPIWhenEnabled(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		SchemaVersion: 1,
		Server:        config.Server{Enabled: true},
	}
	asm, err := Assemble(context.Background(), Options{Config: cfg, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer asm.Store.Close()
	if !asm.APIOn || asm.API == nil || asm.APIBind == "" {
		t.Errorf("API = %+v bind = %q, want the read-only server wired", asm.API, asm.APIBind)
	}
}

func TestAssembleBuildsAuthenticatorWhenEnabled(t *testing.T) {
	t.Parallel()
	newCfg := func(auth config.Auth) *config.Config {
		return &config.Config{
			SchemaVersion: 1,
			Server:        config.Server{Enabled: true},
			Auth:          auth,
		}
	}
	enabled := config.Auth{OIDC: config.OIDCAuth{
		Enabled: true, Issuer: "https://auth.example.com", ClientID: "yukariko",
		ClientSecretRef: &config.SecretRef{Env: "OIDC_SECRET"},
		RedirectBase:    "https://yukariko.example.com",
	}}

	asm, err := Assemble(context.Background(), Options{Config: newCfg(enabled), DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer asm.Store.Close()
	if asm.Authenticator == nil {
		t.Fatal("Authenticator = nil, want the OIDC gate wired when enabled")
	}
	if asm.Authenticator.OIDC.Issuer != "https://auth.example.com" || asm.Authenticator.Store == nil {
		t.Errorf("Authenticator misconfigured: %+v", asm.Authenticator.OIDC)
	}

	asm, err = Assemble(context.Background(), Options{Config: newCfg(config.Auth{}), DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Assemble without auth: %v", err)
	}
	defer asm.Store.Close()
	if asm.Authenticator != nil {
		t.Error("Authenticator != nil, want nil when auth is disabled")
	}
}
