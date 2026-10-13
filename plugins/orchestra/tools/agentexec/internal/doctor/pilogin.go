package doctor

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

const (
	defaultProvider  = "openai-codex"
	authCheckTimeout = 60 * time.Second
)

// configuredProviders are the provider prefixes of the pi models in the
// resolved config, in order.
func configuredProviders(cfg *config.Resolved) []string {
	var found []string
	policy := cfg.Section("external_executors").Object("pi").Object("class_policy")
	for _, cls := range policy.Keys() {
		model, _ := policy.Object(cls).Get("model")
		s, ok := model.(string)
		if !ok || !strings.Contains(s, "/") {
			continue
		}
		provider, _, _ := strings.Cut(s, "/")
		if provider == "" {
			continue
		}
		dup := false
		for _, f := range found {
			dup = dup || f == provider
		}
		if !dup {
			found = append(found, provider)
		}
	}
	if len(found) == 0 {
		return []string{defaultProvider}
	}
	return found
}

// authReady is true when `pi auth check` says the provider has credentials.
func authReady(cfg *config.Resolved, piPath, provider string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), authCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, piPath, "auth", "check", "--provider", provider, "--json", "--no-refresh")
	cmd.Dir, _ = os.Getwd()
	env := append(os.Environ(), "PI_SKIP_VERSION_CHECK=1")
	overrides := tools.PiEnvOverrides(cfg)
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+overrides[k])
	}
	cmd.Env = env
	out, err := cmd.Output()
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		return false
	}
	status := ""
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		v, derr := jsonx.Decode([]byte(lines[i]))
		if derr != nil {
			continue
		}
		if obj, ok := v.(*jsonx.Object); ok {
			s, _ := obj.Get("status")
			status, _ = s.(string)
			break
		}
	}
	return err == nil && status != "not_ready"
}

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
	for _, p := range configuredProviders(cfg) {
		ok := authReady(cfg, piPath, p)
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
