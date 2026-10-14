// Package shelf is a per-worktree `git stash`. git's stash is one stack
// shared by every worktree, and parallel workers popping it swapped each
// other's changes; a shelf entry is a binary patch (untracked files
// included) plus a JSON record in the worktree's OWN git dir
// (.git/worktrees/<name>/orchestra-shelf/), so no other worktree can see or
// pop it. No refs, no commits.
package shelf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

const dirName = "orchestra-shelf"

// Entry is one shelved change set, as recorded in <id>.json.
type Entry struct {
	ID        int      `json:"id"`
	Message   string   `json:"message"`
	KeepIndex bool     `json:"keep_index"`
	Files     []string `json:"files"`
	Created   int64    `json:"created"`
}

// Result is the outcome of push, apply/pop and drop.
type Result struct {
	Status  string // ok | ok-3way | empty | conflicted | error
	Reason  string // error only
	ID      int
	Files   []string
	Dropped bool
	Note    string // conflicted only
	kind    string // push | apply | drop: which keys the JSON carries
}

func (r Result) MarshalJSON() ([]byte, error) {
	o := map[string]any{"status": r.Status}
	switch {
	case r.Status == "error":
		o["reason"] = r.Reason
	case r.Status == "empty":
		o["files"] = nonNil(r.Files)
	case r.Status == "conflicted":
		o["id"] = r.ID
		o["files"] = nonNil(r.Files)
		o["note"] = r.Note
	case r.kind == "drop":
		o["id"] = r.ID
	case r.kind == "apply":
		o["id"] = r.ID
		o["files"] = nonNil(r.Files)
		o["dropped"] = r.Dropped
	default:
		o["id"] = r.ID
		o["files"] = nonNil(r.Files)
	}
	return fsutil.MarshalJSON(o)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// errNoEntry is apply/drop with nothing (or no such id) on the shelf.
var errNoEntry = errors.New("no such shelf entry")

func storeDir(root string) (string, bool) {
	out, err := gitx.Run(root, "rev-parse", "--path-format=absolute", "--git-dir")
	dir := strings.TrimSpace(out)
	if err != nil || dir == "" {
		return "", false
	}
	return filepath.Join(dir, dirName), true
}

// IsLinkedWorktree reports whether root is a linked worktree (as opposed to
// the main working tree, which other tasks and the user share).
func IsLinkedWorktree(root string) bool {
	out, err := gitx.Run(root, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return false
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return false
	}
	return realPath(strings.TrimSpace(lines[0])) != realPath(strings.TrimSpace(lines[1]))
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func zList(root string, args ...string) []string {
	out, err := gitx.Run(root, args...)
	if err != nil {
		return nil
	}
	var list []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			list = append(list, p)
		}
	}
	return list
}

