package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// checksCfg is the `checks` section as the runner uses it.
type checksCfg struct {
	raw              []any // checks.items as written (with their `fast` flag)
	ensure           []checks.Ensure
	retryFailedAlone bool
	slotDir          string
	maxParallel      int
	flakyThreshold   int
}

// checksConfig reads the current `checks` section; the check slots are the
// machine-wide ones `agent-exec check` uses (beside the ledger dir), or
// the state dir's when the config cannot be read.
func (r *runner) checksConfig() checksCfg {
	c := checksCfg{
		retryFailedAlone: true, slotDir: filepath.Join(r.stateDir, "check-slots"),
		maxParallel: 2, flakyThreshold: 3,
	}
	cfg := r.config()
	if cfg == nil {
		return c
	}
	sec := cfg.Section("checks")
	c.raw = asList(objGet(sec, "items"))
	if b, ok := objGet(sec, "retry_failed_alone").(bool); ok {
		c.retryFailedAlone = b
	} else if v := objGet(sec, "retry_failed_alone"); v != nil {
		c.retryFailedAlone = truthy(v)
	}
	if loaded, err := checks.LoadConfig(cfg); err == nil {
		c.ensure = loaded.Ensure
	}
	c.slotDir = filepath.Join(filepath.Dir(absPath(ledger.Dir(cfg))), "check-slots")
	if v, ok := objGet(sec, "max_parallel").(int64); ok {
		c.maxParallel = int(v)
	}
	switch v := objGet(sec, "flaky_threshold").(type) {
	case int64:
		c.flakyThreshold = int(v)
	case float64:
		c.flakyThreshold = int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			c.flakyThreshold = n
		}
	}
	return c
}

// itemsWhere is the check items marked `fast: true` (fast) or the rest.
func (c checksCfg) itemsWhere(fast bool) []checks.Item {
	var out []checks.Item
	for _, raw := range c.raw {
		obj, ok := raw.(*jsonx.Object)
		if !ok {
			continue
		}
		isFast, _ := objGet(obj, "fast").(bool)
		if isFast != fast {
			continue
		}
		b, err := jsonx.Marshal(obj)
		if err != nil {
			continue
		}
		var it checks.Item
		if json.Unmarshal(b, &it) == nil {
			out = append(out, it)
		}
	}
	return out
}

// ensureFailure runs `checks.ensure` in tree; the first failing entry, or
// nil.
func (r *runner) ensureFailure(tree string) *ensureFail {
	cfg := r.config()
	if cfg == nil {
		return nil
	}
	c := r.checksConfig()
	if len(c.ensure) == 0 {
		return nil
	}
	results := checks.Run(tree, nil, checks.Options{Ensure: c.ensure, SlotDir: c.slotDir, MaxParallel: c.maxParallel}).Checks
	for _, res := range results {
		if res.Status == "fail" {
			return &ensureFail{result: res, excerpt: checks.FailureExcerpt(res.Name + "\n" + res.Excerpt)}
		}
	}
	return nil
}

type ensureFail struct {
	result  checks.CheckResult
	excerpt string
}

// --- the --full-retry flaky filter -------------------------------------------------------

// rerunFailedAlone re-runs the failing test files a red output names with
// template ({failed} = the quoted files) while holding every check slot,
// so nothing else disturbs the retry. status: pass | fail | skipped.
func rerunFailedAlone(output, template, cwd, slotDir string, maxParallel int, timeout time.Duration) (string, []string) {
	failed := checks.ParseFailedFiles(output)
	if len(failed) == 0 {
		return "skipped", nil
	}
	quoted := make([]string, len(failed))
	for i, f := range failed {
		quoted[i] = sandbox.ShellQuote(f)
	}
	command := strings.ReplaceAll(template, "{failed}", strings.Join(quoted, " "))
	release := holdAllSlots(slotDir, maxParallel)
	defer release()
	if exit := runShell(cwd, command, timeout, nil); exit != nil && *exit == 0 {
		return "pass", failed
	}
	return "fail", failed
}

const slotPoll = 500 * time.Millisecond

// holdAllSlots takes every slot-N.lock of the machine-wide check slots
// (the files `agent-exec check` takes one of).
func holdAllSlots(dir string, n int) func() {
	if n <= 0 {
		return func() {}
	}
	for {
		_ = os.MkdirAll(dir, 0o755)
		var held []func() error
		for i := 0; i < n; i++ {
			unlock, err := fsutil.Lock(filepath.Join(dir, "slot-"+strconv.Itoa(i)+".lock"), fsutil.Exclusive, 0)
			if err != nil {
				break
			}
			held = append(held, unlock)
		}
		release := func() {
			for i := len(held) - 1; i >= 0; i-- {
				_ = held[i]()
			}
		}
		if len(held) == n {
			return release
		}
		release()
		time.Sleep(slotPoll)
	}
}

// runShell runs command via /bin/sh -c in its own session (timeout 0 = no
// limit; a timeout kills the group); exit is nil when it never finished.
func runShell(dir, command string, timeout time.Duration, env []string) *int {
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	c.Dir = dir
	c.Env = env
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 5 * time.Second
	if err := c.Run(); err != nil && c.ProcessState == nil {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	code := c.ProcessState.ExitCode()
	if ws, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = -int(ws.Signal())
	}
	return &code
}
