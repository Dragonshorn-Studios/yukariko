package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	appIDPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	namePattern       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	keyIDPattern      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`)
	tagPattern        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,127}$`)
	sha256Pattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	platformPattern   = regexp.MustCompile(`^[A-Za-z0-9]+(?:/[A-Za-z0-9]+){0,2}$`)
	bindModePattern   = regexp.MustCompile(`^(ro|rw|z|Z)(?:,(ro|rw|z|Z))*$`)
)

// Validate checks the fully-defaulted configuration and returns all findings.
func (c *Config) Validate() error {
	v := &validator{}

	if !validBind(c.Server.Bind) {
		v.errorf("server.bind", "must be host:port with a port between 1 and 65535, got %q", c.Server.Bind)
	}
	if c.Retention.EventsDays < 1 || c.Retention.HealthDays < 1 || c.Retention.DeploymentsDays < 1 {
		v.errorf("retention", "events_days, health_days, and deployments_days must each be at least 1")
	}
	if c.Limits.CommandOutputBytes < 1024 {
		v.errorf("limits.command_output_bytes", "must be at least 1024 bytes")
	}
	validateReporting(v, &c.Reporting)

	seenIDs := make(map[string]bool, len(c.Apps))
	seenTargets := make(map[string]string)
	for i := range c.Apps {
		a := &c.Apps[i]
		path := fmt.Sprintf("apps[%d]", i)
		if a.ID == "" {
			v.errorf(path+".id", "is required")
		} else if !appIDPattern.MatchString(a.ID) {
			v.errorf(path+".id", "must match %q (lowercase slug)", appIDPattern.String())
		} else if seenIDs[a.ID] {
			v.errorf(path+".id", "duplicate app id %q; app ids must be unique", a.ID)
		} else {
			seenIDs[a.ID] = true
		}
		if a.Retry.Max < a.Retry.Base {
			v.errorf(path+".retry.max", "must be greater than or equal to retry.base")
		}
		validateSource(v, path, a)
		validateDeploy(v, path, a)
		validateSteps(v, path, a)
		if a.Health != nil {
			validateHealth(v, path+".health", a.Health)
		}

		if target, ok := deployTargetKey(a); ok {
			if prev, dup := seenTargets[target]; dup {
				v.errorf(path, "deploys the same target as %s; two apps must never share a deploy target or per-app locks cannot prevent overlap", prev)
			} else {
				seenTargets[target] = path
			}
		}
	}
	return v.err()
}

func validateSource(v *validator, path string, a *App) {
	sp := path + ".source"
	switch a.Source.Mode {
	case SourceGit:
		if a.Source.Registry != nil {
			v.errorf(sp+".registry", "must not be set when source.mode is %q", SourceGit)
		}
		g := a.Source.Git
		if g == nil {
			v.errorf(sp+".git", "is required when source.mode is %q", SourceGit)
			return
		}
		if !isAbsPath(g.Dir) {
			v.errorf(sp+".git.dir", "must be an absolute path to the existing worktree, got %q", g.Dir)
		}
		if g.Branch == "" {
			v.errorf(sp+".git.branch", "is required when source.mode is %q", SourceGit)
		} else if !validGitRefName(g.Branch) {
			v.errorf(sp+".git.branch", "is not a valid branch name: %q", g.Branch)
		}
		if g.Remote != "" && strings.ContainsAny(g.Remote, " \t") {
			v.errorf(sp+".git.remote", "must be a remote name without whitespace, got %q", g.Remote)
		}
	case SourceRegistry:
		if a.Source.Git != nil {
			v.errorf(sp+".git", "must not be set when source.mode is %q", SourceRegistry)
		}
		r := a.Source.Registry
		if r == nil {
			v.errorf(sp+".registry", "is required when source.mode is %q", SourceRegistry)
			return
		}
		if len(r.Images) == 0 {
			v.errorf(sp+".registry.images", "requires at least one image")
			return
		}
		for j, img := range r.Images {
			ip := fmt.Sprintf("%s.registry.images[%d]", sp, j)
			if err := validImageRef(img.Ref); err != nil {
				v.errorf(ip+".ref", "%s", err.Error())
			}
			if img.Platform != "" && !platformPattern.MatchString(img.Platform) {
				v.errorf(ip+".platform", "must look like \"linux/arm64\", got %q", img.Platform)
			}
		}
	default:
		v.errorf(sp+".mode", "must be %q or %q, got %q", SourceGit, SourceRegistry, a.Source.Mode)
	}
}

