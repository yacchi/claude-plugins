package isolate

import (
	"fmt"
	"strings"
)

// FormatLandText is the one-line text form of a land result. It names the
// source the way the result was asked for: the path for --path, the task
// otherwise.
func FormatLandText(r LandResult) string {
	source := r.Task
	if r.Path != "" {
		source = r.Path
	}
	switch r.Status {
	case "landed":
		branch := r.Branch
		if r.Created {
			branch += " (new branch)"
		}
		return fmt.Sprintf("landed %s on %s: %d files (from %s)",
			r.Commit[:min(8, len(r.Commit))], branch, len(r.Files), source) + landNote(r)
	case "empty":
		if r.Path != "" {
			return fmt.Sprintf("nothing to land: %s has no change", source)
		}
		return fmt.Sprintf("nothing to land: %s has no commits past its base", source) + landNote(r)
	}
	line := r.Status
	if r.Reason != "" {
		line += ": " + r.Reason
	}
	if len(r.Files) > 0 {
		shown, more := r.Files, ""
		if len(shown) > 5 {
			shown, more = shown[:5], ", ..."
		}
		line += ": " + strings.Join(shown, ", ") + more
	}
	return line
}

// landNote says what landing an assembly did to the assembly and its request.
// A --path land has neither, so it gets no note.
func landNote(r LandResult) string {
	if r.Path != "" {
		return ""
	}
	note := ""
	switch {
	case r.Removed:
		note += "; assembly removed"
	case r.Reason != "":
		note += "; assembly kept: " + r.Reason
	}
	if r.RequestEnded {
		note += fmt.Sprintf("; request %s ended", r.Request)
	}
	return note
}
