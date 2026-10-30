package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree writes each file under root, creating parent directories.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, text := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// tsJestPkg is a skeleton package entry that passes LoadSkeleton.
func tsJestPkg(id, brief, owned string) string {
	return fmt.Sprintf(`{"id":%q,"spec":"s/%s.md","cls":"light","brief":%q,"risks":[],"est_seconds":60,`+
		`"files_owned":[%s],"verification":"go test ./x/..."}`, id, id, brief, owned)
}

// tsJestFindings is CheckTsJest's findings, one per line after the header, or
// nil when it passes.
func tsJestFindings(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		return nil
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want a *plan.Error", err)
	}
	lines := strings.Split(pe.Msg, "\n")
	if lines[0] != tsJestHeader {
		t.Fatalf("header: got %q", lines[0])
	}
	var out []string
	for _, l := range lines[1:] {
		f, ok := strings.CutPrefix(l, "- ")
		if !ok {
			t.Fatalf("finding line %q lacks the \"- \" prefix", l)
		}
		out = append(out, f)
	}
	return out
}

func TestCheckTsJestText(t *testing.T) {
	for _, tc := range []struct {
		name, brief, spec string
		skeleton          bool
		want              []string
	}{
		{"spec names ts-jest", "b", "run tests with ts-jest\n", false,
			[]string{"a: spec uses ts-jest without isolatedModules"}},
		{"spec mentions isolatedModules", "b", "ts-jest with isolatedModules: true\n", false, nil},
		{"negated mention still counts as mention", "b", "do NOT use ts-jest type checking; isolatedModules: true\n", false, nil},
		{"scoped package name is a word", "b", "preset @ts-jest/presets\n", false,
			[]string{"a: spec uses ts-jest without isolatedModules"}},
		{"not a word match", "b", "nots-jest is unrelated\n", false, nil},
		{"case-sensitive", "b", "TS-JEST is unrelated\n", false, nil},
		{"unreadable spec falls back to the brief", "use ts-jest", "", false,
			[]string{"a: brief uses ts-jest without isolatedModules"}},
		{"readable spec replaces the brief", "use ts-jest", "plain\n", false, nil},
		{"skeleton reads the brief", "use ts-jest", "plain\n", true,
			[]string{"a: brief uses ts-jest without isolatedModules"}},
		{"skeleton ignores the spec", "b", "use ts-jest\n", true, nil},
	} {
		var specs []string
		if tc.spec != "" {
			specs = []string{"s/a.md"}
		}
		file := fixture(t, `{"packages":[`+tsJestPkg("a", tc.brief, `"x.go"`)+`]}`, specs...)
		if tc.spec != "" {
			if err := os.WriteFile(filepath.Join(filepath.Dir(file), "s/a.md"), []byte(tc.spec), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		p, err := LoadSkeleton(file)
		if err != nil {
			t.Fatal(err)
		}
		got := tsJestFindings(t, CheckTsJest(p, "", tc.skeleton))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckTsJestPreamble(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"ts-jest without isolatedModules", "use ts-jest\n", []string{"preamble PRE: uses ts-jest without isolatedModules"}},
		{"isolatedModules mentioned", "ts-jest with isolatedModules: true\n", nil},
	} {
		file := fixture(t, `{"preamble":["pre.md"],"packages":[`+tsJestPkg("a", "b", `"x.go"`)+`]}`, "s/a.md", "pre.md")
		pre := filepath.Join(filepath.Dir(file), "pre.md")
		if err := os.WriteFile(pre, []byte(tc.text), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := LoadSkeleton(file)
		if err != nil {
			t.Fatal(err)
		}
		want := make([]string, len(tc.want))
		for i, w := range tc.want {
			want[i] = strings.Replace(w, "PRE", pre, 1)
		}
		if len(want) == 0 {
			want = nil
		}
		got := tsJestFindings(t, CheckTsJest(p, "", true))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, want)
		}
	}
}

const tsJestCfg = "module.exports = { preset: 'ts-jest' }\n"

func TestCheckTsJestRepoRule(t *testing.T) {
	for _, tc := range []struct {
		name, owned string // owned is a files_owned JSON list body
		files       map[string]string
		want        []string
	}{
		{"exact file owned", `"apps/web/jest.config.js"`,
			map[string]string{"apps/web/jest.config.js": tsJestCfg}, nil},
		{"directory owned", `"apps/web/"`,
			map[string]string{"apps/web/jest.config.js": tsJestCfg}, nil},
		{"a file under the config's dir does not own it", `"apps/web/src/x.ts"`,
			map[string]string{"apps/web/jest.config.js": tsJestCfg},
			[]string{"apps/web/jest.config.js: " + tsJestUnowned}},
		{"a parent directory entry touches the config's dir", `"apps/"`,
			map[string]string{"apps/web/jest.config.js": tsJestCfg}, nil},
		{"an untouched sibling directory's config is not required", `"apps/web2/"`,
			map[string]string{"apps/web/jest.config.js": tsJestCfg}, nil},
		{"an untouched config is not required next to a touched one", `"apps/web2/src/x.ts"`,
			map[string]string{"apps/web/jest.config.js": tsJestCfg, "apps/web2/jest.config.js": tsJestCfg},
			[]string{"apps/web2/jest.config.js: " + tsJestUnowned}},
		{"a root config runs every touched file", `"apps/web/src/x.ts"`,
			map[string]string{"jest.config.js": tsJestCfg}, []string{"jest.config.js: " + tsJestUnowned}},
		{"no configs", `"x.go"`, map[string]string{"x.go": ""}, nil},
		{"isolatedModules in the config", `"x.go"`, map[string]string{
			"jest.config.ts": "export default { preset: 'ts-jest', globals: { isolatedModules: true } }\n",
		}, nil},
		{"referenced tsconfig sets isolatedModules, whitespace tolerant", `"x.go"`, map[string]string{
			"jest.config.js":     "module.exports = { preset: 'ts-jest', tsconfig: 'tsconfig.test.json' }\n",
			"tsconfig.test.json": "{\"compilerOptions\": {\"isolatedModules\" :   true}}\n",
		}, nil},
		{"referenced tsconfig sets it false", `"x.go"`, map[string]string{
			"jest.config.js":     "module.exports = { preset: 'ts-jest', tsconfig: 'tsconfig.test.json' }\n",
			"tsconfig.test.json": "{\"compilerOptions\": {\"isolatedModules\": false}}\n",
		}, []string{"jest.config.js: " + tsJestUnowned}},
		{"referenced tsconfig missing", `"x.go"`, map[string]string{
			"jest.config.js": "module.exports = { preset: 'ts-jest', tsconfig: \"gone.json\" }\n",
		}, []string{"jest.config.js: " + tsJestUnowned}},
		{"<rootDir> binds to the config's dir", `"x.go"`, map[string]string{
			"packages/x/jest.config.cjs": "module.exports = { preset: 'ts-jest', tsconfig: '<rootDir>/tsconfig.json' }\n",
			"packages/x/tsconfig.json":   "{\"compilerOptions\": {\"isolatedModules\": true}}\n",
		}, nil},
		{"node_modules is skipped", `"x.go"`, map[string]string{"node_modules/pkg/jest.config.js": tsJestCfg}, nil},
		{"dot directories are skipped", `"x.go"`, map[string]string{".cache/jest.config.js": tsJestCfg}, nil},
		{"depth 6 directory is walked", `"a/b/c/d/e/f/g/x.go"`, map[string]string{"a/b/c/d/e/f/jest.config.js": tsJestCfg},
			[]string{"a/b/c/d/e/f/jest.config.js: " + tsJestUnowned}},
		{"depth 7 directory is not walked", `"a/b/c/d/e/f/g/x.go"`, map[string]string{"a/b/c/d/e/f/g/jest.config.js": tsJestCfg}, nil},
		{"other extensions are ignored", `"x.go"`, map[string]string{"jest.config.yaml": tsJestCfg}, nil},
		{"jest.config.json is walked", `"x.go"`, map[string]string{"jest.config.json": `{"preset":"ts-jest"}`},
			[]string{"jest.config.json: " + tsJestUnowned}},
		{"package.json jest key names ts-jest", `"x.go"`, map[string]string{
			"package.json": `{"jest":{"preset":"ts-jest"}}`,
		}, []string{"package.json: " + tsJestUnowned}},
		{"package.json jest key sets isolatedModules", `"x.go"`, map[string]string{
			"package.json": `{"jest":{"transform":{"^.+\\.ts$":["ts-jest",{"isolatedModules":true}]}}}`,
		}, nil},
		{"package.json devDependencies are not a jest config", `"x.go"`, map[string]string{
			"package.json": `{"devDependencies":{"ts-jest":"29.0.0"}}`,
		}, nil},
		{"package.json jest key with a tsconfig that sets it", `"x.go"`, map[string]string{
			"package.json":       `{"jest":{"preset":"ts-jest","tsconfig":"tsconfig.test.json"}}`,
			"tsconfig.test.json": `{"compilerOptions":{"isolatedModules":true}}`,
		}, nil},
		{"script config is a finding under its own path", `"x.go"`, map[string]string{
			"package.json":        `{"scripts":{"test":"jest -c config/jest.unit.js"}}`,
			"config/jest.unit.js": tsJestCfg,
		}, []string{"config/jest.unit.js: " + tsJestUnowned}},
		{"script config runs its package.json's dir", `"pkg/src/x.ts"`, map[string]string{
			"pkg/package.json":     `{"scripts":{"test":"jest -c ../config/jest.unit.js"}}`,
			"config/jest.unit.js":  tsJestCfg,
			"other/package.json":   `{"scripts":{"test":"jest -c ../config/jest.other.js"}}`,
			"config/jest.other.js": tsJestCfg,
		}, []string{"config/jest.unit.js: " + tsJestUnowned}},
		{"--config= form in a jest segment", `"x.go"`, map[string]string{
			"package.json":        `{"scripts":{"test":"jest --config=config/jest.unit.js"}}`,
			"config/jest.unit.js": tsJestCfg,
		}, []string{"config/jest.unit.js: " + tsJestUnowned}},
		{"script config also walked is reported once", `"x.go"`, map[string]string{
			"package.json":   `{"scripts":{"test":"jest --config=jest.config.js"}}`,
			"jest.config.js": tsJestCfg,
		}, []string{"jest.config.js: " + tsJestUnowned}},
		{"script config that is missing is ignored", `"x.go"`, map[string]string{
			"package.json": `{"scripts":{"test":"jest -c config/gone.js"}}`,
		}, nil},
		{"-c in a non-jest segment is ignored", `"x.go"`, map[string]string{
			"package.json":   `{"scripts":{"lint":"eslint -c .eslintrc.json && jest"}}`,
			".eslintrc.json": `{"x":"ts-jest"}`,
		}, nil},
		{"-c in a non-jest segment of a multi-segment script is ignored", `"x.go"`, map[string]string{
			"package.json":        `{"scripts":{"ci":"eslint -c config/lint.js && jest --config config/jest.unit.js"}}`,
			"config/lint.js":      tsJestCfg,
			"config/jest.unit.js": `module.exports = { preset: 'ts-jest' }; // isolatedModules: true`,
		}, nil},
	} {
		root := t.TempDir()
		writeTree(t, root, tc.files)
		file := fixture(t, `{"packages":[`+tsJestPkg("a", "b", tc.owned)+`]}`)
		p, err := LoadSkeleton(file)
		if err != nil {
			t.Fatal(err)
		}
		got := tsJestFindings(t, CheckTsJest(p, root, true))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		// Without a root the repo rule is not applied.
		if got := tsJestFindings(t, CheckTsJest(p, "", true)); got != nil {
			t.Errorf("%s: no root: got %v, want nil", tc.name, got)
		}
	}
}

func TestCheckTsJestMessageOrder(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"z/jest.config.js": tsJestCfg, "a/jest.config.js": tsJestCfg})
	file := fixture(t, `{"preamble":["pre.md"],"packages":[`+
		tsJestPkg("a", "use ts-jest", `"z/x.go"`)+","+tsJestPkg("b", "plain", `"a/src/"`)+`]}`, "pre.md")
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "pre.md"), []byte("use ts-jest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadSkeleton(file)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckTsJest(p, root, true)
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want a *plan.Error", err)
	}
	pre := filepath.Join(filepath.Dir(file), "pre.md")
	want := "ts-jest type-checks every test in every jest worker; tests must only transform and types are checked once by tsc --noEmit after integration:\n" +
		"- a: brief uses ts-jest without isolatedModules\n" +
		"- preamble " + pre + ": uses ts-jest without isolatedModules\n" +
		"- a/jest.config.js: " + tsJestUnowned + "\n" +
		"- z/jest.config.js: " + tsJestUnowned
	if pe.Msg != want {
		t.Errorf("message:\n%s\nwant:\n%s", pe.Msg, want)
	}
}

func TestCheckTsJestPasses(t *testing.T) {
	file := fixture(t, `{"packages":[`+tsJestPkg("a", "plain", `"x.go"`)+`]}`)
	p, err := LoadSkeleton(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckTsJest(p, t.TempDir(), true); err != nil {
		t.Errorf("got %v, want nil", err)
	}
}
