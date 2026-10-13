package herdr

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// Prompt sends ONE prompt line to the session's agent, opening a new agent
// on the worktree when the old one is gone. s.Resumed says whether it went
// to an agent that was already running. A blocked agent (a dialog) or a
// prompt that stalls twice is a need.
func (e *Executor) Prompt(ctx context.Context, s *host.Session, promptFiles []string, resultPath string, timeout time.Duration) error {
	fresh := s.Pop("fresh")
	resumed := s.Terminal != "" && !fresh
	if resumed {
		// A live agent is reused only while herdr still knows it.
		status, err := e.status(ctx, s.Terminal, "")
		if err != nil || status == "unknown" {
			e.closeAgent(s)
			resumed = false
		}
	}
	if s.Terminal == "" {
		name, err := e.openAgent(ctx, s, false)
		if err != nil {
			return host.AsNeed(err)
		}
		e.emit(s.Pkg, "start", name, nil)
	}
	name := s.Terminal
	// A domain approved since the last wait poll must reach the server
	// before this prompt runs, or the retried command hits the old list.
	e.NetworkTick()
	if resumed {
		e.emit(s.Pkg, "reuse", name, nil)
	}
	s.Resumed = resumed
	host.ResetResult(resultPath)

	status, err := e.status(ctx, name, "")
	if err != nil {
		return host.Need(host.KindStalled, "agent "+name+" went away: "+err.Error())
	}
	if status == "blocked" {
		return e.blocked(ctx, s, name, resultPath)
	}
	line := host.PromptLine(promptFiles, resultPath)
	if s.Pop("fork_first") && s.Str("warm_worktree") != "" {
		warm := s.Str("warm_worktree")
		line = "Your working tree is " + s.Worktree + ". Earlier messages in this conversation were produced " +
			"in " + warm + "; any path under it maps to the same relative path under your working " +
			"tree. Never read or write under " + warm + ". " + line
	}
	return e.sendPrompt(ctx, s, name, line, resultPath)
}

// sendPrompt sends ONE prompt line. A stalled prompt is resent once after
// a short wait; only a second stall is a need.
func (e *Executor) sendPrompt(ctx context.Context, s *host.Session, name, line, resultPath string) error {
	for attempt := 0; attempt < 2; attempt++ {
		_, _, err := e.call(ctx, callTimeout, "", "agent", "prompt", name, line)
		if err == nil {
			return nil
		}
		code := ""
		if ce := callErr(err); ce != nil {
			code = ce.Code
		}
		switch code {
		case "agent_blocked":
			return e.blocked(ctx, s, name, resultPath)
		case "agent_prompt_stalled":
			if attempt == 0 {
				if err := e.sleep(ctx, promptRetryWait); err != nil {
					return host.AsNeed(err)
				}
				continue
			}
			return host.Need(host.KindStalled, "prompt stalled: "+err.Error()+"\nterminal: "+name+
				"\nworktree: "+s.Worktree+"\nresult: "+resultPath)
		}
		return host.Need(host.KindError, err.Error())
	}
	return nil
}

func (e *Executor) blocked(ctx context.Context, s *host.Session, name, resultPath string) *host.NeedError {
	screen, err := e.screen(ctx, name)
	if err != nil {
		screen = ""
	}
	return host.Need(host.KindStalled, "dialog:\n"+host.TailText(screen)+"\nterminal: "+name+
		"\nworktree: "+s.Worktree+"\nresult: "+resultPath)
}

// WaitResult waits for the result file without sending another prompt. A
// forked package must leave the warm worktree clean: a fork that wrote
// there is a need whatever the outcome. The blocked connections of the
// package ride along as "network_denials" (in the data or the need).
func (e *Executor) WaitResult(ctx context.Context, s *host.Session, resultPath string, timeout time.Duration) (map[string]any, error) {
	data, err := e.waitResult(ctx, s, resultPath, timeout)
	var failure *host.NeedError
	if err != nil {
		failure = host.AsNeed(err)
	}
	e.NetworkTick()
	mark := 0
	if v, ok := s.Get("net_mark"); ok {
		mark = int(host.PyFloat(v, 0))
	}
	denials := e.denials(mark)
	if leaked := e.warmLeak(s); len(leaked) > 0 {
		failure = host.Need(host.KindError, "fork wrote outside its tree: "+strings.Join(leaked, ", "))
	}
	if failure != nil {
		if len(denials) > 0 {
			if failure.Data == nil {
				failure.Data = map[string]any{}
			}
			failure.Data["network_denials"] = denialValues(denials)
		}
		return nil, failure
	}
	if len(denials) > 0 {
		out := make(map[string]any, len(data)+1)
		for k, v := range data {
			out[k] = v
		}
		out["network_denials"] = denialValues(denials)
		data = out
	}
	return data, nil
}

func (e *Executor) waitResult(ctx context.Context, s *host.Session, resultPath string, timeout time.Duration) (map[string]any, error) {
	name := s.Terminal
	if name == "" {
		return nil, host.Need(host.KindStalled, "agent is no longer registered")
	}
	deadline := e.now().Add(timeout)
	for {
		e.NetworkTick()
		if data := host.ReadResult(resultPath); data != nil {
			return data, nil
		}
		status, err := e.status(ctx, name, "")
		if err != nil {
			return nil, host.Need(host.KindStalled, "agent "+name+" went away: "+err.Error())
		}
		if status == "blocked" {
			return nil, e.blocked(ctx, s, name, resultPath)
		}
		// idle/done: the TUI settled, but only the result file ends the task.
		if !e.now().Before(deadline) {
			tail := ""
			if scr, err := e.screen(ctx, name); err == nil {
				tail = host.TailText(scr)
			}
			return nil, host.Need(host.KindTimeout, "no result after "+host.Seconds(timeout)+"s:\n"+tail)
		}
		if e.Poll > 0 {
			if err := e.sleep(ctx, e.Poll); err != nil {
				return nil, host.AsNeed(err)
			}
		}
	}
}

// Close closes the workspace; with removeWorktree the worktree goes too
// and the session leaves the registry. Errors are logged, never returned.
func (e *Executor) Close(s *host.Session, removeWorktree bool) error {
	e.closeAgent(s)
	if !removeWorktree {
		return nil
	}
	if repo := s.Str("repo"); repo != "" {
		if _, err := isolate.Remove(repo, "pkg-"+s.Pkg, true, ""); err != nil {
			e.warnf("worktree removal failed: %s", err)
		}
	}
	e.forget(s)
	return nil
}
