package runner

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
)

// Executor prepares and runs one package dispatch. Dispatch returns the
// dispatch result object (status ok | delegate | unavailable | unroutable
// | needs-permission | runaway | error ...).
type Executor interface {
	Prepare(promptFiles []string, cls, workdir, task, runID string) (string, error)
	Dispatch(token, cls string, exhausted []string, noResume bool) map[string]any
}

// StartLocker is an Executor that cuts the task worktree from the
// integration tree at dispatch start: the runner hands it the lock it holds
// while it writes that tree, so a detach never races an integration.
type StartLocker interface {
	SetStartLock(l sync.Locker)
}

// DispatchPollSeconds is one `dispatch wait` poll: short enough that no
// single call nears a foreground ceiling; Dispatch keeps polling.
const DispatchPollSeconds = 300

// CliExecutor dispatches through `agent-exec dispatch prepare` and
// `dispatch --token ... --detach`, then polls `dispatch wait`, so a long
// worker run is never bounded by one call's timeout. Each call is a child
// process of Argv (this binary).
type CliExecutor struct {
	Argv        []string
	Cwd         string
	PollSeconds int

	mu        sync.Mutex
	startLock sync.Locker
}

// SetStartLock implements StartLocker.
func (c *CliExecutor) SetStartLock(l sync.Locker) {
	c.mu.Lock()
	c.startLock = l
	c.mu.Unlock()
}

// selfArgv is how to invoke this agent-exec: the running binary, never a
// PATH lookup, so the loop drives the agent-exec it was started from.
func selfArgv() []string {
	exe, err := os.Executable()
	if err != nil {
		exe = "agent-exec"
	}
	return []string{exe}
}

func (c *CliExecutor) argv() []string {
	if len(c.Argv) > 0 {
		return c.Argv
	}
	return selfArgv()
}

func (c *CliExecutor) call(args ...string) (map[string]any, string) {
	argv := append(append([]string{}, c.argv()...), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = c.Cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil, err.Error()
	}
	parsed, ok := dispatch.ExtractJSONObject(stdout.String())
	if err != nil || !ok {
		return nil, checks.FailureExcerpt(stderr.String() + stdout.String())
	}
	return normalizeNumbers(parsed).(map[string]any), ""
}

// Prepare implements Executor.
func (c *CliExecutor) Prepare(promptFiles []string, cls, workdir, task, runID string) (string, error) {
	args := []string{"dispatch", "prepare", "--class", cls, "--workdir", workdir,
		"--isolate", "always", "--task", task, "--json"}
	for _, p := range promptFiles {
		args = append(args, "--prompt-file", p)
	}
	if runID != "" {
		args = append(args, "--run-id", runID)
	}
	parsed, msg := c.call(args...)
	token, _ := parsed["token"].(string)
	if parsed == nil || token == "" {
		return "", errors.New("dispatch prepare failed: " + msg)
	}
	return token, nil
}

// Dispatch implements Executor.
func (c *CliExecutor) Dispatch(token, cls string, exhausted []string, noResume bool) map[string]any {
	args := []string{"dispatch", "--token", token, "--capture", "--detach"}
	if cls != "" {
		args = append(args, "--class", cls)
	}
	if len(exhausted) > 0 {
		args = append(args, "--exhausted", strings.Join(exhausted, ","))
	}
	if noResume {
		args = append(args, "--no-resume")
	}
	c.mu.Lock()
	lock := c.startLock
	c.mu.Unlock()
	if lock != nil {
		lock.Lock()
	}
	parsed, msg := c.call(args...)
	if lock != nil {
		lock.Unlock()
	}
	if parsed == nil {
		return map[string]any{"status": "error", "reason": "dispatch --detach failed: " + msg}
	}
	switch parsed["status"] {
	case "detached", "running", "done":
	default:
		return parsed
	}
	poll := c.PollSeconds
	if poll <= 0 {
		poll = DispatchPollSeconds
	}
	for {
		parsed, msg = c.call("dispatch", "wait", "--token", token, "--max-wait", strconv.Itoa(poll))
		if parsed == nil {
			return map[string]any{"status": "error", "reason": "dispatch wait failed: " + msg}
		}
		if parsed["status"] != "running" {
			return parsed
		}
	}
}

// normalizeNumbers turns integral float64s (encoding/json's numbers) back
// into int64, so a re-encoded exit code or port reads "1", not "1.0".
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) && x < 1e15 && x > -1e15 {
			return int64(x)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeNumbers(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = normalizeNumbers(e)
		}
		return x
	}
	return v
}
