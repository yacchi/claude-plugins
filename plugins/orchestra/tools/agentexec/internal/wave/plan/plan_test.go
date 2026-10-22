package plan

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture writes a plan and the spec files it names under a temp dir.
func fixture(t *testing.T, planJSON string, specs ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, s := range specs {
		p := filepath.Join(dir, s)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# spec\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(p, []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadErr(t *testing.T, planJSON string, specs ...string) string {
	t.Helper()
	_, err := Load(fixture(t, planJSON, specs...))
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *plan.Error, got %v", err)
	}
	return pe.Msg
}

func TestLoadValidation(t *testing.T) {
	for _, c := range []struct {
		name, plan, want string
		specs            []string
	}{
		{"invalid json", `{nope`, "invalid JSON in plan ", nil},
		{"missing packages", `{"rules": []}`, "missing required key 'packages'", nil},
		{"missing id", `{"packages":[{"spec":"a.md","cls":"light"}]}`, "package missing required field 'id': {\"cls\":\"light\",\"spec\":\"a.md\"}", []string{"a.md"}},
		{"missing spec", `{"packages":[{"id":"a","cls":"light"}]}`, "a: missing required field 'spec'", nil},
		{"missing cls", `{"packages":[{"id":"a","spec":"a.md"}]}`, "a: missing required field 'cls'", []string{"a.md"}},
		{"bad cls", `{"packages":[{"id":"a","spec":"a.md","cls":"huge"}]}`, "a: invalid cls \"huge\" (must be one of light, standard, deep)", []string{"a.md"}},
		{"duplicate", `{"packages":[{"id":"a","spec":"a.md","cls":"light"},{"id":"a","spec":"a.md","cls":"light"}]}`, "duplicate package id: a", []string{"a.md"}},
		{"unknown dep", `{"packages":[{"id":"a","spec":"a.md","cls":"light","depends_on":["x"]}]}`, "a: depends_on \"x\" is unknown", []string{"a.md"}},
		{"cycle", `{"packages":[{"id":"a","spec":"a.md","cls":"light","depends_on":["b"]},{"id":"b","spec":"a.md","cls":"light","depends_on":["a"]}]}`, "dependency cycle: a -> b -> a", []string{"a.md"}},
		{"spec gone", `{"packages":[{"id":"a","spec":"nope.md","cls":"light"}]}`, "a: spec path does not exist: ", nil},
		{"preamble gone", `{"preamble":["nope.md"],"packages":[]}`, "preamble path does not exist: ", nil},
		{"warm abs", `{"warm_read":["/etc/passwd"],"packages":[]}`, "warm_read path must be repo-relative and stay inside the repo: /etc/passwd", nil},
		{"warm up", `{"warm_read":["a/../../x"],"packages":[]}`, "warm_read path must be repo-relative and stay inside the repo: a/../../x", nil},
		{"warm type", `{"warm_read":"x","packages":[]}`, "plan warm_read must be a list of repo-relative paths", nil},
		{"rules shape", `{"rules":[{"if":"x"}],"packages":[]}`, "plan rules entries must be {'if': <glob>, 'require': [<glob>...]}", nil},
		{"rules type", `{"rules":"x","packages":[]}`, "plan rules must be a list", nil},
	} {
		if msg := loadErr(t, c.plan, c.specs...); !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q does not contain %q", c.name, msg, c.want)
		}
	}
}

func TestLoadDefaultsAndResolution(t *testing.T) {
	path := fixture(t, `{
	  "preamble": ["pre.md"],
	  "external_done": ["EXT"],
	  "warm_read": ["./docs/../docs/g.md", "docs/g.md", "README.md"],
	  "rules": [{"if": "src/**", "require": ["tests/**"]}],
	  "packages": [
	    {"id": "A", "spec": "specs/a.md", "cls": "deep", "unknown": 1, "est_seconds": 240},
	    {"id": "B", "spec": "specs/a.md", "cls": "light", "depends_on": ["A", "EXT"], "files_owned": ["x"], "prio": 3}
	  ]}`, "pre.md", "specs/a.md")
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if p.Packages[0].Spec != filepath.Join(dir, "specs/a.md") || p.Preamble[0] != filepath.Join(dir, "pre.md") {
		t.Fatalf("paths not resolved: %+v %v", p.Packages[0], p.Preamble)
	}
	if !reflect.DeepEqual(p.WarmRead, []string{"docs/g.md", "README.md"}) {
		t.Fatalf("warm_read: %v", p.WarmRead)
	}
	a, b := p.Packages[0], p.Packages[1]
	if len(a.DependsOn) != 0 || len(a.FilesOwned) != 0 || a.Prio != 0 || b.Prio != 3 || a.EstSeconds != 240 || b.EstSeconds != 0 {
		t.Fatalf("defaults: %+v %+v", a, b)
	}
	if !reflect.DeepEqual(p.Rules, []Rule{{If: "src/**", Require: []string{"tests/**"}}}) || p.ExternalDone[0] != "EXT" {
		t.Fatalf("rules/external: %+v %v", p.Rules, p.ExternalDone)
	}
}

