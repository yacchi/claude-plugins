package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// cfgDashUUIDRe matches a claude.ai organization uuid, the key of
// dashboard.artifact_urls.
var cfgDashUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateDashboard checks the dashboard Artifact URLs. dashboard.artifact_url
// is refused outright: one global URL cannot be read from a session signed in
// to another claude.ai organization, so the URL is kept per organization. Each
// dashboard.artifact_urls entry maps an organization uuid to an empty string or
// a claude.ai Artifact URL. It runs once, in Resolve, like validateTiers.
func validateDashboard(values map[string]any) error {
	dash := ObjectAt(values, "dashboard")
	if _, ok := dash["artifact_url"]; ok {
		uuid, name := CurrentOrg()
		who := strings.TrimSpace(uuid + " " + name)
		if who == "" {
			who = "unknown"
		}
		return fmt.Errorf("dashboard.artifact_url was removed: orchestra keeps one dashboard per claude.ai organization. "+
			"Move the URL to dashboard.artifact_urls under the organization it was published in, e.g. "+
			"dashboard: {artifact_urls: {\"<organization uuid>\": \"<url>\"}} (this session's organization: %s), "+
			"or delete the key to publish a new dashboard", who)
	}
	urls, ok := dash["artifact_urls"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid dashboard.artifact_urls %v: must be a map of claude.ai organization uuid to Artifact URL", dash["artifact_urls"])
	}
	for _, org := range slices.Sorted(maps.Keys(urls)) {
		v := urls[org]
		if !cfgDashUUIDRe.MatchString(org) {
			return fmt.Errorf("invalid dashboard.artifact_urls key %q: must be a claude.ai organization uuid", org)
		}
		if s, ok := v.(string); !ok || (s != "" && !isArtifactURL(s)) {
			return fmt.Errorf("invalid dashboard.artifact_urls[%q] %v: must be empty or a claude.ai Artifact URL "+
				"(https://claude.ai/.../artifact...)", org, v)
		}
	}
	return nil
}

func isArtifactURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host == "claude.ai" && u.User == nil &&
		strings.Contains(u.Path, "/artifact")
}

// CurrentOrg is the claude.ai organization this Claude Code profile is signed
// in to: oauthAccount.organizationUuid / organizationName of
// $CLAUDE_CONFIG_DIR/.claude.json when CLAUDE_CONFIG_DIR is set, else of
// ~/.claude.json, else of ~/.claude/.claude.json. "" when none is readable.
func CurrentOrg() (uuid, name string) {
	for _, p := range cfgDashClaudeJSONPaths() {
		var doc struct {
			OAuthAccount struct {
				OrganizationUUID string `json:"organizationUuid"`
				OrganizationName string `json:"organizationName"`
			} `json:"oauthAccount"`
		}
		if fsutil.ReadJSONFile(p, &doc) == nil {
			return doc.OAuthAccount.OrganizationUUID, doc.OAuthAccount.OrganizationName
		}
	}
	return "", ""
}

// cfgDashClaudeJSONPaths lists the profile files CurrentOrg reads, in order.
// CLAUDE_CONFIG_DIR, when set, is the only profile: the home files belong to
// another profile.
func cfgDashClaudeJSONPaths() []string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return []string{filepath.Join(dir, ".claude.json")}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", ".claude.json")}
}

// DashboardURL is the dashboard Artifact URL recorded for org in the resolved
// values (dashboard.artifact_urls[org]); "" when org is "" or has none.
func DashboardURL(values map[string]any, org string) string {
	if org == "" {
		return ""
	}
	urls, _ := ObjectAt(values, "dashboard")["artifact_urls"].(map[string]any)
	s, _ := urls[org].(string)
	return s
}
