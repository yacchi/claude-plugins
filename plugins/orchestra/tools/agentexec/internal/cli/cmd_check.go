package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
)

type checkOpts struct {
	task, path, since, files, session, repo string
	hasTask, hasFiles                       bool
	all, baseline, noRetry                  bool
	format                                  format
}

func newCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "run the config-driven checks over a tree's changed files",
		Args:  cobra.NoArgs,
	}
	o := &checkOpts{}
	fl := cmd.Flags()
	fl.StringVar(&o.task, "task", "", "task id (its worktree)")
	fl.StringVar(&o.path, "path", "", "an arbitrary directory")
	fl.StringVar(&o.since, "since", "", "ref the changed files are measured from (default HEAD)")
	fl.StringVar(&o.files, "files", "", "comma-separated changed-file list override")
	repo := repoFlag(cmd)
	fl.StringVar(&o.session, "session", "", "session id")
	fl.BoolVar(&o.all, "all", false, "do not stop at the first failure")
	fl.BoolVar(&o.baseline, "baseline", false, "re-run failures at the baseline to spot preexisting breakage")
	fl.BoolVar(&o.noRetry, "no-retry", false, "do not retry failed checks alone")
	o.format = formatFlags(cmd, "json", "text")
	cmd.MarkFlagsOneRequired("task", "path")
	cmd.MarkFlagsMutuallyExclusive("task", "path")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		o.repo = *repo
		o.hasTask, o.hasFiles = cmd.Flags().Changed("task"), cmd.Flags().Changed("files")
		return runCheck(cmd.OutOrStdout(), o)
	}
	return cmd
}

func checkOutput(w io.Writer, text bool, status string, tree *string, files int, results []checks.CheckResult) {
	if text {
		var lines []string
		for _, c := range results {
			lines = append(lines, fmt.Sprintf("%s %s %.1fs", c.Name, c.Status, c.Seconds))
		}
		for _, c := range results {
			if c.Excerpt != "" {
				lines = append(lines, c.Excerpt)
			}
		}
		if len(lines) > 0 {
			fmt.Fprintln(w, strings.Join(lines, "\n"))
		}
		return
	}
	objs := make([]any, 0, len(results))
	for _, c := range results {
		objs = append(objs, c.Object())
	}
	var treeV any
	if tree != nil {
		treeV = *tree
	}
	_ = writeJSONLine(w, map[string]any{"status": status, "tree": treeV, "files": files, "checks": objs})
}

func splitFilesFlag(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func nonBlankLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

var checkExit = map[string]int{"pass": 0, "no-checks": 0, "fail": 1, "error": 3, "preexisting": 4}

func runCheck(w io.Writer, o *checkOpts) error {
	text := o.format.is("text")
	fail := func(tree *string) error {
		checkOutput(w, text, "error", tree, 0, nil)
		return &ExitError{Code: 3}
	}

	root, ok := gitx.RepoRoot(o.repo)
	if !ok {
		return fail(nil)
	}
	var tree string
	if o.hasTask {
		wt, _, found := isolate.ResolveWorktree(root, o.task, o.session)
		if !found {
			return fail(nil)
		}
		tree = wt
	} else {
		tree = o.path
		if info, err := os.Stat(tree); tree == "" || err != nil || !info.IsDir() {
			return fail(&tree)
		}
	}

	since := o.since
	if since == "" {
		since = "HEAD"
	}
	var files []string
	switch {
	case o.hasFiles:
		files = splitFilesFlag(o.files)
	case o.hasTask:
		diff, err := isolate.Diff(root, o.task, o.session, false)
		if err != nil || diff.Status == "error" {
			return fail(&tree)
		}
		files = diff.Files
	default:
		set := map[string]bool{}
		if out, err := gitx.Run(tree, "diff", "--name-only", since); err == nil {
			for _, f := range nonBlankLines(out) {
				set[f] = true
			}
		}
		if out, err := gitx.Run(tree, "ls-files", "--others", "--exclude-standard"); err == nil {
			for _, f := range nonBlankLines(out) {
				set[f] = true
			}
		}
		for f := range set {
			files = append(files, f)
		}
		sort.Strings(files)
	}

	cfg, err := config.Resolve()
	if err != nil {
		return &ExitError{Code: 3, Msg: err.Error()}
	}
	cc, err := checks.LoadConfig(cfg)
	if err != nil {
		return &ExitError{Code: 3, Msg: "checks: " + err.Error()}
	}
	slotDir := filepath.Join(filepath.Dir(absPath(ledger.Dir(cfg))), "check-slots")
	if len(cc.Items) == 0 {
		checkOutput(w, text, "no-checks", &tree, len(files), nil)
		return nil
	}

	opts := checks.Options{
		Files: files, SlotDir: slotDir, MaxParallel: cc.MaxParallel, RunAll: o.all,
		Ensure: cc.Ensure, RetryFailedAlone: cc.RetryFailedAlone && !o.noRetry,
	}
	results := checks.Run(tree, cc.Items, opts).Checks

	if o.baseline {
		ref := since
		if o.hasTask {
			ref, _ = isolate.ReadBaseline(tree)
		}
		markPreexisting(root, ref, tree, files, cfg, cc, slotDir, results)
	}

	status := checks.Result{Checks: results}.Status()
	checkOutput(w, text, status, &tree, len(files), results)
	if code := checkExit[status]; code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// markPreexisting re-runs the failed checks at ref in a throwaway worktree
// and relabels those that fail there too as "preexisting".
func markPreexisting(root, ref, tree string, files []string, cfg *config.Resolved, cc checks.Config, slotDir string, results []checks.CheckResult) {
	type failure struct {
		index int
		item  checks.Item
	}
	var failed []failure
	unused := append([]checks.Item(nil), cc.Items...)
	for i, r := range results {
		if r.Status != "fail" {
			continue
		}
		for pos, it := range unused {
			if it.Name == r.Name {
				failed = append(failed, failure{i, it})
				unused = append(unused[:pos], unused[pos+1:]...)
				break
			}
		}
	}
	if ref == "" || len(failed) == 0 {
		return
	}
	tmp, err := os.MkdirTemp("", "orchestra-check-baseline-")
	if err != nil {
		return
	}
	baseTree := filepath.Join(tmp, "wt")
	defer func() {
		_, _ = gitx.Run(root, "worktree", "remove", "--force", baseTree)
		_, _ = gitx.Run(root, "worktree", "prune")
		_ = os.RemoveAll(tmp)
	}()
	if _, err := gitx.Run(root, "worktree", "add", "--detach", "-q", baseTree, ref); err != nil {
		return
	}
	for _, rel := range isolate.DetectCarryDirs(root, cfg.StringList("isolation", "carry_extra")) {
		_ = isolate.CarryDir(root, baseTree, rel)
	}
	items := make([]checks.Item, len(failed))
	for i, f := range failed {
		items[i] = f.item
	}
	base := checks.Run(baseTree, items, checks.Options{
		Files: files, SlotDir: slotDir, MaxParallel: cc.MaxParallel, RunAll: true, RetryFailedAlone: true,
	}).Checks
	for i, f := range failed {
		if i < len(base) && base[i].Status == "fail" {
			results[f.index].Status = "preexisting"
		}
	}
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
