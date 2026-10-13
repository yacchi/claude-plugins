// Package gitx runs git the way agent-exec needs it: stdout captured, stderr
// kept for diagnostics, and a non-zero exit reported as *ExitError rather
// than a panic or a lost status.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ExitError is git exiting non-zero. Code is -1 when git could not be
// started at all (missing binary, missing working directory).
type ExitError struct {
	Code   int
	Stderr string
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
	return run(cwd, nil, nil, args)
}

// RunEnv is Run with an explicit environment (nil inherits the process's).
func RunEnv(cwd string, env []string, args ...string) (string, error) {
	return run(cwd, env, nil, args)
}

// RunInput is Run with stdin fed from input.
func RunInput(cwd string, input []byte, args ...string) (string, error) {
	return run(cwd, nil, input, args)
}

func run(cwd string, env []string, input []byte, args []string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = env
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.String(), &ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
	}
	return "", &ExitError{Code: -1, Stderr: err.Error()}
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
