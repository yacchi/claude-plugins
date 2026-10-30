package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintSkeletonDeletionReadsBrief(t *testing.T) {
	const del = `{"id":"del","spec":"s/del.md","cls":"light","brief":"Delete the old parser (x/p.go:10-80)",` +
		`"risks":[],"est_seconds":60,"files_owned":[%s],"verification":"go test ./x/..."}`
	for _, tc := range []struct {
		owned string
		want  int
	}{{`"x/p.go"`, 1}, {`"x/**"`, 0}} {
		file := fixture(t, `{"commit_message":"feat: x","packages":[`+strings.Replace(del, "%s", tc.owned, 1)+`]}`)
		p, err := LoadSkeleton(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := LintSkeleton(p); len(got) != tc.want {
			t.Errorf("files_owned %s: got %+v, want %d warnings", tc.owned, got, tc.want)
		}
	}
}

func TestLintSpecLengthWarnedOnce(t *testing.T) {
	file := fixture(t, `{"packages":[{"id":"a","spec":"s/a.md","cls":"light"}]}`, "s/a.md")
	long := strings.Repeat("x\n", 151)
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "s/a.md"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	got := Lint(p, "")
	if len(got) != 1 || !strings.Contains(got[0].Message, "spec is 151 lines") {
		t.Errorf("Lint: got %+v, want one spec-length warning", got)
	}
}

func TestLintSkeletonUnusedInterface(t *testing.T) {
	file := fixture(t, `{"commit_message":"feat: x","interfaces":[{"id":"i","path":"contracts/i.json"},{"id":"j","path":"contracts/j.json"}],`+
		`"packages":[{"id":"a","spec":"s/a.md","cls":"light","brief":"b","risks":[],"est_seconds":60,`+
		`"files_owned":["x.go"],"verification":"go test ./x/...","interfaces":["i"]}]}`,
		"contracts/i.json", "contracts/j.json")
	p, err := LoadSkeleton(file)
	if err != nil {
		t.Fatal(err)
	}
	got := LintSkeleton(p)
	if len(got) != 1 || got[0].Pkg != nil || !strings.HasPrefix(got[0].Message, "interface j is used by no package") {
		t.Errorf("LintSkeleton: got %+v, want one plan-level warning for j", got)
	}
}

func TestLintSkeletonCommitMessage(t *testing.T) {
	const pkg = `{"id":"a","spec":"s/a.md","cls":"light","brief":"b","risks":[],"est_seconds":60,` +
		`"files_owned":["x.go"],"verification":"go test ./x/..."}`
	for _, tc := range []struct {
		name, head string
		want       int
	}{
		{"absent", ``, 1},
		{"blank", `"commit_message":"   ",`, 1},
		{"set", `"commit_message":"feat: x\n\nbody",`, 0},
	} {
		p, err := LoadSkeleton(fixture(t, `{`+tc.head+`"packages":[`+pkg+`]}`))
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if got := LintSkeleton(p); len(got) != tc.want {
			t.Errorf("%s: got %+v, want %d warnings", tc.name, got, tc.want)
		}
	}
	_, err := Load(fixture(t, `{"commit_message":["a"],"packages":[`+pkg+`]}`))
	if err == nil || err.Error() != "plan commit_message must be a string" {
		t.Errorf("non-string commit_message: err = %v", err)
	}
}
