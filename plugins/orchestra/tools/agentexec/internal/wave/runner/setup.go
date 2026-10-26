package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

const (
	defaultVerifyMode     = "final"
	defaultConvergeRounds = 2
	// unboundedWidth is the Select limit of `max_in_flight: auto`: every
	// package that may start does.
	unboundedWidth = 1 << 30
)

// fullLock: at most one full-check run in this process at a time, across
// runners.
var fullLock sync.Mutex

// runner is one `wave run`. The scheduler (loop, schedule, integrate,
// full, converge) runs on the calling goroutine; packages run on worker
// goroutines and share the fields guarded by mu.
type runner struct {
	ctx    context.Context
	opts   Options
	deps   Deps
	exec   Executor
	clock  func() float64
	sleep  func(float64)
	stderr io.Writer

	statePath, stateDir string
	store               *state.Store

	plan      *plan.Plan
	root      string
	into      string
	intPath   string
	base      string
	intBranch string
	greenPath string
	fixups    fixupFile

	host     host.Host
	hostName string
	herdr    host.Host // candidate, consumed by selectHost

	verifyMode     string
	convergeRounds int
	// maxInFlight is the width; 0 is `auto`, every startable package.
	maxInFlight int
	memoryGuard bool
	usageEvery  int
	budgetUSD   float64

	// Scheduler-goroutine state: whether new starts are held for memory
	// pressure, and the packages integrated so far (usage notices).
	holding         bool
	integratedTotal int

	// costMu guards the cached run-cost estimate.
	costMu           sync.Mutex
	costSeen, costOK bool
	costAt, costUSD  float64

	// intLock is held while the integration tree is written or checked
	// (integrate, full, convergence) and while a worker starts a dispatch
	// from it.
	intLock sync.Mutex

	mu                  sync.Mutex
	exhausted           map[string]bool
	stopReason          *string
	unavailable         bool
	notes               map[string]string
	widenNotes          map[string]string
	refreshInfo         map[string]refreshState
	answers             map[string]string
	started             int
	waves               int
	integratedSinceFull int
	integrateRounds     int

	notifyWG sync.WaitGroup
}

type refreshState struct {
	fresh, carried bool
	patchFile      string
}

