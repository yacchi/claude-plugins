package integrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
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
	return jsonx.Marshal(jsonx.NewObject().
		Set("status", v.Status).Set("exit", exit).Set("seconds", v.Seconds).
		Set("timed_out", v.TimedOut).Set("excerpt", v.Excerpt).Set("baseline", baseline).
		Set("reason", v.Reason).Set("dirtied", v.Dirtied))
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
		res.Excerpt = FailureExcerpt(out.String())
	}
	if status, err := gitx.Run(dir, "status", "--porcelain"); err == nil {
		res.Dirtied = len(splitLines(status))
	}
	if res.Dirtied > 0 {
		_, _ = gitx.Run(dir, "reset", "-q", "--hard", "HEAD")
		_, _ = gitx.Run(dir, "clean", "-fdq")
	}
	return res
}

// exitCode is Python's returncode: the exit status, or minus the signal
// that killed the process.
func exitCode(ps interface{ Sys() any }) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return -int(ws.Signal())
		}
		return ws.ExitStatus()
	}
	return -1
}

// splitLines is Python's str.splitlines for the separators git can emit.
func splitLines(s string) []string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// The excerpt helpers below are agent_exec_checks.failure_excerpt: the part
// of a failed command's output worth handing to a fixer.

var (
	reANSI   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	reAnchor = regexp.MustCompile(`\bFAIL(?:ED)?\b|[×✖✗]|\berror TS\d+|\berror\[|\bError:|\bAssertionError\b` +
		`|panicked at|\bnot ok\b|^Traceback|^\s*Expected\b|^\s*Received\b|Timed out|timed out`)
	reMarkup = regexp.MustCompile(`^\s*(?:</?[A-Za-z][\w:-]*(?:\s|/?>|$)|/?>$|[\w:-]+="[^"]*"\s*/?>?$)`)
)

const (
	excerptLimit   = 4000
	markupRun      = 4
	contextAfter   = 3
	tailLines      = 30
	pyLineBreakers = "\n\r\v\f\x1c\x1d\x1e\u0085  "
)

// pySplitLines is Python's str.splitlines (no keepends).
func pySplitLines(s string) []string {
	var lines []string
	for len(s) > 0 {
		i := strings.IndexAny(s, pyLineBreakers)
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i])
		r, size := utf8.DecodeRuneInString(s[i:])
		s = s[i+size:]
		if r == '\r' && strings.HasPrefix(s, "\n") {
			s = s[1:]
		}
	}
	return lines
}

func collapseMarkup(lines []string) []string {
	var out, run []string
	flush := func() {
		if len(run) < markupRun {
			out = append(out, run...)
		} else {
			out = append(out, fmt.Sprintf("  … (%d markup lines omitted)", len(run)))
		}
		run = nil
	}
	for _, line := range lines {
		if reMarkup.MatchString(line) {
			run = append(run, line)
			continue
		}
		flush()
		out = append(out, line)
	}
	flush()
	return out
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func clip(text string, limit int) string {
	n := runeLen(text)
	if n <= limit {
		return text
	}
	marker := func(k int) string { return fmt.Sprintf("\n… (%d chars omitted) …\n", k) }
	keep := max(limit-runeLen(marker(n)), 0)
	head := keep * 2 / 3
	tail := keep - head
	r := []rune(text)
	out := string(r[:head]) + marker(n-keep)
	if tail > 0 {
		out += string(r[len(r)-tail:])
	}
	return out
}

// FailureExcerpt picks failure anchors (with a few lines of context) first,
// then the tail of the output, at most 4000 characters. Markup dumps
// collapse to one line.
func FailureExcerpt(text string) string {
	if text == "" {
		return ""
	}
	lines := collapseMarkup(pySplitLines(reANSI.ReplaceAllString(text, "")))
	var picked []int
	seen := map[int]bool{}
	budget := excerptLimit * 2 / 3
	used := 0
	for i, line := range lines {
		if !reAnchor.MatchString(line) {
			continue
		}
		for j := i; j < min(i+1+contextAfter, len(lines)); j++ {
			if seen[j] {
				continue
			}
			seen[j] = true
			picked = append(picked, j)
			used += runeLen(lines[j]) + 1
		}
		if used >= budget {
			break
		}
	}
	var tail []string
	for j := max(len(lines)-tailLines, 0); j < len(lines); j++ {
		if !seen[j] {
			tail = append(tail, lines[j])
		}
	}
	var body string
	if len(picked) > 0 {
		parts := make([]string, len(picked))
		for k, j := range picked {
			parts[k] = lines[j]
		}
		body = strings.Join(parts, "\n")
		if len(tail) > 0 {
			body += "\n--- tail ---\n" + strings.Join(tail, "\n")
		}
	} else {
		body = strings.Join(lines[max(len(lines)-tailLines*2, 0):], "\n")
	}
	return clip(body, excerptLimit)
}
