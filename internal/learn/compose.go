package learn

import (
	"context"
	"fmt"
	"strings"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/docker"
)

// composeCandidate classifies one discovered Compose project into a single
// proposal. Compose is preferred: the project deploys through its own files
// and working directory, and the containers are never reconstructed
// manually. The only source detection is a read-only Git probe at the
// declared working directory.
func composeCandidate(ctx context.Context, p *docker.ComposeProject, git GitProber) candidate {
	pr := &Proposal{
		Mode:            config.DeployCompose,
		SystemCandidate: p.SystemCandidate,
		SystemReasons:   copyStrings(p.SystemReasons),
		Compose: &ComposeProposal{
			ProjectName: p.Name,
			WorkDir:     p.WorkDir,
			ConfigFiles: copyStrings(p.ConfigFiles),
			EnvFiles:    envFilesFromLabels(p),
		},
	}

	// One line of image evidence per service; replicas share the service
	// name, and a replica running a different image is a conflict to
	// resolve, not a guess to make.
	perService := map[string]map[string]bool{}
	for _, c := range p.Containers {
		if c.Detail == nil || c.Service == "" || c.Detail.ImageRef == "" {
			continue
		}
		set := perService[c.Service]
		if set == nil {
			set = map[string]bool{}
			perService[c.Service] = set
		}
		set[c.Detail.ImageRef] = true
	}
	services := sortedSetKeys(perService)
	for _, svc := range services {
		images := sortedSetKeys(perService[svc])
		if len(images) == 0 {
			pr.confirm("deploy.services", fmt.Sprintf("service %s has no image reference", svc))
			continue
		}
		pr.Compose.Services = append(pr.Compose.Services, ServiceImage{Service: svc, Image: images[0]})
		if len(images) > 1 {
			pr.confirm("deploy.services",
				fmt.Sprintf("service %s reports multiple images: %s", svc, strings.Join(images, ", ")))
		}
	}

	// Source detection: Git worktree at the declared working directory,
	// otherwise registry tracking of the observed images (which the user
	// must confirm, because a compose file may build locally).
	switch {
	case p.WorkDir == "":
		pr.confirm("source.mode", "compose working directory unknown; source mode cannot be detected")
		pr.confirm("deploy.compose.work_dir", "working directory missing or conflicting across the project's containers")
	case git == nil:
		pr.confirm("source.mode", "git detection unavailable; cannot check "+p.WorkDir+" for a worktree")
	default:
		info, err := git.Probe(ctx, p.WorkDir)
		switch {
		case err != nil:
			pr.confirm("source.mode", fmt.Sprintf("git detection failed for %s: %v", p.WorkDir, err))
		case !info.InWorkTree:
			pr.Compose.SourceMode = config.SourceRegistry
			for _, svc := range services {
				if images := sortedSetKeys(perService[svc]); len(images) > 0 {
					pr.Compose.RegistryImages = append(pr.Compose.RegistryImages, images[0])
				}
			}
			pr.confirm("source.mode",
				fmt.Sprintf("no Git worktree at %s; proposing registry tracking of the observed images — "+
					"confirm they are pulled from a registry and not built locally", p.WorkDir))
		default:
			pr.Compose.SourceMode = config.SourceGit
			pr.Compose.Git = &GitEvidence{
				Dir:           p.WorkDir,
				Branch:        info.Branch,
				Remote:        info.RemoteName,
				RemoteURL:     info.RemoteURL,
				CredsStripped: info.CredsStripped,
				Detached:      info.Detached,
			}
			if info.Detached || info.Branch == "" {
				pr.confirm("source.git.branch", "worktree is in detached HEAD state; the deployable branch is unknown")
			}
			if info.RemoteName == "" {
				pr.confirm("source.git.remote", "no remote named 'origin' in the worktree; git change detection requires one")
			}
		}
	}
	if len(pr.Compose.ConfigFiles) == 0 {
		pr.confirm("deploy.compose.files", "config files missing or conflicting across the project's containers")
	}

	pr.ID = slug(p.Name, appIDMaxLen)
	pr.finalize()
	return candidate{proposal: pr, base: pr.ID, key: "compose/" + p.Name}
}

// envFilesFromLabels proposes env files only when the (Compose v1)
// environment_file label agrees across all members. Absence or conflict
// yields no claim: the field is optional and never guessed.
func envFilesFromLabels(p *docker.ComposeProject) []string {
	values := map[string]bool{}
	for _, c := range p.Containers {
		if c.Detail == nil {
			continue
		}
		if v := c.Detail.Labels[docker.LabelEnvironmentFiles]; v != "" {
			values[v] = true
		}
	}
	if len(values) != 1 {
		return nil
	}
	for v := range values {
		return splitComposeList(v)
	}
	return nil
}

// splitComposeList splits compose's comma-joined label values, preserving
// order and dropping empties.
func splitComposeList(joined string) []string {
	var out []string
	for _, f := range strings.Split(joined, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
