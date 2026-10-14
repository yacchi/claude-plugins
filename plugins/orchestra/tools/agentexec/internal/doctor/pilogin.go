package doctor

import (
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// annotatePiLogin adds `login` (provider -> ready) to the pi entry of an
// enabled, available `dispatch: cli` pi and, when a provider is not ready, a
// hint telling Claude to run `agent-exec pi login`.
func annotatePiLogin(executors map[string]any, cfg *config.Resolved) {
	entry, _ := executors["pi"].(map[string]any)
	if entry == nil {
		return
	}
	enabled := entry["enabled"]
	dispatch := entry["dispatch"]
	available := entry["available"]
	path := entry["path"]
	piPath, _ := path.(string)
	if enabled != true || dispatch != "cli" || available != true || piPath == "" {
		return
	}
	states := map[string]any{}
	var missing []string
	for _, p := range tools.ConfiguredProviders(cfg) {
		ok := tools.PiAuthReady(cfg, piPath, p, "")
		states[p] = ok
		if !ok {
			missing = append(missing, p)
		}
	}
	entry["login"] = states
	if len(missing) > 0 {
		entry["hint"] = "pi is not logged in for " + strings.Join(missing, ", ") +
			": Claude should run `agent-exec pi login`, show the user the URL (and code) it prints, " +
			"then run `agent-exec pi login --wait`; the user does not run pi themselves"
	}
}
