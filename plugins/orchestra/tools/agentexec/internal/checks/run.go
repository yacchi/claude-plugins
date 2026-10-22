package checks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
)

// DefaultTimeout is a check's timeout in seconds when it declares none.
const DefaultTimeout = 1800

var phaseOrder = map[string]int{"prepare": 0, "lint": 1, "type": 2, "test": 3, "heavy": 4}

// Item is one `checks.items` entry. Timeout is in seconds (0 = default).
type Item struct {
	Name       string   `json:"name"`
	Paths      []string `json:"paths"`
	Run        string   `json:"run"`
	Fix        string   `json:"fix"`
	Cwd        string   `json:"cwd"`
	Timeout    float64  `json:"timeout"`
	Junit      string   `json:"junit"`
	Phase      string   `json:"phase"`
	RetryAlone string   `json:"retry_alone"`
}

// Ensure is one `checks.ensure` entry: run Run unless Exists is present.
type Ensure struct {
	Exists  string  `json:"exists"`
	Run     string  `json:"run"`
	Cwd     string  `json:"cwd"`
	Timeout float64 `json:"timeout"`
}

// Config is the `checks` config section.
type Config struct {
	MaxParallel      int      `json:"max_parallel"`
	Items            []Item   `json:"items"`
	RetryFailedAlone bool     `json:"retry_failed_alone"`
	Ensure           []Ensure `json:"ensure"`
}

// LoadConfig reads the `checks` section (defaults: max_parallel 2, retry on).
func LoadConfig(cfg *config.Resolved) (Config, error) {
	c := Config{MaxParallel: 2, RetryFailedAlone: true}
	if err := config.Decode(cfg, "checks", &c); err != nil {
		return c, err
	}
	return c, nil
}

// Event is a check-start / check-end notification.
type Event struct {
	Event  string
	Detail map[string]any
}

// Options configures Run.
type Options struct {
	Files            []string // changed files, tree-relative
	SlotDir          string   // directory of the machine-wide concurrency lock files
	MaxParallel      int      // 0 = unlimited
	RunAll           bool     // keep going after a failure
	Ensure           []Ensure
	RetryFailedAlone bool
	OnEvent          func(Event)
}

// CheckResult is one check's outcome. Status is pass | fail | skipped |
// preexisting (the last is assigned by the caller's baseline pass).
type CheckResult struct {
	Name     string
	Status   string
	Exit     *int
	Seconds  float64
	TimedOut bool
	Excerpt  string
	Reason   string
	Flaky    []string
}

// Object is the result as a JSON object (name, status, exit, seconds,
// timed_out, excerpt, reason, then flaky when a retry passed).
func (r CheckResult) Object() map[string]any {
	o := map[string]any{"name": r.Name, "status": r.Status}
	if r.Exit == nil {
		o["exit"] = nil
	} else {
		o["exit"] = *r.Exit
	}
	o["seconds"] = r.Seconds
	o["timed_out"] = r.TimedOut
	o["excerpt"] = r.Excerpt
	o["reason"] = r.Reason
	if r.Flaky != nil {
		o["flaky"] = r.Flaky
	}
	return o
}

// Result is the outcome of Run: one entry per check, in execution order
// (a failed `ensure` entry comes first).
type Result struct {
	Checks []CheckResult
}

// Status folds the checks into the command's status: fail > preexisting >
// no-checks (nothing ran) > pass.
func (r Result) Status() string {
	seen := map[string]bool{}
	for _, c := range r.Checks {
		seen[c.Status] = true
	}
	switch {
	case seen["fail"]:
		return "fail"
	case seen["preexisting"]:
		return "preexisting"
	}
	for s := range seen {
		if s != "skipped" {
			return "pass"
		}
	}
	return "no-checks"
}

func emptyResult(name, status, reason string) CheckResult {
	return CheckResult{Name: name, Status: status, Reason: reason}
}

func notify(on func(Event), event string, detail map[string]any) {
	if on != nil {
		on(Event{Event: event, Detail: detail})
	}
}

