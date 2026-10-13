package orca

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// Prompt sends ONE prompt line to the session's terminal, opening a new
// terminal on the worktree when the old one is gone or no longer shows a
// live Claude (after an exit it is a bare shell the line would be typed
// into). s.Resumed says whether it went to the running Claude.
func (e *Executor) Prompt(ctx context.Context, s *host.Session, promptFiles []string, resultPath string, timeout time.Duration) error {
	fresh := s.Pop("fresh")
	resumed := s.Terminal != "" && !fresh
	if resumed {
		screen, err := e.screen(ctx, s.Terminal)
		if err != nil {
			screen = nil
		}
		if host.IsTrust(screen) || !host.HasPromptBox(screen) {
			e.closeTerminal(s)
			resumed = false
		}
	}
	if s.Terminal == "" {
		// Closed after the last prompt (keep_sessions: false) or gone: a
		// new terminal on the same Orca worktree.
		handle, err := e.openTerminal(ctx, s)
		if err != nil {
			return host.AsNeed(err)
		}
		e.emit(s.Pkg, "start", handle, nil)
	}
	handle := s.Terminal
	if resumed {
		e.emit(s.Pkg, "reuse", handle, nil)
	}
	s.Resumed = resumed
	host.ResetResult(resultPath)
	line := host.PromptLine(promptFiles, resultPath)
	send := []string{"terminal", "send", "--terminal", handle, "--text", line, "--enter",
		"--wait-submit", fmt.Sprint(sendWaitSubmit)}
	sendTimeout := (sendWaitSubmit + 30) * time.Second
	result, err := e.call(ctx, sendTimeout, send...)
	id := requestID(result)
	if err != nil {
		var ce *CallError
		if errors.As(err, &ce) {
			id = requestID(ce.Payload["result"])
		}
		if id == "" {
			return host.Need(host.KindError, err.Error())
		}
		// Orca de-duplicates by request id: the prompt runs once.
		if _, err := e.call(ctx, sendTimeout, append(send, "--retry-request", id)...); err != nil {
			return host.Need(host.KindError, err.Error())
		}
	}
	if id != "" {
		ids, _ := s.Get("request_ids")
		list, _ := ids.([]any)
		s.Set("request_ids", append(list, id))
		e.save(s)
	}
	return nil
}

// WaitResult waits for the result file without sending another prompt; a
// permission/choice dialog nobody will answer is a stall.
func (e *Executor) WaitResult(ctx context.Context, s *host.Session, resultPath string, timeout time.Duration) (map[string]any, error) {
	handle := s.Terminal
	if handle == "" {
		return nil, host.Need(host.KindStalled, "terminal is no longer registered")
	}
	deadline := e.Clock.Now().Add(timeout)
	var screen []string
	for {
		if data := host.ReadResult(resultPath); data != nil {
			return data, nil
		}
		var err error
		screen, err = e.screen(ctx, handle)
		if err != nil {
			return nil, host.Need(host.KindStalled, "terminal "+handle+" went away: "+err.Error())
		}
		if host.IsDialog(screen) && !host.IsTrust(screen) {
			lines := host.DialogLines(screen)
			if len(lines) == 0 {
				lines = screen
			}
			return nil, host.Need(host.KindStalled, "dialog:\n"+strings.Join(lines, "\n")+"\nterminal: "+handle+
				"\nworktree: "+s.Worktree+"\nresult: "+resultPath)
		}
		remaining := deadline.Sub(e.Clock.Now())
		if remaining <= 0 {
			return nil, host.Need(host.KindTimeout, "no result after "+host.Seconds(timeout)+"s:\n"+host.TailLines(screen))
		}
		if err := e.waitIdle(ctx, handle, remaining); err != nil {
			return nil, host.AsNeed(err)
		}
	}
}

// Close closes the terminal; with removeWorktree the worktree is unadopted
// and removed through Orca and the session leaves the registry. Errors are
// logged, never returned.
func (e *Executor) Close(s *host.Session, removeWorktree bool) error {
	e.closeTerminal(s)
	if !removeWorktree {
		return nil
	}
	if repo := s.Str("repo"); repo != "" {
		if _, err := isolate.Unadopt(repo, "pkg-"+s.Pkg, ""); err != nil {
			e.warnf("unadopt failed: %s", err)
		}
	}
	if _, err := e.call(context.Background(), worktreeTimeout, "worktree", "rm", "--worktree", "path:"+s.Worktree, "--force"); err != nil {
		e.warnf("%s", err)
	}
	e.forget(s)
	return nil
}

// Shutdown has nothing to stop: Orca is the user's app.
func (e *Executor) Shutdown() {}
