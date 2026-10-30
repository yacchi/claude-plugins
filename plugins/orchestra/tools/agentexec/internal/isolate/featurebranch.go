package isolate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// featureSlugRun matches the runs a slug replaces with one '-'.
var featureSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

// featureMaxSuffix is the last -N suffix OnFeatureBranch tries before it
// gives up and leaves the name to the user.
const featureMaxSuffix = 20

// FeatureSlug is the first of names whose slug has at least 3 characters:
// lowercased, every run of characters outside [a-z0-9] replaced by one '-',
// '-' trimmed from both ends, cut to 40 bytes and trimmed of '-' again.
// "run" when no name gives one.
func FeatureSlug(names ...string) string {
	for _, name := range names {
		s := featureSlugRun.ReplaceAllString(strings.ToLower(name), "-")
		s = strings.Trim(s, "-")
		if len(s) > 40 {
			s = strings.Trim(s[:40], "-")
		}
		if len(s) >= 3 {
			return s
		}
	}
	return "run"
}

// IsDefaultBranch reports whether branch is the repository's default branch at
// root: "main", "master", the target of refs/remotes/origin/HEAD, or the
// init.defaultBranch setting. "" is never the default branch.
func IsDefaultBranch(root, branch string) bool {
	if branch == "" {
		return false
	}
	if branch == "main" || branch == "master" {
		return true
	}
	if out, err := gitx.Run(root, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil &&
		strings.TrimSpace(out) == "origin/"+branch {
		return true
	}
	out, err := gitx.Run(root, "config", "--get", "init.defaultBranch")
	return err == nil && strings.TrimSpace(out) == branch
}

// OnFeatureBranch runs land with the checkout at root off the default branch.
// On any other branch (or a detached HEAD, which land refuses) it just runs
// land. On the default branch it first creates feature/<slug> - or the first
// free of feature/<slug>-2 ... feature/<slug>-20 - at HEAD and points HEAD at
// it, then runs land. When land does not report "landed", a branch it created
// that still points at the commit it was created on is deleted and HEAD goes
// back to the default branch, so the checkout is left as it was found.
// created reports whether the checkout is left on a branch this call created.
func OnFeatureBranch(root, slug string, land func() (LandResult, error)) (res LandResult, created bool, err error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		res, err = land()
		return res, false, err
	}
	out, symErr := gitx.Run(resolved, "symbolic-ref", "--short", "-q", "HEAD")
	current := strings.TrimSpace(out)
	if symErr != nil || current == "" || !IsDefaultBranch(resolved, current) {
		res, err = land()
		return res, false, err
	}
	if slug == "" {
		slug = "run"
	}
	name := freeFeatureBranch(resolved, slug)
	if name == "" {
		return LandResult{Status: "refused", Branch: current,
			Reason: fmt.Sprintf("feature/%s through feature/%s-%d are all taken; land by hand with --feature <another name>", slug, slug, featureMaxSuffix)}, false, nil
	}
	head, revErr := landRevParse(resolved, "HEAD")
	if revErr != nil {
		return LandResult{Status: "error", Branch: current, Reason: fmt.Sprintf("cannot create %s: %v", name, revErr)}, false, nil
	}
	// branch + symbolic-ref instead of switch/checkout: HEAD moves between two
	// refs at the same commit, so the working tree and index stay untouched
	// and no post-checkout hook runs in the user's checkout.
	if _, e := gitx.Run(resolved, "branch", "--no-track", name, head); e != nil {
		return LandResult{Status: "error", Branch: current, Reason: fmt.Sprintf("cannot create %s: %v", name, e)}, false, nil
	}
	if _, e := gitx.Run(resolved, "symbolic-ref", "HEAD", "refs/heads/"+name); e != nil {
		_, _ = gitx.Run(resolved, "symbolic-ref", "HEAD", "refs/heads/"+current)
		_, _ = gitx.Run(resolved, "branch", "-D", name)
		return LandResult{Status: "error", Branch: current, Reason: fmt.Sprintf("cannot create %s: %v", name, e)}, false, nil
	}

	res, err = land()
	if err == nil && res.Status == "landed" {
		return res, true, nil
	}
	// A branch that moved holds a commit a land wrote before failing later;
	// deleting it would lose that commit, so it stays checked out.
	if tip, e := landRevParse(resolved, "refs/heads/"+name); e != nil || tip != head {
		return res, true, err
	}
	if _, e := gitx.Run(resolved, "symbolic-ref", "HEAD", "refs/heads/"+current); e != nil {
		res.Reason += fmt.Sprintf("; could not return to %s: %v", current, e)
		return res, true, err
	}
	if _, e := gitx.Run(resolved, "branch", "-D", name); e != nil {
		res.Reason += fmt.Sprintf("; could not return to %s: %v", current, e)
		return res, true, err
	}
	return res, false, err
}

// freeFeatureBranch is the first of feature/<slug>, feature/<slug>-2 ...
// feature/<slug>-20 that is neither a local branch nor a remote-tracking
// branch name, or "" when all are taken.
func freeFeatureBranch(resolved, slug string) string {
	remote := map[string]bool{}
	if out, err := gitx.Run(resolved, "for-each-ref", "--format=%(refname:lstrip=3)", "refs/remotes"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				remote[line] = true
			}
		}
	}
	for i := 1; i <= featureMaxSuffix; i++ {
		name := "feature/" + slug
		if i > 1 {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		if remote[name] {
			continue
		}
		if _, err := gitx.Run(resolved, "show-ref", "--verify", "-q", "refs/heads/"+name); err == nil {
			continue
		}
		return name
	}
	return ""
}