// Run runs the checks by phase (prepare, lint, type, test, heavy; ties in
// declaration order), stopping at the first failure unless o.RunAll. root is
// the tree the checks run in.
func Run(root string, items []Item, o Options) Result {
	for _, it := range items {
		if _, ok := phaseOrder[phaseOf(it)]; !ok {
			return Result{Checks: []CheckResult{emptyResult("config", "fail", "invalid phase: "+it.Phase)}}
		}
	}
	ordered := append([]Item(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool { return phaseOrder[phaseOf(ordered[i])] < phaseOrder[phaseOf(ordered[j])] })

	var results []CheckResult
	var ensureFailure *CheckResult
	for _, r := range runEnsure(o.Ensure, root, o) {
		if r.Status == "fail" {
			r := r
			ensureFailure = &r
			break
		}
	}
	if ensureFailure != nil {
		results = append(results, *ensureFailure)
		if !o.RunAll {
			for _, it := range ordered {
				results = append(results, emptyResult(it.Name, "skipped", "not run after earlier failure"))
			}
			return Result{Checks: results}
		}
	}
	stop := false
	for _, it := range ordered {
		if stop {
			results = append(results, emptyResult(it.Name, "skipped", "not run after earlier failure"))
			continue
		}
		res, raw := runCheck(it, root, o.Files, o.SlotDir, o.MaxParallel, o.OnEvent)
		if res.Status == "fail" && o.RetryFailedAlone {
			template := it.RetryAlone
			if template == "" && strings.Contains(it.Run, "{files}") {
				template = strings.ReplaceAll(it.Run, "{files}", "{failed}")
			}
			if template != "" {
				status, failed := RerunFailedAlone(raw, template, root, o.SlotDir, o.MaxParallel, timeoutOf(it.Timeout))
				if status == "pass" {
					res.Status = "pass"
					res.Excerpt = ""
					res.Flaky = failed
				}
			}
		}
		results = append(results, res)
		if res.Status == "fail" && !o.RunAll {
			stop = true
		}
	}
	return Result{Checks: results}
}

func phaseOf(it Item) string {
	if it.Phase == "" {
		return "test"
	}
	return it.Phase
}

func timeoutOf(sec float64) time.Duration {
	if sec <= 0 {
		sec = DefaultTimeout
	}
	return time.Duration(sec * float64(time.Second))
}

func runEnsure(entries []Ensure, root string, o Options) []CheckResult {
	var results []CheckResult
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(root, e.Exists)); err == nil {
			continue
		}
		item := Item{Name: "ensure:" + e.Exists, Run: e.Run, Cwd: e.Cwd, Timeout: e.Timeout}
		res, _ := runCheck(item, root, nil, o.SlotDir, o.MaxParallel, o.OnEvent)
		results = append(results, res)
		if res.Status == "fail" {
			break
		}
	}
	return results
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func runCheck(it Item, tree string, files []string, slotDir string, maxParallel int, on func(Event)) (CheckResult, string) {
	cwdRel := it.Cwd
	if cwdRel == "" {
		cwdRel = "."
	}
	cwdDir := filepath.Join(tree, cwdRel)
	timeout := timeoutOf(it.Timeout)

	matched := MatchFiles(it.Paths, files)
	if len(it.Paths) > 0 && len(matched) == 0 {
		return emptyResult(it.Name, "skipped", "no matching files"), ""
	}
	usesFiles := strings.Contains(it.Run, "{files}")
	quoted := quoteFiles(filesForCommand(tree, cwdDir, matched))
	if usesFiles && quoted == "" {
		return emptyResult(it.Name, "skipped", "no matching files"), ""
	}

	notify(on, "check-start", map[string]any{"name": it.Name, "tree": tree})
	junitPath := ""
	if it.Junit != "" {
		junitPath = filepath.Join(cwdDir, it.Junit)
		if isFile(junitPath) {
			_ = os.Remove(junitPath)
		}
	}
	if it.Fix != "" {
		fix := it.Fix
		if strings.Contains(fix, "{files}") {
			fix = strings.ReplaceAll(fix, "{files}", quoted)
		}
		runShell(fix, cwdDir, timeout, slotDir, maxParallel)
	}
	cmd := it.Run
	if usesFiles {
		cmd = strings.ReplaceAll(cmd, "{files}", quoted)
	}
	sh := runShell(cmd, cwdDir, timeout, slotDir, maxParallel)
	passed := sh.exit != nil && *sh.exit == 0 && !sh.timedOut
	status := "fail"
	excerpt := ""
	if passed {
		status = "pass"
	} else {
		excerpt = buildExcerpt(junitPath, sh.stdout+sh.stderr, 4000)
	}
	notify(on, "check-end", map[string]any{"name": it.Name, "status": status, "seconds": sh.seconds})
	return CheckResult{
		Name: it.Name, Status: status, Exit: sh.exit, Seconds: sh.seconds,
		TimedOut: sh.timedOut, Excerpt: excerpt,
	}, sh.stdout + sh.stderr
}

