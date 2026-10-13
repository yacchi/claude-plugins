package conformance

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// SessionID is the fixed CLAUDE_CODE_SESSION_ID every step runs under.
const SessionID = "00000000-0000-4000-8000-0000000c0f0e"

const fixedGitDate = "2026-01-01T00:00:00Z"

// sandbox is one scenario's private world:
//
//	<ROOT>/home   HOME (XDG dirs, CLAUDE_CONFIG_DIR and orchestra state inside)
//	<ROOT>/work   the cwd / repository
//	<ROOT>/bin    PATH: git, the requested fakes, nothing else besides /usr/bin:/bin
//	<ROOT>/tmp    TMPDIR
type sandbox struct {
	root, home, work, bin string
	env                   []string
}

func newSandbox(t *testing.T, sc *Scenario) *sandbox {
	t.Helper()
	root := t.TempDir()
	s := &sandbox{
		root: root,
		home: filepath.Join(root, "home"),
		work: filepath.Join(root, "work"),
		bin:  filepath.Join(root, "bin"),
	}
	for _, d := range []string{s.home, s.work, s.bin, filepath.Join(root, "tmp")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	must(t, os.Symlink(env.git, filepath.Join(s.bin, "git")))
	for _, name := range sc.Setup.Fakes {
		src, ok := env.fakes[name]
		if !ok {
			t.Fatalf("unknown fake executor %q", name)
		}
		must(t, os.Symlink(src, filepath.Join(s.bin, name)))
	}
	gitconfig := filepath.Join(root, "gitconfig")
	must(t, os.WriteFile(gitconfig, []byte(
		"[user]\n\tname = Conformance\n\temail = conformance@example.invalid\n"+
			"[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n"), 0o644))
	s.env = []string{
		"HOME=" + s.home,
		"PATH=" + s.bin + ":/usr/bin:/bin",
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(s.home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(s.home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(s.home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(s.home, ".local", "state"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(s.home, ".claude"),
		"GIT_CONFIG_GLOBAL=" + gitconfig,
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_DATE=" + fixedGitDate,
		"GIT_COMMITTER_DATE=" + fixedGitDate,
		"TZ=UTC",
		"LANG=C.UTF-8",
		"CLAUDE_CODE_SESSION_ID=" + SessionID,
		"ORCHESTRA_ALIVE_DIR=" + filepath.Join(s.home, ".claude", "orchestra", "alive"),
		"ORCHESTRA_UI_STATE=" + filepath.Join(s.home, ".claude", "orchestra", "ui.json"),
		"ORCHESTRA_WAVE_REGISTRY=" + filepath.Join(s.home, ".claude", "orchestra", "waves.jsonl"),
		"ORCHESTRA_WAVE_NO_UI=1",
		"PYTHONDONTWRITEBYTECODE=1",
	}
	s.setup(t, sc)
	return s
}

func (s *sandbox) expand(v string) string {
	return strings.NewReplacer("<ROOT>", s.root, "<HOME>", s.home, "<WORK>", s.work).Replace(v)
}

// resolve maps a scenario path ("~/x" under HOME, else under WORK).
func (s *sandbox) resolve(p string) string {
	p = s.expand(p)
	switch {
	case p == "~":
		return s.home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(s.home, p[2:])
	case filepath.IsAbs(p):
		return p
	}
	return filepath.Join(s.work, p)
}

func (s *sandbox) writeFiles(t *testing.T, files map[string]string) {
	for _, name := range sortedKeys(files) {
		path := s.resolve(name)
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, []byte(s.expand(files[name])), 0o644))
	}
}

func (s *sandbox) setup(t *testing.T, sc *Scenario) {
	t.Helper()
	files := map[string]string{}
	for k, v := range sc.Setup.Files {
		files[k] = v
	}
	_, hasYAML := files["~/.claude/orchestra.yaml"]
	_, hasYML := files["~/.claude/orchestra.yml"]
	if !sc.Setup.KeepSandboxDefault && !hasYAML && !hasYML {
		files["~/.claude/orchestra.yaml"] = "sandbox:\n  mode: off\n"
	}
	s.writeFiles(t, files)
	if sc.Setup.Git {
		s.git(t, "init", "-q")
		s.git(t, "add", "-A")
		s.git(t, "commit", "-q", "--allow-empty", "-m", "initial")
	}
	s.writeFiles(t, sc.Setup.Dirty)
	for name, mode := range sc.Setup.Modes {
		m, err := strconv.ParseUint(mode, 8, 32)
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		must(t, os.Chmod(s.resolve(name), os.FileMode(m)))
	}
	for k, v := range sc.Setup.Env {
		s.env = append(s.env, k+"="+s.expand(v))
	}
}

func (s *sandbox) git(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command(env.git, args...)
	cmd.Dir = s.work
	cmd.Env = s.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// rawStep is what one step produced, before normalization.
type rawStep struct {
	exit           int
	stdout, stderr string
	files          map[string]*string // scenario path -> content (nil: absent)
}

// expandVars substitutes the <@NAME> values saved by earlier steps.
func expandVars(v string, vars map[string]string) string {
	for name, val := range vars {
		v = strings.ReplaceAll(v, "<@"+name+">", val)
	}
	return v
}

// implWrapper is an executable that runs impl with its arguments (the
// AGENT_EXEC_UNDER_TEST of a step), written once per implementation.
func (s *sandbox) implWrapper(t *testing.T, impl []string) string {
	t.Helper()
	quoted := make([]string, len(impl))
	for i, a := range impl {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	body := "#!/bin/sh\nexec " + strings.Join(quoted, " ") + " \"$@\"\n"
	sum := 0
	for _, c := range body {
		sum = sum*31 + int(c)
	}
	path := filepath.Join(s.root, fmt.Sprintf("impl-%x", uint32(sum)))
	if _, err := os.Stat(path); err != nil {
		must(t, os.WriteFile(path, []byte(body), 0o755))
	}
	return path
}

func (s *sandbox) run(t *testing.T, impl, args []string, st Step, vars map[string]string) rawStep {
	t.Helper()
	argv := append([]string(nil), impl...)
	for _, a := range args {
		argv = append(argv, expandVars(a, vars))
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.work
	if st.Cwd != "" {
		cmd.Dir = s.resolve(st.Cwd)
	}
	cmd.Env = append([]string(nil), s.env...)
	cmd.Env = append(cmd.Env, "AGENT_EXEC_UNDER_TEST="+s.implWrapper(t, impl))
	for _, k := range sortedKeys(st.Env) {
		cmd.Env = append(cmd.Env, k+"="+expandVars(s.expand(st.Env[k]), vars))
	}
	cmd.Stdin = strings.NewReader(expandVars(st.Stdin, vars))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", argv, err)
		}
		code = ee.ExitCode()
	}
	return rawStep{exit: code, stdout: stdout.String(), stderr: stderr.String(), files: map[string]*string{}}
}

// snapshot records the step's snapshot files (after its `save`, so a path
// may name a value the step itself printed).
func (s *sandbox) snapshot(st Step, vars map[string]string, out *rawStep) {
	for _, p := range st.Snapshot {
		if b, err := os.ReadFile(s.resolve(expandVars(p, vars))); err == nil {
			content := string(b)
			out.files[p] = &content
		} else {
			out.files[p] = nil
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func describe(argv []string) string {
	return fmt.Sprintf("agent-exec %s", strings.Join(argv, " "))
}
