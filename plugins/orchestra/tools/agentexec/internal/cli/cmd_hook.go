package cli

import (
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/hookrun"
)

// hookStdinLimit caps what session-start reads from Claude Code. The payload
// is a few hundred bytes; the cap only stops a runaway writer from holding
// the hook open.
const hookStdinLimit = 1 << 20

// newHookCmd is the entry point for Claude Code's command hooks (hooks.json)
// and for the orchestra hooks module. It is not orchestration activity, so it
// stays out of the heartbeat in root's PersistentPreRun.
func newHookCmd() *cobra.Command {
	cmd := groupCommand("hook", "entry points for Claude Code's command hooks and the orchestra hooks module")
	cmd.AddCommand(newHookSessionStartCmd(), newHookSeenCmd())
	return cmd
}

// newHookSessionStartCmd always exits 0: a hook's non-zero exit is shown to
// the user at every session start, and the context injection is best effort.
func newHookSessionStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "session-start",
		Short: "SessionStart hook: print the orchestra protocol context for the session, if any",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
