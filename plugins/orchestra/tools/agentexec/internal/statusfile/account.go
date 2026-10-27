package statusfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// AccountFile is the Claude Code global config that names the signed-in
// claude.ai account: $CLAUDE_CONFIG_DIR/.claude.json when set, else
// ~/.claude.json (not under ~/.claude, which is the config dir's default).
func AccountFile() string {
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return filepath.Join(paths.ClaudeConfigDir(), ".claude.json")
	}
	return filepath.Join(paths.Home(), ".claude.json")
}

// Account is the claude.ai account the profile is signed in to: its
// organization name, else its email address. It never returns the account or
// organization uuid. "" when the file is missing, unreadable, not JSON, or
// names neither.
func Account() string {
	data, err := os.ReadFile(AccountFile())
	if err != nil {
		return ""
	}
	var cfg struct {
		OauthAccount *struct {
			OrganizationName string `json:"organizationName"`
			EmailAddress     string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.OauthAccount == nil {
		return ""
	}
	if org := strings.TrimSpace(cfg.OauthAccount.OrganizationName); org != "" {
		return org
	}
	return strings.TrimSpace(cfg.OauthAccount.EmailAddress)
}
