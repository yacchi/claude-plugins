package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/hookrun"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/transcript"
)

// hookStdinLimit caps what session-start reads from Claude Code. The payload
// is a few hundred bytes; the cap only stops a runaway writer from holding
// the hook open.
const hookStdinLimit = 1 << 20

// hookCtxIDPattern bounds the ids `hook context` accepts to one path element,
// so a caller can never point the transcript lookup outside the project roots.
var hookCtxIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// newHookCmd is the entry point for Claude Code's command hooks (hooks.json)
// and for the orchestra hooks module. It is not orchestration activity, so it
// stays out of the heartbeat in root's PersistentPreRun.
func newHookCmd() *cobra.Command {
	cmd := groupCommand("hook", "entry points for Claude Code's command hooks and the orchestra hooks module")
	cmd.AddCommand(newHookSessionStartCmd(), newHookSeenCmd(), newHookContextCmd(), hookEnvTreeCmd(), hookHandoffCmd())
	return cmd
}

// hookEnvTreeCmd prints the pnpm global-virtual-store exports for the working
// directory's tree. The session env block runs it before every Bash command
// of an orchestra tree, so it prints nothing outside a repo and exits 0.
func hookEnvTreeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "tree-env",
		Short:  "print the pnpm global-virtual-store exports for the working directory's tree (the session env block runs it)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return nil
			}
			tree, ok := gitx.RepoRoot(cwd)
			if !ok {
				return nil
			}
			_, _ = cmd.OutOrStdout().Write([]byte(config.PnpmGVSShellPrefix(hookEnvGVSOn(), tree, os.Environ())))
			return nil
		},
	}
}

// hookEnvGVSOn reports whether the pnpm global virtual store is on in the
// resolved config. "auto" counts as on without probing pnpm: under auto the
// tree's own layout decides, inside PnpmGVSShellPrefix (a classic layout
// gets no prefix). This runs before every Bash call of an orchestra tree, so
// it must never probe pnpm; it does not call config.PnpmGVS* here.
func hookEnvGVSOn() bool {
	cfg, err := config.Resolve()
	if err != nil {
		return false
	}
	v, _ := cfg.Lookup("isolation", "pnpm_global_virtual_store")
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "auto"
	}
	return false
}

// hookEnvWrite appends orchestra's session env block to $CLAUDE_ENV_FILE. It
// is best effort: a hook must never print or fail over it.
func hookEnvWrite() {
	exe := hookEnvExe()
	if exe == "" {
		return
	}
	_ = hookrun.WriteSessionEnv(os.Getenv("CLAUDE_ENV_FILE"), exe, config.ProcessTestSlots())
}

// hookEnvExe is the agent-exec binary the session env block names. The
// plugin's stable launcher wins when it exists: the per-hash cache binary is
// deleted by later builds, so it is never named. The symlink is not resolved
// for the launcher, which is the stable path itself.
func hookEnvExe() string {
	if root := os.Getenv("CLAUDE_PLUGIN_ROOT"); root != "" {
		p := filepath.Join(root, "bin", "agent-exec")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}
	return resolved
}

// newHookContextCmd reports how much context a session, or one of its
// subagents, carried in its last response. The hooks module's resume guard
// reads it before SendMessage resumes a subagent. It also names the agent's
// handoff file when one exists. A missing transcript is not an error: the
// fields are null and the guard decides what that means.
func newHookContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context --session ID [--agent ID]",
		Short: "print the context size of a session's (or one subagent's) last response, for the hooks module's resume guard",
		Args:  cobra.NoArgs,
	}
	session := cmd.Flags().String("session", "", "the Claude Code session id")
	agent := cmd.Flags().String("agent", "", "the subagent id; empty for the session's own transcript")
	_ = cmd.MarkFlagRequired("session")
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		if *session == "" || !hookCtxIDPattern.MatchString(*session) {
			return exitf(2, "hook context: invalid --session: %q", *session)
		}
		if *agent != "" && !hookCtxIDPattern.MatchString(*agent) {
			return exitf(2, "hook context: invalid --agent: %q", *agent)
		}
		out := hookCtxResult{}
		path, found := transcript.Find(transcript.Roots("", ""), *session, *agent)
		if found {
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
			out.Transcript = &path
			if tokens, model, ok := transcript.LastUsage(path); ok {
				out.ContextTokens = &tokens
				if model != "" {
					out.Model = &model
				}
				if cfg, err := config.Resolve(); err == nil {
					if limit := config.ResumeMaxTokensFor(cfg, model); limit > 0 {
						out.Limit = &limit
					}
				}
			}
		}
		if *agent != "" {
			f := hookHandoffFile(*session, *agent)
			if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
				out.Handoff = &f
			}
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(out)
	}
	return cmd
}

// hookCtxResult is the one JSON line `hook context` prints. Pointers make an
// absent value print as null, which the TypeScript guard relies on.
type hookCtxResult struct {
	ContextTokens *int64  `json:"context_tokens"`
	Transcript    *string `json:"transcript"`
	Model         *string `json:"model"`
	Limit         *int64  `json:"limit"`
	// Handoff names the handoff file the resume guard quotes.
	Handoff *string `json:"handoff"`
}

// newHookSessionStartCmd always exits 0: a hook's non-zero exit is shown to
// the user at every session start, and the context injection is best effort.
// Every run also appends orchestra's session env block to $CLAUDE_ENV_FILE,
// best effort, before the stdin is read, so the block lands whatever the input.
func newHookSessionStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "session-start",
		Short: "SessionStart hook: print the orchestra protocol context for the session, if any",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hookEnvWrite()
			raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), hookStdinLimit))
			if err != nil {
				return nil
			}
			out := hookrun.SessionStart(raw, time.Now())
			if out == nil {
				return nil
			}
			_, _ = cmd.OutOrStdout().Write(append(out, '\n'))
			return nil
		},
	}
}

// newHookSeenCmd records that a hook event ran in a session, so doctor can
// tell the TypeScript module is loaded. A missing flag is a usage error (exit
// 2, from cobra); a failed write is exit 1 with one line on stderr.
func newHookSeenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seen --session ID --event NAME",
		Short: "record that event NAME ran in session ID (used by the orchestra hooks module)",
		Args:  cobra.NoArgs,
	}
	session := cmd.Flags().String("session", "", "the Claude Code session id")
	event := cmd.Flags().String("event", "", "the hook event that ran")
	_ = cmd.MarkFlagRequired("session")
	_ = cmd.MarkFlagRequired("event")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		if err := hookrun.Record(*session, *event, time.Now()); err != nil {
			return exitf(1, "hook seen: %v", err)
		}
		return nil
	}
	return cmd
}
