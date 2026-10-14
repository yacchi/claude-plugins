package usage

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Tokens is the canonical token vocabulary every source normalizes into, so
// totals can sum across executors. InputTokens is FRESH input (cache writes
// included for claude, which is freshly submitted on the way through);
// CachedInputTokens is input served from a prompt cache. The two never
// double-count each other.
type Tokens struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
}

func (t *Tokens) add(o Tokens, sign int64) {
	t.InputTokens += sign * o.InputTokens
	t.OutputTokens += sign * o.OutputTokens
	t.CachedInputTokens += sign * o.CachedInputTokens
}

// coerceCount is any -> non-negative int, defaulting to 0. Every number that
// reaches the report goes through here, which keeps "every emitted number is
// a non-negative int" true against any shape drift in the source logs.
func coerceCount(v any) int64 {
	switch x := v.(type) {
	case int64:
		if x >= 0 {
			return x
		}
	case int:
		if x >= 0 {
			return int64(x)
		}
	}
	return 0
}

func tokensFrom(v any) Tokens {
	obj, ok := v.(map[string]any)
	if !ok {
		return Tokens{}
	}
	get := func(k string) any { return obj[k] }
	return Tokens{
		InputTokens:       coerceCount(get("input_tokens")),
		OutputTokens:      coerceCount(get("output_tokens")),
		CachedInputTokens: coerceCount(get("cached_input_tokens")),
	}
}

// contribution is what one transcript message adds to the accumulator.
type contribution struct {
	side   string // "main" | "sidechain"
	model  string // "" = none
	tokens Tokens
	detail Detail
}

// claudeAcc accumulates Claude transcript usage.
type claudeAcc struct {
	tokens, main, sidechain Tokens
	byModel                 map[string]*Tokens
	byModelDetail           map[string]*Detail
	messages                map[string]contribution
	records                 int64
}

func newClaudeAcc() *claudeAcc {
	return &claudeAcc{
		byModel:       map[string]*Tokens{},
		byModelDetail: map[string]*Detail{},
		messages:      map[string]contribution{},
	}
}

func (a *claudeAcc) fold(c contribution, sign int64) {
	a.tokens.add(c.tokens, sign)
	if c.side == "sidechain" {
		a.sidechain.add(c.tokens, sign)
	} else {
		a.main.add(c.tokens, sign)
	}
	if c.model != "" {
		bm := a.byModel[c.model]
		if bm == nil {
			bm = &Tokens{}
			a.byModel[c.model] = bm
		}
		bm.add(c.tokens, sign)
		d := a.byModelDetail[c.model]
		if d == nil {
			d = &Detail{}
			a.byModelDetail[c.model] = d
		}
		d.FreshInput += sign * c.detail.FreshInput
		d.CacheWrite5m += sign * c.detail.CacheWrite5m
		d.CacheWrite1h += sign * c.detail.CacheWrite1h
		d.CacheRead += sign * c.detail.CacheRead
		d.Output += sign * c.detail.Output
	}
	a.records += sign
}

// usageDetail is one transcript usage object as the five-way breakdown. Cache
// writes are split by TTL from usage.cache_creation; whatever that object does
// not account for (older transcripts carry only the total) is priced as the
// 5-minute TTL, the default.
func usageDetail(usage map[string]any) Detail {
	get := func(k string) any { return usage[k] }
	writeTotal := coerceCount(get("cache_creation_input_tokens"))
	var write1h int64
	if creation, ok := get("cache_creation").(map[string]any); ok {
		v := creation["ephemeral_1h_input_tokens"]
		write1h = coerceCount(v)
	}
	if write1h > writeTotal {
		write1h = writeTotal
	}
	return Detail{
		FreshInput:   coerceCount(get("input_tokens")),
		CacheWrite5m: writeTotal - write1h,
		CacheWrite1h: write1h,
		CacheRead:    coerceCount(get("cache_read_input_tokens")),
		Output:       coerceCount(get("output_tokens")),
	}
}

