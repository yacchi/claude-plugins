package isolate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// fakeGtr puts a stand-in git-worktree-runner first on PATH, so the tests do
// not depend on (or touch) a real gtr installed on the machine.
func fakeGtr(t *testing.T) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	writeFile(t, filepath.Join(bin, "git-gtr"), `#!/bin/sh
# A stand-in for git-worktree-runner: version, new <branch> (into
# $FAKE_GTR_DIR when set, else the lent gtr.worktrees.dir) and go <branch>.
case "$1" in
version)
	echo "fake-gtr 0.0.0"
	;;
new)
	dir=${FAKE_GTR_DIR:-$(git config --get gtr.worktrees.dir)}
	[ -n "$dir" ] || exit 1
	name=$(printf '%s' "$2" | tr '/' '-')
	mkdir -p "$dir" || exit 1
	git worktree add -q -b "$2" "$dir/$name" HEAD >/dev/null 2>&1 || exit 1
	;;
go)
	git worktree list --porcelain | awk -v want="branch refs/heads/$2" '/^worktree /{p=substr($0,10)} $0==want{print p}'
	;;
*)
	exit 1
	;;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "git-gtr"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGtrTreeLandsInLentDir(t *testing.T) {
	fakeGtr(t)
	repo := newRepo(t)

	res, err := Create(repo, "t1", CreateOptions{Backend: BackendGtr})
	if err != nil || res.Status != "created" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	if res.Backend != BackendGtr {
		t.Fatalf("backend %s, want gtr: the lent dir was not honoured", res.Backend)
	}
	if paths.RealPath(filepath.Dir(res.Path)) != paths.RealPath(Home(repo)) {
		t.Fatalf("tree at %s, want a child of %s", res.Path, Home(repo))
	}
	if filepath.Base(res.Path) != "orchestra-t1" {
		t.Fatalf("tree named %s, want orchestra-t1", filepath.Base(res.Path))
	}
}

func TestGtrTreeElsewhereIsDiscarded(t *testing.T) {
	fakeGtr(t)
	repo := newRepo(t)
	elsewhere := filepath.Join(createDirTemp(t), "elsewhere")
	t.Setenv("FAKE_GTR_DIR", elsewhere)

	res, err := Create(repo, "t1", CreateOptions{Backend: BackendGtr})
	if err != nil || res.Status != "created" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	if res.Backend != BackendGit {
		t.Fatalf("backend %s, want git after gtr put the tree elsewhere", res.Backend)
	}
	if want := filepath.Join(Home(repo), "t1"); res.Path != want {
		t.Fatalf("tree at %s, want %s", res.Path, want)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "orchestra-t1")); !os.IsNotExist(err) {
		t.Fatalf("gtr's tree in %s was not discarded: %v", elsewhere, err)
	}
	list, err := gitx.Run(repo, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "worktree "+paths.RealPath(elsewhere)+"/") {
		t.Fatalf("worktree list still names a tree in %s:\n%s", elsewhere, list)
	}
}