func validateDeploy(v *validator, path string, a *App) {
	dp := path + ".deploy"
	switch a.Deploy.Mode {
	case DeployCompose:
		if a.Deploy.Standalone != nil {
			v.errorf(dp+".standalone", "must not be set when deploy.mode is %q", DeployCompose)
		}
		c := a.Deploy.Compose
		if c == nil {
			v.errorf(dp+".compose", "is required when deploy.mode is %q", DeployCompose)
			return
		}
		if !isAbsPath(c.WorkDir) {
			v.errorf(dp+".compose.work_dir", "must be an absolute path; Yukariko never guesses the Compose working directory, got %q", c.WorkDir)
		}
		if len(c.Files) == 0 {
			v.errorf(dp+".compose.files", "requires at least one Compose file; Yukariko never guesses file paths")
		}
		for j, f := range c.Files {
			if f == "" {
				v.errorf(fmt.Sprintf("%s.compose.files[%d]", dp, j), "must not be empty")
			}
		}
		for j, f := range c.EnvFiles {
			if f == "" {
				v.errorf(fmt.Sprintf("%s.compose.env_files[%d]", dp, j), "must not be empty")
			}
		}
		for j, p := range c.Profiles {
			if p == "" {
				v.errorf(fmt.Sprintf("%s.compose.profiles[%d]", dp, j), "must not be empty")
			}
		}
	case DeployStandalone:
		if a.Deploy.Compose != nil {
			v.errorf(dp+".compose", "must not be set when deploy.mode is %q", DeployStandalone)
		}
		if a.Source.Mode != SourceRegistry {
			v.errorf(dp+".mode", "standalone recreation requires source.mode %q; building standalone containers from Git is not supported", SourceRegistry)
		}
		s := a.Deploy.Standalone
		if s == nil {
			v.errorf(dp+".standalone", "is required when deploy.mode is %q", DeployStandalone)
			return
		}
		validateStandalone(v, dp+".standalone", s)
	default:
		v.errorf(dp+".mode", "must be %q or %q, got %q", DeployCompose, DeployStandalone, a.Deploy.Mode)
	}
}

func validateStandalone(v *validator, sp string, s *StandaloneSpec) {
	if s.Image == "" {
		v.errorf(sp+".image", "is required")
	} else if err := validImageRef(s.Image); err != nil {
		v.errorf(sp+".image", "%s", err.Error())
	}
	if s.Name == "" {
		v.errorf(sp+".name", "is required")
	} else if !namePattern.MatchString(s.Name) {
		v.errorf(sp+".name", "must be a valid container name, got %q", s.Name)
	}
	switch s.Restart {
	case "", "no", "always", "unless-stopped", "on-failure":
	default:
		v.errorf(sp+".restart", "must be one of no, always, unless-stopped, on-failure, got %q", s.Restart)
	}
	for j, e := range s.Env {
		ep := fmt.Sprintf("%s.env[%d]", sp, j)
		if e.Name == "" || !envNamePattern.MatchString(e.Name) {
			v.errorf(ep+".name", "must be a valid environment variable name, got %q", e.Name)
		}
		validateValueOrRef(v, ep, e.Value, e.SecretRef)
	}
	for j, b := range s.Binds {
		if err := validBindSpec(b); err != nil {
			v.errorf(fmt.Sprintf("%s.binds[%d]", sp, j), "%s", err.Error())
		}
	}
	for j, p := range s.Ports {
		if err := validPortSpec(p); err != nil {
			v.errorf(fmt.Sprintf("%s.ports[%d]", sp, j), "%s", err.Error())
		}
	}
	for j, n := range s.Networks {
		if n == "" {
			v.errorf(fmt.Sprintf("%s.networks[%d]", sp, j), "must not be empty")
		}
	}
	for k := range s.Labels {
		if k == "" {
			v.errorf(sp+".labels", "must not contain empty label names")
		}
	}
	if s.HealthCheck != nil && len(s.HealthCheck.Test) == 0 {
		v.errorf(sp+".health_check.test", "is required when health_check is set")
	}
}

func validateSteps(v *validator, path string, a *App) {
	validateStepList(v, path+".steps.pre", a.Steps.Pre)
	validateStepList(v, path+".steps.deploy", a.Steps.Deploy)
	validateStepList(v, path+".steps.post", a.Steps.Post)
}

func validateStepList(v *validator, path string, steps []Step) {
	seen := make(map[string]bool, len(steps))
	for i := range steps {
		s := &steps[i]
		p := fmt.Sprintf("%s[%d]", path, i)
		if s.Name == "" {
			v.errorf(p+".name", "is required")
		} else if seen[s.Name] {
			v.errorf(p+".name", "duplicate step name %q within the same list", s.Name)
		} else {
			seen[s.Name] = true
		}
		if len(s.Command) == 0 || s.Command[0] == "" {
			v.errorf(p+".command", "must be a non-empty argv list; the first element is the executable")
		}
		if s.Dir != "" && !isAbsPath(s.Dir) {
			v.errorf(p+".dir", "must be an absolute path, got %q", s.Dir)
		}
	}
}

