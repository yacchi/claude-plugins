package integrate

import (
	"fmt"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// FormatText is the compact `isolate integrate --text` rendering. Like the
// JSON it never carries patch text.
func FormatText(r Result) string {
	if r.Status == "locked" {
		holder := r.Holder
		if holder == "" {
			holder = "-"
		}
		return fmt.Sprintf("integrate locked  into %s  holder %s", r.Into, holder)
	}
	onto := r.Onto
	if onto == "" {
		onto = "-"
	} else if len(onto) > 12 {
		onto = onto[:12]
	}
	path := string(r.Integration.Path)
	if path == "" {
		path = "-"
	}
	lines := []string{fmt.Sprintf("integrate %s  onto %s  worktree %s", r.Status, onto, path)}
	for _, t := range r.Tasks {
		line := fmt.Sprintf("  %-16s %-10s %d file(s)", t.Task, t.Status, t.FilesChanged)
		if t.RolledBack {
			line += "  rolled-back"
		}
		if t.Revert != "" {
			line += "  revert:" + t.Revert
		}
		if len(t.Conflicts) > 0 {
			line += "  " + isolate.FormatConflicts(t.Conflicts)
		}
		lines = append(lines, line)
	}
	if v := r.Verify; v != nil {
		exit := "None"
		if v.Exit != nil {
			exit = fmt.Sprint(*v.Exit)
		}
		line := fmt.Sprintf("verify %s  exit=%s  %.1fs", v.Status, exit, v.Seconds)
		if v.TimedOut {
			line += "  timed-out"
		}
		if v.Baseline != "" {
			line += "  baseline=" + v.Baseline
		}
		if v.Reason != "" {
			line += "  (" + v.Reason + ")"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "note: "+r.Note)
	return strings.Join(lines, "\n")
}
