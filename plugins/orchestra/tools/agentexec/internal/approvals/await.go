package approvals

import (
	"encoding/json"
	"os"
	"strconv"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/orca"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// AwaitPackage is `wave mark --await`: a package whose interactive session
// stopped on a dialog (or timed out) goes back to the runner, which then
// waits for that same session's result. The session is looked up in the
// herdr registry, then Orca's, without asking the host. It returns 0, or 2
// and why it cannot.
func AwaitPackage(statePath, pkg string) (int, string) {
	store, err := state.Open(statePath)
	if err != nil {
		return 2, err.Error()
	}
	st := store.Snapshot()
	entry := st.Packages[pkg]
	var need *state.Need
	for i := range st.Needs {
		if st.Needs[i].ID != nil && *st.Needs[i].ID == pkg {
			need = &st.Needs[i]
			break
		}
	}
	isHostNeed := false
	if need != nil {
		switch need.Kind {
		case "stalled", "timeout":
			isHostNeed = true
		case "delegate":
			var detail map[string]any
			if json.Unmarshal([]byte(need.Detail), &detail) == nil {
				isHostNeed = detail["orca"] == "stalled" || detail["orca"] == "timeout"
			}
		}
	}
	if entry == nil || entry.Status != "needs" || !isHostNeed {
		return 2, "--await requires an Orca stalled/timeout need"
	}
	stateDir := stateDirOf(statePath)
	var s *host.Session
	for _, reg := range []struct{ file, name string }{
		{herdr.RegistryName, "herdr"}, {orca.RegistryName, "orca"},
	} {
		s, _ = host.ReadSession(stateDir, reg.file, reg.name, pkg)
		if s != nil && s.Terminal != "" {
			break
		}
	}
	if s == nil || s.Terminal == "" || s.Worktree == "" || !isDir(s.Worktree) {
		return 2, "no live Orca session for " + pkg
	}
	if err := store.ClearNeed(pkg); err != nil {
		return 2, err.Error()
	}
	if err := store.SetStatus(pkg, "implementing", "await-orca", nil); err != nil {
		return 2, err.Error()
	}
	return 0, ""
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// --- decoded JSON value helpers ------------------------------------------------

// asMap is a decoded JSON object as a map (nil when v is not an object).
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// valText renders a decoded JSON value for text output; "-" for null.
func valText(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	}
	b, _ := fsutil.MarshalJSON(v)
	return string(b)
}

// truthy reports whether a decoded JSON value is set: non-null, non-zero,
// non-empty.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// textOrEmpty is valText, but "" for an unset value.
func textOrEmpty(v any) string {
	if !truthy(v) {
		return ""
	}
	return valText(v)
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// samePort reports whether two denial ports are the same: numbers compare by
// value (443 == 443.0), anything else by its text.
func samePort(a, b any) bool {
	x, aNum := number(a)
	y, bNum := number(b)
	if aNum || bNum {
		return aNum && bNum && x == y
	}
	return valText(a) == valText(b)
}

func containsPort(list []any, v any) bool {
	for _, e := range list {
		if samePort(e, v) {
			return true
		}
	}
	return false
}

// portLess sorts numeric ports first (by value), then any others by text.
func portLess(a, b any) bool {
	x, aNum := number(a)
	y, bNum := number(b)
	if aNum != bNum {
		return aNum
	}
	if aNum {
		return x < y
	}
	return valText(a) < valText(b)
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
