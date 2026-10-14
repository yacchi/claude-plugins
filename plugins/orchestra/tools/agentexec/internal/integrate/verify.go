package integrate

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// VerifyResult grades one run of a verify command. Baseline ("pass" |
// "fail" | "") and Reason are only filled in by bisection and the skip
// policy; Dirtied counts the porcelain lines the command left behind (they
// are reset away).
type VerifyResult struct {
	Status   string // pass | fail | skipped
	Exit     *int   // nil when the command never finished (timeout, spawn failure)
	Seconds  float64
	TimedOut bool
	Excerpt  string
	Baseline string
	Reason   string
	Dirtied  int
}

func (v VerifyResult) MarshalJSON() ([]byte, error) {
	var exit any
	if v.Exit != nil {
		exit = *v.Exit
	}
	var baseline any
	if v.Baseline != "" {
		baseline = v.Baseline
	}
	return fsutil.MarshalJSON(map[string]any{
		"status":    v.Status,
		"exit":      exit,
		"seconds":   v.Seconds,
		"timed_out": v.TimedOut,
		"excerpt":   v.Excerpt,
		"baseline":  baseline,
		"reason":    v.Reason,
		"dirtied":   v.Dirtied,
	})
}

func skippedVerify(reason string) *VerifyResult {
	return &VerifyResult{Status: "skipped", Reason: reason}
}

// verifyKillGrace is how long output is still collected after a timed-out
// command's process group was killed.
const verifyKillGrace = 5 * time.Second

// RunVerify runs cmd through /bin/sh -c in dir and grades it: exit 0 within
// timeout (0 = no limit) passes. A timeout kills the whole process group
// (the command may fork workers) and fails. Whatever the command left in the
// tree is reset away afterwards. Never fails itself.
func RunVerify(dir, cmd string, timeout time.Duration) VerifyResult {
	start := time.Now()
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = verifyKillGrace
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out

	var exit *int
	timedOut := false
	if err := c.Start(); err == nil {
		werr := c.Wait()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			timedOut = true
		} else if werr == nil || c.ProcessState != nil {
			code := exitCode(c.ProcessState)
			exit = &code
		}
	}
	res := VerifyResult{
		Status:   "fail",
		Exit:     exit,
		Seconds:  time.Since(start).Seconds(),
		TimedOut: timedOut,
	}
	if exit != nil && *exit == 0 && !timedOut {
		res.Status = "pass"
	} else {
		res.Excerpt = checks.FailureExcerpt(out.String())
	}
	if status, err := gitx.Run(dir, "status", "--porcelain"); err == nil {
		res.Dirtied = len(fsutil.SplitLines(status))
	}
	if res.Dirtied > 0 {
		_, _ = gitx.Run(dir, "reset", "-q", "--hard", "HEAD")
		_, _ = gitx.Run(dir, "clean", "-fdq")
	}
	return res
}

// exitCode is the exit status, or minus the signal that killed the process.
func exitCode(ps interface{ Sys() any }) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return -int(ws.Signal())
		}
		return ws.ExitStatus()
	}
	return -1
}
