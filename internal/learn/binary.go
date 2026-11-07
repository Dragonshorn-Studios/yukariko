package learn

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// goBinaryKind is the app.json kind the Marller installer writes. Other
// app.json files in a home directory are not proposals.
const goBinaryKind = "go-binary"

var binaryAppID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type binaryManifest struct {
	Schema      int      `json:"schema"`
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Kind        string   `json:"kind"`
	Bin         string   `json:"bin"`
	Args        []string `json:"args"`
	Unit        string   `json:"unit"`
	HealthURL   string   `json:"health_url"`
	Release     struct {
		Repo          string `json:"repo"`
		Asset         string `json:"asset"`
		ArchiveBinary string `json:"archive_binary"`
	} `json:"release"`
}

// BinaryProposals reads app.json one directory below home. Only kind
// go-binary is considered. A manifest that claims that kind but cannot be
// imported is returned as a refused proposal so the operator sees why.
// The scan does not follow symlinks and does not recurse.
func BinaryProposals(home string) []*Proposal {
	entries, err := os.ReadDir(home)
	if err != nil {
		return []*Proposal{{
			ID:       "home",
			Mode:     config.DeployBinary,
			Verdict:  VerdictUnsupported,
			Blockers: []string{"cannot read " + home + ": " + err.Error()},
		}}
	}
	var out []*Proposal
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(home, entry.Name())
		manifestPath := filepath.Join(dir, "app.json")
		info, err := os.Lstat(manifestPath)
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var manifest binaryManifest
		if err := json.Unmarshal(raw, &manifest); err != nil || manifest.Kind != goBinaryKind {
			continue
		}
		out = append(out, proposalFromManifest(dir, manifest))
	}
	return out
}

func proposalFromManifest(dir string, m binaryManifest) *Proposal {
	id := m.ID
	if id == "" {
		id = homeSlug(filepath.Base(dir))
	}
	p := &Proposal{
		ID:      id,
		Mode:    config.DeployBinary,
		Verdict: VerdictReady,
		Binary: &BinaryProposal{
			Dir:           dir,
			Bin:           m.Bin,
			Unit:          m.Unit,
			DisplayName:   m.DisplayName,
			HealthURL:     m.HealthURL,
			Repo:          m.Release.Repo,
			Asset:         m.Release.Asset,
			ArchiveBinary: m.Release.ArchiveBinary,
			Args:          m.Args,
		},
	}
	var blockers []string
	if m.Schema != 1 {
		blockers = append(blockers, "app.json schema is not 1")
	}
	if !binaryAppID.MatchString(id) {
		blockers = append(blockers, "id "+id+" is not a yukariko app id")
	}
	if m.Bin == "" || m.Unit == "" || m.Release.Repo == "" || m.Release.Asset == "" || m.Release.ArchiveBinary == "" {
		blockers = append(blockers, "app.json is missing bin, unit, or release fields")
	}
	if m.HealthURL != "" {
		u, err := url.Parse(m.HealthURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			blockers = append(blockers, "health_url must be an absolute http or https URL")
		}
	}
	if len(blockers) > 0 {
		p.Verdict = VerdictUnsupported
		p.Blockers = blockers
	}
	return p
}

func homeSlug(name string) string {
	name = strings.TrimPrefix(name, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "app"
	}
	if len(out) > appIDMaxLen {
		return out[:appIDMaxLen]
	}
	return out
}
