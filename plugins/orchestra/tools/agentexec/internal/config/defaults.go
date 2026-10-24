package config

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultQuietDomains is background traffic of Claude Code and herdr that no
// worker task needs: still blocked and audited, never surfaced as a denial.
var DefaultQuietDomains = []string{
	"mcp-proxy.anthropic.com", "downloads.claude.ai", "statsig.anthropic.com",
	"datadoghq.com", "*.datadoghq.com", "sentry.io", "*.sentry.io", "herdr.dev",
}

// DefaultAllowedDomains is the only host a hosted Claude Code session needs.
var DefaultAllowedDomains = []string{"api.anthropic.com"}

// DefaultExcludePaths are dependency and cache directories kept out of every
// task's change set even when the repository does not ignore them: an entry
// without an inner "/" matches that directory name at any depth.
var DefaultExcludePaths = []string{
	".pnpm-store/", "node_modules/", ".venv/", "__pycache__/", ".turbo/", ".next/", ".gradle/", ".terraform/",
}

// DefaultMaxAddedFiles is how many new files one task may add before
// integrate refuses it (0 = no limit).
const DefaultMaxAddedFiles = 500

// DefaultUsageEvery is how many integrated packages `wave run` lets pass
// between two usage notices.
const DefaultUsageEvery = 10

// PnpmGVSEnv turns pnpm's global virtual store on for one process (pnpm 12
// reads pnpm_config_*, not npm_config_*). Every process orchestra starts in
// a tree prepared that way must carry it: a pnpm run without it rebuilds the
// classic layout, and one with it converts a classic layout.
const PnpmGVSEnv = "pnpm_config_enable_global_virtual_store=true"

// pnpmGVSMajor is the first pnpm major that turns the global virtual store on
// by itself, which is what isolation.pnpm_global_virtual_store "auto" follows.
const pnpmGVSMajor = 12

// pnpmVersionRe takes the major of `pnpm --version` output ("12.0.0").
var pnpmVersionRe = regexp.MustCompile(`^(\d+)\.\d+`)

var (
	pnpmProbeMu sync.Mutex
	pnpmProbes  = map[string]bool{}
	// pnpmVersionSeam replaces the `pnpm --version` probe (a test seam). Inside
	// a test binary "auto" is off without one, so no test depends on the pnpm
	// on PATH.
	pnpmVersionSeam func(dir string) ([]byte, error)
)

// PnpmGVS is isolation.pnpm_global_virtual_store of cfg: true or false as
// written, and "auto" as the pnpm of cfg's root decides (off for a nil cfg).
func PnpmGVS(cfg *Resolved) bool {
	v, _ := cfg.Lookup("isolation", "pnpm_global_virtual_store")
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "auto" && pnpmGVSAuto(cfg.Root)
	}
	return false
}

// pnpmGVSAuto probes the pnpm that would run in root, once per root per
// process.
func pnpmGVSAuto(root string) bool {
	pnpmProbeMu.Lock()
	defer pnpmProbeMu.Unlock()
	if pnpmVersionSeam == nil && testing.Testing() {
		return false
	}
	on, ok := pnpmProbes[root]
	if !ok {
		on = pnpmMajorAtLeast(root, pnpmGVSMajor)
		pnpmProbes[root] = on
	}
	return on
}

// runPnpmVersion is `pnpm --version` in dir, so corepack and packageManager
// pins there apply, with a short timeout.
func runPnpmVersion(dir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pnpm", "--version")
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	return cmd.Output()
}

// pnpmMajorAtLeast reads the pnpm version of dir. A missing pnpm, a timeout,
// or output that is not a version reads as false.
func pnpmMajorAtLeast(dir string, major int) bool {
	probe := pnpmVersionSeam
	if probe == nil {
		probe = runPnpmVersion
	}
	out, err := probe(dir)
	if err != nil {
		return false
	}
	m := pnpmVersionRe.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return false
	}
	n, err := strconv.Atoi(m[1])
	return err == nil && n >= major
}

// validatePnpmGVS rejects isolation.pnpm_global_virtual_store unless it is
// true, false or "auto". It runs once, in Resolve, like validateTiers.
func validatePnpmGVS(values map[string]any) error {
	v := ObjectAt(values, "isolation")["pnpm_global_virtual_store"]
	switch x := v.(type) {
	case bool:
		return nil
	case string:
		if x == "auto" {
			return nil
		}
	}
	return fmt.Errorf("invalid isolation.pnpm_global_virtual_store %v: must be true, false or \"auto\"", v)
}

