// Package archtest enforces the module's architecture: the package
// dependency graph of the port plan and the 800-line cap per Go file.
package archtest

import (
	"bufio"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	module   = "github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec"
	root     = "../.."
	maxLines = 800
)

// deps lists, per package (relative to the module), the internal packages
// it may import directly. A package may also import anything reachable from
// those (the edges are transitive), and every package may use the
// foundation leaves below. A package missing from this table fails the
// test: add it here deliberately, with its place in the graph.
var deps = map[string][]string{
	"internal/fsutil":             {},
	"internal/jsonx":              {},
	"internal/paths":              {},
	"internal/yaml11":             {"internal/jsonx"},
	"internal/watchdog":           {},
	"internal/wave/plan":          {},
	"internal/config":             {"internal/yaml11", "internal/jsonx", "internal/paths", "internal/fsutil"},
	"internal/tools":              {"internal/config"},
	"internal/gitx":               {"internal/fsutil"},
	"internal/isolate":            {"internal/gitx", "internal/config", "internal/paths"},
	"internal/integrate":          {"internal/isolate"},
	"internal/shelf":              {"internal/gitx"},
	"internal/sandbox":            {"internal/config", "internal/paths", "internal/gitx"},
	"internal/srt":                {"internal/sandbox", "internal/tools"},
	"internal/readiness":          {"internal/config", "internal/tools", "internal/paths"},
	"internal/cooldown":           {"internal/config"},
	"internal/route":              {"internal/config", "internal/readiness", "internal/cooldown"},
	"internal/executor/pi":        {"internal/config", "internal/tools"},
	"internal/capture":            {"internal/sandbox", "internal/srt", "internal/watchdog", "internal/executor/pi"},
	"internal/ledger":             {"internal/config"},
	"internal/telemetry":          {"internal/config"},
	"internal/usage":              {"internal/ledger", "internal/telemetry", "internal/config"},
	"internal/dispatch":           {"internal/route", "internal/readiness", "internal/cooldown", "internal/isolate", "internal/capture", "internal/ledger", "internal/telemetry", "internal/sandbox"},
	"internal/checks":             {"internal/config", "internal/fsutil"},
	"internal/wave/state":         {"internal/gitx", "internal/paths"},
	"internal/host":               {"internal/config", "internal/tools", "internal/sandbox", "internal/srt", "internal/isolate", "internal/checks"},
	"internal/host/herdr":         {"internal/host"},
	"internal/host/orca":          {"internal/host"},
	"internal/approvals":          {"internal/wave/state", "internal/sandbox", "internal/host/herdr"},
	"internal/wave/runner":        {"internal/wave/plan", "internal/wave/state", "internal/checks", "internal/isolate", "internal/integrate", "internal/host", "internal/host/herdr", "internal/host/orca", "internal/approvals", "internal/ui", "internal/cooldown", "internal/ledger", "internal/dispatch"},
	"internal/ui":                 {"internal/wave/state", "internal/isolate", "internal/dispatch", "internal/cooldown", "internal/usage", "internal/config"},
	"internal/pilogin":            {"internal/host/herdr", "internal/tools", "internal/sandbox"},
	"internal/doctor":             {"internal/readiness", "internal/route", "internal/cooldown", "internal/sandbox", "internal/srt", "internal/tools", "internal/host/herdr"},
	"internal/install":            {"internal/paths"},
	"internal/cli":                {"*"},
	"cmd/agent-exec":              {"internal/cli"},
	"internal/testutil/fakepi":    {},
	"internal/testutil/fakeherdr": {},
	"internal/conformance":        {"*"},
	"internal/archtest":           {},
}

// leaves may be imported by any package.
var leaves = []string{"internal/fsutil", "internal/jsonx", "internal/paths"}

// thirdParty are the only non-standard imports allowed outside tests.
var thirdParty = []string{"github.com/spf13/cobra", "go.yaml.in/yaml/v3", "golang.org/x/sys/unix"}

// forbidden pins edges the plan rules out explicitly, whatever the table
// says.
var forbidden = [][2]string{
	{"internal/ui", "internal/wave/runner"},
}

func closure(pkg string, seen map[string]bool) {
	for _, d := range deps[pkg] {
		if !seen[d] {
			seen[d] = true
			closure(d, seen)
		}
	}
}

func allowed(pkg string) map[string]bool {
	seen := map[string]bool{}
	closure(pkg, seen)
	for _, l := range leaves {
		seen[l] = true
	}
	return seen
}

func TestTableIsAcyclic(t *testing.T) {
	for pkg := range deps {
		seen := map[string]bool{}
		closure(pkg, seen)
		if seen[pkg] {
			t.Errorf("%s depends on itself through the table", pkg)
		}
	}
	for _, f := range forbidden {
		if allowed(f[0])[f[1]] {
			t.Errorf("the table lets %s reach %s", f[0], f[1])
		}
	}
}

type goFile struct {
	path, pkg string
	test      bool
	imports   []string
}

func scan(t *testing.T) []goFile {
	t.Helper()
	var files []goFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || (strings.HasPrefix(d.Name(), ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		gf := goFile{path: path, pkg: filepath.ToSlash(rel), test: strings.HasSuffix(path, "_test.go")}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			gf.imports = append(gf.imports, p)
		}
		files = append(files, gf)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestImportGraph(t *testing.T) {
	for _, f := range scan(t) {
		rule, known := deps[f.pkg]
		if !known {
			t.Errorf("%s: package %s is not in the archtest table", f.path, f.pkg)
			continue
		}
		ok := allowed(f.pkg)
		for _, imp := range f.imports {
			if rel, internal := strings.CutPrefix(imp, module+"/"); internal {
				if len(rule) > 0 && rule[0] == "*" || rel == f.pkg {
					continue
				}
				if f.test && strings.HasPrefix(rel, "internal/testutil/") {
					continue
				}
				if !ok[rel] {
					t.Errorf("%s: %s may not import %s", f.path, f.pkg, rel)
				}
				continue
			}
			if !isStd(imp) && !f.test && !contains(thirdParty, imp) {
				t.Errorf("%s: third-party import %s is not allowed", f.path, imp)
			}
		}
	}
}

func isStd(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestFileLength(t *testing.T) {
	for _, f := range scan(t) {
		fh, err := os.Open(f.path)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			n++
		}
		fh.Close()
		if n > maxLines {
			t.Errorf("%s has %d lines (max %d): split it", f.path, n, maxLines)
		}
	}
}
