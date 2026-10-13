package checks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFailureExcerpt(t *testing.T) {
	if FailureExcerpt("") != "" {
		t.Fatal("empty input must give an empty excerpt")
	}
	out := FailureExcerpt("\x1b[31mFAIL\x1b[39m src/a.test.ts\n\x1b]8;;http://x\x07link\x1b]8;;\x07")
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "FAIL src/a.test.ts") {
		t.Fatalf("ansi not stripped: %q", out)
	}

	noise := strings.Repeat("progress line\n", 5000) + "progress line 4999"
	out = excerptLimit(" FAIL  src/pages/Join.test.tsx > shows the approval note\n"+noise, 2000)
	if !strings.Contains(out, "shows the approval note") || !strings.Contains(out, "progress line 4999") {
		t.Fatalf("head or tail lost: %q", out)
	}
	if utf8.RuneCountInString(out) > 2000 {
		t.Fatalf("over the limit: %d", utf8.RuneCountInString(out))
	}

	dom := strings.Repeat("    <div\n      class=\"flex\"\n    >\n      <span>\n      </span>\n    </div>\n", 20)
	out = FailureExcerpt("TestingLibraryElementError: Unable to find an element\n" + dom + " ❯ src/a.test.tsx:12:5")
	if !strings.Contains(out, "markup lines omitted") || strings.Contains(out, `class="flex"`) ||
		!strings.Contains(out, "src/a.test.tsx:12:5") {
		t.Fatalf("markup not collapsed: %q", out)
	}

	text := strings.Repeat("x\n", 200) + "Expected: \"公演A\"\nReceived: \"改名した公演\"\n" + strings.Repeat("y\n", 200)
	out = excerptLimit(text, 1500)
	if !strings.Contains(out, "Expected: \"公演A\"") || !strings.Contains(out, "Received: \"改名した公演\"") {
		t.Fatalf("assertion diff lost: %q", out)
	}

	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, "line "+string(rune('a'+i%26)))
	}
	lines = append(lines, "line LAST")
	out = FailureExcerpt(strings.Join(lines, "\n"))
	if !strings.Contains(out, "line LAST") || strings.Count(out, "\n") > 60 {
		t.Fatalf("tail fallback wrong: %d lines", strings.Count(out, "\n"))
	}

	many := strings.Repeat("error TS2322: bad "+strings.Repeat("z", 80)+"\n", 1000)
	for _, limit := range []int{100, 500, 4000} {
		if n := utf8.RuneCountInString(excerptLimit(many, limit)); n > limit {
			t.Fatalf("limit %d exceeded: %d", limit, n)
		}
	}
}

