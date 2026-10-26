package isolate

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// TrackedDir is one directory under an isolation.exclude_paths entry that a
// commit tracks files in. Such a commit checks the files out into every
// worktree, which is what the scan exists to catch.
type TrackedDir struct {
	Dir   string // repo-relative, trailing "/", e.g. "packages/web/node_modules/"
	Files int    // tracked blobs under Dir
	Bytes int64  // their summed blob sizes
}

// trackedRefusalMax bounds how many directories TrackedRefusal names; the
// rest are counted, so the message stays one line for a huge tree.
const trackedRefusalMax = 5

// TrackedExcluded lists the directories under f's excluded entries that rev
// tracks files in. dir is any directory inside the repository (paths are
// repo-root relative regardless). Sorted by Bytes descending, then Dir
// ascending; nil when there are none or f excludes nothing.
//
// It reads ls-tree rather than the working tree because the problem is what
// the start commit carries: ignored or untracked directories on disk never
// reach a worktree and are not reported.
func TrackedExcluded(dir, rev string, f ChangeFilter) ([]TrackedDir, error) {
	entries := f.entries()
	if len(entries) == 0 {
		return nil, nil
	}
	out, err := gitx.Run(dir, "ls-tree", "-r", "-l", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	// A file's owner depends only on its parent directory, so the decision
	// is cached per directory: an 80,000-file tree runs path.Match per
	// directory, not per file.
	owners := map[string]string{}
	agg := map[string]*TrackedDir{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		parent := path.Dir(p)
		owner, seen := owners[parent]
		if !seen {
			owner = trackedOwner(entries, parent)
			owners[parent] = owner
		}
		if owner == "" {
			continue
		}
		td := agg[owner]
		if td == nil {
			td = &TrackedDir{Dir: owner}
			agg[owner] = td
		}
		td.Files++
		td.Bytes += size
	}
	var list []TrackedDir
	for _, td := range agg {
		list = append(list, *td)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Bytes != list[j].Bytes {
			return list[i].Bytes > list[j].Bytes
		}
		return list[i].Dir < list[j].Dir
	})
	return list, nil
}

// trackedOwner is the Dir an excluded entry assigns to a file in parent ("" =
// none). When several entries match, the shortest Dir wins, so a file counts
// once at the outermost excluded level.
func trackedOwner(entries []string, parent string) string {
	if parent == "." {
		return ""
	}
	dirs := strings.Split(parent, "/")
	owner := ""
	for _, e := range entries {
		n := trackedMatchLen(strings.Split(e, "/"), dirs)
		if n == 0 {
			continue
		}
		d := strings.Join(dirs[:n], "/") + "/"
		if owner == "" || len(d) < len(owner) {
			owner = d
		}
	}
	return owner
}

// trackedMatchLen is how many leading components of dirs the entry pattern
// pat covers (0 = no match). A single-segment pattern matches the first
// directory component it names, at any depth; a multi-segment one must match
// from the repository root.
func trackedMatchLen(pat, dirs []string) int {
	if len(pat) == 1 {
		for i, d := range dirs {
			if ok, _ := path.Match(pat[0], d); ok {
				return i + 1
			}
		}
		return 0
	}
	if len(dirs) < len(pat) {
		return 0
	}
	for i, seg := range pat {
		if ok, _ := path.Match(seg, dirs[i]); !ok {
			return 0
		}
	}
	return len(pat)
}

// TrackedRefusal is the guiding error for a start commit whose tracked files
// include such directories (dirs non-empty). It names the fix in git terms,
// because the commit is the user's and orchestra must not rewrite it.
func TrackedRefusal(rev string, dirs []TrackedDir) string {
	if len(dirs) == 0 {
		return ""
	}
	var files int
	var bytes int64
	for _, d := range dirs {
		files += d.Files
		bytes += d.Bytes
	}
	list := make([]string, 0, trackedRefusalMax+1)
	for _, d := range dirs[:min(len(dirs), trackedRefusalMax)] {
		list = append(list, fmt.Sprintf("%s (%d files, %s)", d.Dir, d.Files, trackedSize(d.Bytes)))
	}
	if extra := len(dirs) - trackedRefusalMax; extra > 0 {
		list = append(list, fmt.Sprintf("and %d more", extra))
	}
	short := rev
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("%s tracks %d files (%s) under dependency or cache directories that isolation.exclude_paths keeps out of every change: %s. Every worktree orchestra creates would check them out. Take them out of the branch (git rm -r --cached %s, add it to .gitignore, commit; the same for each directory), or list a directory in isolation.keep_paths if the repository tracks it on purpose.",
		short, files, trackedSize(bytes), strings.Join(list, ", "), dirs[0].Dir)
}

// trackedSize formats n bytes in binary units with one decimal place from
// MiB up, the precision a user needs to judge a clone's cost.
func trackedSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
