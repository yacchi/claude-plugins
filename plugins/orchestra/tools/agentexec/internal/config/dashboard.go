package config

import (
	"fmt"
	"net/url"
	"strings"
)

// validateDashboard rejects a dashboard.artifact_url (the claude.ai Artifact
// page the orchestra-wave watcher writes wave progress to) that is neither
// empty nor an https://claude.ai/ URL whose path names an artifact. It runs
// once, in Resolve, like validateTiers.
func validateDashboard(values map[string]any) error {
	v := ObjectAt(values, "dashboard")["artifact_url"]
	if s, ok := v.(string); ok && (s == "" || isArtifactURL(s)) {
		return nil
	}
	return fmt.Errorf("invalid dashboard.artifact_url %v: must be empty or a claude.ai Artifact URL "+
		"(https://claude.ai/.../artifact...)", v)
}

func isArtifactURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host == "claude.ai" && u.User == nil &&
		strings.Contains(u.Path, "/artifact")
}
