// Package capture runs one CLI executor child to completion and captures
// its normalized result: the sandboxed spawn (own process group, unbounded
// line length, srt control channel), the watchdog, the parse, and the
// sandbox-denial loop that auto-grants harmless cache paths and resumes
// the same session.
package capture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/executor/pi"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/watchdog"
)

// The child-env additions orchestra owns (pi's managed agent dir) are part
// of every policy sandbox.Prepare builds: pi's state dir is writable.
func init() {
	sandbox.SetExecutorEnv(ExecutorEnv)
}

// ExecutorEnv is the child-env additions orchestra owns for executor.
func ExecutorEnv(executor string, cfg *config.Resolved) map[string]string {
	if executor != "pi" {
		return map[string]string{}
	}
	return tools.PiEnvOverrides(cfg)
}

// ErrExecutorNotFound is a run whose executor binary does not resolve
// (Python's `(127, None)`): nothing was spawned and there is no result.
var ErrExecutorNotFound = errors.New("executor not found")

// NotFoundExitCode is the exit code of an ErrExecutorNotFound dispatch.
const NotFoundExitCode = 127

// ErrUnknownExecutor is a Request for an executor capture has no runner
// for; it never falls back to another executor's argv shape.
var ErrUnknownExecutor = errors.New("no executor registered for profile")

// stderr tail of a failed srt-wrapped run: bounded in lines and characters.
const (
	stderrTailLines = 20
	stderrTailChars = 2000
)

// denialInput is the raw material of sandbox-denial detection: the run's
// stdout (its tool_execution_end records) and stderr.
type denialInput struct {
	Stdout, Stderr string
}

// runner runs one capture under spec (a seam for the grant-loop tests).
var runner = runPi

// runPi runs pi headlessly and parses it. The prompt goes on stdin and
// PI_SKIP_VERSION_CHECK is set so pi never stalls on an update check. inp
// is nil when there is nothing to scan for denials (no policy, or a
// runaway that was cut short).
func runPi(ctx context.Context, r Request, spec sandbox.Spec, prompt, resume string) (*jsonx.Object, *denialInput, error) {
	res := tools.Resolve("pi", r.Cfg)
	if res.Path == "" {
		return nil, nil, ErrExecutorNotFound
	}
	argv := pi.BuildArgv(res.Path, r.Model, r.Effort, r.Workdir, prompt, resume, "json",
		tools.PiContextFiles(r.Cfg))
	env := append(os.Environ(), pi.SkipVersionCheckEnv)
	for k, v := range ExecutorEnv("pi", r.Cfg) {
		env = append(env, k+"="+v)
	}
	env = sandbox.ChildEnv(spec, env)
	out, err := spawn(ctx, spawnOptions{
		Argv: argv, Cwd: existingDir(r.Workdir), Env: env, Input: prompt,
		Spec: spec, Watchdog: newWatchdog(r), Label: "srt pi",
	})
	if err != nil {
		return nil, nil, err
	}
	result := pi.ParseJSONL(out.Stdout, out.Stderr, out.ExitCode, resume != "")
	if spec.Backend == "srt" && out.ExitCode != 0 {
		if tail := stderrTail(out.Stderr); tail != "" {
			result.Set("stderr_tail", tail)
		}
	}
	if len(out.Network) > 0 {
		result.Set("network_denials", netDenials(out.Network))
	}
	if out.Runaway != nil {
		result.Set("status", "runaway").Set("reason", out.Runaway.Reason).
			Set("runaway", out.Runaway.Object())
		return result, nil, nil
	}
	if spec.Policy == nil {
		return result, nil, nil
	}
	return result, &denialInput{Stdout: out.Stdout, Stderr: out.Stderr}, nil
}

// newWatchdog guards a run with the `watchdog:` section, or not at all
// when the section is not an object (Python: watchdog=None).
func newWatchdog(r Request) *watchdog.Watchdog {
	if r.Cfg == nil || r.Cfg.Values == nil {
		return watchdog.New(watchdog.DefaultConfig(), classOr(r.Class))
	}
	v, _ := r.Cfg.Values.Get("watchdog")
	sec, ok := v.(*jsonx.Object)
	if !ok || sec == nil {
		return nil
	}
	return watchdog.New(watchdog.ConfigFrom(sec), classOr(r.Class))
}

func classOr(cls string) string {
	if cls == "" {
		return "standard"
	}
	return cls
}

// existingDir is the child's cwd: the workdir when it is a directory, else
// "" (inherit), so a not-yet-created path still runs.
func existingDir(p string) string {
	if p == "" {
		return ""
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}

func netDenials(list []srt.NetDenial) []any {
	out := make([]any, 0, len(list))
	for _, d := range list {
		var port any
		if d.Port != nil {
			port = int64(*d.Port)
		}
		out = append(out, jsonx.NewObject().Set("host", d.Host).Set("port", port))
	}
	return out
}

// stderrTail is the last stderrTailLines non-blank lines of a child's
// stderr, srt's [SandboxDebug] lines left out, cut to its last
// stderrTailChars characters; "" when nothing is left.
func stderrTail(stderr string) string {
	var lines []string
	for _, line := range pySplitLines(stderr) {
		if strings.TrimFunc(line, isSpace) == "" || strings.HasPrefix(line, "[SandboxDebug]") {
			continue
		}
		lines = append(lines, strings.TrimRightFunc(line, isSpace))
	}
	if len(lines) > stderrTailLines {
		lines = lines[len(lines)-stderrTailLines:]
	}
	runes := []rune(strings.Join(lines, "\n"))
	if len(runes) > stderrTailChars {
		runes = runes[len(runes)-stderrTailChars:]
	}
	return string(runes)
}

// isSpace is Python's str.isspace for one rune.
func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// pySplitLines is str.splitlines().
func pySplitLines(s string) []string {
	var out []string
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			out = append(out, b.String())
			b.Reset()
		case '\r':
			out = append(out, b.String())
			b.Reset()
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// ReadPromptFiles reads UTF-8 prompt files (universal newlines, as Python's
// text mode) and joins them in order: one file verbatim; several with each
// non-last non-empty text's trailing newlines replaced by one blank line,
// empty files skipped. The error names the path that could not be read.
func ReadPromptFiles(paths []string) (string, error) {
	texts := make([]string, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil || !validUTF8(raw) {
			return "", fmt.Errorf("could not read prompt file: %s", p)
		}
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		texts = append(texts, strings.ReplaceAll(text, "\r", "\n"))
	}
	if len(texts) == 1 {
		return texts[0], nil
	}
	var nonempty []string
	for _, t := range texts {
		if t != "" {
			nonempty = append(nonempty, t)
		}
	}
	if len(nonempty) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(nonempty))
	for _, t := range nonempty[:len(nonempty)-1] {
		parts = append(parts, strings.TrimRight(t, "\n"))
	}
	parts = append(parts, nonempty[len(nonempty)-1])
	return strings.Join(parts, "\n\n"), nil
}