// PnpmGVSTree reports whether processes in tree run under the global
// virtual store: on (the setting, see PnpmGVS), tree is a linked worktree
// (its .git is a file, so never the user's main checkout) whose root holds
// pnpm-lock.yaml, and its node_modules, when present, is not of the classic
// layout (a clone fallback, or a tree the user installed: the env would
// convert it).
func PnpmGVSTree(on bool, tree string) bool {
	if !on || tree == "" || !isFile(filepath.Join(tree, ".git")) ||
		!isFile(filepath.Join(tree, "pnpm-lock.yaml")) {
		return false
	}
	nm := filepath.Join(tree, "node_modules")
	if _, err := os.Lstat(nm); err != nil {
		return true // nothing installed yet: the first install builds it
	}
	return pnpmGVSLayout(nm)
}

// pnpmGVSLayout reports whether node_modules dir was installed with the
// global virtual store: its virtualStoreDir lies outside it (in the store's
// links), where the classic one is node_modules/.pnpm. Unreadable reads as
// classic.
func pnpmGVSLayout(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, ".modules.yaml"))
	if err != nil {
		return false
	}
	var meta struct {
		VirtualStoreDir string `yaml:"virtualStoreDir"`
	}
	if yaml.Unmarshal(raw, &meta) != nil || meta.VirtualStoreDir == "" {
		return false
	}
	v := meta.VirtualStoreDir
	if !filepath.IsAbs(v) {
		v = filepath.Join(dir, v)
	}
	rel, err := filepath.Rel(dir, v)
	return err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

var (
	pnpmCIWarn   sync.Once
	pnpmCIStderr io.Writer = os.Stderr
)

// PnpmGVSOn is isolation.pnpm_global_virtual_store of the resolved config,
// read once per process (a test seam).
var PnpmGVSOn = sync.OnceValue(func() bool {
	cfg, err := Resolve()
	return err == nil && PnpmGVS(cfg)
})

// TreeEnv is the env of a command orchestra runs in tree: the process's own
// with the TreeEntries of tree set, else nil (inherited as is).
func TreeEnv(tree string) []string {
	on := PnpmGVSOn()
	env := os.Environ()
	entries := TreeEntries(on, tree, env)
	if len(entries) == 0 {
		return nil
	}
	return withEntries(env, entries)
}

// WithPnpmGVS is env plus PnpmGVSEnv, for a process in a tree prepared under
// the global virtual store by another path (isolate's carry install).
func WithPnpmGVS(env []string) []string {
	warnPnpmCI(env)
	return append(append([]string(nil), env...), PnpmGVSEnv)
}

// warnPnpmCI warns once per process when env sets a truthy CI: pnpm ignores
// the global virtual store under CI. The setting is left alone.
func warnPnpmCI(env []string) {
	ci := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CI="); ok {
			ci = v
		}
	}
	switch strings.ToLower(strings.TrimSpace(ci)) {
	case "", "0", "false":
	default:
		pnpmCIWarn.Do(func() {
			fmt.Fprintf(pnpmCIStderr, "agent-exec: CI=%s is set: pnpm ignores its global virtual store under CI "+
				"(isolation.pnpm_global_virtual_store), so installs in orchestra trees build the classic layout\n", ci)
		})
	}
}

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
		"tiers", obj(
			"light", modelEffort("haiku", "medium"),
			"standard", modelEffort("haiku", "high"),
			"deep", modelEffort("opus", "medium"),
			"review", modelEffort("sonnet", "medium"),
			"planner", modelEffort("opus", "max"),
		),
		"external_executors", obj(
			"pi", obj(
				"enabled", true,
				"dispatch", "cli",
				"agent_dir", "managed",
				"context_files", true,
				"classes", strs("light", "standard", "deep", "independent-review"),
				"class_policy", obj(
					"light", modelEffort("openai-codex/gpt-5.6-luna", "medium"),
					"standard", modelEffort("openai-codex/gpt-5.6-luna", "high"),
					"deep", modelEffort("openai-codex/gpt-6.1-sol", "high"),
					"independent-review", modelEffort("openai-codex/gpt-6.1-sol", "medium"),
				),
			),
		),
		"priority", obj(
			"light", obj("investigation", strs("claude", "pi"), "default", strs("claude", "pi")),
			"standard", obj("default", strs("claude", "pi")),
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
		"usage_routing", obj(
			"enabled", true,
			"thresholds", obj("five_hour", int64(80), "seven_day", int64(90)),
			"cache_seconds", int64(120),
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
			"exclude_paths", strs(DefaultExcludePaths...),
			"keep_paths", []any{},
			"max_added_files", int64(DefaultMaxAddedFiles),
			"pnpm_global_virtual_store", "auto",
		),
		"throttle", obj("heavy_slots", "auto", "test_slots", "auto", "heavy_extra", []any{}),
		"wave", obj(
			"max_in_flight", "auto",
			"memory_guard", true,
			"usage_every", int64(DefaultUsageEvery),
			"budget_usd", int64(0),
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
