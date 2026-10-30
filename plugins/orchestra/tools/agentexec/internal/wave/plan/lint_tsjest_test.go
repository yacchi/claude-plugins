package plan

import (
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

func TestLintTsJestSpecText(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		want       int
	}{
		{"ts-jest without isolatedModules", "run tests with ts-jest\n", 1},
		{"isolatedModules mentioned", "ts-jest with isolatedModules: true\n", 0},
		{"negated mention still counts as mention", "do NOT use ts-jest type checking; isolatedModules: true\n", 0},
		{"scoped package name is a word", "preset @ts-jest/presets\n", 1},
		{"not a word match", "nots-jest is unrelated\n", 0},
		{"case-sensitive", "TS-JEST is unrelated\n", 0},
	} {
		file := fixture(t, `{"packages":[{"id":"a","spec":"s/a.md","cls":"light"}]}`, "s/a.md")
		if err := os.WriteFile(filepath.Join(filepath.Dir(file), "s/a.md"), []byte(tc.spec), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := Load(file)
		if err != nil {
			t.Fatal(err)
		}
		got := Lint(p, "")
		if len(got) != tc.want {
			t.Errorf("%s: got %+v, want %d warnings", tc.name, got, tc.want)
			continue
		}
		if tc.want == 1 && (got[0].Pkg == nil || *got[0].Pkg != "a" || got[0].Message != tsJestMessage) {
			t.Errorf("%s: got %+v, want package a with the ts-jest message", tc.name, got[0])
		}
	}
}

func TestLintTsJestPreamble(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       int
	}{
		{"ts-jest without isolatedModules", "use ts-jest\n", 1},
		{"do NOT use ts-jest type checking; isolatedModules: true", "do NOT use ts-jest type checking; isolatedModules: true\n", 0},
	} {
		file := fixture(t, `{"preamble":["pre.md"],"packages":[{"id":"a","spec":"s/a.md","cls":"light"}]}`, "s/a.md", "pre.md")
		if err := os.WriteFile(filepath.Join(filepath.Dir(file), "pre.md"), []byte(tc.text), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := Load(file)
		if err != nil {
			t.Fatal(err)
		}
		got := Lint(p, "")
		if len(got) != tc.want {
			t.Errorf("%s: got %+v, want %d warnings", tc.name, got, tc.want)
			continue
		}
		if tc.want == 1 && (got[0].Pkg != nil || got[0].Message != tsJestMessage) {
			t.Errorf("%s: got %+v, want a plan-level ts-jest warning", tc.name, got[0])
		}
	}
}

func TestLintSkeletonTsJest(t *testing.T) {
	const pkg = `{"id":"a","spec":"s/a.md","cls":"light","brief":"Use ts-jest for the tests","risks":[],"est_seconds":60,` +
		`"files_owned":["x.go"],"verification":"go test ./x/..."}`
	file := fixture(t, `{"commit_message":"feat: x","preamble":["pre.md"],"packages":[`+pkg+`]}`, "pre.md")
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "pre.md"), []byte("use ts-jest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadSkeleton(file)
	if err != nil {
		t.Fatal(err)
	}
	got := LintSkeleton(p)
	if len(got) != 2 {
		t.Fatalf("got %+v, want the brief and the preamble warned", got)
	}
	if got[0].Pkg == nil || *got[0].Pkg != "a" || got[0].Message != tsJestMessage {
		t.Errorf("brief: got %+v, want package a with the ts-jest message", got[0])
	}
	if got[1].Pkg != nil || got[1].Message != tsJestMessage {
		t.Errorf("preamble: got %+v, want a plan-level ts-jest message", got[1])
	}
}

func TestLintJestConfigRule(t *testing.T) {
	const cfg = "module.exports = { preset: 'ts-jest' }\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []string // repo-relative config paths expected to warn
	}{
		{"ts-jest without isolatedModules", map[string]string{"jest.config.js": cfg}, []string{"jest.config.js"}},
		{"mjs config", map[string]string{"jest.config.mjs": cfg}, []string{"jest.config.mjs"}},
		{"other extensions are ignored", map[string]string{"jest.config.json": cfg}, nil},
		{"no ts-jest mention", map[string]string{"jest.config.js": "module.exports = {}\n"}, nil},
		{"isolatedModules in the config", map[string]string{
			"jest.config.ts": "export default { preset: 'ts-jest', globals: { isolatedModules: true } }\n",
		}, nil},
		{"referenced tsconfig sets isolatedModules, whitespace tolerant", map[string]string{
			"jest.config.js":     "module.exports = { preset: 'ts-jest', tsconfig: 'tsconfig.test.json' }\n",
			"tsconfig.test.json": "{\"compilerOptions\": {\"isolatedModules\" :   true}}\n",
		}, nil},
		{"referenced tsconfig sets it false", map[string]string{
			"jest.config.js":     "module.exports = { preset: 'ts-jest', tsconfig: 'tsconfig.test.json' }\n",
			"tsconfig.test.json": "{\"compilerOptions\": {\"isolatedModules\": false}}\n",
		}, []string{"jest.config.js"}},
		{"referenced tsconfig missing", map[string]string{
			"jest.config.js": "module.exports = { preset: 'ts-jest', tsconfig: \"gone.json\" }\n",
		}, []string{"jest.config.js"}},
		{"missing tsconfig referenced twice warns once", map[string]string{
			"jest.config.js": "module.exports = { preset: 'ts-jest', a: 'gone.json', b: 'gone.json' }\n",
		}, []string{"jest.config.js"}},
		{"<rootDir> binds to the config's dir", map[string]string{
			"packages/x/jest.config.cjs": "module.exports = { preset: 'ts-jest', tsconfig: '<rootDir>/tsconfig.json' }\n",
			"packages/x/tsconfig.json":   "{\"compilerOptions\": {\"isolatedModules\": true}}\n",
		}, nil},
		{"node_modules is skipped", map[string]string{"node_modules/pkg/jest.config.js": cfg}, nil},
		{"dot directories are skipped", map[string]string{".cache/jest.config.js": cfg}, nil},
		{"depth 6 directory is walked", map[string]string{"a/b/c/d/e/f/jest.config.js": cfg},
			[]string{"a/b/c/d/e/f/jest.config.js"}},
		{"depth 7 directory is not walked", map[string]string{"a/b/c/d/e/f/g/jest.config.js": cfg}, nil},
	} {
		root := t.TempDir()
		writeTree(t, root, tc.files)
		p, err := Load(fixture(t, `{"packages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		got := Lint(p, root)
		var paths []string
		for _, w := range got {
			path, ok := strings.CutSuffix(w.Message, ": "+tsJestMessage)
			if !ok || w.Pkg != nil {
				t.Errorf("%s: got %+v, want a repo-relative ts-jest warning", tc.name, w)
				continue
			}
			paths = append(paths, path)
		}
		if !reflect.DeepEqual(paths, tc.want) {
			t.Errorf("%s: got warned paths %v, want %v", tc.name, paths, tc.want)
		}
	}
}
