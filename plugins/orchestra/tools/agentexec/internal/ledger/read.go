package ledger

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// NoOrdinal is FileInfo's ordinal for a file without a "NNN-" prefix.
const NoOrdinal = -1

// ForEachLine calls fn with every line of path, splitting on \n, \r\n and a
// lone \r as Python's text-mode iteration does. Unreadable files yield
// nothing. A streaming reader: a long transcript is never held in memory.
func ForEachLine(path string, fn func(line string)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		chunk, err := r.ReadString('\n')
		if chunk != "" {
			chunk = strings.ReplaceAll(chunk, "\r\n", "\n")
			parts := strings.Split(strings.ReplaceAll(chunk, "\r", "\n"), "\n")
			for i, p := range parts {
				if i == len(parts)-1 && p == "" {
					break
				}
				fn(p)
			}
		}
		if err != nil {
			return
		}
	}
}

// ReadFile returns the JSON-object lines of a ledger file; anything else
// (blank, malformed, non-object) is skipped.
func ReadFile(path string) []map[string]any {
	var records []map[string]any
	ForEachLine(path, func(line string) {
		v, err := fsutil.DecodeJSON([]byte(line))
		if err != nil {
			return
		}
		if obj, ok := v.(map[string]any); ok {
			records = append(records, obj)
		}
	})
	return records
}

// Matcher is what a selector is validated against (a *regexp.Regexp).
type Matcher interface{ MatchString(string) bool }

// ValidSelector reports whether value is a usable run/session selector.
func ValidSelector(value string, pattern Matcher) bool {
	return value != "." && value != ".." && pattern.MatchString(value)
}

type runSelector struct{}

func (runSelector) MatchString(s string) bool {
	return RunIDPattern.MatchString(s) || OrdinalPattern.MatchString(s)
}

// RunSelector matches a --run value: a run id or an ordinal.
var RunSelector Matcher = runSelector{}

// FileInfo is the execution ordinal (NoOrdinal if none) and run id of a
// ledger file name.
func FileInfo(path string) (int, string, bool) {
	name := filepath.Base(path)
	if name == "no.run.jsonl" {
		return NoOrdinal, "no.run", true
	}
	if m := filePattern.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, m[2], true
	}
	if strings.HasSuffix(name, ".jsonl") {
		return NoOrdinal, strings.TrimSuffix(name, ".jsonl"), true
	}
	return NoOrdinal, "", false
}

func orderKey(path string) (int, int, string) {
	ordinal, run, _ := FileInfo(path)
	switch {
	case run == "no.run":
		return 2, 0, run
	case ordinal == NoOrdinal:
		return 1, 0, run
	}
	return 0, ordinal, run
}

// sessionFiles lists the ledger files directly inside dir in execution order
// (numeric ordinals first, un-numbered by name, no.run last).
func sessionFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".jsonl") && isFile(path) {
			files = append(files, path)
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		a1, a2, a3 := orderKey(files[i])
		b1, b2, b3 := orderKey(files[j])
		if a1 != b1 {
			return a1 < b1
		}
		if a2 != b2 {
			return a2 < b2
		}
		return a3 < b3
	})
	return files
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// SessionFiles is the ledger files of one session directory, in order.
func SessionFiles(ledgerDir, session string) []string {
	if !ValidSelector(session, SessionPattern) {
		return nil
	}
	return sessionFiles(filepath.Join(ledgerDir, session))
}

// Paths lists every session's ledger files once (by real path): sessions in
// name order, each session's files in execution order. Files sitting directly
// in the ledger directory (the retired flat layout) are not read.
func Paths(ledgerDir string) []string {
	entries, err := os.ReadDir(ledgerDir)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		dir := filepath.Join(ledgerDir, e.Name())
		if !isDir(dir) {
			continue
		}
		for _, path := range sessionFiles(dir) {
			key := paths.RealPath(path)
			if !seen[key] {
				seen[key] = true
				out = append(out, path)
			}
		}
	}
	return out
}

// SessionOrdinalMap maps run id -> execution ordinal for a session (the
// current $CLAUDE_CODE_SESSION_ID when session is empty).
func SessionOrdinalMap(ledgerDir, session string) map[string]int {
	if session == "" {
		session = os.Getenv("CLAUDE_CODE_SESSION_ID")
	}
	out := map[string]int{}
	if !ValidSelector(session, SessionPattern) {
		return out
	}
	for _, path := range sessionFiles(filepath.Join(ledgerDir, session)) {
		if ordinal, run, _ := FileInfo(path); ordinal != NoOrdinal {
			out[run] = ordinal
		}
	}
	return out
}

// ResolveRunSelector turns an ordinal into its run id (current session only)
// and passes a full run id through; ok is false for anything unusable.
func ResolveRunSelector(ledgerDir, selector string) (string, bool) {
	if !OrdinalPattern.MatchString(selector) {
		if ValidSelector(selector, RunIDPattern) {
			return selector, true
		}
		return "", false
	}
	ordinal, _ := strconv.Atoi(selector)
	session := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if !ValidSelector(session, SessionPattern) {
		return "", false
	}
	for _, path := range sessionFiles(filepath.Join(ledgerDir, session)) {
		if found, run, _ := FileInfo(path); found == ordinal {
			return run, true
		}
	}
	return "", false
}

// SelectedPaths are the ledger files of the given runs and sessions, once
// each. A run selector matches by run id in any session.
func SelectedPaths(ledgerDir string, runIDs, sessionIDs []string) []string {
	var resolved []string
	for _, id := range runIDs {
		if r, ok := ResolveRunSelector(ledgerDir, id); ok {
			resolved = append(resolved, r)
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, path := range Paths(ledgerDir) {
		_, run, _ := FileInfo(path)
		matches := false
		for _, r := range resolved {
			if run == r {
				matches = true
			}
		}
		for _, s := range sessionIDs {
			if filepath.Dir(path) == filepath.Join(ledgerDir, s) {
				matches = true
			}
		}
		if !matches {
			continue
		}
		key := paths.RealPath(path)
		if !seen[key] {
			seen[key] = true
			out = append(out, path)
		}
	}
	return out
}