// addLine folds one raw transcript line into the accumulator when it carries
// usage at or after cutoff.
func (a *claudeAcc) addLine(line string, cutoff time.Time) {
	v, err := fsutil.DecodeJSON([]byte(line))
	if err != nil {
		return
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	message := config.ObjectAt(obj, "message")
	if message == nil {
		return
	}
	usage := config.ObjectAt(message, "usage")
	if usage == nil {
		return
	}
	tsV := obj["timestamp"]
	ts, ok := ParseTimestamp(tsV)
	if !ok || ts.Before(cutoff) {
		return
	}
	get := func(k string) any { return usage[k] }
	c := contribution{
		side: "main",
		tokens: Tokens{
			InputTokens:       coerceCount(get("input_tokens")) + coerceCount(get("cache_creation_input_tokens")),
			OutputTokens:      coerceCount(get("output_tokens")),
			CachedInputTokens: coerceCount(get("cache_read_input_tokens")),
		},
		detail: usageDetail(usage),
	}
	if sc, _ := obj["isSidechain"]; sc == true {
		c.side = "sidechain"
	}
	if m, _ := message["model"]; m != nil {
		if model, ok := m.(string); ok {
			c.model = model
		}
	}
	// One API response is written to the transcript once per content block
	// (a thinking block, a text block and each tool_use block are separate
	// lines), every one carrying the same usage; a resumed or forked session
	// copies earlier lines again. Count each message id once, and let a later
	// line replace an earlier one (a streamed response's final line carries
	// the final output_tokens).
	if idV, _ := message["id"]; idV != nil {
		if id, ok := idV.(string); ok && id != "" {
			if prev, seen := a.messages[id]; seen {
				if prev == c {
					return
				}
				a.fold(prev, -1)
			}
			a.messages[id] = c
		}
	}
	a.fold(c, 1)
}

func (a *claudeAcc) addFile(path string, cutoff time.Time) {
	ledger.ForEachLine(path, func(line string) { a.addLine(line, cutoff) })
}

// dirEntryKind classifies an entry the way os.walk does: symlinks to
// directories are listed as directories but never entered.
func walkEntries(dir string) (files, dirs, enter []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, nil
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir():
			dirs = append(dirs, name)
			enter = append(enter, name)
		case e.Type()&os.ModeSymlink != 0:
			if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.IsDir() {
				dirs = append(dirs, name)
			} else {
				files = append(files, name)
			}
		default:
			files = append(files, name)
		}
	}
	sort.Strings(files)
	sort.Strings(dirs)
	sort.Strings(enter)
	return files, dirs, enter
}

func isRegular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// sessionFiles lists a project's top-level session transcripts and every
// JSONL file below a "subagents" directory.
func sessionFiles(project string) []string {
	var out []string
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		files, _, enter := walkEntries(dir)
		inSubagents := rel == "" || containsPart(rel, "subagents")
		if inSubagents {
			for _, name := range files {
				if strings.HasSuffix(name, ".jsonl") && isRegular(filepath.Join(dir, name)) {
					out = append(out, filepath.Join(dir, name))
				}
			}
		}
		for _, name := range enter {
			if !inSubagents && name != "subagents" {
				continue
			}
			walk(filepath.Join(dir, name), filepath.Join(rel, name))
		}
	}
	walk(project, "")
	return out
}

func containsPart(rel, part string) bool {
	for _, p := range strings.Split(rel, string(filepath.Separator)) {
		if p == part {
			return true
		}
	}
	return false
}

// walkJSONL lists every .jsonl file below dir.
func walkJSONL(dir string) []string {
	var out []string
	var walk func(d string)
	walk = func(d string) {
		files, _, enter := walkEntries(d)
		for _, name := range files {
			if strings.HasSuffix(name, ".jsonl") && isRegular(filepath.Join(d, name)) {
				out = append(out, filepath.Join(d, name))
			}
		}
		for _, name := range enter {
			walk(filepath.Join(d, name))
		}
	}
	walk(dir)
	return out
}

