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
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
)

// The check command parses its own arguments: the Python parser reported
// duplicates and unknown tokens in argument order, which pflag cannot.
func newCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "check",
		Short:              "run the config-driven checks over a tree's changed files",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheck(cmd.OutOrStdout(), args)
		},
	}
	// Declared for --help only; DisableFlagParsing means they are never parsed.
	strFlag(cmd, "task", "task id (its worktree)")
	strFlag(cmd, "path", "an arbitrary directory")
	strFlag(cmd, "since", "ref the changed files are measured from (default HEAD)")
	strFlag(cmd, "files", "comma-separated changed-file list override")
	strFlag(cmd, "repo", "repository path (default cwd)")
	strFlag(cmd, "session", "session id")
	boolFlag(cmd, "all", "do not stop at the first failure")
	boolFlag(cmd, "baseline", "re-run failures at the baseline to spot preexisting breakage")
	boolFlag(cmd, "no-retry", "do not retry failed checks alone")
	formatFlags(cmd, "json", "text")
	return cmd
}

type checkOpts struct {
	task, path, since, files, session  *string
	repo                               string
	all, baseline, json, text, noRetry bool
}

func parseCheckArgs(args []string) (checkOpts, string) {
	o := checkOpts{}
	if wd, err := os.Getwd(); err == nil {
		o.repo = wd
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if seen[tok] {
			return o, "duplicate option: " + tok
		}
		switch tok {
		case "--task", "--path", "--since", "--files", "--repo", "--session":
			if i+1 >= len(args) {
				return o, "missing value for " + tok
			}
			seen[tok] = true
			v := args[i+1]
			i++
			switch tok {
			case "--task":
				o.task = &v
			case "--path":
				o.path = &v
			case "--since":
				o.since = &v
			case "--files":
				o.files = &v
			case "--repo":
				o.repo = v
			case "--session":
				o.session = &v
			}
		case "--all", "--baseline", "--json", "--text", "--no-retry":
			seen[tok] = true
			switch tok {
			case "--all":
				o.all = true
			case "--baseline":
				o.baseline = true
			case "--json":
				o.json = true
			case "--text":
				o.text = true
			case "--no-retry":
				o.noRetry = true
			}
		default:
			return o, "unknown option: " + tok
		}
	}
	if o.json && o.text {
		return o, "--json and --text are mutually exclusive"
	}
	if (o.task == nil) == (o.path == nil) {
		return o, "provide exactly one of --task or --path"
	}
	return o, ""
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
	_ = writeJSONLine(w, jsonx.NewObject().Set("status", status).Set("tree", treeV).Set("files", files).Set("checks", objs))
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

func runCheck(w io.Writer, args []string) error {
	o, perr := parseCheckArgs(args)
	if perr != "" {
		return exitf(2, "check: %s", perr)
	}
	fail := func(tree *string) error {
		checkOutput(w, o.text, "error", tree, 0, nil)
		return &ExitError{Code: 3}
	}

	root, ok := gitx.RepoRoot(o.repo)
	if !ok {
		return fail(nil)
	}
	session := ""
	if o.session != nil {
		session = *o.session
	}
	var tree string
	if o.task != nil {
		wt, _, found := isolate.ResolveWorktree(root, *o.task, session)
		if !found {
			return fail(nil)
		}
		tree = wt
	} else {
		tree = *o.path
		if info, err := os.Stat(tree); tree == "" || err != nil || !info.IsDir() {
			return fail(&tree)
		}
	}

	var files []string
	switch {
	case o.files != nil:
		files = splitFilesFlag(*o.files)
	case o.task != nil:
		diff, err := isolate.Diff(root, *o.task, session, false)
		if err != nil || diff.Status == "error" {
			return fail(&tree)
		}
		files = diff.Files
	default:
		since := "HEAD"
		if o.since != nil && *o.since != "" {
			since = *o.since
		}
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
		checkOutput(w, o.text, "no-checks", &tree, len(files), nil)
		return nil
	}

	opts := checks.Options{
		Files: files, SlotDir: slotDir, MaxParallel: cc.MaxParallel, RunAll: o.all,
		Ensure: cc.Ensure, RetryFailedAlone: cc.RetryFailedAlone && !o.noRetry,
	}
	results := checks.Run(tree, cc.Items, opts).Checks

	if o.baseline {
		var ref string
		if o.task != nil {
			ref, _ = isolate.ReadBaseline(tree)
		} else if o.since != nil && *o.since != "" {
			ref = *o.since
		} else {
			ref = "HEAD"
		}
		markPreexisting(root, ref, tree, files, cfg, cc, slotDir, results)
	}

	status := checks.Result{Checks: results}.Status()
	checkOutput(w, o.text, status, &tree, len(files), results)
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
		_ = isolate.CopyTreeFast(filepath.Join(root, rel), filepath.Join(baseTree, rel))
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
