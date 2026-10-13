package ui

// Snapshot parity against the Python dashboard (removed with the Python
// implementation at the cutover): both servers read one fixture HOME and
// their /api/snapshot documents must agree (generated_at aside).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
)

const pythonUI = "../../../agent_exec_ui.py"

func pythonWithYAML(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not installed: no Python implementation to compare with")
	}
	out, err := exec.Command("uv", "run", "--quiet", "--no-project", "--with", "pyyaml",
		"python", "-c", "import sys, yaml; print(sys.executable)").Output()
	if err != nil {
		t.Skipf("no Python with PyYAML: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// startPython runs the Python dashboard on the fixture and returns its port
// and token.
func startPython(t *testing.T, python, script, cwd string) (int, string) {
	t.Helper()
	info := filepath.Join(filepath.Dir(os.Getenv("ORCHESTRA_UI_STATE")), "py-ui.json")
	cmd := exec.Command(python, script, "--serve", "--port", "0", "--idle-minutes", "5")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "ORCHESTRA_UI_STATE="+info)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(info)
		if err == nil {
			var doc struct {
				Port  int    `json:"port"`
				Token string `json:"token"`
			}
			if json.Unmarshal(raw, &doc) == nil && doc.Port > 0 {
				return doc.Port, doc.Token
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the Python dashboard did not start: %s", stderr.String())
	return 0, ""
}

func fetchSnapshot(t *testing.T, port int, token string) map[string]any {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/snapshot?t=%s", port, token))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	delete(out, "generated_at")
	return out
}

func TestSnapshotMatchesPython(t *testing.T) {
	python := pythonWithYAML(t)
	script, err := filepath.Abs(pythonUI)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	cwd := filepath.Join(e.tmp, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	// two repos: one with a package tree holding an uncommitted file
	repo := filepath.Join(e.tmp, "repo")
	e.write(filepath.Join(repo, "README.md"), "hi\n")
	runGit(t, repo, "init", "-q", "-b", "main", ".")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")
	tree := filepath.Join(e.tmp, "tree")
	e.write(filepath.Join(tree, "x.txt"), "x\n")
	runGit(t, tree, "init", "-q", "-b", "main", ".")

	planDir := filepath.Join(e.tmp, "plan")
	plan := e.write(filepath.Join(planDir, "plan.json"),
		`{"preamble": ["pre.md"], "packages": [{"id": "A-1", "spec": "specs/a1.md"}, {"id": "A-2", "path": "specs/a2.md"}]}`)
	e.write(filepath.Join(planDir, "specs", "a1.md"), "SPEC")

	one := e.makeWave("w1", "<b>detail</b>\nsecond line", false)
	doc := e.stateDoc("<b>detail</b>\nsecond line")
	doc["plan"] = plan
	doc["packages"].(map[string]any)["A-1"].(map[string]any)["tree"] = tree
	doc["packages"].(map[string]any)["A-1"].(map[string]any)["since"] = 1000.5
	doc["packages"].(map[string]any)["A-2"].(map[string]any)["since"] = 2000.0
	doc["needs"] = []any{
		map[string]any{"id": "A-1", "kind": "escalate", "detail": "line one\nline two", "at": 1.0},
		map[string]any{"id": nil, "kind": "environment", "detail": "ensure failed", "at": 2.0},
	}
	doc["updated"] = 3000.0
	e.write(one, mustJSON(t, doc))
	e.write(filepath.Join(filepath.Dir(one), "context", "A-1.md"), "ctx")
	e.write(filepath.Join(filepath.Dir(one), "orca-sessions.json"), mustJSON(t, map[string]any{
		"k": map[string]any{"wave_state": one, "pkg": "A-1", "terminal": "t1", "worktree": "/wt"},
		"j": map[string]any{"pkg": "A-2"},
	}))
	two := e.makeWave("w2", "", false)
	e.write(two, "{broken")
	e.register(one, repo)
	e.register(two, repo)
	e.register(one, repo) // re-registered: moves to the end

	e.write(filepath.Join(e.home, ".claude", "orchestra", "runs", "sess", "001-r1.jsonl"),
		mustJSON(t, map[string]any{"executor": "pi", "model": "m", "cls": "light", "status": "ok",
			"paths": []string{"x"}, "duration_s": 1.5, "finished_at": "2026-01-01T00:00:00Z", "tokens_used": 5})+"\n"+
			"not json\n"+
			mustJSON(t, map[string]any{"executor": "claude", "status": "unavailable", "corr": "c1"})+"\n")
	e.write(filepath.Join(e.home, ".claude", "orchestra", "executor-state.json"),
		mustJSON(t, map[string]any{"pi": map[string]any{"until": 1.0, "reason": "quota"}, "x": 3}))

	// a detached dispatch, alive (this process) and one that died without a result
	cfg, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	cfgDir := dispatch.DetachDir(cfg)
	e.write(filepath.Join(cfgDir, "dsp-00000000000a.json"), mustJSON(t, map[string]any{
		"pid": os.Getpid(), "token": "dsp-00000000000a", "started": 123.0, "executor": "pi", "wait_calls": 2}))
	e.write(filepath.Join(cfgDir, "dsp-00000000000b.json"), mustJSON(t, map[string]any{
		"pid": 1 << 22, "token": "dsp-00000000000b", "started": 100.0}))

	goServer := e.start(time.Minute)
	pyPort, pyToken := startPython(t, python, script, cwd)

	got := fetchSnapshot(t, goServer.Port(), testToken)
	want := fetchSnapshot(t, pyPort, pyToken)

	// error wording of the standard libraries differs; the slot is what counts
	scrub := func(m map[string]any) {
		for _, w := range m["waves"].([]any) {
			wave := w.(map[string]any)
			if wave["error"] != nil {
				wave["error"] = "<error>"
			}
		}
	}
	scrub(got)
	scrub(want)
	for _, key := range []string{"cooldown", "dispatch", "running", "waves", "worktrees"} {
		if !reflect.DeepEqual(got[key], want[key]) {
			g, _ := json.MarshalIndent(got[key], "", " ")
			w, _ := json.MarshalIndent(want[key], "", " ")
			t.Errorf("%s differs from Python's\n--- go\n%s\n--- python\n%s", key, g, w)
		}
	}
	// usage: Python walked the report's own keys (cost, scope, sources,
	// totals) as if they were executors, so its slot never held real
	// numbers; the Go slot is {"<source>/<model>": tokens} (CHANGES.md).
	if usage, ok := got["usage"].(map[string]any); !ok || usage["error"] != nil {
		t.Errorf("usage = %v", got["usage"])
	}
	if len(got) != len(want) {
		t.Errorf("top-level keys: go %d, python %d", len(got), len(want))
	}
}