func TestStemAndOverlaps(t *testing.T) {
	for in, want := range map[string]string{
		"src/**": "src", "src/a.ts": "src/a.ts", "./src/a/b.ts": "src/a/b.ts", "src/a/": "src/a", "*.md": "",
	} {
		if got := Stem(in); got != want {
			t.Errorf("Stem(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"src/**", "src/a.ts", true},
		{"src/a", "src/ab", false},
		{"src/a/", "./src/a/b.ts", true},
		{"*.md", "src/whatever/x.ts", true},
		{"src/a/**", "lib/b/**", false},
		{"src/a.ts", "src/a.ts", true},
	} {
		if got := Overlaps([]string{c.a}, []string{c.b}); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func mk(pkgs ...Package) *Plan { return &Plan{Packages: pkgs} }

func P(id string, deps []string, files []string, prio float64) Package {
	return Package{ID: id, DependsOn: deps, FilesOwned: files, Prio: prio}
}

func TestBlocked(t *testing.T) {
	p := mk(P("A", nil, nil, 0), P("B", []string{"A"}, nil, 0), P("C", []string{"B"}, nil, 0))
	got := Blocked(p, Status{"A": "failed"})
	if !reflect.DeepEqual(got, map[string]string{"B": "A", "C": "B"}) {
		t.Fatalf("transitive: %v", got)
	}
	if got := Blocked(p, Status{"A": "needs"}); len(got) != 2 {
		t.Fatalf("needs: %v", got)
	}
	if got := Blocked(p, Status{"A": "integrated"}); len(got) != 0 {
		t.Fatalf("integrated: %v", got)
	}
	if got := Blocked(p, nil); len(got) != 0 {
		t.Fatalf("missing counts as pending: %v", got)
	}
	ext := mk(P("B", []string{"EXT"}, nil, 0))
	if got := Blocked(ext, Status{"EXT": "failed"}); len(got) != 0 {
		t.Fatalf("external dependency never blocks: %v", got)
	}
}

func TestSelect(t *testing.T) {
	ab := mk(P("A", nil, nil, 0), P("B", []string{"A"}, nil, 0))
	check := func(name string, p *Plan, st Status, limit int, want ...string) {
		t.Helper()
		got := Select(p, st, limit)
		if len(got) == 0 && len(want) == 0 {
			return
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	check("deps", ab, nil, 10, "A")
	check("integrated unblocks", ab, Status{"A": "integrated"}, 10, "B")
	check("external done", &Plan{Packages: []Package{P("B", []string{"EXT"}, nil, 0)}, ExternalDone: []string{"EXT"}}, nil, 10, "B")
	check("blocked excluded", ab, Status{"A": "failed"}, 10)
	check("prio", mk(P("A", nil, nil, 0), P("B", nil, nil, 5), P("C", nil, nil, 1)), nil, 10, "B", "C", "A")
	check("plan order", mk(P("A", nil, nil, 0), P("B", nil, nil, 0)), nil, 10, "A", "B")
	ov := mk(P("A", nil, []string{"src/a/**"}, 0), P("B", nil, []string{"src/a/x.ts"}, 0))
	check("overlap in flight", ov, Status{"A": "implementing"}, 10)
	check("overlap among candidates", ov, nil, 10, "A")
	three := mk(P("A", nil, nil, 0), P("B", nil, nil, 0), P("C", nil, nil, 0))
	check("limit minus in flight", three, Status{"A": "implementing"}, 2, "B")
	check("never negative", three, Status{"A": "implementing", "B": "implementing"}, 1)
	check("zero", three, nil, 0)
	check("ready counts as in flight", three, Status{"A": "ready"}, 1)
	check("needs is not pending", three, Status{"A": "needs"}, 10, "B", "C")
}

func TestSelectCriticalPathOrder(t *testing.T) {
	// Diamond under D (D -> E, F -> G) and a longer chain under C
	// (C -> H -> I -> J); A and B have nothing below them.
	shapes := []Package{
		P("A", nil, nil, 0), P("B", nil, nil, 0), P("C", nil, nil, 0), P("D", nil, nil, 0),
		P("E", []string{"D"}, nil, 0), P("F", []string{"D"}, nil, 0), P("G", []string{"E", "F"}, nil, 0),
		P("H", []string{"C"}, nil, 0), P("I", []string{"H"}, nil, 0), P("J", []string{"I"}, nil, 0),
	}
	withPrio := append([]Package(nil), shapes...)
	withPrio[1].Prio = 1
	allTimed := append([]Package(nil), shapes...)
	for i := range allTimed {
		allTimed[i].EstSeconds = 10
	}
	allTimed[4].EstSeconds = 100 // the diamond's E branch outweighs the chain
	partlyTimed := append([]Package(nil), allTimed...)
	partlyTimed[9].EstSeconds = 0
	for _, c := range []struct {
		name string
		pkgs []Package
		want []string
	}{
		{"longest chain first, ties by plan order", shapes, []string{"C", "D", "A", "B"}},
		{"prio still wins", withPrio, []string{"B", "C", "D", "A"}},
		{"est_seconds sums when every package has one", allTimed, []string{"D", "C", "A", "B"}},
		{"counts when one package lacks est_seconds", partlyTimed, []string{"C", "D", "A", "B"}},
	} {
		if got := Select(mk(c.pkgs...), nil, 10); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	chain := CriticalPath(mk(shapes...))
	if chain["D"] != 2 || chain["C"] != 3 || chain["G"] != 0 || chain["E"] != 1 {
		t.Errorf("chain lengths: %v", chain)
	}
}

func TestCriticalPathIsComputedOncePerPlan(t *testing.T) {
	p := mk(P("A", nil, nil, 0), P("B", []string{"A"}, nil, 0))
	CriticalPath(p)["A"] = 99 // a cached map comes back as is
	if CriticalPath(p)["A"] != 99 {
		t.Fatal("recomputed although the plan did not change")
	}
	p.Packages = append(p.Packages, P("C", []string{"B"}, nil, 0))
	if got := CriticalPath(p); got["A"] != 2 || got["B"] != 1 {
		t.Fatalf("not recomputed after an appended package: %v", got)
	}
}

func TestSummary(t *testing.T) {
	p := mk(P("A", nil, nil, 0), P("B", []string{"A"}, nil, 0), P("C", []string{"A"}, nil, 0))
	s := Summary(p, Status{"A": "pending", "B": "integrated"})
	for _, st := range Statuses {
		if _, ok := s[st]; !ok {
			t.Errorf("missing %s", st)
		}
	}
	if s["pending"] != 2 || s["integrated"] != 1 || s["total"] != 3 || s["runnable"] != 1 {
		t.Fatalf("%v", s)
	}
	s = Summary(p, Status{"A": "failed"})
	if s["failed"] != 1 || s["blocked"] != 2 || s["runnable"] != 0 {
		t.Fatalf("blocked: %v", s)
	}
}

func TestTopoOrder(t *testing.T) {
	if got := TopoOrder(mk(P("B", nil, nil, 0), P("A", nil, nil, 0))); !reflect.DeepEqual(got, []string{"B", "A"}) {
		t.Fatalf("ties by plan order: %v", got)
	}
	chain := mk(P("C", []string{"B"}, nil, 0), P("B", []string{"A"}, nil, 0), P("A", nil, nil, 0))
	if got := TopoOrder(chain); !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
		t.Fatalf("chain: %v", got)
	}
	ext := mk(P("B", []string{"EXT", "A"}, nil, 0), P("A", nil, nil, 0))
	if got := TopoOrder(ext); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("external: %v", got)
	}
}

func TestLint(t *testing.T) {
	dir := t.TempDir()
	spec := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	long := strings.Repeat("x\n", 151)
	p := &Plan{
		WarmRead: []string{"README.md", "gone.md"},
		Rules:    []Rule{{If: "src/**", Require: []string{"tests/**"}}},
		Packages: []Package{
			{ID: "many", Spec: spec("a.md", "ok\n"), FilesOwned: []string{"a", "b", "c", "d"}},
			{ID: "three", Spec: spec("b.md", "ok\n"), FilesOwned: []string{"a", "b", "c"}},
			{ID: "long", Spec: spec("c.md", long)},
			{ID: "edge", Spec: spec("d.md", strings.Repeat("x\n", 150))},
			{ID: "del", Spec: spec("e.md", "Delete the old thing\n"), FilesOwned: []string{"old/a.go"}},
			{ID: "del-glob", Spec: spec("f.md", "remove it\n"), FilesOwned: []string{"old/**"}},
			{ID: "ja", Spec: spec("g.md", "旧実装を削除\n"), FilesOwned: []string{"x.go"}},
			{ID: "binary", Spec: spec("h.md", "\xff\xfedelete\n"), FilesOwned: []string{"x.go"}},
			{ID: "nospec", Spec: filepath.Join(dir, "missing.md"), FilesOwned: []string{"x.go"}},
			{ID: "rule", Spec: spec("i.md", "ok\n"), FilesOwned: []string{"src/a.ts"}},
			{ID: "rule-ok", Spec: spec("j.md", "ok\n"), FilesOwned: []string{"src/a.ts", "tests/a.ts"}},
		},
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var order []string
	for _, w := range Lint(p, dir) {
		key := "-"
		if w.Pkg != nil {
			key = *w.Pkg
		}
		got[key] = w.Message
		order = append(order, key)
	}
	if want := []string{"-", "many", "long", "del", "ja", "rule"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("warnings for %v, want %v: %v", order, want, got)
	}
	if got["-"] != "warm_read file does not exist in the repo: gone.md" ||
		!strings.Contains(got["many"], "owns 4 files; split into packages of 1-3 files") ||
		!strings.Contains(got["long"], "spec is 151 lines") ||
		got["rule"] != "files_owned matching src/** are missing required ownership: tests/**" {
		t.Fatalf("messages: %v", got)
	}
	if n := len(Lint(p, "")); n != 5 {
		t.Fatalf("without a root the warm_read warning is dropped: %d", n)
	}
}