func homeDir(home string) string {
	if home != "" {
		return home
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// projectsRoots are the existing Claude project roots ($CLAUDE_CONFIG_DIR
// first, then ~/.claude), de-duplicated by real path.
func projectsRoots(home, configDir string) []string {
	if configDir == "" {
		configDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	var candidates []string
	if configDir != "" {
		candidates = append(candidates, filepath.Join(configDir, "projects"))
	}
	candidates = append(candidates, filepath.Join(homeDir(home), ".claude", "projects"))
	var roots []string
	seen := map[string]bool{}
	for _, root := range candidates {
		real := paths.RealPath(root)
		if seen[real] || !isDir(root) {
			continue
		}
		seen[real] = true
		roots = append(roots, root)
	}
	return roots
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// projectSlug is the ~/.claude/projects directory name for a working
// directory: every "/" replaced by "-".
func projectSlug(cwd string) string { return strings.ReplaceAll(cwd, "/", "-") }

func projectDirs(root string, allProjects bool, cwd string) []string {
	if allProjects {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range entries {
			if p := filepath.Join(root, e.Name()); isDir(p) {
				out = append(out, p)
			}
		}
		return out
	}
	if cwd == "" {
		cwd = getwd()
	}
	if dir := filepath.Join(root, projectSlug(cwd)); isDir(dir) {
		return []string{dir}
	}
	return nil
}

func scopeName(allProjects bool) string {
	if allProjects {
		return "all-projects"
	}
	return "cwd"
}

// collectWindow reads the Claude transcripts of the window, scoped to the
// working directory's project unless allProjects is set.
func collectWindow(o ReportOptions) *Source {
	s := &Source{Status: "unavailable", Scope: scopeName(o.AllProjects), Breakdown: true}
	roots := projectsRoots(o.Home, o.ConfigDir)
	if len(roots) == 0 {
		s.Note = "no claude projects directory"
		s.ByModel = map[string]Tokens{}
		return s
	}
	acc := newClaudeAcc()
	var sessions int64
	seenProjects, seenFiles := map[string]bool{}, map[string]bool{}
	for _, root := range roots {
		for _, dir := range projectDirs(root, o.AllProjects, o.Cwd) {
			key := paths.RealPath(dir)
			if seenProjects[key] {
				continue
			}
			seenProjects[key] = true
			for _, path := range sessionFiles(dir) {
				fkey := paths.RealPath(path)
				if seenFiles[fkey] {
					continue
				}
				seenFiles[fkey] = true
				before := acc.records
				acc.addFile(path, o.Since)
				if acc.records > before {
					sessions++
				}
			}
		}
	}
	s.Status = "empty"
	if acc.records != 0 {
		s.Status = "ok"
	}
	s.Note = ""
	s.fillClaude(acc, sessions)
	return s
}

func (s *Source) fillClaude(acc *claudeAcc, sessions int64) {
	s.Tokens, s.Main, s.Sidechain = acc.tokens, acc.main, acc.sidechain
	s.ByModel = map[string]Tokens{}
	for k, v := range acc.byModel {
		s.ByModel[k] = *v
	}
	s.ByModelDetail = map[string]Detail{}
	for k, v := range acc.byModelDetail {
		s.ByModelDetail[k] = *v
	}
	s.HasDetail = true
	s.Sessions = sessions
	s.Records = acc.records
}

// scopeFiles are the transcripts of the given runs (a workflow directory
// below some session's subagents) and sessions (its own transcript plus its
// subagents), once each.
func scopeFiles(home, configDir string, runIDs, sessionIDs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, root := range projectsRoots(home, configDir) {
		projects, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, p := range projects {
			project := filepath.Join(root, p.Name())
			if !isDir(project) {
				continue
			}
			var candidates []string
			for _, run := range runIDs {
				sessions, err := os.ReadDir(project)
				if err != nil {
					continue
				}
				for _, s := range sessions {
					wf := filepath.Join(project, s.Name(), "subagents", "workflows", run)
					if isDir(wf) {
						candidates = append(candidates, wf)
					}
				}
			}
			for _, sid := range sessionIDs {
				candidates = append(candidates,
					filepath.Join(project, sid+".jsonl"),
					filepath.Join(project, sid, "subagents"))
			}
			for _, c := range candidates {
				var found []string
				if isRegular(c) && strings.HasSuffix(c, ".jsonl") {
					found = []string{c}
				} else {
					found = walkJSONL(c)
				}
				for _, path := range found {
					key := paths.RealPath(path)
					if !seen[key] {
						seen[key] = true
						out = append(out, path)
					}
				}
			}
		}
	}
	return out
}

// collectScoped reads the transcripts of the selected runs/sessions, with no
// time cutoff.
func collectScoped(o ReportOptions) *Source {
	acc := newClaudeAcc()
	files := scopeFiles(o.Home, o.ConfigDir, o.RunIDs, o.SessionIDs)
	for _, path := range files {
		acc.addFile(path, time.Time{})
	}
	if acc.records == 0 {
		return notAttributable("no matching transcript data")
	}
	s := &Source{Status: "ok", Scope: "session", Breakdown: true}
	if len(o.RunIDs) > 0 {
		s.Scope = "run"
	}
	s.fillClaude(acc, int64(len(files)))
	return s
}
