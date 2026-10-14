package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/integrate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
)

// clsBump is the class a fix-up for a package's failure gets.
var clsBump = map[string]string{"light": "standard", "standard": "deep", "deep": "deep"}

// fixupFile is fixups.json: the convergence rounds spent, the next fix-up
// number, and the fix-up packages (kept as stored, so a rewrite keeps
// them byte for byte).
type fixupFile struct {
	rounds, next int
	packages     []fixupPkg
}

type fixupPkg struct {
	pkg plan.Package
	raw any // the decoded object as stored or built
}

func (r *runner) fixupsPath() string { return filepath.Join(r.stateDir, "fixups.json") }

func loadFixups(path string) fixupFile {
	f := fixupFile{rounds: 0, next: 1}
	data, err := os.ReadFile(path)
	if err != nil {
		return f
	}
	doc, err := fsutil.DecodeJSON(data)
	obj, ok := doc.(map[string]any)
	if err != nil || !ok {
		return f
	}
	if n, ok := objGet(obj, "rounds").(int64); ok && n >= 0 {
		f.rounds = int(n)
	}
	if n, ok := objGet(obj, "next").(int64); ok && n >= 1 {
		f.next = int(n)
	}
	for _, e := range asList(objGet(obj, "packages")) {
		po, ok := e.(map[string]any)
		if !ok {
			continue
		}
		id, ok := objGet(po, "id").(string)
		if !ok {
			continue
		}
		pkg := plan.Package{ID: id, Spec: str(objGet(po, "spec")), Cls: str(objGet(po, "cls"))}
		for _, d := range asList(objGet(po, "depends_on")) {
			pkg.DependsOn = append(pkg.DependsOn, str(d))
		}
		for _, g := range asList(objGet(po, "files_owned")) {
			pkg.FilesOwned = append(pkg.FilesOwned, str(g))
		}
		switch p := objGet(po, "prio").(type) {
		case int64:
			pkg.Prio = float64(p)
		case float64:
			pkg.Prio = p
		}
		f.packages = append(f.packages, fixupPkg{pkg: pkg, raw: po})
	}
	return f
}

func (r *runner) saveFixups() {
	pkgs := make([]any, len(r.fixups.packages))
	for i, p := range r.fixups.packages {
		pkgs[i] = p.raw
	}
	b, err := fsutil.MarshalJSONIndent(map[string]any{
		"rounds":   r.fixups.rounds,
		"next":     r.fixups.next,
		"packages": pkgs,
	})
	if err == nil {
		_ = writeFileAtomic(r.fixupsPath(), append(b, '\n'))
	}
}

type failure struct{ name, excerpt string }

// finalChecks runs the full checks on the integration tree: every check
// item without `fast: true` over the files changed since the base, then
// --full. ok is false when nothing is configured or the environment failed
// (the run stopped).
func (r *runner) finalChecks() ([]failure, bool) {
	c := r.checksConfig()
	items := c.itemsWhere(false)
	full := r.opts.Full
	if len(items) == 0 && full == "" {
		return nil, false
	}
	fullLock.Lock()
	defer fullLock.Unlock()
	r.intLock.Lock()
	defer r.intLock.Unlock()
	r.emit("full-start", "", map[string]any{"mode": "final"})
	started := time.Now()
	if f := r.ensureFailure(r.intPath); f != nil {
		r.need("", "environment", "full checks: ensure failed\n"+f.excerpt)
		r.emit("environment", "", map[string]any{"sha": nullable(r.intHead()), "excerpt": f.excerpt})
		r.stop("environment: ensure failed before the full checks")
		return nil, false
	}
	var failures []failure
	if len(items) > 0 {
		var files []string
		if out, err := gitRun(r.intPath, "diff", "--name-only", r.base+"..HEAD"); err == nil {
			files = nonBlank(splitLines(out))
		}
		results := checks.Run(r.intPath, items, checks.Options{
			Files: files, SlotDir: c.slotDir, MaxParallel: c.maxParallel, RunAll: true,
			RetryFailedAlone: c.retryFailedAlone,
		}).Checks
		for _, res := range results {
			if res.Status == "fail" {
				failures = append(failures, failure{res.Name, res.Excerpt})
			}
		}
	}
	if full != "" {
		res := integrate.RunVerify(r.intPath, full, r.opts.FullTimeout)
		if res.Status != "pass" && !r.fullFlaky(res) {
			failures = append(failures, failure{"full", res.Excerpt})
		}
	}
	r.cleanIntegration("full")
	status := "pass"
	if len(failures) > 0 {
		status = "fail"
	}
	r.emit("full-end", "", map[string]any{"status": status, "mode": "final", "seconds": roundSeconds(time.Since(started))})
	lines := make([]string, len(failures))
	for i, f := range failures {
		lines[i] = fmt.Sprintf("%s: %s", f.name, firstLine(f.excerpt))
	}
	r.event("full", "", status+"\n"+strings.Join(lines, "\n"))
	return failures, true
}

