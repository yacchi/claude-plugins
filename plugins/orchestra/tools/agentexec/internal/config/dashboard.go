package config

import "fmt"

// checkRemovedDashboard rejects a config layer that still sets the dashboard
// key: the key was removed in 0.73.0 and progress is mirrored by the hooks
// module's board, published with /orchestra:board.
func checkRemovedDashboard(path string, layer map[string]any) error {
	if _, ok := layer["dashboard"]; !ok {
		return nil
	}
	return fmt.Errorf("%s: the dashboard key was removed in orchestra 0.73.0: nothing about the dashboard is configured any more. "+
		"Since 0.74.0 the orchestra hooks module mirrors ~/.claude/orchestra/status.json to one claude.ai page per account; "+
		"run /orchestra:board in a Claude Code session to publish it. Delete the dashboard key from this file", path)
}
