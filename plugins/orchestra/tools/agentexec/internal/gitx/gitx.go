// Package gitx runs git the way agent-exec needs it: stdout captured, stderr
// kept for diagnostics, and a non-zero exit reported as *ExitError rather
// than a panic or a lost status.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// ExitError is git exiting non-zero. Code is -1 when git could not be
// started at all (missing binary, missing working directory) or was killed
// at its deadline (TimedOut).
type ExitError struct {
	Code     int
	Stderr   string
	TimedOut bool
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("git exited %d", e.Code)
	}
	return fmt.Sprintf("git exited %d: %s", e.Code, msg)
}

// Run runs `git args...` in cwd ("" = the current directory) and returns
// its stdout.
func Run(cwd string, args ...string) (string, error) {
	return run(cwd, nil, nil, 0, args)
}

// RunEnv is Run with an explicit environment (nil inherits the process's).
func RunEnv(cwd string, env []string, args ...string) (string, error) {
	return run(cwd, env, nil, 0, args)
}

// RunInput is Run with stdin fed from input.
func RunInput(cwd string, input []byte, args ...string) (string, error) {
	return run(cwd, nil, input, 0, args)
}

// run runs git; timeout 0 means no limit, and a timed-out git is killed and
// reported as Code -1.
func run(cwd string, env []string, input []byte, timeout time.Duration, args []string) (string, error) {
	end := trace.Begin(trace.KindGit, gitOp(args), trace.Attrs{Cwd: cwd, Detail: strings.Join(args, " ")})
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	// A hook's background child holding the pipes open must not outlive
	// the deadline either.
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = cwd
	cmd.Env = env
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		end(0)
		return stdout.String(), nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		end(-1)
		return stdout.String(), &ExitError{Code: -1, TimedOut: true,
			Stderr: fmt.Sprintf("git %s timed out after %s\n%s", gitOp(args), timeout, stderr.String())}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		end(ee.ExitCode())
		return stdout.String(), &ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
	}
	end(-1)
	return "", &ExitError{Code: -1, Stderr: err.Error()}
}

// CommitTimeout bounds each step of CommitAll: a hung hook manager or a
// stale index lock must never stall an integration. A variable only so
// tests can shorten it.
var CommitTimeout = 120 * time.Second

// noHooks turns off the hook managers that still run hooks --no-verify
// does not cover (prepare-commit-msg, post-commit).
var noHooks = []string{"LEFTHOOK=0", "HUSKY=0"}

// CommitAll stages everything in cwd (only pathspec when given) and commits
// it as orchestra with message, --allow-empty, with the repository's hooks
// off: verifying the result is the caller's business, never a hook's. A
// failing step's error is *ExitError whose Stderr carries git's stderr and
// then its stdout (hook managers report on either).
func CommitAll(cwd, message string, pathspec ...string) error {
	env := append(os.Environ(), noHooks...)
	steps := [][]string{
		append([]string{"add", "-A"}, pathspec...),
		{"-c", "user.email=orchestra@local", "-c", "user.name=orchestra",
			"commit", "-q", "--no-verify", "--allow-empty", "-m", message},
	}
	for _, args := range steps {
		started := time.Now()
		out, err := run(cwd, env, nil, CommitTimeout, args)
		if err != nil {
			var ee *ExitError
			if errors.As(err, &ee) {
				if ee.TimedOut {
					removeOwnIndexLock(cwd, started)
				}
				return &ExitError{Code: ee.Code, Stderr: ee.Stderr + out, TimedOut: ee.TimedOut}
			}
			return err
		}
	}
	return nil
}

