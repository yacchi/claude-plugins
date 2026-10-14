package isolate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

var errInvalidSession = errors.New("invalid session id")

// CollectResult is `isolate collect`'s output, for one task or (Collected
// set, IsSession) for every tree of a session.
type CollectResult struct {
	Status    string // collected | absent | error
	Task      string
	Reason    string
	Digest    string
	Session   string
	IsSession bool
	Collected []CollectResult
}

func (r CollectResult) MarshalJSON() ([]byte, error) {
	o := map[string]any{"status": r.Status}
	switch {
	case r.IsSession:
		collected := r.Collected
		if collected == nil {
			collected = []CollectResult{}
		}
		o["session"] = r.Session
		o["collected"] = collected
	case r.Status == "error" && r.Task == "":
		o["reason"] = r.Reason
	case r.Status == "error":
		o["task"] = r.Task
		o["reason"] = r.Reason
	case r.Status == "absent":
		o["task"] = r.Task
	default:
		o["task"] = r.Task
		o["digest"] = r.Digest
	}
	return fsutil.MarshalJSON(o)
}

// Collect marks task's worktree collected without printing its patch: the
// explicit "I have taken this content out by other means" verb for routes
// (integration worktrees above all) nothing else ever collects.
func Collect(root, task, session string) (CollectResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return CollectResult{Status: "error", Reason: "not a git repository"}, nil
	}
	task, err := SanitizeTaskID(task)
	if err != nil {
		return CollectResult{}, err
	}
	wt, found, err := resolveWorktree(resolved, task, session)
	if err != nil {
		return CollectResult{}, err
	}
	if !found {
		return CollectResult{Status: "absent", Task: task}, nil
	}
	diff, err := Diff(resolved, task, session, true)
	if err != nil {
		return CollectResult{}, err
	}
	if diff.Status != "ok" {
		reason := diff.Reason
		if reason == "" {
			reason = "could not compute diff for " + task
		}
		return CollectResult{Status: "error", Task: task, Reason: reason}, nil
	}
	digest, _ := MarkCollected(wt.Path, diff.Files, diff.Patch)
	return CollectResult{Status: "collected", Task: task, Digest: digest}, nil
}

// CollectSession collects every worktree of session.
func CollectSession(root, session string) (CollectResult, error) {
	wanted := sessionComponent(session)
	if wanted == "" {
		return CollectResult{}, errInvalidSession
	}
	entries, _ := List(root, "")
	res := CollectResult{Status: "collected", Session: wanted, IsSession: true, Collected: []CollectResult{}}
	for _, e := range entries {
		if string(e.Session) != wanted {
			continue
		}
		one, err := Collect(root, e.Task, wanted)
		if err != nil {
			return CollectResult{}, err
		}
		res.Collected = append(res.Collected, one)
	}
	return res, nil
}

// FormatCollectText is the compact `isolate collect --text` rendering (no
// patch text).
func FormatCollectText(r CollectResult) string {
	if r.IsSession {
		lines := []string{fmt.Sprintf("collect %s  session %s", r.Status, r.Session)}
		for _, e := range r.Collected {
			lines = append(lines, fmt.Sprintf("  %-16s %-10s %s", noneIfEmpty(e.Task), e.Status, short(e.Digest)))
		}
		return strings.Join(lines, "\n")
	}
	return fmt.Sprintf("collect %-10s task %-16s %s", r.Status, noneIfEmpty(r.Task), short(r.Digest))
}

func short(digest string) string {
	if digest == "" {
		return "-"
	}
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// noneIfEmpty prints "-" for a missing value.
func noneIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
