package doctor

import (
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// annotatePiLogin adds `login` (provider -> ready) to the pi entry of an
// enabled, available `dispatch: cli` pi and, when a provider is not ready, a
// hint telling Claude to run `agent-exec pi login`.
func annotatePiLogin(executors map[string]any, cfg *config.Resolved) {
	entry, _ := executors["pi"].(*jsonx.Object)
	if entry == nil {
		return
	}
	enabled, _ := entry.Get("enabled")
	dispatch, _ := entry.Get("dispatch")
	available, _ := entry.Get("available")
	path, _ := entry.Get("path")
	piPath, _ := path.(string)
	if enabled != true || dispatch != "cli" || available != true || piPath == "" {
		return
	}
	states := jsonx.NewObject()
	var missing []string
	for _, p := range tools.ConfiguredProviders(cfg) {
		ok := tools.PiAuthReady(cfg, piPath, p, "")
		states.Set(p, ok)
		if !ok {
			missing = append(missing, p)
		}
	}
	entry.Set("login", states)
	if len(missing) > 0 {
		entry.Set("hint", "pi is not logged in for "+strings.Join(missing, ", ")+
			": Claude should run `agent-exec pi login`, show the user the URL (and code) it prints, "+
			"then run `agent-exec pi login --wait`; the user does not run pi themselves")
	}
}