// removeOwnIndexLock removes the index.lock a git step killed at its
// deadline left in cwd's git dir, so the caller's rollback can run. Only a
// lock no older than the step's start (second granularity, for coarse
// filesystems) can be the killed git's own; an older one belongs to someone
// else and is never touched.
func removeOwnIndexLock(cwd string, started time.Time) {
	dir, err := GitDir(cwd)
	if err != nil {
		return
	}
	lock := filepath.Join(dir, "index.lock")
	st, err := os.Stat(lock)
	if err != nil || st.ModTime().Before(started.Truncate(time.Second)) {
		return
	}
	if os.Remove(lock) == nil {
		fmt.Fprintf(os.Stderr, "agent-exec: removed %s left by a timed-out git step\n", lock)
	}
}

// subSubcommands are the git subcommands whose first operand names the
// operation (`git worktree add`, `git stash push`).
var subSubcommands = map[string]bool{"worktree": true, "stash": true, "submodule": true, "remote": true}

// gitOp names a git call for the timing log: "git <subcommand>", plus the
// operand for subSubcommands. Global options (-c k=v, -C dir, other flags)
// are skipped.
func gitOp(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-c" || a == "-C" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if subSubcommands[a] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			return "git " + a + " " + args[i+1]
		}
		return "git " + a
	}
	return "git"
}

// RepoRoot is the working tree root containing path, or false outside a
// repository (or when path does not exist).
func RepoRoot(path string) (string, bool) {
	out, err := Run(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(out)
	return root, root != ""
}

// IsDirty reports uncommitted work worth protecting: tracked changes or
// untracked files. Ignored files (node_modules and friends) do not count.
func IsDirty(path string) bool {
	root, ok := RepoRoot(path)
	if !ok {
		return false
	}
	out, err := Run(root, "status", "--porcelain")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// ConfigEnv returns base (nil: the process environment) plus pairs added to
// git's config lookup through the GIT_CONFIG_COUNT protocol, so the settings
// live for one subprocess and no config file is touched. An existing count
// in base is extended, not overwritten.
func ConfigEnv(pairs [][2]string, base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	env := append([]string(nil), base...)
	if len(pairs) == 0 {
		return env
	}
	start := 0
	if v, ok := lookup(env, "GIT_CONFIG_COUNT"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			start = n
		}
	}
	for i, p := range pairs {
		env = setEnv(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", start+i), p[0])
		env = setEnv(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", start+i), p[1])
	}
	return setEnv(env, "GIT_CONFIG_COUNT", strconv.Itoa(start+len(pairs)))
}

func lookup(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func setEnv(env []string, key, value string) []string {
	out := env[:0]
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

// Worktree is one entry of `git worktree list --porcelain`. Branch is the
// short name ("" when detached or bare).
type Worktree struct {
	Path, Branch, Head string
	Detached           bool
}

// WorktreeList lists the worktrees of the repository at root, main tree
// first.
func WorktreeList(root string) ([]Worktree, error) {
	out, err := Run(root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []Worktree
	cur := Worktree{}
	flush := func() {
		if cur.Path != "" {
			list = append(list, cur)
		}
		cur = Worktree{}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			cur.Path = strings.TrimSpace(line[len("worktree "):])
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimSpace(line[len("branch "):]), "refs/heads/")
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimSpace(line[len("HEAD "):])
		case strings.TrimSpace(line) == "detached":
			cur.Detached = true
		}
	}
	flush()
	return list, nil
}

// ApplyPatch3Way applies patch in dir with `git apply --3way`: where a plain
// apply would fail on context drift, the 3-way form degrades to conflict
// markers plus unmerged index entries, a reportable outcome. A non-nil error
// is *ExitError carrying git's exit code.
func ApplyPatch3Way(dir string, patch []byte) error {
	_, err := RunInput(dir, patch, "apply", "--3way", "--binary", "--whitespace=nowarn", "-")
	return err
}

// GitDir is the absolute git dir of a worktree (for a linked worktree,
// .git/worktrees/<name> of the main repository).
func GitDir(worktree string) (string, error) {
	out, err := Run(worktree, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		return "", errors.New("git rev-parse --absolute-git-dir printed nothing")
	}
	return dir, nil
}
