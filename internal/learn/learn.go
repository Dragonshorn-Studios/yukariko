// Package learn turns read-only Docker discovery (internal/docker) into
// explicit, reviewable configuration proposals while refusing unsafe
// guesses.
//
// Classification is deterministic and side-effect free. The only host
// interaction is read-only Git probing — `.git` presence plus
// `git rev-parse` and `git remote get-url` through the runner — and only at
// a Compose project's declared working directory, never an arbitrary disk
// scan. Uncertain values become Confirmations that block import until a
// user resolves them; unsupported or ambiguous standalone semantics produce
// a refused verdict with reasons. Environment values never reach this
// package (discovery carries names only), and Git URLs are
// credential-stripped before they enter a proposal.
//
// This package only proposes: interactive selection, diffing, and writing
// YAML belong to the learn flow (#7); deploys belong to #11/#12.
package learn

import (
	"context"
	"sort"
	"strconv"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

// Proposal verdicts.
const (
	// VerdictReady marks a proposal with complete evidence; it can be
	// imported as-is once the user selects it.
	VerdictReady = "ready"
	// VerdictNeedsConfirmation marks a proposal with uncertain values that
	// block import until resolved; see Confirmations and TODOs.
	VerdictNeedsConfirmation = "needs_confirmation"
	// VerdictUnsupported marks a refused candidate: its semantics are
	// unsupported or ambiguous and it must not be auto-imported; see
	// Blockers.
	VerdictUnsupported = "unsupported"
)

// appIDMaxLen matches the config app-ID pattern [a-z0-9][a-z0-9-]{0,62}.
const appIDMaxLen = 63

// Confirmation is one uncertain field. A proposal carrying confirmations
// cannot be imported until the user supplies or explicitly accepts a value
// for each.
type Confirmation struct {
	Field  string `json:"field"`  // configuration path the user must resolve, e.g. source.git.branch
	Reason string `json:"reason"` // why the value could not be determined from evidence
}

// ServiceImage is one Compose service's observed image evidence.
type ServiceImage struct {
	Service string `json:"service"`
	Image   string `json:"image"` // reference as observed on the container
}

// ComposeProposal is the evidence-backed draft for one Compose project.
// Compose is always preferred over reconstructing its containers manually:
// the project is proposed as a single app deployed through its own files.
type ComposeProposal struct {
	ProjectName string         `json:"project_name"`
	WorkDir     string         `json:"work_dir,omitempty"` // "" = unknown (see the proposal's confirmations)
	ConfigFiles []string       `json:"config_files,omitempty"`
	EnvFiles    []string       `json:"env_files,omitempty"` // proposed only when label evidence exists
	Profiles    []string       `json:"profiles,omitempty"`  // no per-container evidence exists today; always nil
	Services    []ServiceImage `json:"services,omitempty"`

	SourceMode     string       `json:"source_mode,omitempty"` // config.SourceGit or config.SourceRegistry; "" = undetermined
	Git            *GitEvidence `json:"git,omitempty"`
	RegistryImages []string     `json:"registry_images,omitempty"` // observed per-service images, when SourceMode is registry
}

// StandaloneProposal is the observed launch spec of one standalone container
// plus its reproducibility verdict. Environment keys are names only.
type StandaloneProposal struct {
	ContainerName string                 `json:"container_name"`
	Image         string                 `json:"image,omitempty"`
	Entrypoint    []string               `json:"entrypoint,omitempty"`
	Command       []string               `json:"command,omitempty"`
	EnvKeys       []string               `json:"env_keys,omitempty"` // names only; values were never captured
	Binds         []string               `json:"binds,omitempty"`
	Ports         []string               `json:"ports,omitempty"`
	Networks      []string               `json:"networks,omitempty"`
	Restart       string                 `json:"restart,omitempty"`
	Labels        map[string]string      `json:"labels,omitempty"`
	User          string                 `json:"user,omitempty"`
	WorkDir       string                 `json:"work_dir,omitempty"`
	HealthCheck   *docker.HealthCheckDef `json:"health_check,omitempty"` // nil = none or explicitly disabled
}

// GitEvidence is the read-only Git worktree evidence behind a git-source
// proposal. The URL is credential-stripped; it informs the user, and the
// host's Git credentials keep working as before.
type GitEvidence struct {
	Dir           string `json:"dir"`                      // the directory that was probed (the compose workdir)
	Branch        string `json:"branch,omitempty"`         // "" when detached or unknown
	Remote        string `json:"remote,omitempty"`         // remote name found in the worktree; "" = none
	RemoteURL     string `json:"remote_url,omitempty"`     // credential-stripped; informational
	CredsStripped bool   `json:"creds_stripped,omitempty"` // true when the remote URL carried credentials
	Detached      bool   `json:"detached,omitempty"`
}

// Proposal is one reviewable candidate app.
type Proposal struct {
	ID              string   `json:"id"`                         // stable candidate app ID (config slug, unique here)
	Mode            string   `json:"mode"`                       // config.DeployCompose or config.DeployStandalone
	SystemCandidate bool     `json:"system_candidate,omitempty"` // likely Yukariko/system; callers exclude by default
	SystemReasons   []string `json:"system_reasons,omitempty"`

	Verdict       string         `json:"verdict"` // VerdictReady | VerdictNeedsConfirmation | VerdictUnsupported
	Confirmations []Confirmation `json:"confirmations,omitempty"`
	// TODOs are manual supply items (environment values as refs/values);
	// like confirmations they block import until handled.
	TODOs []string `json:"todos,omitempty"`
	// Blockers are non-empty only for refused candidates.
	Blockers []string `json:"blockers,omitempty"`

	Compose    *ComposeProposal    `json:"compose,omitempty"`    // non-nil iff Mode is compose
	Standalone *StandaloneProposal `json:"standalone,omitempty"` // non-nil iff Mode is standalone

	// Docker is the endpoint the candidate was discovered on (nil = the
	// invoking user's default daemon). Non-default candidates carry the
	// resolved host URL, not a context name: contexts live in the invoking
	// user's ~/.docker/contexts and the service user may not share them.
	Docker *config.DockerEndpoint `json:"docker,omitempty"`
}

// candidate pairs a proposal with the inputs of unique-ID assignment.
type candidate struct {
	proposal *Proposal
	base     string // sanitized slug before disambiguation
	key      string // stable tie-breaker: kind + original name
}

// Proposals classifies a discovery report into reviewable proposals: one per
// Compose project, one per standalone container. System candidates are
// included but flagged; callers must exclude them by default and may let
// users opt in. The result is sorted by ID and fully deterministic. git may
// be nil, in which case Compose source detection records an explicit
// confirmation instead of probing.
func Proposals(ctx context.Context, report *docker.Report, git GitProber) []*Proposal {
	if report == nil {
		return nil
	}
	cands := make([]candidate, 0, len(report.Projects)+len(report.Standalone))
	for _, p := range report.Projects {
		cands = append(cands, composeCandidate(ctx, p, git))
	}
	for _, c := range report.Standalone {
		cands = append(cands, standaloneCandidate(c))
	}
	assignUniqueIDs(cands)
	out := make([]*Proposal, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.proposal)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// assignUniqueIDs gives every proposal a config-valid, unique, deterministic
// ID: the sanitized candidate name, suffixed -2, -3, … when two candidates
// sanitize to the same slug, ordered by candidate kind then original name.
func assignUniqueIDs(cands []candidate) {
	sort.Slice(cands, func(i, j int) bool { return cands[i].key < cands[j].key })
	seen := make(map[string]int, len(cands))
	for _, c := range cands {
		n := seen[c.base]
		seen[c.base] = n + 1
		if n == 0 {
			c.proposal.ID = c.base
			continue
		}
		suffix := "-" + strconv.Itoa(n+1)
		c.proposal.ID = truncateSlug(c.base, appIDMaxLen-len(suffix)) + suffix
	}
}

// slug reduces a container or project name to a config-valid app-ID slug:
// lowercase, non-alphanumerics collapsed to single dashes, trimmed, capped
// at maxLen runes, never empty.
func slug(name string, maxLen int) string {
	var b []byte
	lastDash := true // also prevents a leading dash
	for _, r := range []rune(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b = append(b, byte(r))
			lastDash = false
		case r >= 'A' && r <= 'Z':
			b = append(b, byte(r-'A'+'a'))
			lastDash = false
		default:
			if !lastDash {
				b = append(b, '-')
				lastDash = true
			}
		}
	}
	out := string(b)
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	if r := []rune(out); len(r) > maxLen {
		out = string(r[:maxLen])
		for len(out) > 0 && out[len(out)-1] == '-' {
			out = out[:len(out)-1]
		}
	}
	if out == "" {
		out = "app"
	}
	return out
}

// truncateSlug caps a slug at maxLen runes without leaving a trailing dash;
// maxLen <= 0 collapses to the fallback "app" through slug's emptiness rule
// at the call site of truncateSlug's consumer — here it degrades to "app".
func truncateSlug(base string, maxLen int) string {
	if maxLen < 1 {
		return "app"
	}
	r := []rune(base)
	if len(r) <= maxLen {
		return base
	}
	out := string(r[:maxLen])
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	if out == "" {
		out = "app"
	}
	return out
}

// finalize derives the verdict from the recorded evidence items and puts
// every list into a deterministic order.
func (pr *Proposal) finalize() {
	pr.Confirmations = dedupConfirmations(pr.Confirmations)
	pr.TODOs = dedupSorted(pr.TODOs)
	pr.Blockers = dedupSorted(pr.Blockers)
	pr.SystemReasons = dedupSorted(pr.SystemReasons)
	switch {
	case len(pr.Blockers) > 0:
		pr.Verdict = VerdictUnsupported
	case len(pr.Confirmations) > 0 || len(pr.TODOs) > 0:
		pr.Verdict = VerdictNeedsConfirmation
	default:
		pr.Verdict = VerdictReady
	}
}

func (pr *Proposal) confirm(field, reason string) {
	pr.Confirmations = append(pr.Confirmations, Confirmation{Field: field, Reason: reason})
}

func (pr *Proposal) block(reason string) {
	pr.Blockers = append(pr.Blockers, reason)
}

func dedupConfirmations(in []Confirmation) []Confirmation {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].Field != in[j].Field {
			return in[i].Field < in[j].Field
		}
		return in[i].Reason < in[j].Reason
	})
	out := in[:1]
	for _, c := range in[1:] {
		if c != out[len(out)-1] {
			out = append(out, c)
		}
	}
	return out
}

func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	i := 1
	for j := 1; j < len(out); j++ {
		if out[j] != out[j-1] {
			out[i] = out[j]
			i++
		}
	}
	return out[:i]
}

func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func sortedSetKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