type integratedEntry struct {
	pid   string
	files []string
}

// integratedFiles is (pid, files) for every integrate commit since the
// base, oldest first; a later `orchestra revert` drops the package's entry.
func (r *runner) integratedFiles() []integratedEntry {
	out, err := gitRun(r.intPath, "log", "--reverse", "--format=%x00%s", "--name-only", r.base+"..HEAD")
	if err != nil {
		return nil
	}
	byTask := r.taskMap()
	var entries []integratedEntry
	blocks := strings.Split(out, "\x00")
	for _, block := range blocks[1:] {
		lines := nonBlank(splitLines(block))
		if len(lines) == 0 {
			continue
		}
		subject, files := lines[0], lines[1:]
		if task, ok := strings.CutPrefix(subject, "orchestra integrate "); ok {
			if pid, ok := byTask[task]; ok {
				entries = append(entries, integratedEntry{pid, files})
			}
		} else if task, ok := strings.CutPrefix(subject, "orchestra revert "); ok {
			if pid, ok := byTask[task]; ok {
				kept := entries[:0]
				for _, e := range entries {
					if e.pid != pid {
						kept = append(kept, e)
					}
				}
				entries = kept
			}
		}
	}
	return entries
}

var (
	lineSuffix = regexp.MustCompile(`(:\d+)+:?$`)
	tokenSep   = regexp.MustCompile(`[\s"',()]+`)
)

// failurePaths is the repo-relative paths a failure names: its failing
// test files plus every path-like token, absolute ones inside the
// integration tree made relative. Order kept, duplicates dropped.
func (r *runner) failurePaths(excerpt string) []string {
	roots := []string{realPath(r.intPath) + "/", strings.TrimRight(r.intPath, "/") + "/"}
	var out []string
	add := func(p string) {
		p = strings.Trim(p, "`'\"()[]{}<>,;!?")
		p = strings.TrimRight(lineSuffix.ReplaceAllString(p, ""), ".:")
		if filepath.IsAbs(p) {
			found := false
			for _, root := range roots {
				if strings.HasPrefix(p, root) {
					p, found = p[len(root):], true
					break
				}
			}
			if !found {
				return
			}
		}
		p = strings.TrimPrefix(p, "./")
		if p == "" || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "-") || !pathLike(p) {
			return
		}
		if !containsStr(out, p) {
			out = append(out, p)
		}
	}
	for _, p := range checks.ParseFailedFiles(excerpt) {
		add(p)
	}
	for _, tok := range tokenSep.Split(excerpt, -1) {
		add(tok)
	}
	return out
}

// owner is the package a failing path belongs to: the latest integrated
// one whose diff changed it, else the latest one whose files_owned match.
func (r *runner) owner(path string, integrated []integratedEntry) string {
	for i := len(integrated) - 1; i >= 0; i-- {
		if containsStr(integrated[i].files, path) {
			return integrated[i].pid
		}
	}
	for i := len(integrated) - 1; i >= 0; i-- {
		for _, glob := range r.pkg(integrated[i].pid).FilesOwned {
			if checks.GlobMatch(glob, path) {
				return integrated[i].pid
			}
		}
	}
	return ""
}

type failureGroup struct {
	owner     string // "" = the catch-all
	failures  []failure
	diffFiles []string
}

// mapFailures assigns each failure to every package one of its paths
// belongs to; a failure no package matches goes to the catch-all (last).
func (r *runner) mapFailures(failures []failure) []failureGroup {
	integrated := r.integratedFiles()
	var groups []failureGroup
	index := map[string]int{}
	for _, f := range failures {
		var owners []string
		for _, p := range r.failurePaths(f.excerpt) {
			if o := r.owner(p, integrated); o != "" && !containsStr(owners, o) {
				owners = append(owners, o)
			}
		}
		if len(owners) == 0 {
			owners = []string{""}
		}
		for _, o := range owners {
			i, ok := index[o]
			if !ok {
				i = len(groups)
				index[o] = i
				groups = append(groups, failureGroup{owner: o})
			}
			groups[i].failures = append(groups[i].failures, f)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].owner != "" && groups[j].owner == "" })
	files := map[string][]string{}
	for _, e := range integrated {
		files[e.pid] = e.files
	}
	for i := range groups {
		groups[i].diffFiles = files[groups[i].owner]
	}
	return groups
}

