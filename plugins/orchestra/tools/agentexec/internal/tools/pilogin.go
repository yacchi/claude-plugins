package tools

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// DefaultLoginProvider is the provider checked when the config names none.
const DefaultLoginProvider = "openai-codex"

// PiSkipVersionCheck is the profile env every pi process runs with.
const PiSkipVersionCheck = "PI_SKIP_VERSION_CHECK=1"

// AuthCheckTimeout bounds one credential probe.
const AuthCheckTimeout = 60 * time.Second

// ConfiguredProviders are the provider prefixes of the pi models in the
// resolved config, in order (the default provider when there are none).
func ConfiguredProviders(cfg *config.Resolved) []string {
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
		return []string{DefaultLoginProvider}
	}
	return found
}

// PiLoginEnv is the environment a pi process runs in (the process env plus
// the profile's and the managed agent dir's additions) and the additions
// that come from the config, by name.
func PiLoginEnv(cfg *config.Resolved) (env []string, overrides map[string]string) {
	overrides = PiEnvOverrides(cfg)
	env = append(os.Environ(), PiSkipVersionCheck)
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+overrides[k])
	}
	return env, overrides
}

// PiAuthReady is true when `pi auth check --provider P --json --no-refresh`
// says the provider has credentials. It is the one probe `agent-exec doctor`
// and `agent-exec pi login` share. cwd "" is the current directory.
func PiAuthReady(cfg *config.Resolved, piPath, provider, cwd string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), AuthCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, piPath, "auth", "check", "--provider", provider, "--json", "--no-refresh")
	cmd.Dir = cwd
	if cmd.Dir == "" {
		cmd.Dir, _ = os.Getwd()
	}
	cmd.Env, _ = PiLoginEnv(cfg)
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
