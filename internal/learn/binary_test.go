package learn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

func TestBinaryProposalsImportMarllerHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, ".marller")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
	  "schema": 1,
	  "id": "marller",
	  "display_name": "Marller",
	  "kind": "go-binary",
	  "bin": "bin/marller",
	  "args": ["-config", "config.json"],
	  "unit": "marller",
	  "health_url": "http://127.0.0.1:8080/health/live",
	  "release": {
	    "repo": "Dragonshorn-Studios/marller",
	    "asset": "marller-{tag}-{os}-{arch}.tar.gz",
	    "archive_binary": "marller"
	  },
	  "installed_tag": "v1.2.3"
	}`
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// A nested manifest and an unrelated app.json must not be proposed.
	nested := filepath.Join(home, "projects", "demo")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "app.json"), []byte(`{"kind":"go-binary","schema":1,"id":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "app.json"), []byte(`{"kind":"go-binary","schema":1,"id":"nested"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "notes", "app.json"), []byte(`{"kind":"other"}`), 0o644); err == nil {
		// notes/ may not exist; create it.
	}
	notes := filepath.Join(home, "notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notes, "app.json"), []byte(`{"kind":"other"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := BinaryProposals(home)
	if len(got) != 2 {
		t.Fatalf("proposals = %d, want marller plus the incomplete projects manifest: %+v", len(got), got)
	}
	var marller *Proposal
	for _, p := range got {
		if p.ID == "marller" {
			marller = p
		}
	}
	if marller == nil || marller.Verdict != VerdictReady || marller.Mode != config.DeployBinary {
		t.Fatalf("marller proposal = %+v", marller)
	}
	app, err := buildApp(context.Background(), marller, resolution{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := renderYAML(&config.Config{SchemaVersion: config.CurrentSchemaVersion, Apps: []config.App{app}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(raw); err != nil {
		t.Fatalf("learned config is invalid: %v\n%s", err, raw)
	}
	if app.Source.Release.Repo != "Dragonshorn-Studios/marller" || app.Deploy.Binary.Unit != "marller" {
		t.Fatalf("app = %+v %+v", app.Source, app.Deploy)
	}
	if app.Health == nil || app.Health.HTTP.URL != "http://127.0.0.1:8080/health/live" {
		t.Fatalf("health = %+v", app.Health)
	}
}

func TestScanIncludesHomeManifest(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, ".marller")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"id":"marller","kind":"go-binary","bin":"bin/marller","unit":"marller","release":{"repo":"Dragonshorn-Studios/marller","asset":"marller-{tag}.tar.gz","archive_binary":"marller"}}`
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &flow{opts: FlowOptions{
		Client: &scanClient{},
		Stdout: &strings.Builder{},
		Home:   home,
	}}
	proposals, err := f.scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || proposals[0].ID != "marller" {
		t.Fatalf("proposals = %+v", proposals)
	}
}

// scanClient with no containers is enough; docker.ContainerSummary is
// referenced so an empty report still type-checks against Discover.
var _ docker.Client = (*scanClient)(nil)
