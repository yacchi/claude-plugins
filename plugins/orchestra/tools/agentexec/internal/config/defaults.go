package config

import (
	"maps"
	"slices"
)

// DefaultQuietDomains is background traffic of Claude Code and herdr that no
// worker task needs: still blocked and audited, never surfaced as a denial.
var DefaultQuietDomains = []string{
	"mcp-proxy.anthropic.com", "downloads.claude.ai", "statsig.anthropic.com",
	"datadoghq.com", "*.datadoghq.com", "sentry.io", "*.sentry.io", "herdr.dev",
}

// DefaultAllowedDomains is the only host a hosted Claude Code session needs.
var DefaultAllowedDomains = []string{"api.anthropic.com"}

// ToolOrder and ClassOrder are the built-in tools and worker classes in the
// order listings present them (see OrderedKeys).
var (
	ToolOrder  = []string{"pi", "herdr", "srt"}
	ClassOrder = []string{"light", "standard", "deep", "independent-review"}
)

// OrderedKeys lists the keys of m: those named in first, in that order,
// then every other key sorted.
func OrderedKeys(m map[string]any, first []string) []string {
	out := make([]string, 0, len(m))
	listed := make(map[string]bool, len(first))
	for _, k := range first {
		listed[k] = true
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !listed[k] {
			out = append(out, k)
		}
	}
	return out
}

// obj builds an Object from alternating keys and values.
func obj(kv ...any) map[string]any {
	o := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		o[kv[i].(string)] = kv[i+1]
	}
	return o
}

func strs(s ...string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func modelEffort(model, effort string) map[string]any {
	return obj("model", model, "effort", effort)
}

// Defaults returns a fresh copy of the built-in configuration, the lowest layer every resolution starts from. Integers are
// int64, as the YAML and JSON loaders produce them.
func Defaults() map[string]any {
	claudeModels := func() map[string]any {
		return obj("light", "sonnet", "standard", "opus", "deep", "opus")
	}
	return obj(
		"tools", obj(
			"installer", "auto",
			"pi", obj("source", "auto", "spec", "npm:@earendil-works/pi-coding-agent",
				"version", "1.2.0", "bin", "pi"),
			"herdr", obj("source", "auto", "spec", "aqua:herdrdev/herdr",
				"version", "0.8.2", "bin", "herdr"),
			"srt", obj("source", "auto", "spec", "npm:@anthropic-ai/sandbox-runtime",
				"version", "0.0.79", "bin", "srt"),
		),
		"watchdog", obj(
			"enabled", true,
			"idle_seconds", int64(600),
			"tool_idle_seconds", int64(900),
			"repeat_limit", int64(3),
			"wall_seconds", obj("light", int64(1200), "standard", int64(2400),
				"deep", int64(5400), "independent-review", int64(2400)),
		),
		"tiers", obj("light", "haiku", "standard", "sonnet", "deep", "opus", "review", "sonnet"),
		"external_executors", obj(
			"pi", obj(
				"enabled", true,
				"dispatch", "cli",
				"agent_dir", "managed",
				"context_files", true,
				"classes", strs("light", "standard", "deep", "independent-review"),
				"class_policy", obj(
					"light", modelEffort("openai-codex/gpt-5.6-luna", "medium"),
					"standard", modelEffort("openai-codex/gpt-5.6-luna", "medium"),
					"deep", modelEffort("openai-codex/gpt-6.1-sol", "high"),
					"independent-review", modelEffort("openai-codex/gpt-6.1-sol", "medium"),
				),
			),
		),
		"priority", obj(
			"light", obj("investigation", strs("claude", "pi"), "default", strs("claude", "pi")),
			"standard", obj("default", strs("pi", "claude")),
			"deep", obj("default", strs("claude", "pi")),
			"review", obj("default", strs("claude")),
			"independent-review", obj("default", strs("pi")),
		),
		"enforcement", obj(
			"light_class", "off",
			"worker_vcs", "block",
			"turn_edits", int64(8),
			"opus_generalist", "nudge",
			"session_cleanup", "on",
		),
		"cooldown", obj(
			"enabled", true,
			"path", "~/.claude/orchestra/executor-state.json",
			"seconds", obj("rate-limit", int64(900), "quota", int64(3600), "credits", int64(3600),
				"auth", int64(0), "nonzero-exit", int64(0), "sandbox", int64(0)),
		),
		"sandbox", obj(
			"mode", "auto",
			"allow_write", []any{},
			"deny_read", []any{},
			"network", obj(
				"mode", "allowlist",
				"ecosystem", true,
				"allowed_domains", strs(DefaultAllowedDomains...),
				"denied_domains", []any{},
				"quiet_domains", strs(DefaultQuietDomains...),
			),
		),
		"telemetry", obj("enabled", false, "dir", "~/.claude/orchestra/telemetry"),
		"ledger", obj("enabled", true, "dir", "~/.claude/orchestra/runs", "retention_days", int64(30)),
		"isolation", obj(
			"carry_extra", []any{},
			"carry_files", strs(".claude/settings.local.json", ".claude/orchestra.local.yaml",
				"CLAUDE.local.md"),
		),
		"checks", obj(
			"max_parallel", int64(2),
			"items", []any{},
			"retry_failed_alone", true,
			"flaky_threshold", int64(3),
			"ensure", []any{},
		),
		"orca", obj(
			"enabled", "auto",
			"command", "claude --model {model} --permission-mode {permission_mode}",
			"models", claudeModels(),
			"permission_mode", "auto",
			"user_config", "isolate",
			"add_dirs", []any{},
			"auto_trust", false,
			"startup_timeout", int64(90),
			"task_timeout", int64(3600),
			"keep_sessions", true,
			"setup", "skip",
		),
		"interactive_host", "auto",
		"herdr", obj(
			"enabled", "auto",
			"args", "--model {model} --permission-mode {permission_mode}",
			"models", claudeModels(),
			"permission_mode", "auto",
			"user_config", "isolate",
			"add_dirs", []any{},
			"auto_trust", true,
			"startup_timeout", int64(90),
			"task_timeout", int64(3600),
			"keep_sessions", true,
			"warm", obj("enabled", true, "ttl_seconds", int64(3000), "min_packages", int64(2)),
		),
	)
}