type shellResult struct {
	exit           *int
	stdout, stderr string
	timedOut       bool
	seconds        float64
}

// runShell runs command via /bin/sh -c in its own session, killing the whole
// process group on timeout. It holds one of the maxParallel check slots and
// one slot of the machine-wide heavy pool while it runs. A command that
// cannot be started is reported as output of a failed run rather than
// raised.
func runShell(command, cwd string, timeout time.Duration, slotDir string, maxParallel int) shellResult {
	release := acquireSlot(slotDir, maxParallel)
	defer release()
	defer holdHeavy(command, timeout)()
	start := time.Now()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return shellResult{stderr: err.Error() + "\n", seconds: time.Since(start).Seconds()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timedOut := false
	var err error
	var expired <-chan time.Time // timeout <= 0: no limit
	if timeout > 0 {
		expired = time.After(timeout)
	}
	select {
	case err = <-done:
	case <-expired:
		timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	res := shellResult{stdout: stdout.String(), stderr: stderr.String(), timedOut: timedOut, seconds: time.Since(start).Seconds()}
	if !timedOut {
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = -int(ws.Signal())
			}
		}
		res.exit = &code
	}
	return res
}

const slotPoll = 500 * time.Millisecond

func slotPath(dir string, n int) string { return filepath.Join(dir, "slot-"+strconv.Itoa(n)+".lock") }

// acquireSlot blocks until one of maxParallel lock files is free and returns
// its release. maxParallel 0 means unlimited: nothing is taken.
func acquireSlot(dir string, maxParallel int) func() {
	if maxParallel <= 0 {
		return func() {}
	}
	_ = os.MkdirAll(dir, 0o755)
	for {
		for n := 0; n < maxParallel; n++ {
			if unlock, err := fsutil.Lock(slotPath(dir, n), fsutil.Exclusive, 0); err == nil {
				return func() { _ = unlock() }
			}
		}
		time.Sleep(slotPoll)
	}
}

// heavyPool is the machine-wide pool of heavy commands (`throttle.heavy_slots`),
// read once per process.
var heavyPool = sync.OnceValue(func() slot.Pool {
	var size any
	if cfg, err := config.Resolve(); err == nil {
		size, _ = cfg.Lookup("throttle", "heavy_slots")
	}
	return slot.New(slot.Heavy, slot.SizeOf(size))
})

// holdHeavy takes one heavy slot for command and returns its release. Any
// failure runs the command unadmitted: admission must never fail a check.
func holdHeavy(command string, timeout time.Duration) func() {
	p := heavyPool()
	if p.Off() {
		return func() {}
	}
	ttl := 24 * time.Hour // no timeout: the owner's death still frees it
	if timeout > 0 {
		ttl = timeout + time.Minute
	}
	op, _ := slot.Classify(command, nil)
	lease, _, err := p.Acquire(context.Background(), slot.Request{Owner: os.Getpid(), TTL: ttl, Op: op})
	if err != nil {
		return func() {}
	}
	return func() { _ = p.Release(lease.Token) }
}

// RerunFailedAlone re-runs the failing files output names with template
// ({failed} = the shell-quoted files) while holding every check slot, so
// the retry is not disturbed by other checks; like any check command it
// takes only one slot of the machine-wide heavy pool. status is pass,
// fail, or skipped (no file named); timeout <= 0 means no limit.
func RerunFailedAlone(output, template, cwd, slotDir string, maxParallel int, timeout time.Duration) (status string, failed []string) {
	failed = ParseFailedFiles(output)
	if len(failed) == 0 {
		return "skipped", nil
	}
	command := strings.ReplaceAll(template, "{failed}", quoteFiles(failed))
	if maxParallel > 0 {
		var held []func() error
		for {
			_ = os.MkdirAll(slotDir, 0o755)
			held = held[:0]
			for n := 0; n < maxParallel; n++ {
				unlock, err := fsutil.Lock(slotPath(slotDir, n), fsutil.Exclusive, 0)
				if err != nil {
					break
				}
				held = append(held, unlock)
			}
			if len(held) == maxParallel {
				break
			}
			for _, u := range held {
				_ = u()
			}
			time.Sleep(slotPoll)
		}
		defer func() {
			for i := len(held) - 1; i >= 0; i-- {
				_ = held[i]()
			}
		}()
	}
	sh := runShell(command, cwd, timeout, slotDir, 0)
	if sh.exit != nil && *sh.exit == 0 && !sh.timedOut {
		return "pass", failed
	}
	return "fail", failed
}
