package hookrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// sessionEnvFakeExe is an agent-exec stand-in whose `hook tree-env` prints a
// probe export, so a test can see the eval ran with the right arguments. Its
// path has a space and a quote, the cases the quoting must survive.
func sessionEnvFakeExe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "it's dir", "agent-exec")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho \"export SESSION_ENV_PROBE=$1-$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// sessionEnvRun runs script the way Claude Code inlines CLAUDE_ENV_FILE: in
// front of a command, chained with &&. The probe line prints the variables
// the block may set. Returns stdout and the run error.
func sessionEnvRun(t *testing.T, shell, dir, script string, env ...string) (string, error) {
	t.Helper()
	bin, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s not on PATH: %v", shell, err)
	}
	args := []string{"-c", strings.TrimSuffix(script, "\n") + ` && printf '%s|%s|%s' "${SESSION_ENV_PROBE:-}" "${ORCHESTRA_NODE_CPUS:-}" "${NODE_OPTIONS:-}"`}
	if shell == "zsh" {
		args = append([]string{"-f"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(sessionEnvBaseEnv(), env...)
	out, err := cmd.Output()
	return string(out), err
}

// sessionEnvBaseEnv is the process env without the variables the block sets
// or the tests control.
func sessionEnvBaseEnv() []string {
	drop := map[string]bool{
		"NODE_OPTIONS":                            true,
		"ORCHESTRA_NODE_CPUS":                     true,
		"SESSION_ENV_PROBE":                       true,
		"pnpm_config_enable_global_virtual_store": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			env = append(env, kv)
		}
	}
	return env
}

func TestSessionEnvScriptPlain(t *testing.T) {
	got := SessionEnvScript("/opt/bin/agent-exec", 0)
	want := "# orchestra session env\n" +
		"if [ -f .git ] && [ -z \"${pnpm_config_enable_global_virtual_store:-}\" ]; then eval \"$('/opt/bin/agent-exec' hook tree-env 2>/dev/null)\"; fi\n" +
		"true\n"
	if got != want {
		t.Fatalf("SessionEnvScript =\n%s\nwant\n%s", got, want)
	}
}

func TestSessionEnvScriptQuotesExe(t *testing.T) {
	got := SessionEnvScript("/tmp/it's dir/agent-exec", 0)
	if !strings.Contains(got, `eval "$('/tmp/it'\''s dir/agent-exec' hook tree-env 2>/dev/null)"`) {
		t.Fatalf("exe not quoted as one word:\n%s", got)
	}
}

func TestSessionEnvScriptNodeLine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const exe = "/opt/bin/agent-exec"
	got := SessionEnvScript(exe, 2)
	tree := SessionEnvScript(exe, 0)
	treeLine := strings.Split(tree, "\n")[1]
	want := SessionEnvMarker + "\n" + config.NodeCapShellPrefix(2) + "\n" + treeLine + "\ntrue\n"
	if got != want {
		t.Fatalf("SessionEnvScript(exe, 2) =\n%s\nwant\n%s", got, want)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if last := lines[len(lines)-1]; last != "true" {
		t.Fatalf("last line = %q, want true", last)
	}
}

func TestSessionEnvWriteMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	const exe = "/opt/bin/agent-exec"
	if err := WriteSessionEnv(path, exe, 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != SessionEnvScript(exe, 0) {
		t.Fatalf("content =\n%s", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestSessionEnvWriteKeepsOtherHooks(t *testing.T) {
	const exe = "/opt/bin/agent-exec"
	script := SessionEnvScript(exe, 0)
	for name, have := range map[string]string{
		"terminated":   "export OTHER=1\n",
		"unterminated": "export OTHER=1",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "env")
			if err := os.WriteFile(path, []byte(have), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := WriteSessionEnv(path, exe, 0); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "export OTHER=1\n" + script
			if string(got) != want {
				t.Fatalf("content =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestSessionEnvWriteOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	const exe = "/opt/bin/agent-exec"
	if err := WriteSessionEnv(path, exe, 0); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionEnv(path, exe, 0); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(second), SessionEnvMarker) != 1 || string(second) != string(first) {
		t.Fatalf("second call changed the file:\n%s", second)
	}
}

func TestSessionEnvWriteLeavesExistingBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	have := SessionEnvScript("/old/agent-exec", 0)
	if err := os.WriteFile(path, []byte(have), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionEnv(path, "/new/agent-exec", 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != have {
		t.Fatalf("existing block changed:\n%s", got)
	}
}

func TestSessionEnvWriteSkipsEmptyAndRelative(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := WriteSessionEnv("", "/opt/bin/agent-exec", 0); err != nil {
		t.Fatalf("empty path: %v", err)
	}
	if err := WriteSessionEnv("rel/env", "/opt/bin/agent-exec", 0); err != nil {
		t.Fatalf("relative path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "rel", "env")); !os.IsNotExist(err) {
		t.Fatalf("relative path created a file: %v", err)
	}
}

func TestSessionEnvShellRuns(t *testing.T) {
	probe := regexp.MustCompile(`^hook-tree-env\|`)
	for _, shell := range []string{"sh", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			exe := sessionEnvFakeExe(t)
			gitFile := func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /nowhere\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			}
			t.Run("git file runs tree-env", func(t *testing.T) {
				out, err := sessionEnvRun(t, shell, gitFile(t), SessionEnvScript(exe, 0))
				if err != nil {
					t.Fatalf("status: %v", err)
				}
				if out != "hook-tree-env||" {
					t.Fatalf("stdout = %q, want hook-tree-env||", out)
				}
			})
			t.Run("git directory does nothing", func(t *testing.T) {
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				out, err := sessionEnvRun(t, shell, dir, SessionEnvScript(exe, 0))
				if err != nil {
					t.Fatalf("status: %v", err)
				}
				if out != "||" {
					t.Fatalf("stdout = %q, want ||", out)
				}
			})
			t.Run("no git does nothing", func(t *testing.T) {
				out, err := sessionEnvRun(t, shell, t.TempDir(), SessionEnvScript(exe, 0))
				if err != nil {
					t.Fatalf("status: %v", err)
				}
				if out != "||" {
					t.Fatalf("stdout = %q, want ||", out)
				}
			})
			t.Run("pnpm virtual store already set does nothing", func(t *testing.T) {
				out, err := sessionEnvRun(t, shell, gitFile(t), SessionEnvScript(exe, 0),
					"pnpm_config_enable_global_virtual_store=true")
				if err != nil {
					t.Fatalf("status: %v", err)
				}
				if out != "||" {
					t.Fatalf("stdout = %q, want ||", out)
				}
			})
			t.Run("missing exe still exits 0", func(t *testing.T) {
				missing := filepath.Join(t.TempDir(), "gone", "agent-exec")
				out, err := sessionEnvRun(t, shell, gitFile(t), SessionEnvScript(missing, 0))
				if err != nil {
					t.Fatalf("status: %v", err)
				}
				if out != "||" {
					t.Fatalf("stdout = %q, want ||", out)
				}
			})
			t.Run("test slots cap node", func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				out, err := sessionEnvRun(t, shell, gitFile(t), SessionEnvScript(exe, 2),
					"NODE_OPTIONS=--max-old-space-size=64")
				if err != nil {
					t.Fatalf("status: %v", err)
				}
				if !probe.MatchString(out) {
					t.Fatalf("stdout = %q, want probe hook-tree-env", out)
				}
				fields := strings.Split(out, "|")
				if len(fields) != 3 || fields[1] == "" || strings.Trim(fields[1], "0123456789") != "" {
					t.Fatalf("CPUs field = %q, want digits", out)
				}
				want := config.NodeCapRequire(config.NodeCapPreloadPath())
				if !strings.Contains(fields[2], want) || !strings.Contains(fields[2], "--max-old-space-size=64") {
					t.Fatalf("NODE_OPTIONS = %q, want %q and the old option", fields[2], want)
				}
			})
		})
	}
}