func validateHealth(v *validator, hp string, h *Health) {
	if h.HTTP != nil {
		pp := hp + ".http"
		u, err := url.Parse(h.HTTP.URL)
		if err != nil || u.Host == "" {
			v.errorf(pp+".url", "must be an absolute http or https URL, got %q", h.HTTP.URL)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			v.errorf(pp+".url", "scheme must be http or https, got %q", u.Scheme)
		}
		if len(h.HTTP.Status) != 2 {
			v.errorf(pp+".status", "must be a [min, max] inclusive pair, got %v", h.HTTP.Status)
		} else if h.HTTP.Status[0] < 100 || h.HTTP.Status[1] > 599 || h.HTTP.Status[0] > h.HTTP.Status[1] {
			v.errorf(pp+".status", "must satisfy 100 <= min <= max <= 599, got %v", h.HTTP.Status)
		}
		for j, hd := range h.HTTP.Headers {
			hdp := fmt.Sprintf("%s.headers[%d]", pp, j)
			if !headerNamePattern.MatchString(hd.Name) {
				v.errorf(hdp+".name", "must be a valid HTTP header name, got %q", hd.Name)
			}
			validateValueOrRef(v, hdp, hd.Value, hd.SecretRef)
		}
	}
	if len(h.Command) > 0 && h.Command[0] == "" {
		v.errorf(hp+".command", "must be a non-empty argv list")
	}
}

func validateReporting(v *validator, r *Reporting) {
	o := &r.Outbound
	if o.Enabled {
		op := "reporting.outbound"
		u, err := url.Parse(o.URL)
		if err != nil || u.Host == "" {
			v.errorf(op+".url", "must be an absolute URL when outbound reporting is enabled, got %q", o.URL)
		} else if u.Scheme != "https" && !isLoopbackHost(u) {
			v.errorf(op+".url", "must use https (plain http is allowed only on loopback hosts for tests), got %q", u.Scheme)
		}
		if o.HostID == "" {
			v.errorf(op+".host_id", "is required when outbound reporting is enabled")
		} else if !appIDPattern.MatchString(o.HostID) {
			v.errorf(op+".host_id", "must match %q", appIDPattern.String())
		}
		if o.SecretRef == nil {
			v.errorf(op+".secret_ref", "is required when outbound reporting is enabled; secrets are references only and literal values are rejected")
		} else {
			validateSecretRef(v, op+".secret_ref", o.SecretRef)
		}
	}
	i := &r.Inbound
	if i.Enabled {
		ip := "reporting.inbound"
		if len(i.Hosts) == 0 {
			v.errorf(ip+".hosts", "requires at least one allowlisted host when inbound reporting is enabled")
		}
		seenHosts := make(map[string]bool, len(i.Hosts))
		for j, h := range i.Hosts {
			hp := fmt.Sprintf("%s.hosts[%d]", ip, j)
			if !appIDPattern.MatchString(h.ID) {
				v.errorf(hp+".id", "must match %q", appIDPattern.String())
			} else if seenHosts[h.ID] {
				v.errorf(hp+".id", "duplicate host id %q", h.ID)
			} else {
				seenHosts[h.ID] = true
			}
			if len(h.Keys) == 0 {
				v.errorf(hp+".keys", "requires at least one key; removing the host revokes it")
			}
			seenKeys := make(map[string]bool, len(h.Keys))
			for k, key := range h.Keys {
				kp := fmt.Sprintf("%s.keys[%d]", hp, k)
				if !keyIDPattern.MatchString(key.KeyID) {
					v.errorf(kp+".key_id", "must match %q", keyIDPattern.String())
				} else if seenKeys[key.KeyID] {
					v.errorf(kp+".key_id", "duplicate key id %q for host %q", key.KeyID, h.ID)
				} else {
					seenKeys[key.KeyID] = true
				}
				if key.SecretRef == nil {
					v.errorf(kp+".secret_ref", "is required; secrets are references only and literal values are rejected")
				} else {
					validateSecretRef(v, kp+".secret_ref", key.SecretRef)
				}
			}
		}
		if i.MaxBodyBytes < 1024 || i.MaxBodyBytes > 10<<20 {
			v.errorf(ip+".max_body_bytes", "must be between 1024 and 10485760 bytes")
		}
		if i.RateLimit.Events < 1 {
			v.errorf(ip+".rate_limit.events", "must be at least 1")
		}
	}
}