func entries(dir string) []Entry {
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var list []Entry
	for _, n := range names {
		if !strings.HasSuffix(n.Name(), ".json") {
			continue
		}
		if e, ok := readEntry(filepath.Join(dir, n.Name())); ok {
			list = append(list, e)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// readEntry accepts a record whose id is a JSON integer.
func readEntry(path string) (Entry, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return Entry{}, false
	}
	id, ok := raw["id"]
	if !ok {
		return Entry{}, false
	}
	n, err := strconv.Atoi(string(bytes.TrimSpace(id)))
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	_ = json.Unmarshal(b, &e)
	e.ID = n
	return e, true
}

// List is the entries of root's shelf, oldest first.
func List(root string) ([]Entry, error) {
	dir, ok := storeDir(root)
	if !ok {
		return []Entry{}, nil
	}
	list := entries(dir)
	if list == nil {
		list = []Entry{}
	}
	return list, nil
}

// Push sets the changes under paths (default: the whole tree) aside.
// Without keepIndex the tree returns to HEAD for those paths; with it, to
// the index, so the staged state is what remains (how a worker checks that
// one commit's worth of changes stands on its own).
func Push(root string, paths []string, msg string, keepIndex bool) (Result, error) {
	dir, ok := storeDir(root)
	if !ok {
		return Result{Status: "error", Reason: "not a git repository"}, nil
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	spec := append([]string{"--"}, paths...)
	git := func(args ...string) { _, _ = gitx.Run(root, args...) }
	withSpec := func(args ...string) []string { return append(args, spec...) }

	untracked := zList(root, withSpec("ls-files", "--others", "--exclude-standard", "-z")...)
	if len(untracked) > 0 {
		// Intent-to-add makes new files show up in `git diff` as additions.
		git(append([]string{"add", "--intent-to-add", "--"}, untracked...)...)
	}
	resetUntracked := func() {
		if len(untracked) > 0 {
			git(append([]string{"reset", "-q", "--"}, untracked...)...)
		}
	}
	var base []string
	if !keepIndex {
		base = []string{"HEAD"}
	}
	changed := zList(root, withSpec(append([]string{"diff", "--name-only", "-z"}, base...)...)...)
	added := toSet(zList(root, withSpec(append([]string{"diff", "--name-only", "-z", "--diff-filter=A"}, base...)...)...))
	if len(changed) == 0 {
		resetUntracked()
		return Result{Status: "empty", Files: []string{}, kind: "push"}, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		resetUntracked()
		return Result{Status: "error", Reason: "could not write the patch"}, nil
	}
	id := 0
	if list := entries(dir); len(list) > 0 {
		id = list[len(list)-1].ID + 1
	}
	patch := filepath.Join(dir, fmt.Sprintf("%d.patch", id))
	_, err := gitx.Run(root, withSpec(append([]string{"diff", "--binary", "--output=" + patch}, base...)...)...)
	if _, statErr := os.Stat(patch); err != nil || statErr != nil {
		resetUntracked()
		return Result{Status: "error", Reason: "could not write the patch"}, nil
	}

	// Put the tree back: untracked (and, without keepIndex, staged-new)
	// files are removed; everything else is checked out from HEAD or the
	// index.
	untrackedSet := toSet(untracked)
	resetUntracked()
	if !keepIndex {
		var tracked []string
		for _, p := range changed {
			if !untrackedSet[p] {
				tracked = append(tracked, p)
			}
		}
		git(append([]string{"reset", "-q", "--"}, tracked...)...)
	}
	var restore []string
	for _, p := range changed {
		if !added[p] && !untrackedSet[p] {
			restore = append(restore, p)
		}
	}
	if len(restore) > 0 {
		args := append([]string{"checkout", "-q"}, base...)
		git(append(append(args, "--"), restore...)...)
	}
	for rel := range union(untrackedSet, added) {
		_ = os.Remove(filepath.Join(root, rel))
	}

	meta := Entry{ID: id, Message: msg, KeepIndex: keepIndex, Files: changed, Created: time.Now().Unix()}
	b, err := fsutil.MarshalJSON(meta)
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", id)), b, 0o644); err != nil {
		return Result{}, err
	}
	return Result{Status: "ok", ID: id, Files: changed, kind: "push"}, nil
}

func toSet(list []string) map[string]bool {
	s := make(map[string]bool, len(list))
	for _, v := range list {
		s[v] = true
	}
	return s
}

func union(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// pick is the entry id names ("" = the newest).
func pick(root, id string) (string, Entry, error) {
	dir, _ := storeDir(root)
	list := entries(dir)
	if len(list) == 0 {
		return dir, Entry{}, errNoEntry
	}
	if id == "" {
		return dir, list[len(list)-1], nil
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return dir, Entry{}, errNoEntry
	}
	for _, e := range list {
		if e.ID == n {
			return dir, e, nil
		}
	}
	return dir, Entry{}, errNoEntry
}

func forget(dir string, id int) {
	for _, ext := range []string{".patch", ".json"} {
		_ = os.Remove(filepath.Join(dir, fmt.Sprintf("%d%s", id, ext)))
	}
}

// Apply re-applies entry id ("" = the newest), falling back to a 3-way
// apply when the tree moved since the push. With drop (pop) the entry is
// deleted, but only after a clean apply; a conflicted one is kept.
func Apply(root, id string, drop bool) (Result, error) {
	dir, e, err := pick(root, id)
	if err != nil {
		return Result{Status: "error", Reason: err.Error()}, nil
	}
	patch := filepath.Join(dir, fmt.Sprintf("%d.patch", e.ID))
	status := "ok"
	if _, err := gitx.Run(root, "apply", "--binary", patch); err != nil {
		if _, err := gitx.Run(root, "apply", "--binary", "--3way", patch); err != nil {
			return Result{Status: "conflicted", ID: e.ID, Files: e.Files,
				Note: fmt.Sprintf("entry kept; resolve the markers, then `agent-exec shelf drop %d`", e.ID)}, nil
		}
		_, _ = gitx.Run(root, append([]string{"reset", "-q", "--"}, e.Files...)...)
		status = "ok-3way"
	}
	if drop {
		forget(dir, e.ID)
	}
	return Result{Status: status, ID: e.ID, Files: e.Files, Dropped: drop, kind: "apply"}, nil
}

// Drop deletes entry id ("" = the newest).
func Drop(root, id string) (Result, error) {
	dir, e, err := pick(root, id)
	if err != nil {
		return Result{Status: "error", Reason: err.Error()}, nil
	}
	forget(dir, e.ID)
	return Result{Status: "ok", ID: e.ID, kind: "drop"}, nil
}
