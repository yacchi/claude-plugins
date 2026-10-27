// Command fakeherdr is a stand-in `herdr` (and, invoked as `orca`, a
// stand-in `orca`) for tests of the interactive hosts: no terminal, no
// Claude, fully scripted, so agent-exec can be driven by it as a
// binary on PATH.
//
// All state lives in $FAKEHERDR_DIR: scenario.json (read on every call, so
// a test may change it between calls), state.json (what the fake
// remembers), calls.jsonl (one {"argv", "env"} line per call; env holds
// XDG_CONFIG_HOME and which Claude session markers were set). Calls are
// serialized with a lock file.
//
// herdr scenario keys (all optional):
//
//	running        bool      the server is already up
//	not_ready      bool      `agent start` fails with agent_not_ready (until keys are sent)
//	start_timeout  bool      `agent start` fails with code "timeout" (not agent_not_ready)
//	screen         string    what `agent read` and `pane read` show
//	statuses       []string  the agent_status sequence of `agent get` (the last repeats; default idle)
//	stalls         int       the next N `agent prompt` calls fail with agent_prompt_stalled
//	fail_warm      bool      a warm reader agent (p-warm*) never starts
//	after_keys     []string  screens `agent read` shows after send-keys, one per read (the last repeats)
//	get_error      string    `agent get` fails with this error code
//	prompt_result  object    written to the result file a package prompt names
//	prompt_write   object    path -> content, written on every package prompt
//
// A warm reader prompt ("Read each of these files ...") always gets
// {"status": "ok"}. `server` marks the server running and exits. The open
// workspaces are remembered: `workspace list` reports them, `workspace
// close` and `server stop` drop them.
//
// orca scenario keys: status_ok (default true), trust (the folder-trust
// dialog shows first), never_ready, prompts: {pkg: [{files: {rel: body},
// result: {...}, after_waits: n, dialog: bool}]}. Worktrees are real `git
// worktree add`s under $FAKEHERDR_DIR/workspaces.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:]))
}

type call struct {
	scenario map[string]any
	state    map[string]any
	args     []string
	out      *os.File
	errw     *os.File
}

func run(name string, args []string) int {
	dir := os.Getenv("FAKEHERDR_DIR")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "fakeherdr: FAKEHERDR_DIR is not set")
		return 2
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeherdr:", err)
		return 2
	}
	defer lock.Close()
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	orca := strings.HasPrefix(name, "orca")
	if orca {
		var kept []string
		for _, a := range args {
			if a != "--json" {
				kept = append(kept, a)
			}
		}
		args = kept
	}
	logCall(dir, args)
	c := &call{scenario: readJSON(filepath.Join(dir, "scenario.json")), args: args, out: os.Stdout, errw: os.Stderr}
	statePath := filepath.Join(dir, "state.json")
	c.state = readJSON(statePath)
	var code int
	if orca {
		if len(c.state) == 0 {
			c.state = map[string]any{"worktrees": map[string]any{}, "terminals": map[string]any{},
				"next": 0.0, "prompt_index": map[string]any{}, "waits": 0.0}
		}
		code = c.orca(dir)
	} else {
		if len(c.state) == 0 {
			c.state = initHerdr(c.scenario)
		}
		code = c.herdr()
	}
	writeJSON(statePath, c.state)
	return code
}

func logCall(dir string, args []string) {
	env := map[string]any{"XDG_CONFIG_HOME": os.Getenv("XDG_CONFIG_HOME")}
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", "HERDR_CONFIG_PATH"} {
		if _, ok := os.LookupEnv(k); ok {
			env[k] = true
		}
	}
	line, _ := json.Marshal(map[string]any{"argv": args, "env": env})
	f, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

func readJSON(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func writeJSON(path string, v any) {
	b, _ := json.Marshal(v)
	_ = os.WriteFile(path, b, 0o644)
}

func writeFile(path, text string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(text), 0o644)
}

func (c *call) emit(v any) {
	if v == nil {
		return
	}
	b, _ := json.Marshal(v)
	c.out.Write(append(b, '\n'))
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return false
}

func strList(v any) []any {
	l, _ := v.([]any)
	return append([]any(nil), l...)
}

func opt(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func has(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}
