package isolate

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mergeTrees plants the hierarchy asm (assembly) > feat (integration) > t1
// (work) on landRepo's repository.
func mergeTrees(t *testing.T) (repo, asm, feat, t1 string) {
	t.Helper()
	repo = landRepo(t)
	asm = plantTree(t, repo, "asm", RoleAssembly, "")
	feat = plantTree(t, asm, "feat", RoleIntegration, asm)
	t1 = plantTree(t, feat, "t1", "", feat)
	return repo, asm, feat, t1
}

// mergeIntegrate simulates integrating work tree task into feat: content is
// written into the work tree and collected there, then committed in feat the
// way the integrator does.
func mergeIntegrate(t *testing.T, repo, work, task, feat, file, content string) {
	t.Helper()
	writeFile(t, filepath.Join(work, file), content)
	if res, err := DiffAndCollect(repo, task); err != nil || res.Status != "ok" {
		t.Fatalf("collect %s: %+v, %v", task, res, err)
	}
	writeFile(t, filepath.Join(feat, file), content)
	landCommit(t, feat, "orchestra integrate "+task)
}

func mergeRun(t *testing.T, repo, task, msg string) MergeResult {
	t.Helper()
	res, err := Merge(repo, task, msg)
	if err != nil {
		t.Fatalf("merge %s: %v", task, err)
	}
	return res
}

