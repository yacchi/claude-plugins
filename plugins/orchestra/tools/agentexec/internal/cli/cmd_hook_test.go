package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/hookrun"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/transcript"
)

// hookCmdRun executes the agent-exec command line with stdin and returns
// stdout and the exit code the process would use.
func hookCmdRun(t *testing.T, stdin string, args ...string) (string, int) {
	t.Helper()
	root := newRoot()
	var stdout, stderr bytes.Buffer
	root.SetArgs(args)
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	cmd, err := root.ExecuteC()
	return stdout.String(), report(cmd, err, &stderr)
}

// hookCmdIsolate points the state directory (where hookrun records stamps)
// at a temporary HOME.
func hookCmdIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "")
}

// hookEnvIsolate is hookCmdIsolate plus an empty CLAUDE_ENV_FILE, so a test
// never appends to the real session's env file, and an empty plugin root and
// config root of its own.
func hookEnvIsolate(t *testing.T) {
	t.Helper()
	hookCmdIsolate(t)
	t.Setenv("CLAUDE_ENV_FILE", "")
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")
	t.Setenv("ORCHESTRA_CONFIG_ROOT", t.TempDir())
}

// hookEnvOrchestraTree makes a linked worktree of a temporary repo, prepared
// the way orchestra prepares a tree (baseline marker, pnpm lockfile), and
// returns its path.
func hookEnvOrchestraTree(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	hookEnvIsolate(t)
	repo := t.TempDir()
	wt := filepath.Join(t.TempDir(), "wt")
	hookEnvGit(t, repo, "init", "-q")
	hookEnvGit(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	hookEnvGit(t, repo, "worktree", "add", "-q", wt)
	raw, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "gitdir:"))
	if err := os.WriteFile(filepath.Join(gitdir, config.BaselineMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wt
}

// hookEnvGit runs git in dir and fails the test on error.
func hookEnvGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// hookEnvWriteFile writes content to path, creating its directory first.
func hookEnvWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHookCmdSessionStartInjectsForOpus(t *testing.T) {
	hookEnvIsolate(t)
	in := `{"session_id":"s1","source":"startup","model":"claude-opus-5","cwd":"` + t.TempDir() + `"}`
	out, code := hookCmdRun(t, in, "hook", "session-start")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var line struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if !strings.HasPrefix(line.HookSpecificOutput.AdditionalContext, "<orchestra-router>") {
		t.Errorf("context does not start with <orchestra-router>: %q", line.HookSpecificOutput.AdditionalContext)
	}
}

func TestHookCmdSessionStartSilentForSonnet(t *testing.T) {
	hookEnvIsolate(t)
	in := `{"session_id":"s1","source":"startup","model":"claude-sonnet-5","cwd":"` + t.TempDir() + `"}`
	if out, code := hookCmdRun(t, in, "hook", "session-start"); code != 0 || out != "" {
		t.Errorf("exit %d, output %q; want exit 0 and no output", code, out)
	}
}

func TestHookCmdSessionStartExitsZeroOnBadInput(t *testing.T) {
	hookEnvIsolate(t)
	if out, code := hookCmdRun(t, "not json", "hook", "session-start"); code != 0 || out != "" {
		t.Errorf("exit %d, output %q; want exit 0 and no output", code, out)
	}
}

func TestHookCmdSeenRecordsEvent(t *testing.T) {
	hookCmdIsolate(t)
	if _, code := hookCmdRun(t, "", "hook", "seen", "--session", "s1", "--event", "module"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, ok := hookrun.Seen("s1")[hookrun.EventModule]; !ok {
		t.Errorf("Seen(s1) = %v; want %q recorded", hookrun.Seen("s1"), hookrun.EventModule)
	}
}

func TestHookCmdSeenMissingEventIsUsageError(t *testing.T) {
	hookCmdIsolate(t)
	if _, code := hookCmdRun(t, "", "hook", "seen", "--session", "s1"); code != 2 {
		t.Errorf("exit %d; want 2", code)
	}
}

// hookCtxTranscript writes a subagent transcript for session "s1" under a
// temporary CLAUDE_CONFIG_DIR and points the command at it.
func hookCtxTranscript(t *testing.T, lines string) string {
	t.Helper()
	hookCmdIsolate(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	sub := filepath.Join(dir, "projects", "p", "s1", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "agent-a1.jsonl")
	if err := os.WriteFile(file, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestHookCmdContextReportsLastSubagentResponse(t *testing.T) {
	file := hookCtxTranscript(t, `{"message":{"usage":{"input_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}}
{"message":{"usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}}
`)
	out, code := hookCmdRun(t, "", "hook", "context", "--session", "s1", "--agent", "a1")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		ContextTokens *int64  `json:"context_tokens"`
		Transcript    *string `json:"transcript"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if got.ContextTokens == nil || *got.ContextTokens != 60 {
		t.Errorf("context_tokens = %v; want 60 (last line only)", got.ContextTokens)
	}
	if got.Transcript == nil || *got.Transcript != file {
		t.Errorf("transcript = %v; want %q", got.Transcript, file)
	}
}

func TestHookCmdContextUnknownAgentIsNull(t *testing.T) {
	hookCtxTranscript(t, `{"message":{"usage":{"input_tokens":1}}}`)
	out, code := hookCmdRun(t, "", "hook", "context", "--session", "s1", "--agent", "nope")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if want := `{"context_tokens":null,"transcript":null,"model":null,"limit":null,"handoff":null}` + "\n"; out != want {
		t.Errorf("output %q; want %q", out, want)
	}
}

func TestHookCmdContextInvalidIDIsUsageError(t *testing.T) {
	hookCmdIsolate(t)
	if _, code := hookCmdRun(t, "", "hook", "context"); code != 2 {
		t.Errorf("missing --session: exit %d; want 2", code)
	}
	if _, code := hookCmdRun(t, "", "hook", "context", "--session", "a/b"); code != 2 {
		t.Errorf("--session with / : exit %d; want 2", code)
	}
}

func TestHookCmdContextReportsModelAndLimit(t *testing.T) {
	hookCtxTranscript(t, `{"message":{"model":"claude-haiku-5-5","usage":{"input_tokens":5}}}
`)
	out, code := hookCmdRun(t, "", "hook", "context", "--session", "s1", "--agent", "a1")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		Model *string `json:"model"`
		Limit *int64  `json:"limit"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if got.Model == nil || *got.Model != "claude-haiku-5-5" || got.Limit == nil || *got.Limit != 90000 {
		t.Errorf("model/limit = %v/%v; want claude-haiku-5-5/90000 (output %q)", got.Model, got.Limit, out)
	}
}

// hookEnvOpusInput is a SessionStart payload for an Opus session in cwd.
func hookEnvOpusInput(cwd string) string {
	return `{"session_id":"s1","source":"startup","model":"claude-opus-5","cwd":"` + cwd + `"}`
}

func TestHookEnvSessionStartWritesBlock(t *testing.T) {
	hookEnvIsolate(t)
	envFile := filepath.Join(t.TempDir(), "env.sh")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	out, code := hookCmdRun(t, hookEnvOpusInput(t.TempDir()), "hook", "session-start")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(out, `{"hookSpecificOutput"`) {
		t.Errorf("stdout %q; want the router JSON", out)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := hookrun.SessionEnvScript(exe, 0); string(got) != want {
		t.Errorf("env file = %q; want %q", got, want)
	}
}

func TestHookEnvSessionStartPrefersPluginRootExe(t *testing.T) {
	hookEnvIsolate(t)
	root := t.TempDir()
	exe := filepath.Join(root, "bin", "agent-exec")
	hookEnvWriteFile(t, exe, "#!/bin/sh\n")
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env.sh")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	t.Setenv("CLAUDE_PLUGIN_ROOT", root)
	if _, code := hookCmdRun(t, hookEnvOpusInput(t.TempDir()), "hook", "session-start"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := hookrun.SessionEnvScript(exe, 0); string(got) != want {
		t.Errorf("env file = %q; want %q", got, want)
	}
}

func TestHookEnvSessionStartPluginRootWithoutExe(t *testing.T) {
	hookEnvIsolate(t)
	envFile := filepath.Join(t.TempDir(), "env.sh")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	t.Setenv("CLAUDE_PLUGIN_ROOT", t.TempDir())
	if _, code := hookCmdRun(t, hookEnvOpusInput(t.TempDir()), "hook", "session-start"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := hookrun.SessionEnvScript(exe, 0); string(got) != want {
		t.Errorf("env file = %q; want %q", got, want)
	}
}

func TestHookEnvSessionStartBadInputStillWrites(t *testing.T) {
	hookEnvIsolate(t)
	envFile := filepath.Join(t.TempDir(), "env.sh")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	out, code := hookCmdRun(t, "not json", "hook", "session-start")
	if code != 0 || out != "" {
		t.Errorf("exit %d, output %q; want exit 0 and no output", code, out)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("env file not written: %v", err)
	}
	if !strings.Contains(string(got), hookrun.SessionEnvMarker) {
		t.Errorf("env file %q lacks the orchestra block", got)
	}
}

func TestHookEnvSessionStartUnwritableEnvFile(t *testing.T) {
	hookEnvIsolate(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "f")
	hookEnvWriteFile(t, blocker, "")
	t.Setenv("CLAUDE_ENV_FILE", filepath.Join(blocker, "env.sh"))
	out, code := hookCmdRun(t, hookEnvOpusInput(t.TempDir()), "hook", "session-start")
	if code != 0 {
		t.Fatalf("exit %d; want 0", code)
	}
	if !strings.HasPrefix(out, `{"hookSpecificOutput"`) {
		t.Errorf("stdout %q; want the router JSON", out)
	}
}

func TestHookEnvTreeAutoPrintsPrefix(t *testing.T) {
	tree := hookEnvOrchestraTree(t)
	t.Chdir(tree)
	out, code := hookCmdRun(t, "", "hook", "tree-env")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	root, ok := gitx.RepoRoot(tree)
	if !ok {
		t.Fatal("gitx.RepoRoot: not a repo")
	}
	if want := config.PnpmGVSShellPrefix(true, root, os.Environ()); out == "" || out != want {
		t.Errorf("output %q; want %q", out, want)
	}
}

func TestHookEnvTreeClassicLayoutSilent(t *testing.T) {
	tree := hookEnvOrchestraTree(t)
	hookEnvWriteFile(t, filepath.Join(tree, "node_modules", ".modules.yaml"), "virtualStoreDir: .pnpm\n")
	t.Chdir(tree)
	if out, code := hookCmdRun(t, "", "hook", "tree-env"); code != 0 || out != "" {
		t.Errorf("exit %d, output %q; want exit 0 and no output", code, out)
	}
}

func TestHookEnvTreeOffSilent(t *testing.T) {
	tree := hookEnvOrchestraTree(t)
	hookEnvWriteFile(t, filepath.Join(os.Getenv("ORCHESTRA_CONFIG_ROOT"), ".claude", "orchestra.yaml"),
		"isolation:\n  pnpm_global_virtual_store: false\n")
	t.Chdir(tree)
	if out, code := hookCmdRun(t, "", "hook", "tree-env"); code != 0 || out != "" {
		t.Errorf("exit %d, output %q; want exit 0 and no output", code, out)
	}
}

func TestHookEnvTreeOutsideRepoSilent(t *testing.T) {
	hookEnvIsolate(t)
	t.Chdir(t.TempDir())
	if out, code := hookCmdRun(t, "", "hook", "tree-env"); code != 0 || out != "" {
		t.Errorf("exit %d, output %q; want exit 0 and no output", code, out)
	}
}

// hookHandoffRun runs `hook handoff` and decodes its JSON line.
func hookHandoffRun(t *testing.T, args ...string) (struct {
	Path   *string `json:"path"`
	Reason string  `json:"reason"`
}, int) {
	t.Helper()
	out, code := hookCmdRun(t, "", append([]string{"hook", "handoff"}, args...)...)
	var got struct {
		Path   *string `json:"path"`
		Reason string  `json:"reason"`
	}
	if code == 0 {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("output %q: %v", out, err)
		}
	}
	return got, code
}

func TestHookHandoffWritesFromTranscript(t *testing.T) {
	hookCtxTranscript(t, `{"type":"user","message":{"role":"user","content":"build it"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"built"}]}}
`)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out, code := hookCmdRun(t, "", "hook", "handoff", "--session", "s1", "--agent", "a1", "--type", "orchestra:impl")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	file := transcript.HandoffPath("s1", "a1")
	if want := `{"path":"` + file + `"}` + "\n"; out != want {
		t.Errorf("stdout %q; want %q", out, want)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Handoff: orchestra:impl a1\n", "## Goal\nbuild it\n", "## Answer\nbuilt\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("handoff file lacks %q:\n%s", want, got)
		}
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v; want 0644", fi.Mode().Perm())
	}
}

func TestHookHandoffAnswerFlagWins(t *testing.T) {
	hookCtxTranscript(t, `{"type":"user","message":{"role":"user","content":"build it"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"built"}]}}
`)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, code := hookHandoffRun(t, "--session", "s1", "--agent", "a1", "--answer", "final words"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got, err := os.ReadFile(transcript.HandoffPath("s1", "a1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "## Answer\nfinal words\n") {
		t.Errorf("handoff file lacks the --answer text:\n%s", got)
	}
}

func TestHookHandoffWithoutTranscriptStillWrites(t *testing.T) {
	hookCmdIsolate(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	got, code := hookHandoffRun(t, "--session", "s1", "--agent", "zz", "--answer", "done")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got.Path == nil {
		t.Fatalf("path null, reason %q", got.Reason)
	}
	body, err := os.ReadFile(*got.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Handoff: agent zz\n", "## Answer\ndone\n"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("handoff file lacks %q:\n%s", want, body)
		}
	}
}

func TestHookHandoffWriteFailureReports(t *testing.T) {
	hookCmdIsolate(t)
	state := filepath.Join(t.TempDir(), "claude-orchestra")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file where the handoffs directory belongs makes the write fail.
	hookEnvWriteFile(t, filepath.Join(state, "handoffs"), "")
	t.Setenv("XDG_STATE_HOME", filepath.Dir(state))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	got, code := hookHandoffRun(t, "--session", "s1", "--agent", "a1")
	if code != 0 {
		t.Fatalf("exit %d; want 0", code)
	}
	if got.Path != nil || got.Reason == "" {
		t.Errorf("path %v, reason %q; want null path and a reason", got.Path, got.Reason)
	}
}

func TestHookHandoffInvalidIDIsUsageError(t *testing.T) {
	hookCmdIsolate(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{
		{"--session", "a/b", "--agent", "a1"},
		{"--session", "s1", "--agent", "../x"},
		{"--session", "s1", "--agent", ""},
		{"--session", "s1"},
	} {
		if _, code := hookCmdRun(t, "", append([]string{"hook", "handoff"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d; want 2", args, code)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "claude-orchestra", "handoffs")); !os.IsNotExist(err) {
		t.Errorf("handoffs directory exists after invalid ids (stat err %v)", err)
	}
}

func TestHookHandoffExpiresOldDirectories(t *testing.T) {
	hookCmdIsolate(t)
	state := filepath.Join(t.TempDir(), "claude-orchestra")
	t.Setenv("XDG_STATE_HOME", filepath.Dir(state))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := filepath.Join(state, "handoffs")
	hookEnvWriteFile(t, filepath.Join(root, "old", "x.md"), "")
	hookEnvWriteFile(t, filepath.Join(root, "recent", "x.md"), "")
	hookEnvWriteFile(t, filepath.Join(root, "stray"), "")
	now := time.Now()
	aged := func(p string, age time.Duration) {
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	aged(filepath.Join(root, "old"), 8*24*time.Hour)
	aged(filepath.Join(root, "recent"), 6*24*time.Hour)
	aged(filepath.Join(root, "stray"), 8*24*time.Hour)
	if _, code := hookHandoffRun(t, "--session", "s1", "--agent", "a1"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(root, "old")); !os.IsNotExist(err) {
		t.Errorf("old directory kept (stat err %v); want removed", err)
	}
	for _, keep := range []string{"recent", "stray", "s1"} {
		if _, err := os.Stat(filepath.Join(root, keep)); err != nil {
			t.Errorf("%s removed: %v", keep, err)
		}
	}
}

func TestHookContextNamesHandoffFile(t *testing.T) {
	hookCtxTranscript(t, `{"message":{"usage":{"input_tokens":1}}}
`)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, code := hookHandoffRun(t, "--session", "s1", "--agent", "a1", "--answer", "x"); code != 0 {
		t.Fatalf("handoff exit %d", code)
	}
	out, code := hookCmdRun(t, "", "hook", "context", "--session", "s1", "--agent", "a1")
	if code != 0 {
		t.Fatalf("context exit %d", code)
	}
	var got struct {
		Handoff *string `json:"handoff"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if want := transcript.HandoffPath("s1", "a1"); got.Handoff == nil || *got.Handoff != want {
		t.Errorf("handoff = %v; want %q", got.Handoff, want)
	}
	if out, _ := hookCmdRun(t, "", "hook", "context", "--session", "s1"); !strings.Contains(out, `"handoff":null`) {
		t.Errorf("without --agent: output %q; want handoff null", out)
	}
}