// makeFixups turns red full checks into fix-<n> packages.
func (r *runner) makeFixups(failures []failure, round int) []string {
	specDir := filepath.Join(r.stateDir, "fixups")
	_ = os.MkdirAll(specDir, 0o755)
	var created []string
	for _, g := range r.mapFailures(failures) {
		number := r.fixups.next
		for r.hasPkg(fmt.Sprintf("fix-%d", number)) {
			number++
		}
		r.fixups.next = number + 1
		fid := fmt.Sprintf("fix-%d", number)
		spec := filepath.Join(specDir, fid+".md")
		lines := []string{fmt.Sprintf("# Fix-up %s (convergence round %d)", fid, round), "",
			"Every package is integrated, but these full checks are red on the " +
				"integration tree. Make them pass; stay inside files_owned; do not commit.", ""}
		for _, f := range g.failures {
			lines = append(lines, "## "+f.name, "", checks.FailureExcerpt(f.excerpt), "")
		}
		var cls string
		var owned []string
		if g.owner == "" {
			cls, owned = "deep", []string{"**"}
			lines = append(lines, "No package's changes match these failures.")
		} else {
			original := r.pkg(g.owner)
			cls = clsBump[original.Cls]
			if cls == "" {
				cls = "deep"
			}
			owned = append([]string{}, original.FilesOwned...)
			for _, f := range g.diffFiles {
				if !containsStr(owned, f) {
					owned = append(owned, f)
				}
			}
			lines = append(lines, "The failures point at package "+g.owner+".",
				"Its original spec: "+original.Spec, "Files it changed:")
			diff := g.diffFiles
			if len(diff) == 0 {
				diff = []string{"(none)"}
			}
			for _, f := range diff {
				lines = append(lines, "- "+f)
			}
		}
		_ = os.WriteFile(spec, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		pkg := plan.Package{ID: fid, Spec: spec, Cls: cls, DependsOn: []string{}, FilesOwned: owned}
		raw := map[string]any{
			"id":          fid,
			"spec":        spec,
			"cls":         cls,
			"depends_on":  []any{},
			"files_owned": owned,
			"prio":        0,
		}
		r.plan.Packages = append(r.plan.Packages, pkg)
		r.fixups.packages = append(r.fixups.packages, fixupPkg{pkg: pkg, raw: raw})
		created = append(created, fid)
		names := make([]any, len(g.failures))
		for i, f := range g.failures {
			names[i] = f.name
		}
		r.emit("fixup", fid, map[string]any{"round": round, "for": nullable(g.owner), "cls": cls, "checks": names})
	}
	r.fixups.rounds = round
	r.saveFixups()
	for _, fid := range created {
		_ = r.store.SetStatus(fid, "pending", fmt.Sprintf("convergence round %d", round), nil)
	}
	return created
}

// converge (verify_mode final): once every package is integrated (or held
// by a need), run the full checks, schedule fix-ups for red ones, repeat up
// to converge_rounds times, then hand the rest over as a need.
func (r *runner) converge() {
	for !r.stopped() && !r.isUnavailable() {
		failures, ok := r.finalChecks()
		if !ok {
			return
		}
		if len(failures) == 0 {
			r.fullGreen()
			return
		}
		if r.fixups.rounds >= r.convergeRounds {
			parts := make([]string, len(failures))
			for i, f := range failures {
				parts[i] = fmt.Sprintf("## %s\n%s", f.name, checks.FailureExcerpt(f.excerpt))
			}
			r.need("", "post-integration", fmt.Sprintf("full checks still red after %d "+
				"convergence round(s):\n%s", r.fixups.rounds, strings.Join(parts, "\n\n")))
			r.stop(fmt.Sprintf("full checks red after %d convergence round(s)", r.fixups.rounds))
			return
		}
		r.makeFixups(failures, r.fixups.rounds+1)
		r.runWaves()
	}
}