func validateSecretRef(v *validator, path string, s *SecretRef) {
	switch {
	case s.Env != "" && s.File != "":
		v.errorf(path, "must set exactly one of env or file, both are set")
	case s.Env == "" && s.File == "":
		v.errorf(path, "must set exactly one of env or file, neither is set")
	case s.Env != "" && !envNamePattern.MatchString(s.Env):
		v.errorf(path+".env", "must be a valid environment variable name, got %q", s.Env)
	case s.File != "" && !isAbsPath(s.File):
		v.errorf(path+".file", "must be an absolute path, got %q", s.File)
	}
}

// validateValueOrRef enforces exactly one of a plain non-secret value or a
// secret reference.
func validateValueOrRef(v *validator, path, value string, ref *SecretRef) {
	hasValue := value != ""
	hasRef := ref != nil && (ref.Env != "" || ref.File != "")
	switch {
	case hasValue && hasRef:
		v.errorf(path, "must set either value or secret_ref, not both; literal secret values are never allowed")
	case !hasValue && !hasRef:
		v.errorf(path, "must set either value (non-secret, safe to log) or secret_ref")
	case hasRef:
		validateSecretRef(v, path+".secret_ref", ref)
	}
}

// deployTargetKey builds the identity two apps must never share, so per-app
// locks always cover a whole deploy target.
func deployTargetKey(a *App) (string, bool) {
	switch a.Deploy.Mode {
	case DeployCompose:
		if a.Deploy.Compose == nil {
			return "", false
		}
		c := a.Deploy.Compose
		return fmt.Sprintf("compose:%s|%s|%s", c.WorkDir, c.ProjectName, strings.Join(c.Files, ",")), true
	case DeployStandalone:
		if a.Deploy.Standalone == nil {
			return "", false
		}
		return "standalone:" + a.Deploy.Standalone.Name, true
	default:
		return "", false
	}
}

// isAbsPath accepts POSIX-style absolute paths on every platform so configs
// written for Linux hosts validate identically during Windows development.
func isAbsPath(p string) bool {
	return strings.HasPrefix(p, "/") || filepath.IsAbs(p)
}

func validGitRefName(name string) bool {
	if name == "" || strings.ContainsAny(name, " \t\n\r") || strings.Contains(name, "..") ||
		strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".lock") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return false
	}
	return true
}

func validImageRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(ref, " \t\n\r") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, ":") {
		return fmt.Errorf("not a valid image reference: %q", ref)
	}
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		digest := ref[at+1:]
		if !sha256Pattern.MatchString(digest) {
			return fmt.Errorf("digest references must be sha256:<64 hex digits>, got %q", digest)
		}
		if at == 0 {
			return fmt.Errorf("not a valid image reference: %q", ref)
		}
		return nil
	}
	if c := strings.LastIndex(ref, ":"); c > strings.LastIndex(ref, "/") {
		tag := ref[c+1:]
		if !tagPattern.MatchString(tag) {
			return fmt.Errorf("not a valid tag: %q", tag)
		}
	}
	return nil
}

func validBindSpec(b string) error {
	parts := strings.Split(b, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("must be source:destination[:mode], got %q", b)
	}
	for _, p := range parts[:2] {
		if p == "" {
			return fmt.Errorf("source and destination must not be empty, got %q", b)
		}
	}
	if len(parts) == 3 && !bindModePattern.MatchString(parts[2]) {
		return fmt.Errorf("mode must be ro, rw, z, or Z (comma-separated), got %q", parts[2])
	}
	return nil
}

func validPortSpec(p string) error {
	spec := p
	proto := ""
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		proto = spec[i+1:]
		spec = spec[:i]
		if proto != "tcp" && proto != "udp" {
			return fmt.Errorf("protocol must be tcp or udp, got %q", proto)
		}
	}
	parts := strings.Split(spec, ":")
	var host, container string
	switch len(parts) {
	case 2:
		host, container = parts[0], parts[1]
	case 3:
		if parts[0] == "" {
			return fmt.Errorf("host IP must not be empty, got %q", p)
		}
		host, container = parts[1], parts[2]
	default:
		return fmt.Errorf("must be [host-ip:]host-port:container-port[/proto], got %q", p)
	}
	if err := validPortNumber(container); err != nil {
		return fmt.Errorf("container port: %s", err.Error())
	}
	if err := validPortNumber(host); err != nil {
		return fmt.Errorf("host port: %s", err.Error())
	}
	return nil
}

func validPortNumber(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("%q is not a number", s)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("%d is outside 1-65535", n)
	}
	return nil
}

// isLoopbackHost reports whether a URL targets localhost; plain http is
// allowed there for tests only.
func isLoopbackHost(u *url.URL) bool {
	h := u.Hostname()
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}