func wallClock() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func newRunner(ctx context.Context, o Options, d Deps) *runner {
	if ctx == nil {
		ctx = context.Background()
	}
	if o.Repo == "" {
		o.Repo, _ = os.Getwd()
	}
	r := &runner{
		ctx: ctx, opts: o, deps: d, exec: d.Executor, clock: d.Clock, sleep: d.Sleep, stderr: d.Stderr,
		exhausted: map[string]bool{}, notes: map[string]string{}, widenNotes: map[string]string{},
		refreshInfo: map[string]refreshState{}, answers: map[string]string{},
		verifyMode: "gated", convergeRounds: defaultConvergeRounds,
	}
	if r.clock == nil {
		r.clock = wallClock
	}
	if r.sleep == nil {
		r.sleep = func(s float64) { time.Sleep(time.Duration(s * float64(time.Second))) }
	}
	if r.stderr == nil {
		r.stderr = os.Stderr
	}
	if len(r.deps.AgentExec) == 0 {
		r.deps.AgentExec = selfArgv()
	}
	if r.exec == nil {
		r.exec = &CliExecutor{Argv: r.deps.AgentExec}
	}
	if d.Host != nil {
		r.host = d.Host
	} else {
		r.host = d.Orca
		r.herdr = d.Herdr
	}
	r.hostName = "orca"
	if r.host != nil {
		r.hostName = r.host.Name()
	}
	r.statePath = absPath(o.State)
	r.stateDir = filepath.Dir(r.statePath)
	r.store = &state.Store{Path: r.statePath, Clock: r.clock}
	r.greenPath = filepath.Join(r.stateDir, "green-tree")
	return r
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// config is the current config (nil when it cannot be resolved).
func (r *runner) config() *config.Resolved {
	load := r.deps.Config
	if load == nil {
		load = config.Resolve
	}
	cfg, err := load()
	if err != nil {
		return nil
	}
	return cfg
}

func (r *runner) task(pid string) string { return "pkg-" + pid }

func (r *runner) pkg(pid string) plan.Package {
	p, _ := r.plan.Package(pid)
	return p
}

func (r *runner) hasPkg(pid string) bool {
	_, ok := r.plan.Package(pid)
	return ok
}

// --- init ----------------------------------------------------------------------

func (r *runner) init() error {
	p, err := plan.Load(r.opts.Plan)
	if err != nil {
		return err
	}
	r.plan = p
	r.applyOverrides()
	root, ok := gitRepoRoot(r.opts.Repo)
	if !ok {
		return fmt.Errorf("not a git repository: %s", r.opts.Repo)
	}
	r.root = root
	warnings := plan.Lint(r.plan, r.root)
	r.fixups = loadFixups(r.fixupsPath())
	for _, fp := range r.fixups.packages {
		if !r.hasPkg(fp.pkg.ID) {
			r.plan.Packages = append(r.plan.Packages, fp.pkg)
		}
	}
	into, err := isolate.SanitizeTaskID(r.opts.Into)
	if err != nil {
		return err
	}
	r.into = into
	ids := make([]string, len(r.plan.Packages))
	for i, pkg := range r.plan.Packages {
		ids[i] = pkg.ID
	}
	if _, err := r.store.Init(absPath(r.opts.Plan), ids, r.into); err != nil {
		return err
	}
	for _, w := range warnings {
		pkg := ""
		if w.Pkg != nil {
			pkg = *w.Pkg
		}
		r.emit("plan-warning", pkg, map[string]any{"message": w.Message})
		fmt.Fprintf(r.stderr, "plan-warning: %s: %s\n", derefOr(w.Pkg, "-"), w.Message)
	}

	// Integration worktree: the same create path integrate takes, so a
	// later `isolate integrate --into` reuses it as is.
	head := r.revParse(r.root, "HEAD")
	if head == "" {
		return fmt.Errorf("could not resolve HEAD in %s", r.root)
	}
	// A start commit that tracks excluded dependency stores would check
	// them out into every package worktree. A scan error fails open: the
	// Create below reports real git trouble itself.
	if dirs, err := isolate.TrackedExcluded(r.root, head, isolate.CurrentFilter()); err == nil && len(dirs) > 0 {
		return errors.New(isolate.TrackedRefusal(head, dirs))
	}
	created, err := isolate.Create(r.root, r.into, isolate.CreateOptions{Backend: isolate.BackendGit, Onto: head})
	if err != nil || created.Status == "error" || created.Path == "" || !isDir(created.Path) {
		reason := created.Reason
		if err != nil {
			reason = err.Error()
		}
		return fmt.Errorf("could not create the integration worktree %s: %s", r.into, orUnknown(reason))
	}
	_ = isolate.WriteRole(created.Path, isolate.RoleIntegration)
	r.intPath = created.Path
	r.base = head
	if b, ok := isolate.ReadBaseline(created.Path); ok && b != "" {
		r.base = b
	}
	task, path, base := r.into, r.intPath, r.base
	if err := r.store.SetIntegration(state.Integration{Task: &task, Path: &path, Base: &base}); err != nil {
		return err
	}
	if r.opts.Full != "" && r.lastGreen() == "" {
		r.recordGreen(r.base)
	}
	for _, dir := range []string{"context", "corrections", "carry"} {
		if err := os.MkdirAll(filepath.Join(r.stateDir, dir), 0o755); err != nil {
			return err
		}
	}
	r.initHost()
	r.resolveSettings()
	if sl, ok := r.exec.(StartLocker); ok {
		sl.SetStartLock(&r.intLock)
	}

	// Packages a crashed run left mid-flight.
	st := r.store.Snapshot()
	for _, pkg := range r.plan.Packages {
		ps := st.Packages[pkg.ID]
		if ps == nil || (ps.Status != "implementing" && ps.Status != "fixing") {
			continue
		}
		if files, ok := r.treeFiles(pkg.ID); ok && len(files) > 0 {
			_ = r.store.SetStatus(pkg.ID, "verifying", "resumed with changes", nil)
		} else {
			_ = r.store.SetStatus(pkg.ID, "pending", "resumed without changes", nil)
		}
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// applyOverrides adds `wave mark --widen` globs (plan-overrides.json) and
// returns, per package, the globs this call newly added. The runner calls it
// again before every dispatch because a mark lands while the run is live.
func (r *runner) applyOverrides() map[string][]string {
	added := map[string][]string{}
	raw, err := os.ReadFile(filepath.Join(r.stateDir, "plan-overrides.json"))
	if err != nil {
		return added
	}
	var overrides map[string]any
	if json.Unmarshal(raw, &overrides) != nil {
		return added
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.plan.Packages {
		pkg := &r.plan.Packages[i]
		entry, _ := overrides[pkg.ID].(map[string]any)
		extra, _ := entry["files_owned_add"].([]any)
		for _, g := range extra {
			glob, ok := g.(string)
			if ok && !containsStr(pkg.FilesOwned, glob) {
				pkg.FilesOwned = append(append([]string{}, pkg.FilesOwned...), glob)
				added[pkg.ID] = append(added[pkg.ID], glob)
			}
		}
	}
	return added
}

// noteWidening records, for pid's next context files, the paths a mark just
// widened and the mark's detail, so a reused session (which keeps the old
// contract in its head) is told what changed. No new paths clears the note.
func (r *runner) noteWidening(pid string, added []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(added) == 0 {
		delete(r.widenNotes, pid)
		return
	}
	note := "files_owned was widened by the instructor; you now also own: " + strings.Join(added, ", ") + "."
	if p := r.store.Snapshot().Packages[pid]; p != nil && p.Detail != "" {
		note += "\nInstructor note: " + p.Detail
	}
	r.widenNotes[pid] = note
}

func (r *runner) waveCfg(cfg *config.Resolved) map[string]any {
	if cfg == nil {
		return nil
	}
	return cfg.Section("wave")
}

func objGet(o map[string]any, k string) any {
	if o == nil {
		return nil
	}
	v := o[k]
	return v
}

// explicitWidth is a usable integer width (bools and floats are not).
func explicitWidth(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, x >= 1
	case int64:
		return int(x), x >= 1
	}
	return 0, false
}

// resolveSettings: an explicit option wins over `wave.*` config, which
// wins over the default; the in-flight width is reported once.
func (r *runner) resolveSettings() {
	wave := r.waveCfg(r.config())
	mode := strings.ToLower(strings.TrimSpace(r.opts.VerifyMode))
	if mode == "" {
		if v := objGet(wave, "verify_mode"); v != nil {
			mode = strings.ToLower(strings.TrimSpace(valText(v)))
		} else {
			mode = defaultVerifyMode
		}
	}
	if mode != "final" && mode != "gated" {
		mode = defaultVerifyMode
	}
	r.verifyMode = mode
	rounds := defaultConvergeRounds
	if r.opts.ConvergeRounds != nil {
		rounds = *r.opts.ConvergeRounds
	} else if n, ok := objGet(wave, "converge_rounds").(int64); ok {
		rounds = int(n)
	}
	if rounds < 0 {
		rounds = defaultConvergeRounds
	}
	r.convergeRounds = rounds

	width, source := r.opts.MaxInFlight, "option"
	if width < 1 {
		var ok bool
		width, ok = explicitWidth(objGet(wave, "max_in_flight"))
		source = "config"
		if !ok {
			width, source = 0, "auto"
		}
	}
	r.maxInFlight = width
	if width == 0 {
		r.emit("max-in-flight", "", map[string]any{"value": "auto (unbounded)", "source": source})
		fmt.Fprintln(r.stderr, "max-in-flight: auto (unbounded)")
	} else {
		r.emit("max-in-flight", "", map[string]any{"value": width, "source": source})
		fmt.Fprintf(r.stderr, "max-in-flight: %d (%s)\n", width, source)
	}

	r.memoryGuard = objGet(wave, "memory_guard") != false
	r.usageEvery = config.DefaultUsageEvery
	if n, ok := objGet(wave, "usage_every").(int64); ok && n >= 0 {
		r.usageEvery = int(n)
	}
	r.budgetUSD = 0
	if r.opts.BudgetUSD != nil {
		r.budgetUSD = *r.opts.BudgetUSD
	} else {
		switch n := objGet(wave, "budget_usd").(type) {
		case int64:
			r.budgetUSD = float64(n)
		case float64:
			r.budgetUSD = n
		}
	}
	if r.budgetUSD > 0 && r.opts.RunID == "" {
		r.emit("budget-off", "", map[string]any{"budget_usd": r.budgetUSD, "reason": "no --run-id"})
		fmt.Fprintf(r.stderr, "warning: budget $%.2f ignored: the run's cost needs --run-id\n", r.budgetUSD)
		r.budgetUSD = 0
	}
}

func (r *runner) interactiveMode() string {
	mode := r.opts.InteractiveHost
	if mode == "" {
		if cfg := r.config(); cfg != nil {
			if v, ok := cfg.Lookup("interactive_host"); ok && v != nil {
				mode = valText(v)
			}
		}
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "auto", "herdr", "orca", "none":
		return mode
	}
	return "auto"
}

// selectHost picks the interactive host per `interactive_host` when herdr
// is a candidate; with only one candidate it is that one.
func (r *runner) selectHost() {
	if r.herdr == nil {
		return
	}
	herdrHost, orcaHost := r.herdr, r.host
	r.herdr = nil
	mode := r.interactiveMode()
	r.host, r.hostName = nil, "orca"
	switch mode {
	case "none":
		return
	case "herdr":
		r.host, r.hostName = herdrHost, mode
		return
	case "orca":
		r.host, r.hostName = orcaHost, mode
		return
	}
	type cand struct {
		name string
		h    host.Host
	}
	var missing []string
	for _, c := range []cand{{"orca", orcaHost}, {"herdr", herdrHost}} {
		if c.h == nil || c.h.Enabled() == host.TriFalse {
			continue
		}
		if ok, reason := c.h.Available(); !ok && c.h.Enabled() == host.TriTrue {
			missing = append(missing, c.name+" unavailable: "+reason)
		}
	}
	for _, c := range []cand{{"herdr", herdrHost}, {"orca", orcaHost}} {
		if c.h == nil || c.h.Enabled() == host.TriFalse {
			continue
		}
		if ok, _ := c.h.Available(); ok {
			r.host, r.hostName = c.h, c.name
			return
		}
	}
	// No host at all: an `enabled: true` backend that is unreachable still
	// stops the run.
	if len(missing) > 0 {
		r.stop(missing[0])
	}
}

// initHost keeps the selected host only when it is enabled and reachable;
// `enabled: true` with the host unreachable stops the run before anything
// starts.
func (r *runner) initHost() {
	r.selectHost()
	if r.host == nil {
		return
	}
	enabled := r.host.Enabled()
	if enabled == host.TriFalse {
		r.host = nil
		return
	}
	if ok, reason := r.host.Available(); !ok {
		r.host = nil
		if enabled == host.TriTrue {
			r.stop(fmt.Sprintf("%s unavailable: %s", r.hostName, reason))
		}
		return
	}
	r.intBranch = r.base
	if out, err := gitRun(r.intPath, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "HEAD" {
			r.intBranch = b
		}
	}
	name := r.hostName
	if rt, ok := r.host.(host.Runtime); ok {
		rt.SetStateDir(r.stateDir)
		rt.SetEventFunc(func(pkg string, detail map[string]any) {
			d := make(map[string]any, len(detail)+1)
			for k, v := range detail {
				d[k] = v
			}
			d["host"] = name
			r.emit("orca-session", pkg, d)
		})
	}
	_ = os.MkdirAll(filepath.Join(r.stateDir, "orca"), 0o755)
	if w, ok := r.host.(host.Warm); ok {
		w.ConfigureWarm(r.warmReadSet(), r.modelCounts())
	}
}

// warmReadSet is the repo-relative files a warm reader reads: the preamble
// files inside the repo, then the plan's warm_read.
func (r *runner) warmReadSet() []string {
	root := realPath(r.root)
	var out []string
	for _, p := range r.plan.Preamble {
		rel, err := filepath.Rel(root, realPath(p))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	for _, rel := range r.plan.WarmRead {
		if !containsStr(out, rel) {
			out = append(out, rel)
		}
	}
	return out
}

func realPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return absPath(p)
}

// modelCounts is the packages still to start, per interactive-host model.
func (r *runner) modelCounts() map[string]int {
	st := r.store.Snapshot()
	counts := map[string]int{}
	for _, pkg := range r.plan.Packages {
		if ps := st.Packages[pkg.ID]; ps != nil && ps.Status != "pending" {
			continue
		}
		counts[r.host.ModelFor(pkg.Cls)]++
	}
	return counts
}

func (r *runner) shutdownHost() {
	if r.host == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(r.stderr, "agent-exec: %s: shutdown failed: %v\n", r.hostName, p)
		}
	}()
	r.host.Shutdown()
}

// treeFiles is the files the package's worktree changed; false when it has
// no tree.
func (r *runner) treeFiles(pid string) ([]string, bool) {
	diff, err := isolate.Diff(r.root, r.task(pid), "", false)
	if err != nil || diff.Status != "ok" {
		return nil, false
	}
	return diff.Files, true
}
