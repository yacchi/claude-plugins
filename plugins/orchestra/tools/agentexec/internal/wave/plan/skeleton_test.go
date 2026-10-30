package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const skeletonPkg = `{"id":"a","spec":"s/a.md","cls":"light","brief":"b","risks":[],"est_seconds":60,` +
	`"files_owned":["x.go"],"verification":"go test ./x/..."}`

func TestLoadSkeletonSkipsOnlySpecStat(t *testing.T) {
	file := fixture(t, `{"packages":[`+skeletonPkg+`]}`)
	if _, err := LoadSkeleton(file); err != nil {
		t.Fatalf("LoadSkeleton with no spec file: %v", err)
	}
	want := "a: spec path does not exist: " + filepath.Join(filepath.Dir(file), "s/a.md")
	if _, err := Load(file); err == nil || err.Error() != want {
		t.Errorf("Load with no spec file: got %v want %q", err, want)
	}
}

func TestInterfacePathResolvesAgainstPlanDir(t *testing.T) {
	file := fixture(t, `{"interfaces":[{"id":"i","path":"contracts/i.json"}],"packages":[`+skeletonPkg+`]}`,
		"contracts/i.json")
	p, err := LoadSkeleton(file)
	if err != nil {
		t.Fatal(err)
	}
	iface, ok := p.Interface("i")
	if !ok || iface.Path != filepath.Join(filepath.Dir(file), "contracts", "i.json") {
		t.Errorf("interface i: got %+v ok=%v", iface, ok)
	}
}

func TestNewKeysAreOptionalButTyped(t *testing.T) {
	specs := []string{"s/a.md", "contracts/i.json"}
	plain := `{"packages":[{"id":"a","spec":"s/a.md","cls":"light"}]}`
	if _, err := Load(fixture(t, plain, specs...)); err != nil {
		t.Fatalf("plan without new keys: %v", err)
	}
	const ifaceErr = "plan interfaces must be a list of {'id': <string>, 'path': <path>} objects"
	cases := []struct{ planJSON, want string }{
		{`{"packages":[{"id":"a","spec":"s/a.md","cls":"light","brief":1}]}`, "a: brief must be a string"},
		{`{"packages":[{"id":"a","spec":"s/a.md","cls":"light","risks":"x"}]}`, "a: risks must be a list of strings"},
		{`{"packages":[{"id":"a","spec":"s/a.md","cls":"light","interfaces":[1]}]}`,
			"a: interfaces must be a list of interface ids"},
		{`{"packages":[{"id":"a","spec":"s/a.md","cls":"light","interfaces":["nope"]}]}`, `a: unknown interface "nope"`},
		{`{"interfaces":[{"id":"i"}],"packages":[{"id":"a","spec":"s/a.md","cls":"light"}]}`, ifaceErr},
		{`{"interfaces":"contracts/i.json","packages":[{"id":"a","spec":"s/a.md","cls":"light"}]}`, ifaceErr},
	}
	for _, tc := range cases {
		if got := loadErr(t, tc.planJSON, specs...); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.planJSON, got, tc.want)
		}
	}
}

func TestSkeletonEstBoundary(t *testing.T) {
	for _, tc := range []struct {
		est string
		ok  bool
	}{{"300", true}, {"1", true}, {"301", false}, {"0", false}} {
		pkg := strings.Replace(skeletonPkg, `"est_seconds":60`, `"est_seconds":`+tc.est, 1)
		_, err := LoadSkeleton(fixture(t, `{"packages":[`+pkg+`]}`))
		if (err == nil) != tc.ok {
			t.Errorf("est_seconds %s: err=%v, want ok=%v", tc.est, err, tc.ok)
		}
	}
	noEst := strings.Replace(skeletonPkg, `"est_seconds":60,`, ``, 1)
	if _, err := Load(fixture(t, `{"packages":[{"id":"a","spec":"s/a.md","cls":"light"}]}`, "s/a.md")); err != nil {
		t.Errorf("Load without est_seconds: %v", err)
	}
	if _, err := LoadSkeleton(fixture(t, `{"packages":[`+noEst+`]}`)); err == nil {
		t.Errorf("LoadSkeleton without est_seconds: want error")
	}
}

func TestSpecWarningsInterfaceVerbatim(t *testing.T) {
	const iface = "{\"step\": \"skeleton\"}\n  \"plan\": \"<path>\"\n"
	for _, tc := range []struct {
		name, spec string
		warnings   int
	}{
		{"re-indented in a fence", "```json\n    {\"step\": \"skeleton\"}\n        \"plan\": \"<path>\"\n```\n", 0},
		{"line missing", "```json\n{\"step\": \"skeleton\"}\n```\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := fixture(t, `{"interfaces":[{"id":"i","path":"contracts/i.json"}],"packages":[`+
				`{"id":"a","spec":"s/a.md","cls":"light","interfaces":["i"]}]}`, "s/a.md", "contracts/i.json")
			dir := filepath.Dir(file)
			if err := os.WriteFile(filepath.Join(dir, "contracts/i.json"), []byte(iface), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "s/a.md"), []byte(tc.spec), 0o644); err != nil {
				t.Fatal(err)
			}
			p, err := Load(file)
			if err != nil {
				t.Fatal(err)
			}
			got := SpecWarnings(p, p.Packages[0])
			if len(got) != tc.warnings {
				t.Fatalf("got %d warnings %+v, want %d", len(got), got, tc.warnings)
			}
			if tc.warnings == 1 {
				want := "package a spec does not contain interface i verbatim; paste " +
					filepath.Join(dir, "contracts", "i.json") + " into it unchanged"
				if got[0].Message != want {
					t.Errorf("message %q, want %q", got[0].Message, want)
				}
			}
		})
	}
}

func TestEstimateHeaviestChain(t *testing.T) {
	p := &Plan{Packages: []Package{
		{ID: "A", EstSeconds: 100},
		{ID: "B", EstSeconds: 50, DependsOn: []string{"A"}},
		{ID: "C", EstSeconds: 30, DependsOn: []string{"B"}},
		{ID: "D", EstSeconds: 200},
	}}
	if total, critical := Estimate(p); total != 380 || critical != 200 {
		t.Errorf("got total %d critical %d, want 380 and 200", total, critical)
	}
}
