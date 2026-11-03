package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/transcript"
)

// hookHandoffMaxAge is how long a session's handoff directory is kept. A
// handoff is only briefing material for a successor started soon after the
// agent returned, so a week covers any run that is still being resumed.
const hookHandoffMaxAge = 7 * 24 * time.Hour

// hookHandoffFile is the absolute path of an agent's handoff file. Both the
// handoff writer and `hook context` name it, so the path must not depend on
// the working directory of the caller.
func hookHandoffFile(session, agent string) string {
	p := transcript.HandoffPath(session, agent)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// hookHandoffResult is the one JSON line `hook handoff` prints. Path is null
// when the write failed, and Reason then says why.
type hookHandoffResult struct {
	Path   *string `json:"path"`
	Reason string  `json:"reason,omitempty"`
}

// hookHandoffCmd writes the handoff file of a subagent that has just
// returned. The hooks module calls it at turn.complete, so it exits 0 even
// when the write fails: the printed reason is the only report.
func hookHandoffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "handoff --session ID --agent ID [--type T] [--answer TEXT]",
		Short: "write the handoff file of a returning subagent (used by the orchestra hooks module)",
		Args:  cobra.NoArgs,
	}
	session := cmd.Flags().String("session", "", "the Claude Code session id")
	agent := cmd.Flags().String("agent", "", "the subagent id")
	typ := cmd.Flags().String("type", "", "the subagent type, named in the file header; \"agent\" when empty")
	answer := cmd.Flags().String("answer", "", "the subagent's final answer; replaces the one read from its transcript")
	_ = cmd.MarkFlagRequired("session")
	_ = cmd.MarkFlagRequired("agent")
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		// Ids are checked before any file is touched, so a bad id never
		// creates the handoffs directory or expires anything.
		if *session == "" || !hookCtxIDPattern.MatchString(*session) {
			return exitf(2, "hook handoff: invalid --session: %q", *session)
		}
		if *agent == "" || !hookCtxIDPattern.MatchString(*agent) {
			return exitf(2, "hook handoff: invalid --agent: %q", *agent)
		}
		h := transcript.Handoff{}
		if path, ok := transcript.Find(transcript.Roots("", ""), *session, *agent); ok {
			if got, ok := transcript.ReadHandoff(path); ok {
				h = got
			}
		}
		if *answer != "" {
			h.Answer = *answer
		}
		agentType := *typ
		if agentType == "" {
			agentType = "agent"
		}
		now := time.Now()
		file := hookHandoffFile(*session, *agent)
		result := hookHandoffResult{}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			result.Reason = err.Error()
		} else if err := fsutil.AtomicWriteFile(file, []byte(transcript.RenderHandoff(h, agentType, *agent, now)), 0o644); err != nil {
			result.Reason = err.Error()
		} else {
			result.Path = &file
		}
		hookHandoffExpire(now)
		return json.NewEncoder(c.OutOrStdout()).Encode(result)
	}
	return cmd
}

// hookHandoffExpire removes the session directories under the handoffs
// directory that have not been written for hookHandoffMaxAge. It is best
// effort: a failure to expire never fails the write that triggered it.
// Entries that are not directories, including symlinks, are left alone.
func hookHandoffExpire(now time.Time) {
	root := filepath.Join(paths.StateDir(), "handoffs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= hookHandoffMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}