func mergeHead(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

func TestMergeSquashesIntoAssemblyAndRemovesTheTree(t *testing.T) {
	repo, asm, feat, t1 := mergeTrees(t)
	userHead := mergeHead(t, repo)
	asmHead := mergeHead(t, asm)
	mergeIntegrate(t, repo, t1, "t1", feat, "a.txt", "one\nTWO\nthree\nfour\nfive\n")

	res := mergeRun(t, repo, "feat", "feat: change two\n\nbody\n")
	if res.Status != "merged" || res.Assembly != "asm" || !res.Removed || res.Reason != "" ||
		!slices.Equal(res.Files, []string{"a.txt"}) {
		t.Fatalf("merge: %+v", res)
	}
	if got := mergeHead(t, asm); got != res.Commit {
		t.Errorf("assembly HEAD = %s, want the merge commit %s", got, res.Commit)
	}
	if got := strings.TrimSpace(git(t, asm, "log", "-1", "--format=%s")); got != "feat: change two" {
		t.Errorf("assembly subject = %q", got)
	}
	if got := strings.TrimSpace(git(t, asm, "rev-parse", "HEAD^")); got != asmHead {
		t.Errorf("assembly gained more than one commit: parent %s, want %s", got, asmHead)
	}
	if got := readFile(t, filepath.Join(asm, "a.txt")); got != "one\nTWO\nthree\nfour\nfive\n" {
		t.Errorf("assembly a.txt = %q", got)
	}
	for _, p := range []string{feat, t1} {
		if exists(p) {
			t.Errorf("%s still exists", p)
		}
	}
	if got := mergeHead(t, repo); got != userHead {
		t.Errorf("user checkout moved: %s, want %s", got, userHead)
	}
	b, err := res.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["removed"] != true || m["commit"] != res.Commit || m["reason"] != nil {
		t.Errorf("JSON = %s, %v", b, err)
	}
}

func TestMergeKeepsTreeWithUnintegratedChild(t *testing.T) {
	repo, asm, feat, t1 := mergeTrees(t)
	t2 := plantTree(t, feat, "t2", "", feat)
	mergeIntegrate(t, repo, t1, "t1", feat, "a.txt", "one\nTWO\nthree\nfour\nfive\n")
	writeFile(t, filepath.Join(t2, "b.txt"), "uncollected\n")
	asmHead := mergeHead(t, asm)

	res := mergeRun(t, repo, "feat", "feat: change two")
	if res.Status != "has-children" || len(res.Children) != 1 || res.Children[0].Task != "t2" {
		t.Fatalf("merge: %+v", res)
	}
	if got := mergeHead(t, asm); got != asmHead {
		t.Errorf("assembly HEAD moved to %s", got)
	}
	if !exists(feat) || !exists(t2) {
		t.Errorf("feat or t2 was removed")
	}
	if !strings.HasPrefix(FormatMergeText(res), "has-children: ") || !strings.HasSuffix(FormatMergeText(res), ": t2") {
		t.Errorf("text = %q", FormatMergeText(res))
	}
}

func TestMergeEmptyIntegrationTreeIsRemoved(t *testing.T) {
	repo, asm, _, _ := mergeTrees(t)
	fresh := plantTree(t, asm, "fresh", RoleIntegration, asm)
	asmHead := mergeHead(t, asm)

	res := mergeRun(t, repo, "fresh", "feat: nothing")
	if res.Status != "empty" || !res.Removed {
		t.Fatalf("merge: %+v", res)
	}
	if exists(fresh) {
		t.Errorf("empty integration tree kept")
	}
	if got := mergeHead(t, asm); got != asmHead {
		t.Errorf("assembly HEAD moved to %s", got)
	}
}

func TestMergeRefusals(t *testing.T) {
	repo, asm, feat, t1 := mergeTrees(t)
	plantTree(t, repo, "lone", RoleIntegration, "")
	mergeIntegrate(t, repo, t1, "t1", feat, "a.txt", "one\nTWO\nthree\nfour\nfive\n")
	asmHead := mergeHead(t, asm)

	for _, c := range []struct{ task, msg, reason string }{
		{"asm", "feat: x", "asm is not an integration tree"},
		{"t1", "feat: x", "t1 is not an integration tree"},
		{"lone", "feat: x", "integration tree lone has no assembly to merge into"},
		{"feat", "orchestra integrate x", landOwnReason},
	} {
		res := mergeRun(t, repo, c.task, c.msg)
		if res.Status != "refused" || !strings.Contains(res.Reason, c.reason) {
			t.Errorf("merge %s %q: %+v", c.task, c.msg, res)
		}
	}
	if _, err := Merge(repo, "feat", "  \n"); err == nil {
		t.Errorf("empty message: no error")
	}
	if got := mergeHead(t, asm); got != asmHead || !exists(feat) {
		t.Errorf("a refusal changed the assembly or removed feat")
	}
}

func TestMergeConflictKeepsEverything(t *testing.T) {
	repo, asm, feat, t1 := mergeTrees(t)
	mergeIntegrate(t, repo, t1, "t1", feat, "a.txt", "one\nTWO\nthree\nfour\nfive\n")
	writeFile(t, filepath.Join(asm, "a.txt"), "one\ndeux\nthree\nfour\nfive\n")
	landCommit(t, asm, "feat: deux")
	asmHead := mergeHead(t, asm)

	res := mergeRun(t, repo, "feat", "feat: change two")
	if res.Status != "conflict" || !slices.Equal(res.Files, []string{"a.txt"}) ||
		!strings.Contains(res.Reason, "merge the assembly's branch orchestra/asm") {
		t.Fatalf("merge: %+v", res)
	}
	if got := mergeHead(t, asm); got != asmHead {
		t.Errorf("assembly HEAD moved to %s", got)
	}
	if !exists(feat) {
		t.Errorf("feat was removed")
	}
}

func TestMergeDirtyIntegrationTree(t *testing.T) {
	repo, _, feat, t1 := mergeTrees(t)
	mergeIntegrate(t, repo, t1, "t1", feat, "a.txt", "one\nTWO\nthree\nfour\nfive\n")
	writeFile(t, filepath.Join(feat, "b.txt"), "pending\n")

	res := mergeRun(t, repo, "feat", "feat: change two")
	if res.Status != "dirty" || !slices.Equal(res.Files, []string{"b.txt"}) {
		t.Fatalf("merge: %+v", res)
	}
	if !exists(feat) {
		t.Errorf("feat was removed")
	}
}
