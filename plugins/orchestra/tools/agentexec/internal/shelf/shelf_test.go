package shelf

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct{ repo, w1, w2 string }

func setup(t *testing.T) fixture {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{repo: filepath.Join(tmp, "repo"), w1: filepath.Join(tmp, "w1"), w2: filepath.Join(tmp, "w2")}
	write(t, filepath.Join(f.repo, "src/app.ts"), "old\n")
	write(t, filepath.Join(f.repo, "src/keep.ts"), "keep\n")
	logo := make([]byte, 256)
	for i := range logo {
		logo[i] = byte(i)
	}
	writeBytes(t, filepath.Join(f.repo, "logo.bin"), logo)
	git(t, f.repo, "init", "-q", "-b", "main")
	git(t, f.repo, "add", "-A")
	git(t, f.repo, "commit", "-q", "-m", "init")
	git(t, f.repo, "worktree", "add", "-q", "-b", "orchestra/s/t1", f.w1)
	git(t, f.repo, "worktree", "add", "-q", "-b", "orchestra/s/t2", f.w2)
	return f
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) { writeBytes(t, path, []byte(content)) }

func writeBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func push(t *testing.T, root string, paths []string, keepIndex bool) Result {
	t.Helper()
	r, err := Push(root, paths, "", keepIndex)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func apply(t *testing.T, root, id string, drop bool) Result {
	t.Helper()
	r, err := Apply(root, id, drop)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func entryIDs(t *testing.T, root string) []int {
	t.Helper()
	list, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int{}
	for _, e := range list {
		ids = append(ids, e.ID)
	}
	return ids
}

func TestRoundTripRestoresTrackedUntrackedAndBinary(t *testing.T) {
	f := setup(t)
	write(t, filepath.Join(f.w1, "src/app.ts"), "fix\n")
	write(t, filepath.Join(f.w1, "src/new.test.ts"), "test\n")
	reversed := make([]byte, 256)
	for i := range reversed {
		reversed[i] = byte(255 - i)
	}
	writeBytes(t, filepath.Join(f.w1, "logo.bin"), reversed)
	r, err := Push(f.w1, nil, "check the test fails without the fix", false)
	if err != nil || r.Status != "ok" {
		t.Fatalf("%+v %v", r, err)
	}
	if st := git(t, f.w1, "status", "--porcelain"); st != "" {
		t.Fatalf("tree not back at HEAD (the store must not land in it): %q", st)
	}
	list, _ := List(f.w1)
	if len(list) != 1 || list[0].Message != "check the test fails without the fix" || list[0].Created == 0 {
		t.Fatalf("entry %+v", list)
	}
	if r := apply(t, f.w1, "", true); r.Status != "ok" || !r.Dropped {
		t.Fatalf("pop %+v", r)
	}
	if read(t, filepath.Join(f.w1, "src/app.ts")) != "fix\n" ||
		read(t, filepath.Join(f.w1, "src/new.test.ts")) != "test\n" ||
		read(t, filepath.Join(f.w1, "logo.bin")) != string(reversed) {
		t.Fatal("not restored")
	}
	if ids := entryIDs(t, f.w1); len(ids) != 0 {
		t.Fatalf("entry kept after pop: %v", ids)
	}
}

func TestWorktreesNeverSeeEachOthersEntries(t *testing.T) {
	f := setup(t)
	write(t, filepath.Join(f.w1, "src/app.ts"), "from w1\n")
	write(t, filepath.Join(f.w2, "src/app.ts"), "from w2\n")
	push(t, f.w1, nil, false)
	push(t, f.w2, nil, false)
	if ids := entryIDs(t, f.w1); len(ids) != 1 {
		t.Fatalf("w1 sees %v", ids)
	}
	apply(t, f.w1, "", true)
	apply(t, f.w2, "", true)
	if read(t, filepath.Join(f.w1, "src/app.ts")) != "from w1\n" || read(t, filepath.Join(f.w2, "src/app.ts")) != "from w2\n" {
		t.Fatal("work swapped between worktrees")
	}
	if s := git(t, f.repo, "stash", "list"); s != "" {
		t.Fatalf("refs/stash touched: %q", s)
	}
}

func TestPathspecShelvesOnlyThosePaths(t *testing.T) {
	f := setup(t)
	write(t, filepath.Join(f.w1, "src/app.ts"), "fix\n")
	write(t, filepath.Join(f.w1, "src/keep.ts"), "also changed\n")
	r := push(t, f.w1, []string{"src/app.ts"}, false)
	if strings.Join(r.Files, ",") != "src/app.ts" {
		t.Fatalf("%+v", r)
	}
	if read(t, filepath.Join(f.w1, "src/app.ts")) != "old\n" || read(t, filepath.Join(f.w1, "src/keep.ts")) != "also changed\n" {
		t.Fatal("pathspec not honored")
	}
	apply(t, f.w1, "", true)
	if read(t, filepath.Join(f.w1, "src/app.ts")) != "fix\n" {
		t.Fatal("not restored")
	}
}

func TestKeepIndexLeavesTheStagedState(t *testing.T) {
	f := setup(t)
	write(t, filepath.Join(f.w1, "src/app.ts"), "commit one\n")
	git(t, f.w1, "add", "src/app.ts")
	write(t, filepath.Join(f.w1, "src/keep.ts"), "commit two\n")
	write(t, filepath.Join(f.w1, "src/extra.ts"), "commit two, new\n")
	if r := push(t, f.w1, nil, true); r.Status != "ok" {
		t.Fatalf("%+v", r)
	}
	if read(t, filepath.Join(f.w1, "src/app.ts")) != "commit one\n" || read(t, filepath.Join(f.w1, "src/keep.ts")) != "keep\n" {
		t.Fatal("staged state not kept / unstaged not shelved")
	}
	if _, err := os.Stat(filepath.Join(f.w1, "src/extra.ts")); err == nil {
		t.Fatal("untracked file not shelved")
	}
	git(t, f.w1, "commit", "-q", "-m", "one")
	apply(t, f.w1, "", true)
	if read(t, filepath.Join(f.w1, "src/keep.ts")) != "commit two\n" || read(t, filepath.Join(f.w1, "src/extra.ts")) != "commit two, new\n" {
		t.Fatal("not restored on top of the commit")
	}
}

func TestStagedNewFileIsShelvedAndRestored(t *testing.T) {
	f := setup(t)
	write(t, filepath.Join(f.w1, "src/staged.ts"), "staged new\n")
	git(t, f.w1, "add", "src/staged.ts")
	push(t, f.w1, nil, false)
	if st := git(t, f.w1, "status", "--porcelain"); st != "" {
		t.Fatalf("status %q", st)
	}
	apply(t, f.w1, "", true)
	if read(t, filepath.Join(f.w1, "src/staged.ts")) != "staged new\n" {
		t.Fatal("not restored")
	}
}

func TestEmptyPushLeavesNothing(t *testing.T) {
	f := setup(t)
	if r := push(t, f.w1, nil, false); r.Status != "empty" {
		t.Fatalf("%+v", r)
	}
	if ids := entryIDs(t, f.w1); len(ids) != 0 {
		t.Fatalf("%v", ids)
	}
}

func TestConflictingPopKeepsTheEntry(t *testing.T) {
	f := setup(t)
	write(t, filepath.Join(f.w1, "src/app.ts"), "mine\n")
	push(t, f.w1, nil, false)
	write(t, filepath.Join(f.w1, "src/app.ts"), "someone else\n")
	git(t, f.w1, "commit", "-q", "-am", "moved on")
	r := apply(t, f.w1, "", true)
	if r.Status != "conflicted" || r.Note != "entry kept; resolve the markers, then `agent-exec shelf drop 0`" {
		t.Fatalf("%+v", r)
	}
	if ids := entryIDs(t, f.w1); len(ids) != 1 {
		t.Fatalf("entry dropped: %v", ids)
	}
}

func TestStackApplyKeepsDropRemovesAndIDsAddress(t *testing.T) {
	f := setup(t)
	write(t, filepath.Join(f.w1, "src/app.ts"), "first\n")
	push(t, f.w1, nil, false)
	write(t, filepath.Join(f.w1, "src/keep.ts"), "second\n")
	push(t, f.w1, nil, false)
	if r := apply(t, f.w1, "0", true); r.ID != 0 || read(t, filepath.Join(f.w1, "src/app.ts")) != "first\n" {
		t.Fatalf("pop 0: %+v", r)
	}
	if ids := entryIDs(t, f.w1); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("left %v", ids)
	}
	if r := apply(t, f.w1, "", false); r.Status != "ok" || r.Dropped || len(entryIDs(t, f.w1)) != 1 {
		t.Fatalf("apply kept: %+v", r)
	}
	if r, _ := Drop(f.w1, "9"); r.Status != "error" || r.Reason != "no such shelf entry" {
		t.Fatalf("missing id: %+v", r)
	}
	if r, _ := Drop(f.w1, ""); r.Status != "ok" || r.ID != 1 || len(entryIDs(t, f.w1)) != 0 {
		t.Fatalf("drop: %+v", r)
	}
	if r, _ := Apply(f.w1, "", true); r.Status != "error" {
		t.Fatalf("empty shelf: %+v", r)
	}
}

func TestOnlyLinkedWorktreesCountAsLinked(t *testing.T) {
	f := setup(t)
	if IsLinkedWorktree(f.repo) || !IsLinkedWorktree(f.w1) {
		t.Fatal("main/linked detection wrong")
	}
}
