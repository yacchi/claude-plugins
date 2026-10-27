package config

import "fmt"

// checkRemovedDashboard rejects a config layer that still sets the dashboard
// key: the claude.ai Artifact dashboard was removed in 0.73.0 and progress
// goes to ~/.claude/orchestra/status.json.
func checkRemovedDashboard(path string, layer map[string]any) error {
	if _, ok := layer["dashboard"]; !ok {
		return nil
	}
	return fmt.Errorf("%s: the dashboard key was removed in orchestra 0.73.0: the claude.ai Artifact dashboard is gone, "+
		"and wave and plan progress is written to ~/.claude/orchestra/status.json (agent-exec status show; docs/status-file.md). "+
		"Delete the dashboard key from this file", path)
}
