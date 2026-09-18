package learn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

// ErrAborted is returned when interactive input ends (EOF) mid-flow. The
// CLI maps it to the interrupted exit code; no changes were made.
var ErrAborted = errors.New("learn: input ended")

// ErrDeclined is returned when the user declines the final write. This is
// a clean outcome, not an error condition for the user, but callers may
// want to distinguish it from a completed write.
var ErrDeclined = errors.New("learn: write declined")

// FlowOptions configures one learn run. Client and Stdout are required;
// ConfigPath is required and validated by the CLI layer.
type FlowOptions struct {
	ConfigPath string
	DryRun     bool // full preview, never writes
	JSONOut    bool // machine-readable proposals, never writes, no prompts
	// IncludeSystem lets system/infrastructure candidates into the
	// selectable list; they are excluded by default.
	IncludeSystem bool
	Client        docker.Client // read-only discovery
	// Endpoints lists additional local daemons to scan after the default
	// one (opts.Client is always the default daemon's client). Each entry
	// carries its own scoped client; nil means a default-daemon-only scan.
	// Production wiring derives it from docker.CLIClient.LocalEndpoints.
	Endpoints []EndpointScan
	Git       GitProber // optional; nil disables worktree detection
	Stdin     io.Reader
	Stdout    io.Writer
	// Now stamps the backup file name; zero means time.Now. Injectable for
	// deterministic tests.
	Now time.Time
}

// EndpointScan is one additional local daemon to scan during learn.
type EndpointScan struct {
	Endpoint docker.Endpoint
	Client   docker.Client
}

// FlowResult reports what one run decided. Imported lists candidate IDs
// merged into the proposed document; Wrote reports whether it reached disk.
type FlowResult struct {
	Wrote      bool
	BackupPath string
	Imported   []string
	Skipped    []string // "id: reason"
	DiffOutput string   // the unified diff that was shown ("" when nothing changed)
}

// RunFlow executes the interactive learn flow: scan (read-only), present
// candidates, select, resolve required fields, preview a unified YAML diff,
// and write only after an explicit confirmation. Docker is never mutated and
// nothing is deployed. --json out prints proposals without any prompts.
func RunFlow(ctx context.Context, opts FlowOptions) (FlowResult, error) {
	if opts.Client == nil || opts.Stdout == nil || opts.ConfigPath == "" {
		return FlowResult{}, errors.New("learn: config path, client, and stdout are required")
	}
	f := &flow{opts: opts, in: bufioScanner(opts.Stdin)}

	original, existed, err := f.loadOriginal()
	if err != nil {
		return FlowResult{}, err
	}

	proposals, err := f.scan(ctx)
	if err != nil {
		return FlowResult{}, err
	}

	cleanStaleTemps(opts.ConfigPath)

	if opts.JSONOut {
		return f.printJSON(proposals)
	}

	selected, err := f.present(proposals)
	if err != nil {
		return FlowResult{}, err
	}
	var apps []config.App
	for _, p := range selected {
		app, skipReason, err := f.resolve(ctx, p)
		if err != nil {
			return FlowResult{Imported: ids(apps), Skipped: f.skips}, err
		}
		if skipReason != "" {
			f.skips = append(f.skips, p.ID+": "+skipReason)
			continue
		}
		apps = append(apps, app)
	}
	result := FlowResult{Imported: ids(apps), Skipped: f.skips}
	if len(apps) == 0 {
		fmt.Fprintln(opts.Stdout, "nothing selected to import.")
		return result, nil
	}

	newBytes, err := f.merge(original, existed, apps)
	if err != nil {
		return result, err
	}
	diffText := Diff(opts.ConfigPath+" (current)", opts.ConfigPath+" (proposed)", original, newBytes)
	result.DiffOutput = diffText
	if diffText != "" {
		fmt.Fprint(opts.Stdout, diffText)
	} else {
		fmt.Fprintln(opts.Stdout, "no changes; the configuration already matches this selection.")
		return result, nil // idempotent rerun
	}

	// The generated document must pass the strict validator before the
	// original file is touched at all.
	if _, err := config.Parse(newBytes); err != nil {
		return result, fmt.Errorf("generated configuration is invalid; nothing was written: %w", err)
	}

	if opts.DryRun {
		fmt.Fprintln(opts.Stdout, "dry-run: no changes written.")
		return result, nil
	}

	fmt.Fprintln(opts.Stdout, "Manual settings (intervals, retries, steps, health, enabled) of existing apps are preserved.")
	answer, err := f.prompt("Write changes to " + opts.ConfigPath + "? [y/N]")
	if err != nil {
		return result, err
	}
	if !isYes(answer) {
		fmt.Fprintln(opts.Stdout, "declined; no changes written.")
		return result, ErrDeclined
	}

	backup, err := f.write(original, existed, newBytes)
	if err != nil {
		return result, fmt.Errorf("write failed; the original configuration is intact: %w", err)
	}
	result.Wrote = true
	result.BackupPath = backup
	fmt.Fprintf(opts.Stdout, "wrote %s.\n", opts.ConfigPath)
	if backup != "" {
		fmt.Fprintf(opts.Stdout, "backup: %s\n", backup)
	}
	fmt.Fprintln(opts.Stdout, "learn does not deploy anything; monitoring starts when the daemon runs.")
	return result, nil
}