func TestAnchorBoundaries(t *testing.T) {
	for line, want := range map[string]bool{
		"FAILED test":        true,
		"FAILURE":            false,
		"xFAIL":              false,
		"テストFAIL":            false, // Python's \b is Unicode-aware
		"TypeError: x":       false,
		"Error: x":           true,
		"  Expected: 1":      true,
		"  Expectedly":       false,
		"error TS2322":       true,
		"AssertionError":     true,
		"AssertionErrors":    false,
		"not ok 3":           true,
		"cannot ok":          false,
		"the test timed out": true,
	} {
		if got := isAnchor(line); got != want {
			t.Errorf("isAnchor(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestClipKeepsHeadAndTail(t *testing.T) {
	out := clip(strings.Repeat("A", 500)+strings.Repeat("B", 500), 200)
	if !strings.HasPrefix(out, "A") || !strings.HasSuffix(out, "B") || utf8.RuneCountInString(out) > 200 {
		t.Fatalf("clip: %q", out)
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.ts", "a.ts", true},
		{"**/*.ts", "x/y/a.ts", true},
		{"**/*.ts", "a.tsx", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/bc", false},
		{"a/**/b", "ax/b", false},
		{"src/**", "src/a.ts", true},
		{"src/**", "src/x/a.ts", true},
		{"src/**", "srcx/a.ts", false},
		{"src/*.ts", "src/a.ts", true},
		{"src/*.ts", "src/x/a.ts", false},
		{"a?.ts", "ab.ts", true},
		{"a?.ts", "abc.ts", false},
		{"a?.ts", "a.ts", false},
		{"a.b", "axb", false},
	} {
		if got := GlobMatch(c.pattern, c.path); got != c.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
	files := []string{"a.ts", "b.tsx", "c.py"}
	if got := MatchFiles(nil, files); !reflect.DeepEqual(got, files) {
		t.Errorf("no patterns: %v", got)
	}
	if got := MatchFiles([]string{"**/*.ts", "**/*.tsx"}, files); !reflect.DeepEqual(got, []string{"a.ts", "b.tsx"}) {
		t.Errorf("filter: %v", got)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"a/b.ts":    "a/b.ts",
		"":          "''",
		"a b":       "'a b'",
		"it's":      `'it'"'"'s'`,
		"$x":        "'$x'",
		"ü":         "'ü'",
		"a=b,c:d@e": "a=b,c:d@e",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFailedFiles(t *testing.T) {
	cases := map[string][]string{
		" FAIL  src/a.test.ts > case one\n FAIL  src/a.test.ts > case two\n FAIL src/b.test.ts": {"src/a.test.ts", "src/b.test.ts"},
		"FAILED tests/test_x.py::test_a - assert\nFAILED tests/test_y.py::TestC::test_b":        {"tests/test_x.py", "tests/test_y.py"},
		"\x1b[31mFAIL\x1b[39m src/c.test.ts":                                                    {"src/c.test.ts"},
		"all fine\nFAILED but no file here":                                                     nil,
	}
	for in, want := range cases {
		if got := ParseFailedFiles(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseFailedFiles(%q) = %v, want %v", in, got, want)
		}
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runOpts(t *testing.T, files ...string) Options {
	return Options{Files: files, SlotDir: filepath.Join(t.TempDir(), "slots"), MaxParallel: 2, RetryFailedAlone: true}
}

func statuses(r Result) []string {
	var out []string
	for _, c := range r.Checks {
		out = append(out, c.Name+"="+c.Status)
	}
	return out
}

func TestRunStopsAtFirstFailureUnlessAll(t *testing.T) {
	tree := t.TempDir()
	items := []Item{
		{Name: "a", Run: "true"},
		{Name: "b", Run: "exit 3"},
		{Name: "c", Run: "true"},
	}
	o := runOpts(t)
	got := statuses(Run(tree, items, o))
	if want := []string{"a=pass", "b=fail", "c=skipped"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	o.RunAll = true
	res := Run(tree, items, o)
	if want := []string{"a=pass", "b=fail", "c=pass"}; !reflect.DeepEqual(statuses(res), want) {
		t.Fatalf("--all: %v", statuses(res))
	}
	if res.Checks[1].Exit == nil || *res.Checks[1].Exit != 3 || res.Status() != "fail" {
		t.Fatalf("exit/status: %+v %s", res.Checks[1], res.Status())
	}
}

func TestRunPhaseOrderAndInvalidPhase(t *testing.T) {
	tree := t.TempDir()
	items := []Item{
		{Name: "t1", Run: "true"},
		{Name: "h1", Phase: "heavy", Run: "true"},
		{Name: "p1", Phase: "prepare", Run: "true"},
		{Name: "l1", Phase: "lint", Run: "true"},
		{Name: "t2", Run: "true"},
	}
	got := statuses(Run(tree, items, runOpts(t)))
	if want := []string{"p1=pass", "l1=pass", "t1=pass", "t2=pass", "h1=pass"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v", got)
	}
	bad := Run(tree, []Item{{Name: "x", Phase: "nightly", Run: "true"}}, runOpts(t))
	if len(bad.Checks) != 1 || bad.Checks[0].Name != "config" || bad.Checks[0].Reason != "invalid phase: nightly" {
		t.Fatalf("invalid phase: %+v", bad.Checks)
	}
}

func TestRunSkipsAndFilesPlaceholder(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, "src/a.ts", "")
	write(t, tree, "web/d.tsx", "")
	items := []Item{
		{Name: "nomatch", Paths: []string{"*.py"}, Run: "exit 1"},
		{Name: "files", Paths: []string{"src/**"}, Run: "echo FAIL {files}; exit 1"},
		{Name: "gone", Paths: []string{"**/*.js"}, Run: "echo {files}"},
		{Name: "cwd", Paths: []string{"**/*.tsx"}, Cwd: "web", Run: "echo FAIL {files}; exit 1"},
		{Name: "outside", Paths: []string{"src/**"}, Cwd: "web", Run: "echo {files}"},
	}
	o := runOpts(t, "src/a.ts", "web/d.tsx", "removed.js")
	o.RunAll = true
	o.RetryFailedAlone = false
	res := Run(tree, items, o)
	want := []string{"nomatch=skipped", "files=fail", "gone=skipped", "cwd=fail", "outside=skipped"}
	if !reflect.DeepEqual(statuses(res), want) {
		t.Fatalf("got %v", statuses(res))
	}
	if res.Checks[1].Excerpt != "FAIL src/a.ts" || res.Checks[3].Excerpt != "FAIL d.tsx" {
		t.Fatalf("excerpts: %q %q", res.Checks[1].Excerpt, res.Checks[3].Excerpt)
	}
	if res.Checks[0].Reason != "no matching files" {
		t.Fatalf("reason: %q", res.Checks[0].Reason)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	tree := t.TempDir()
	res := Run(tree, []Item{{Name: "slow", Timeout: 0.3, Run: "sleep 30 & sleep 30"}}, runOpts(t))
	c := res.Checks[0]
	if c.Status != "fail" || !c.TimedOut || c.Exit != nil {
		t.Fatalf("timeout result: %+v", c)
	}
	if c.Seconds > 10 {
		t.Fatalf("took %vs", c.Seconds)
	}
}

func TestRunFixRunsFirstAndIsIgnored(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, "a.txt", "")
	items := []Item{{Name: "c", Fix: "echo fixed > fixed.txt; exit 9", Run: "test -e fixed.txt"}}
	res := Run(tree, items, runOpts(t))
	if res.Checks[0].Status != "pass" {
		t.Fatalf("%+v", res.Checks[0])
	}
}

func TestRunJunitDeletedBeforeRunAndNamesLead(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, "reports/junit.xml", "<stale/>")
	items := []Item{{Name: "j", Junit: "reports/junit.xml", Run: "test ! -e reports/junit.xml || exit 7; mkdir -p reports; " +
		`printf '%s' '<testsuite><testcase classname="A" name="ok"/><testcase classname="A" name="bad"><failure message="boom&#10;more"/></testcase></testsuite>' > reports/junit.xml; echo FAIL A > /dev/stderr; exit 1`}}
	o := runOpts(t)
	o.RetryFailedAlone = false
	res := Run(tree, items, o)
	c := res.Checks[0]
	if c.Exit == nil || *c.Exit != 1 {
		t.Fatalf("stale junit was not deleted first: %+v", c)
	}
	if !strings.HasPrefix(c.Excerpt, "A > bad: boom\n") || !strings.Contains(c.Excerpt, "FAIL A") {
		t.Fatalf("excerpt: %q", c.Excerpt)
	}
}

func TestRunEnsure(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, "have", "")
	o := runOpts(t)
	o.Ensure = []Ensure{
		{Exists: "have", Run: "exit 1"},
		{Exists: "built/marker", Run: "mkdir -p built && : > built/marker"},
	}
	res := Run(tree, []Item{{Name: "needs", Run: "test -e built/marker"}}, o)
	if want := []string{"needs=pass"}; !reflect.DeepEqual(statuses(res), want) {
		t.Fatalf("%v", statuses(res))
	}

	o.Ensure = []Ensure{{Exists: "missing", Run: "echo FAIL building; exit 1"}}
	res = Run(tree, []Item{{Name: "a", Run: "true"}}, o)
	if want := []string{"ensure:missing=fail", "a=skipped"}; !reflect.DeepEqual(statuses(res), want) {
		t.Fatalf("stop: %v", statuses(res))
	}
	o.RunAll = true
	res = Run(tree, []Item{{Name: "a", Run: "true"}}, o)
	if want := []string{"ensure:missing=fail", "a=pass"}; !reflect.DeepEqual(statuses(res), want) {
		t.Fatalf("all: %v", statuses(res))
	}
}

func TestRetryRescuesFlaky(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, "a.test.js", "")
	marker := filepath.Join(t.TempDir(), "seen")
	script := `if [ -e '` + marker + `' ]; then exit 0; fi; : > '` + marker + `'; echo "FAIL $1"; exit 1`
	item := Item{Name: "u", Run: "sh -c '" + strings.ReplaceAll(script, "'", `'"'"'`) + "' x {files}"}
	res := Run(tree, []Item{item}, runOpts(t, "a.test.js"))
	c := res.Checks[0]
	if c.Status != "pass" || !reflect.DeepEqual(c.Flaky, []string{"a.test.js"}) || c.Excerpt != "" {
		t.Fatalf("not rescued: %+v", c)
	}
	if c.Exit == nil || *c.Exit != 1 {
		t.Fatalf("first exit code lost: %+v", c.Exit)
	}
}

func TestRetryNotApplicable(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, "a.test.js", "")
	// No {files} and no retry_alone: never retried.
	res := Run(tree, []Item{{Name: "plain", Run: "echo FAIL a.test.js; exit 1"}}, runOpts(t, "a.test.js"))
	if res.Checks[0].Status != "fail" || res.Checks[0].Flaky != nil {
		t.Fatalf("%+v", res.Checks[0])
	}
	// Output names no file: nothing to retry.
	res = Run(tree, []Item{{Name: "nofile", Run: "echo boom {files}; exit 1"}}, runOpts(t, "a.test.js"))
	if res.Checks[0].Status != "fail" {
		t.Fatalf("%+v", res.Checks[0])
	}
	// Retry disabled.
	o := runOpts(t, "a.test.js")
	o.RetryFailedAlone = false
	res = Run(tree, []Item{{Name: "off", Run: "echo FAIL {files}; exit 1"}}, o)
	if res.Checks[0].Status != "fail" || res.Checks[0].Flaky != nil {
		t.Fatalf("%+v", res.Checks[0])
	}
}

func TestResultStatus(t *testing.T) {
	mk := func(ss ...string) Result {
		var r Result
		for _, s := range ss {
			r.Checks = append(r.Checks, CheckResult{Status: s})
		}
		return r
	}
	for want, r := range map[string]Result{
		"no-checks":   mk(),
		"pass":        mk("pass", "skipped"),
		"fail":        mk("pass", "preexisting", "fail"),
		"preexisting": mk("pass", "preexisting"),
	} {
		if got := r.Status(); got != want {
			t.Errorf("Status(%v) = %s, want %s", r.Checks, got, want)
		}
	}
	if got := mk("skipped", "skipped").Status(); got != "no-checks" {
		t.Errorf("all skipped = %s", got)
	}
}

func TestEvents(t *testing.T) {
	tree := t.TempDir()
	var events []string
	o := runOpts(t)
	o.OnEvent = func(e Event) { events = append(events, e.Event+":"+e.Detail["name"].(string)) }
	Run(tree, []Item{{Name: "a", Run: "true"}, {Name: "s", Paths: []string{"*.py"}, Run: "true"}}, o)
	if want := []string{"check-start:a", "check-end:a"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v", events)
	}
}
