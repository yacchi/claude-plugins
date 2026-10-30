package config

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// NodeCPUsEnv is the variable the Node CPU cap preload (nodecap.cjs) reads:
// the CPU count os.availableParallelism() and os.cpus() report.
const NodeCPUsEnv = "ORCHESTRA_NODE_CPUS"

// NodeOptionsEnv is Node's own options variable, which carries the preload.
const NodeOptionsEnv = "NODE_OPTIONS"

//go:embed nodecap.cjs
var nodeCapPreload []byte

// NodeCapPreloadPath is where agent-exec keeps the Node CPU cap preload:
// <state dir>/node/cpucap.cjs (readable from every sandbox: they deny reads
// of secret stores only).
func NodeCapPreloadPath() string {
	return filepath.Join(paths.StateDir(), "node", "cpucap.cjs")
}

// ensureNodeCapPreload writes the embedded preload to NodeCapPreloadPath
// when the file there is missing or different, atomically, and returns the
// path.
func ensureNodeCapPreload() (string, error) {
	path := NodeCapPreloadPath()
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, nodeCapPreload) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := fsutil.AtomicWriteFile(path, nodeCapPreload, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// NodeCapCPUs is the CPU count a Node process sees under a test pool of
// testSlots: the cores shared out over the slots (at least 1) plus one,
// since jest and vitest start that count minus one workers. 0 when the pool
// is off.
func NodeCapCPUs(testSlots int) int {
	if testSlots <= 0 {
		return 0
	}
	return max(1, CPUCount()/testSlots) + 1
}

// NodeCapEntries are the env entries that cap the CPUs every Node process
// sees (NodeCapCPUs of testSlots), given env (the env it would otherwise
// get): NodeCPUsEnv and NODE_OPTIONS with `--require "<preload>"` put before
// env's own value (not added twice). Nil when the test pool is off or the
// preload cannot be written.
func NodeCapEntries(testSlots int, env []string) []string {
	n := NodeCapCPUs(testSlots)
	if n == 0 {
		return nil
	}
	path, err := ensureNodeCapPreload()
	if err != nil {
		return nil
	}
	require := NodeCapRequire(path)
	opts, _ := envValue(env, NodeOptionsEnv)
	switch {
	case strings.Contains(opts, require):
	case opts == "":
		opts = require
	default:
		opts = require + " " + opts
	}
	return []string{NodeCPUsEnv + "=" + strconv.Itoa(n), NodeOptionsEnv + "=" + opts}
}

// NodeCapRequire is the NODE_OPTIONS option that preloads path, quoted as
// Node reads NODE_OPTIONS (a backslash escapes the next character).
func NodeCapRequire(path string) string {
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path)
	return `--require "` + quoted + `"`
}

// ProcessTestSlots is the test pool's size as this process's config reads
// it (PoolSize), read once (a test seam). Inside a test binary it is 0, so
// no test writes the preload into the machine's state dir unless it asks.
var ProcessTestSlots = sync.OnceValue(func() int {
	if testing.Testing() {
		return 0
	}
	cfg, err := Resolve()
	if err != nil {
		cfg = nil
	}
	return PoolSize(cfg, PoolTest)
})

// WithNodeCap is env (nil: the process's own) with the NodeCapEntries of the
// process's test pool set; env as is when there are none.
func WithNodeCap(env []string) []string {
	entries := NodeCapEntries(ProcessTestSlots(), envOrProcess(env))
	if len(entries) == 0 {
		return env
	}
	return withEntries(envOrProcess(env), entries)
}

func envOrProcess(env []string) []string {
	if env == nil {
		return os.Environ()
	}
	return env
}

// NodeCapShellPrefix is the shell that sets the NodeCapEntries of testSlots
// before a command, the NODE_OPTIONS require put before the shell's own:
// `export ORCHESTRA_NODE_CPUS=<N>; export NODE_OPTIONS='--require
// "<preload>" '"${NODE_OPTIONS:-}"; `. "" when there are no entries.
func NodeCapShellPrefix(testSlots int) string {
	var b strings.Builder
	for _, kv := range NodeCapEntries(testSlots, nil) {
		name, value, _ := strings.Cut(kv, "=")
		b.WriteString("export " + name + "=")
		if name == NodeOptionsEnv {
			b.WriteString(shellQuote(value+" ") + `"${NODE_OPTIONS:-}"`)
		} else {
			b.WriteString(value) // the CPU count: digits only
		}
		b.WriteString("; ")
	}
	return b.String()
}

// shellQuote is s as one single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