type flow struct {
	opts  FlowOptions
	in    *bufio.Scanner
	skips []string
}

func bufioScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4<<10), 64<<10)
	return sc
}

// loadOriginal reads the existing configuration, requiring it to be valid.
// A missing file starts a fresh document.
func (f *flow) loadOriginal() (original []byte, existed bool, err error) {
	b, err := os.ReadFile(f.opts.ConfigPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read config: %w", err)
	}
	if _, err := config.Load(f.opts.ConfigPath); err != nil {
		return nil, true, fmt.Errorf("existing configuration is invalid; fix it before running learn: %w", err)
	}
	return b, true, nil
}

// printJSON writes machine-readable proposals; no prompts, no writes.
func (f *flow) printJSON(proposals []*Proposal) (FlowResult, error) {
	out, err := json.MarshalIndent(proposals, "", "  ")
	if err != nil {
		return FlowResult{}, fmt.Errorf("marshal proposals: %w", err)
	}
	fmt.Fprintln(f.opts.Stdout, string(out))
	return FlowResult{}, nil
}

// candidateView is one selectable line in the presentation list.
type candidateView struct {
	proposal *Proposal
	hint     string
}

// present lists candidates and reads the selection. System candidates are
// excluded unless IncludeSystem is set; refused candidates are shown but
// never selectable.
func (f *flow) present(proposals []*Proposal) ([]*Proposal, error) {
	var selectable []candidateView
	var refused []*Proposal
	hiddenSystem := 0
	for _, p := range proposals {
		switch {
		case p.Verdict == VerdictUnsupported:
			refused = append(refused, p)
		case p.SystemCandidate && !f.opts.IncludeSystem:
			hiddenSystem++
		default:
			selectable = append(selectable, candidateView{proposal: p, hint: describe(p)})
		}
	}

	w := f.opts.Stdout
	if len(refused) > 0 {
		fmt.Fprintln(w, "Refused candidates (unsafe or ambiguous; configure manually if needed):")
		for _, p := range refused {
			fmt.Fprintf(w, "  - %s (%s): %s\n", p.ID, p.Mode, strings.Join(p.Blockers, "; "))
		}
		fmt.Fprintln(w)
	}
	if len(selectable) == 0 {
		fmt.Fprintf(w, "no importable candidates found.%s\n", skippedSystemNote(hiddenSystem))
		return nil, nil
	}
	fmt.Fprintln(w, "Discovered candidates:")
	for i, c := range selectable {
		fmt.Fprintf(w, "  %d) %-16s %-10s %s\n", i+1, c.proposal.ID, "["+c.proposal.Mode+"]", c.hint)
		if c.proposal.Verdict == VerdictNeedsConfirmation {
			fmt.Fprintf(w, "     needs input: %s\n", confirmationFields(c.proposal))
		}
	}
	if hiddenSystem > 0 {
		fmt.Fprintf(w, "  (%d system/infrastructure candidate(s) hidden; run with --include-system to review them)\n", hiddenSystem)
	}

	answer, err := f.prompt("Select candidates to import (comma-separated numbers, 'all', empty for none)")
	if err != nil {
		return nil, err
	}
	return parseSelection(answer, selectable)
}

func skippedSystemNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d system/infrastructure candidate(s) hidden)", n)
}

// describe renders one presentation line of evidence.
func describe(p *Proposal) string {
	switch p.Mode {
	case "compose":
		c := p.Compose
		return fmt.Sprintf("%d service(s) in %s", len(c.Services), or(c.WorkDir, "unknown directory"))
	case "standalone":
		return fmt.Sprintf("image %s", or(p.Standalone.Image, "unknown"))
	default:
		return ""
	}
}

func confirmationFields(p *Proposal) string {
	fields := make([]string, 0, len(p.Confirmations)+len(p.TODOs))
	for _, c := range p.Confirmations {
		fields = append(fields, c.Field)
	}
	fields = append(fields, fmt.Sprintf("%d environment value(s)", len(p.TODOs)))
	return strings.Join(fields, ", ")
}

// parseSelection maps the user's answer onto proposals. An empty answer is
// a clean empty selection.
func parseSelection(answer string, selectable []candidateView) ([]*Proposal, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil, nil
	}
	if strings.EqualFold(answer, "all") {
		out := make([]*Proposal, len(selectable))
		for i, c := range selectable {
			out[i] = c.proposal
		}
		return out, nil
	}
	var out []*Proposal
	seen := map[int]bool{}
	for _, part := range strings.Split(answer, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > len(selectable) {
			return nil, fmt.Errorf("invalid selection %q: enter numbers between 1 and %d, or 'all'", part, len(selectable))
		}
		if !seen[n-1] {
			seen[n-1] = true
			out = append(out, selectable[n-1].proposal)
		}
	}
	return out, nil
}

// resolution carries the interactively supplied values for one candidate.
type resolution struct {
	branch       string
	remote       string
	workDir      string
	files        []string
	image        string
	env          []config.EnvVar
	healthDocker bool
	registryOK   bool
}

