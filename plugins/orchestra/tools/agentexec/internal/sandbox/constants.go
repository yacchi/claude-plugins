// Package sandbox confines CLI executor children (defense in depth on top
// of worktree isolation).
//
// A sandboxed worker may only WRITE inside its task's tree (plus temp,
// caches and its own executor state), may not READ well-known secret
// stores, and may not signal processes outside its sandbox. Network stays
// open (model API) unless sandbox-runtime (srt, package srt) restricts it
// to an allowlist.
//
// One policy (buildPolicy) feeds every backend:
//
//   - macOS seatbelt: sandbox-exec -D ... -p <SBPL> -- argv.
//   - Linux bwrap (preferred, only when a real probe succeeds).
//   - Linux landlock (fallback): agent-exec re-executes itself as
//     `agent-exec _sandbox-exec --write W ... -- argv`; only that child
//     restricts itself (ExecMain), then execs argv.
//   - srt: sandbox-runtime applies its own Seatbelt/bubblewrap from the same
//     policy plus an outbound domain allowlist; it replaces the backends
//     above, never wraps them.
//   - none: mode off, or nothing usable on this host.
//
// Every path is realpath'd (see realPath) before it
// reaches a rule: on macOS /tmp is /private/tmp and $TMPDIR lives under
// /private/var/folders, and a Seatbelt rule on the unresolved spelling
// silently never matches. The rendered SBPL is pinned by
// sbpl_fixture_darwin_test.go.
package sandbox

// Modes of sandbox.mode and sandbox.network.mode.
var (
	modes        = []string{"auto", "required", "off"}
	networkModes = []string{"allowlist", "off"}
)

// DefaultAllowedDomains is the only host a hosted Claude Code session needs.
var DefaultAllowedDomains = []string{"api.anthropic.com"}

// DefaultQuietDomains is background traffic no worker task needs: still
// blocked and audited, never surfaced as a denial to approve.
var DefaultQuietDomains = []string{
	"mcp-proxy.anthropic.com", "downloads.claude.ai", "statsig.anthropic.com",
	"datadoghq.com", "*.datadoghq.com", "sentry.io", "*.sentry.io", "herdr.dev",
}

// DefaultDenyRead lists the secret stores no worker has a reason to read.
var DefaultDenyRead = []string{
	"~/.ssh",
	"~/.aws",
	"~/.gnupg",
	"~/.config/gh",
	"~/.netrc",
	"~/.docker/config.json",
	"~/.kube",
	"~/Library/Keychains",
}

// hostReadable are DefaultDenyRead entries the herdr host must read (Claude
// Code keeps its login in the Keychain).
var hostReadable = []string{"~/Library/Keychains"}

// Package/build caches every toolchain a worker runs writes into, writable
// from the first run (with pnpm's store, see pnpmStoreDir). Only the cache
// dirs and the lock/metadata files a tool must write beside them, never a
// toolchain home that also holds executables run later outside any
// sandbox (~/.cargo/bin, ~/Library/pnpm/bin, ~/.bun/bin, ~/.pub-cache/bin,
// ~/.gradle/wrapper; not ~/.local/share/uv/tools either: a sandboxed child
// gets UV_TOOL_DIR in temp instead).
var (
	seedCacheWrites       = []string{"~/.cache", "~/.npm", "~/go/pkg/mod"}
	seedCacheWritesDarwin = []string{"~/Library/Caches"}
	seedPackageCaches     = []string{
		"~/go/pkg/sumdb",
		"~/.pnpm-store",
		"~/.yarn/berry/cache",
		"~/.bun/install/cache",
		"~/.cargo/registry",
		"~/.cargo/git",
		"~/.cargo/.package-cache",
		"~/.cargo/.package-cache-mutate",
		"~/.cargo/.global-cache",
		"~/.cargo/.global-cache-journal",
		"~/.gradle/caches",
		"~/.m2/repository",
		"~/.ivy2/cache",
		"~/.nuget/packages",
		"~/.pub-cache/hosted",
		"~/.pub-cache/git",
		"~/.composer/cache",
		"~/.terraform.d/plugin-cache",
	}
)

// pnpmStorePin pins pnpm's store to the seeded one (pnpm 12 reads only
// pnpm_config_*). npm_config_store_dir is never set: npm warns about the
// unknown key on every run and means to reject it in its next major. pnpm
// <= 10, which reads only that name, may still move its store into the
// tree; isolation.exclude_paths keeps it out of the change set.
const pnpmStorePin = "pnpm_config_store_dir"

// pnpmStoreEnv are the names a user's explicit store dir may come in.
var pnpmStoreEnv = []string{pnpmStorePin, "npm_config_store_dir"}

// cacheRootsRel are the shared package caches a denied write may be
// AUTO-granted under (root + one more component). An allowlist: each
// ecosystem verifies what it pulls out of its cache (lockfile/checksum).
var cacheRootsRel = []string{
	"~/.cache",
	"~/Library/Caches",
	"~/.npm",
	"~/.pnpm-store",
	"~/Library/pnpm/store",
	"~/.local/share/pnpm/store",
	"~/.yarn/berry/cache",
	"~/go/pkg/mod",
	"~/.cargo/registry",
	"~/.cargo/git",
	"~/.gradle/caches",
	"~/.m2/repository",
	"~/.bun/install/cache",
	"~/.nuget/packages",
	"~/.pub-cache",
	"~/.composer/cache",
	"~/.ivy2/cache",
	"~/.terraform.d/plugin-cache",
}

// tamperFiles no worker may write whatever the writable set says.
var tamperFiles = []string{
	"~/.claude/orchestra.yaml",
	"~/.claude/orchestra.yml",
	"~/.claude/orchestra/executor-state.json",
	"~/.local/share/uv/tools",
}

// What pi LOADS as code or instructions on a later, unsandboxed run.
var (
	piDenySubdirs = []string{"extensions", "skills", "prompts", "themes", "packages",
		"npm", "git", "bin", "install"}
	piDenyFiles = []string{"settings.json", "mcp.json", "models.json", "AGENTS.md",
		"SYSTEM.md", "APPEND_SYSTEM.md", "keybindings.json"}
)

const (
	uvToolDirName = "orchestra-uv-tools"
	// ForceEnv=none makes every backend probe fail (tests of mode: required).
	ForceEnv    = "AGENT_EXEC_SANDBOX_BACKEND"
	SandboxExec = "/usr/bin/sandbox-exec"

	stateDirRel = "~/.local/state/claude-orchestra"

	// SrtSettingsDir holds the herdr server's srt settings and control FIFO.
	SrtSettingsDir = "herdr-srt"
	// CLISrtDir holds the settings and debug logs of CLI executor runs.
	CLISrtDir = "cli-srt"
	// SrtControlFD is the fd srt reads live config lines from.
	SrtControlFD = 3
	AuditDir     = "audit"
	NetworkAudit = "network.jsonl"
)

// userSources may add a grant under the user rules.
var userSources = []string{"user", "instructor"}

// What Claude Code must not be able to plant in its own config dir.
var claudeTamperNames = []string{"settings.json", "settings.local.json", "hooks", "skills",
	"commands", "agents", "plugins", "CLAUDE.md"}

// inlinePluginData is the one writable spot under the config dir's plugins/.
const inlinePluginData = "plugins/data/orchestra-inline"

// claudePluginNames are denied by name under plugins/ even before they exist.
var claudePluginNames = []string{"cache", "marketplaces", "repos", "installed_plugins.json",
	"known_marketplaces.json", "config.json"}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
