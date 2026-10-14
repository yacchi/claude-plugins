// Package readiness reports whether agent-exec is installed where the
// instructor can call it and which CLI executors are ready to be routed to.
// doctor and route share it.
package readiness

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// KnownExecutors are the executors agent-exec can drive, with the dispatch
// mode an unset config means.
var KnownExecutors = map[string]string{"pi": "cli"}

var knownBindirs = []string{"~/.local/bin", "/usr/local/bin", "~/bin", "~/.claude/bin"}

var shimExecRE = regexp.MustCompile(`exec\s+"([^"]+)"\s+"\$@"`)

// ExecReady is one executor's readiness verdict.
type ExecReady struct {
	OK      bool     `json:"ok"`
	Missing []string `json:"missing"`
}

// Shim describes the `agent-exec` launcher found on PATH or in a known bindir.
type Shim struct {
	Installed  bool    `json:"installed"`
	Path       *string `json:"path"`
	Target     *string `json:"target"`
	AnchoredTo *string `json:"anchored_to"`
	OnPath     bool    `json:"on_path"`
}

// Permission is the best-effort scan for the Bash(agent-exec:*) allow rule.
type Permission struct {
	Rule           string   `json:"rule"`
	Found          bool     `json:"found"`
	Sources        []string `json:"sources"`
	BestEffortNote string   `json:"best_effort_note"`
}

// Report is the readiness part of the doctor report. Executors holds one
// object per known or configured executor (doctor appends its own
// annotations); Ready only covers enabled `dispatch: cli` known executors.
type Report struct {
	Shim       Shim
	Permission Permission
	Executors  map[string]any
	Ready      map[string]ExecReady
}

func isExecutableFile(path string) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	return syscall.Access(path, 1) == nil
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func pathDirs() []string {
	var out []string
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func candidateShimDirs() []string {
	dirs := pathDirs()
	for _, p := range knownBindirs {
		dirs = append(dirs, fsutil.ExpandUser(p))
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		key := abs(d)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

func classifyAnchor(target string) *string {
	if target == "" {
		return nil
	}
	v := "other"
	switch {
	case strings.Contains(target, "/marketplaces/"):
		v = "marketplace-clone"
	case strings.Contains(target, "/cache/"):
		v = "cache"
	}
	return &v
}

// FindShim looks for an executable `agent-exec` on PATH, then in the known
// bindirs.
func FindShim() Shim {
	onPathDirs := map[string]bool{}
	for _, p := range pathDirs() {
		onPathDirs[abs(p)] = true
	}
	for _, d := range candidateShimDirs() {
		candidate := filepath.Join(d, "agent-exec")
		if !isExecutableFile(candidate) {
			continue
		}
		var target string
		if raw, err := os.ReadFile(candidate); err == nil {
			if m := shimExecRE.FindStringSubmatch(string(raw)); m != nil {
				target = m[1]
			}
		}
		path := abs(candidate)
		s := Shim{Installed: true, Path: &path, AnchoredTo: classifyAnchor(target), OnPath: onPathDirs[abs(d)]}
		if target != "" {
			s.Target = &target
		}
		return s
	}
	return Shim{}
}

const permissionRule = "Bash(agent-exec:*)"

// ScanPermission looks for the allow rule in the settings files it knows of.
func ScanPermission() Permission {
	candidates := []string{
		fsutil.ExpandUser("~/.claude/settings.json"),
		filepath.Join(".", ".claude", "settings.json"),
		filepath.Join(".", ".claude", "settings.local.json"),
	}
	sources := []string{}
	seen := map[string]bool{}
	for _, path := range candidates {
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		key, err := filepath.EvalSymlinks(path)
		if err != nil {
			key = path
		}
		key = abs(key)
		if seen[key] {
			continue
		}
		seen[key] = true
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		v, err := fsutil.DecodeJSON(raw)
		if err != nil {
			continue
		}
		data, _ := v.(map[string]any)
		allow := config.ObjectAt(data, "permissions")["allow"]
		list, _ := allow.([]any)
		for _, e := range list {
			if e == permissionRule {
				sources = append(sources, abs(path))
				break
			}
		}
	}
	return Permission{
		Rule:    permissionRule,
		Found:   len(sources) > 0,
		Sources: sources,
		BestEffortNote: "best-effort scan of ~/.claude/settings.json and " +
			"./.claude/settings.json(.local); does not see enterprise " +
			"policy or CLI --allowedTools",
	}
}

// Compute builds the readiness report for a resolved config.
func Compute(cfg *config.Resolved) Report {
	r := Report{
		Shim:       FindShim(),
		Permission: ScanPermission(),
		Executors:  map[string]any{},
		Ready:      map[string]ExecReady{},
	}
	external := cfg.Section("external_executors")
	nameSet := map[string]bool{}
	for _, k := range slices.Sorted(maps.Keys(external)) {
		nameSet[k] = true
	}
	for k := range KnownExecutors {
		nameSet[k] = true
	}
	names := make([]string, 0, len(nameSet))
	for k := range nameSet {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, name := range names {
		ecfg := config.ObjectAt(external, name)
		defDispatch, known := KnownExecutors[name]
		enabled := false
		if v, _ := ecfg["enabled"]; v == true {
			enabled = true
		}
		var dispatch any
		if v, ok := ecfg["dispatch"]; ok && v != nil {
			dispatch = v
		} else if known {
			dispatch = defDispatch
		}
		binary := name
		if b, ok := tools.KnownBinaries[name]; ok {
			binary = b
		}
		tool := tools.Resolve(name, cfg)
		available := tool.Path != ""

		entry := map[string]any{
			"enabled":      enabled,
			"dispatch":     dispatch,
			"binary":       binary,
			"available":    available,
			"path":         nilIfEmpty(tool.Path),
			"source":       nilIfEmpty(tool.Source),
			"tool_version": nilIfEmpty(tool.Version),
		}
		if tool.Hint != "" {
			entry["hint"] = tool.Hint
		}
		if dispatch == "agent" {
			entry["note"] = "dispatch: agent — CLI binary presence is informational; " +
				"actual availability is the subagent's session " +
				"resolution, which agent-exec cannot determine"
		}
		r.Executors[name] = entry

		if !known {
			entry["note"] = "unknown executor: ignored by route"
		} else if enabled && dispatch == "cli" {
			missing := []string{}
			if !r.Shim.Installed {
				missing = append(missing, "shim-not-installed")
			}
			if !r.Shim.OnPath {
				missing = append(missing, "agent-exec-not-on-path")
			}
			if !r.Permission.Found {
				missing = append(missing, "permission-rule-absent")
			}
			if !available {
				missing = append(missing, "executor-binary-unavailable")
			}
			ok := available && r.Permission.Found && r.Shim.Installed && r.Shim.OnPath
			r.Ready[name] = ExecReady{OK: ok, Missing: missing}
		}
	}
	return r
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
