package runner

import (
	"os"
	"path/filepath"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// MarkOptions is one `wave mark`. The CLI has checked the combination:
// exactly one of ClearEnvironment, Await, Recheck, Widen or Status applies
// (in that order). Widen/Recheck nil = not given.
type MarkOptions struct {
	State, Pkg, Status, Detail string
	Widen, Recheck             []string
	ClearEnvironment           bool
	Await                      string
}

// Mark hands packages back to the runner. The result is the JSON object
// to print; on failure (exit != 0) it holds only "error", the message.
func Mark(o MarkOptions) (map[string]any, int) {
	fail := func(code int, msg string) (map[string]any, int) {
		return map[string]any{"error": msg}, code
	}
	if !isFile(o.State) {
		return fail(3, "no state file at "+o.State)
	}
	store := &state.Store{Path: o.State, Clock: wallClock}
	if o.ClearEnvironment {
		_ = store.ClearNeed("")
		_ = store.ClearStopped()
		return map[string]any{"environment_cleared": true}, 0
	}
	st, err := store.Load()
	if err != nil {
		return fail(3, err.Error())
	}
	if o.Await != "" {
		if code, msg := approvals.AwaitPackage(o.State, o.Await); code != 0 {
			return fail(code, msg)
		}
		return map[string]any{"pkg": o.Await, "status": "implementing", "detail": "await-orca"}, 0
	}
	if o.Recheck != nil {
		for _, id := range o.Recheck {
			if st.Packages[id] == nil {
				return fail(2, "unknown package: "+id)
			}
		}
		for _, id := range o.Recheck {
			if p := st.Packages[id]; p.Tree == nil || *p.Tree == "" || !isDir(*p.Tree) {
				return fail(2, "package has no tree: "+id)
			}
		}
		detail := o.Detail
		if detail == "" {
			detail = "marked for recheck"
		}
		for _, id := range o.Recheck {
			_ = store.ClearNeed(id)
			_ = store.SetStatus(id, "verifying", detail, nil)
		}
		return map[string]any{"pkg": o.Recheck, "status": "verifying"}, 0
	}
	if o.Pkg == "" || st.Packages[o.Pkg] == nil {
		pkg := o.Pkg
		if pkg == "" {
			pkg = "None"
		}
		return fail(2, "unknown package: "+pkg)
	}
	if o.Widen != nil {
		if err := widen(o.State, o.Pkg, o.Widen); err != nil {
			return fail(1, err.Error())
		}
		detail := o.Detail
		if detail == "" {
			detail = "widened ownership"
		}
		_ = store.ClearNeed(o.Pkg)
		_ = store.SetStatus(o.Pkg, "pending", detail, nil)
		return map[string]any{"pkg": o.Pkg, "status": "pending", "widen": o.Widen}, 0
	}
	stored := o.Status
	if stored == "ready" {
		stored = "verifying"
	}
	detail := o.Detail
	if detail == "" {
		detail = "marked by the instructor"
	}
	_ = store.SetStatus(o.Pkg, stored, detail, nil)
	_ = store.ClearNeed(o.Pkg)
	return map[string]any{"pkg": o.Pkg, "status": stored}, 0
}

// widen adds globs to the package's files_owned_add in plan-overrides.json
// (the runner applies them when it loads the plan).
func widen(statePath, pkg string, globs []string) error {
	path := filepath.Join(filepath.Dir(absPath(statePath)), "plan-overrides.json")
	overrides := jsonx.NewObject()
	if raw, err := os.ReadFile(path); err == nil {
		if doc, err := jsonx.Decode(raw); err == nil {
			if obj, ok := doc.(*jsonx.Object); ok {
				overrides = obj
			}
		}
	}
	entry, ok := objGet(overrides, pkg).(*jsonx.Object)
	if !ok {
		entry = jsonx.NewObject()
		overrides.Set(pkg, entry)
	}
	owned := asList(objGet(entry, "files_owned_add"))
	if _, present := entry.Get("files_owned_add"); !present {
		owned = []any{}
	}
	for _, g := range globs {
		if !containsAnyStr(owned, g) {
			owned = append(owned, g)
		}
	}
	entry.Set("files_owned_add", owned)
	b, err := jsonx.MarshalIndent(overrides)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func containsAnyStr(list []any, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// configOrDefaults is the current config, or the defaults when it cannot
// be resolved.
func (r *runner) configOrDefaults() *config.Resolved {
	if cfg := r.config(); cfg != nil {
		return cfg
	}
	return &config.Resolved{Values: config.Defaults()}
}