// resolve walks a candidate's confirmations and TODOs through interactive
// prompts. ok=false means the candidate must be skipped; the reason says why.
// scan discovers candidates on the default daemon and every additional
// configured local endpoint (rootless and multi-daemon hosts). Candidates
// from a non-default endpoint are stamped with its resolved host URL; IDs
// colliding across daemons are disambiguated with the context name so the
// merged document keeps unique app IDs.
func (f *flow) scan(ctx context.Context) ([]*Proposal, error) {
	scans := []EndpointScan{{Client: f.opts.Client}}
	scans = append(scans, f.opts.Endpoints...)
	var out []*Proposal
	seen := map[string]bool{}
	for i, scan := range scans {
		report, err := docker.Discover(ctx, scan.Client)
		if err != nil {
			if i == 0 {
				return nil, fmt.Errorf("discovery failed: %w", err)
			}
			// Additional endpoints are opportunistic: an unreachable
			// rootless daemon must not block learning the default one.
			fmt.Fprintf(f.opts.Stdout, "skipping docker context %q: %v\n", scan.Endpoint.Name, err)
			continue
		}
		var endpoint *config.DockerEndpoint
		if scan.Endpoint.Host != "" {
			endpoint = &config.DockerEndpoint{Host: scan.Endpoint.Host}
		}
		for _, p := range Proposals(ctx, report, f.opts.Git) {
			if endpoint != nil {
				p.Docker = endpoint
			}
			if seen[p.ID] {
				p.ID = disambiguatedID(p.ID, scan.Endpoint.Name)
				if seen[p.ID] {
					continue
				}
			}
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// disambiguatedID appends a slug-safe form of the context name, staying
// within the config app-ID pattern and length.
func disambiguatedID(id, contextName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(contextName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	suffix := b.String()
	if suffix == "" {
		suffix = "alt"
	}
	if len(id)+1+len(suffix) > 63 {
		id = id[:63-1-len(suffix)]
	}
	return id + "-" + suffix
}

func (f *flow) resolve(ctx context.Context, p *Proposal) (app config.App, skipReason string, err error) {
	fmt.Fprintf(f.opts.Stdout, "\n== %s (%s) ==\n", p.ID, p.Mode)
	var res resolution
	for _, c := range p.Confirmations {
		switch c.Field {
		case "source.git.branch":
			if res.branch, err = f.promptRequired("Branch to track"); err != nil {
				return config.App{}, "", err
			}
		case "source.git.remote":
			if res.remote, err = f.promptDefault("Git remote", "origin"); err != nil {
				return config.App{}, "", err
			}
		case "deploy.compose.work_dir":
			if res.workDir, err = f.promptRequired("Compose working directory"); err != nil {
				return config.App{}, "", err
			}
		case "deploy.compose.files":
			raw, err := f.promptRequired("Compose files (comma-separated, in override order)")
			if err != nil {
				return config.App{}, "", err
			}
			res.files = splitComposeList(raw)
		case "standalone.image":
			if res.image, err = f.promptDefault("Image reference (pinned tag or digest)", p.Standalone.Image); err != nil {
				return config.App{}, "", err
			}
		case "standalone.health_check":
			ans, err := f.promptDefault("Track this app's docker health status?", "y")
			if err != nil {
				return config.App{}, "", err
			}
			res.healthDocker = isYes(ans)
		case "source.mode":
			if strings.Contains(c.Reason, "registry tracking") {
				ans, err := f.promptDefault("Track the observed images from a registry?", "y")
				if err != nil {
					return config.App{}, "", err
				}
				if !isYes(ans) {
					return config.App{}, "registry tracking declined", nil
				}
				res.registryOK = true
				continue
			}
			return config.App{}, c.Reason, nil // unresolvable interactively
		default:
			return config.App{}, "unresolved: " + c.Field + " (" + c.Reason + ")", nil
		}
	}
	// Standalone environment: every key needs a value or a reference.
	for _, key := range envKeysOf(p) {
		ans, err := f.prompt(fmt.Sprintf("env %s — [e]nv ref name, [p]lain value, [s]kip app", key))
		if err != nil {
			return config.App{}, "", err
		}
		switch strings.ToLower(strings.TrimSpace(ans)) {
		case "e":
			name, err := f.promptRequired("  environment variable name for the secret_ref of " + key)
			if err != nil {
				return config.App{}, "", err
			}
			res.env = append(res.env, config.EnvVar{Name: key, SecretRef: &config.SecretRef{Env: name}})
		case "p":
			val, err := f.promptRequired("  plain (non-secret) value for " + key)
			if err != nil {
				return config.App{}, "", err
			}
			res.env = append(res.env, config.EnvVar{Name: key, Value: val})
		case "s":
			return config.App{}, "environment value for " + key + " not provided", nil
		default:
			return config.App{}, "environment value for " + key + " not provided", nil
		}
	}
	app, err = buildApp(ctx, p, res)
	if err != nil {
		return config.App{}, err.Error(), nil
	}
	// The endpoint the candidate was discovered on is part of the observed
	// reality and travels into the app (learn-owned, like source/deploy).
	app.Docker = p.Docker
	return app, "", nil
}

func envKeysOf(p *Proposal) []string {
	if p.Standalone != nil {
		return p.Standalone.EnvKeys
	}
	return nil
}

// prompt prints the prompt and reads one line.
func (f *flow) prompt(label string) (string, error) {
	fmt.Fprintf(f.opts.Stdout, "%s: ", label)
	if !f.in.Scan() {
		if err := f.in.Err(); err != nil {
			return "", fmt.Errorf("%w: %v", ErrAborted, err)
		}
		return "", ErrAborted
	}
	return strings.TrimSpace(f.in.Text()), nil
}

// promptDefault accepts an empty answer as the default.
func (f *flow) promptDefault(label, def string) (string, error) {
	if def != "" {
		label = fmt.Sprintf("%s [%s]", label, def)
	}
	ans, err := f.prompt(label)
	if err != nil {
		return "", err
	}
	if ans == "" {
		return def, nil
	}
	return ans, nil
}

// promptRequired re-asks up to three times when the answer is empty; a
// still-empty answer skips the candidate rather than fabricating a value.
func (f *flow) promptRequired(label string) (string, error) {
	for i := 0; i < 3; i++ {
		ans, err := f.prompt(label + " (required)")
		if err != nil {
			return "", err
		}
		if ans != "" {
			return ans, nil
		}
		fmt.Fprintln(f.opts.Stdout, "  a value is required; leaving it empty skips this candidate.")
	}
	return "", nil
}

func isYes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func ids(apps []config.App) []string {
	out := make([]string, len(apps))
	for i, a := range apps {
		out[i] = a.ID
	}
	return out
}

// merge folds the resolved apps into the raw document (learn owns
// source/deploy; every other existing field survives untouched) and renders
// it. Rendering uses the raw decode so defaults the user never wrote are
// not materialized into the file.
func (f *flow) merge(original []byte, existed bool, apps []config.App) ([]byte, error) {
	var cfg *config.Config
	if existed {
		var err error
		cfg, err = config.DecodeRaw(original)
		if err != nil {
			return nil, fmt.Errorf("decode existing config: %w", err)
		}
	} else {
		cfg = &config.Config{SchemaVersion: config.CurrentSchemaVersion}
	}
	for _, app := range apps {
		replaced := false
		for i := range cfg.Apps {
			if cfg.Apps[i].ID != app.ID {
				continue
			}
			keep := cfg.Apps[i]
			if keep.DisplayName != "" {
				app.DisplayName = keep.DisplayName
			}
			app.Enabled = keep.Enabled
			app.Interval = keep.Interval
			app.Timeout = keep.Timeout
			app.Retry = keep.Retry
			app.Steps = keep.Steps
			app.Health = keep.Health
			cfg.Apps[i] = app
			replaced = true
			break
		}
		if !replaced {
			cfg.Apps = append(cfg.Apps, app)
		}
	}
	return renderYAML(cfg)
}

// write performs backup, atomic temp write, and rename. The document was
// already validated before this is called; the original is untouched on any
// failure. The replacement preserves the original's permissions AND
// ownership: a root `yukariko learn` must not leave a config the systemd
// service user can no longer read.
func (f *flow) write(original []byte, existed bool, newBytes []byte) (backupPath string, err error) {
	path := f.opts.ConfigPath
	mode := fs.FileMode(0o644)
	uid, gid := -1, -1
	if existed {
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
			uid, gid = fileOwner(fi)
		}
		now := f.opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		backupPath = path + ".learn-backup-" + now.Format("20060102-150405")
		if err := os.WriteFile(backupPath, original, mode); err != nil {
			return "", fmt.Errorf("write backup: %w", err)
		}
		if err := preserveOwner(backupPath, uid, gid); err != nil {
			return "", fmt.Errorf("preserve backup owner: %w", err)
		}
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".learn-tmp-*")
	if err != nil {
		return backupPath, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName) // best effort; the original is still intact
		}
	}()
	if _, err = tmp.Write(newBytes); err != nil {
		tmp.Close()
		return backupPath, fmt.Errorf("write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return backupPath, fmt.Errorf("sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return backupPath, fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Chmod(tmpName, mode); err != nil {
		return backupPath, fmt.Errorf("chmod temp file: %w", err)
	}
	if err = preserveOwner(tmpName, uid, gid); err != nil {
		return backupPath, fmt.Errorf("preserve owner: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return backupPath, fmt.Errorf("replace config: %w", err)
	}
	return backupPath, nil
}

// cleanStaleTemps removes leftover temp files from an interrupted previous
// run. Best effort: failures are ignored.
func cleanStaleTemps(path string) {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".learn-tmp-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		os.Remove(m)
	}
}
