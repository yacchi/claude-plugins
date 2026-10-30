package config

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

// PnpmGVSEnv turns pnpm's global virtual store on for one process (pnpm 10
// reads npm_config_*, not pnpm_config_*). Every process orchestra starts in
// a tree prepared that way must carry it: a pnpm run without it rebuilds the
// classic layout, and one with it converts a classic layout.
const PnpmGVSEnv = "pnpm_config_enable_global_virtual_store=true"

// pnpmGVSMajor is the first pnpm major that reads pnpm_config_*, so
// PnpmGVSEnv reaches it. isolation.pnpm_global_virtual_store "auto" turns the
// global virtual store on for pnpm of this major and later.
const pnpmGVSMajor = 11

// pnpmProbe is one `pnpm --version` probe of a config root: the trimmed
// output when pnpm ran, else the error that stopped it.
type pnpmProbe struct {
	out string
	err error
}

// pnpmVersionRe takes the major of `pnpm --version` output ("12.0.0").
var pnpmVersionRe = regexp.MustCompile(`^(\d+)\.\d+`)

var (
	pnpmProbeMu sync.Mutex
	pnpmProbes  = map[string]pnpmProbe{}
	// pnpmVersionSeam replaces the `pnpm --version` probe (a test seam). Inside
	// a test binary "auto" is off without one, so no test depends on the pnpm
	// on PATH.
	pnpmVersionSeam func(dir string) ([]byte, error)
)

// PnpmGVS is isolation.pnpm_global_virtual_store of cfg: true or false as
// written, and "auto" as the pnpm of cfg's root decides (off for a nil cfg).
func PnpmGVS(cfg *Resolved) bool {
	on, _ := PnpmGVSStatus(cfg)
	return on
}

// PnpmGVSStatus is PnpmGVS with the reason for its answer: the setting as
// written, or for "auto" the pnpm version of cfg's root.
func PnpmGVSStatus(cfg *Resolved) (on bool, reason string) {
	v, _ := cfg.Lookup("isolation", "pnpm_global_virtual_store")
	switch x := v.(type) {
	case bool:
		if x {
			return true, "set to true"
		}
		return false, "set to false"
	case string:
		if x == "auto" {
			return pnpmGVSAuto(cfg.Root)
		}
	}
	return false, "setting is not true, false or \"auto\""
}

// pnpmGVSAuto decides "auto" from the pnpm that would run in root. The probe
// runs once per root per process, so every lookup of a root gives one answer.
func pnpmGVSAuto(root string) (bool, string) {
	pnpmProbeMu.Lock()
	defer pnpmProbeMu.Unlock()
	if pnpmVersionSeam == nil && testing.Testing() {
		return false, "auto: not probed in tests"
	}
	p, ok := pnpmProbes[root]
	if !ok {
		p = readPnpmVersion(root)
		pnpmProbes[root] = p
	}
	return p.verdict()
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

// readPnpmVersion runs the pnpm version probe in dir, through the seam when
// one is set.
func readPnpmVersion(dir string) pnpmProbe {
	probe := pnpmVersionSeam
	if probe == nil {
		probe = runPnpmVersion
	}
	out, err := probe(dir)
	if err != nil {
		return pnpmProbe{err: err}
	}
	return pnpmProbe{out: strings.TrimSpace(string(out))}
}

// verdict is the "auto" answer for a probe and why. A missing pnpm, a timeout,
// or output that is not a version leaves the global virtual store off.
func (p pnpmProbe) verdict() (bool, string) {
	if p.err != nil {
		return false, "auto: pnpm version unknown (" + p.err.Error() + ")"
	}
	m := pnpmVersionRe.FindStringSubmatch(p.out)
	if m == nil {
		return false, "auto: pnpm version unknown (" + p.out + ")"
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return false, "auto: pnpm version unknown (" + p.out + ")"
	}
	if n >= pnpmGVSMajor {
		return true, "auto: pnpm " + p.out + " (" + strconv.Itoa(pnpmGVSMajor) + " or later)"
	}
	return false, "auto: pnpm " + p.out + " is older than " + strconv.Itoa(pnpmGVSMajor)
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

// validateLand rejects isolation.land unless it is "auto" or "ask". It runs
// once, in Resolve, like validatePnpmGVS.
func validateLand(values map[string]any) error {
	v := ObjectAt(values, "isolation")["land"]
	if x, ok := v.(string); ok && (x == "auto" || x == "ask") {
		return nil
	}
	return fmt.Errorf("invalid isolation.land %v: must be \"auto\" or \"ask\"", v)
}

// PnpmGVSTree reports whether processes in tree run under the global
// virtual store: on (the setting, see PnpmGVS), tree is a linked worktree
// (LinkedWorktree, so never the user's main checkout) and it holds at least
// one pnpm project (PnpmProjects). A node_modules of the classic layout
// there needs no guard: orchestra's trees are disposable, and the carry
// rebuilds every project's node_modules under the store.
func PnpmGVSTree(on bool, tree string) bool {
	return on && LinkedWorktree(tree) && len(PnpmProjects(tree)) > 0
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
			"planner", modelEffort("opus", "xhigh"),
		),
		"planning", obj(
			"redo", modelEffort("opus", "max"),
			"spec_writers", obj(
				"light", modelEffort("sonnet", "medium"),
				"standard", modelEffort("opus", "high"),
				"deep", modelEffort("opus", "max"),
			),
			"writers_concurrent", int64(8),
			"review", true,
			"redo_when", obj(
				"structural_findings", int64(1),
				"deep_packages", int64(3),
				"interface_users", int64(5),
			),
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
			"usage_guard", int64(95),
			"usage_advise", int64(75),
			"fanout", obj("children", int64(6), "concurrent", int64(2), "scout_calls", int64(15), "checker_calls", int64(8), "writer_calls", int64(40)),
			"resume_max_tokens", int64(200000),
			"resume_max_tokens_by_model", obj("haiku", int64(100000)),
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
			"land", "auto",
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
