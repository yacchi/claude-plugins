package approvals

import (
	"encoding/json"
	"os"
	"strconv"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/orca"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
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

// --- Python value helpers ------------------------------------------------------

// asMap is a decoded JSON object as a map (state files decode objects as
// *jsonx.Object).
func asMap(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case *jsonx.Object:
		if x == nil {
			return nil
		}
		m := make(map[string]any, x.Len())
		for _, k := range x.Keys() {
			m[k], _ = x.Get(k)
		}
		return m
	}
	return nil
}

// pyStr is Python's str() of a decoded JSON value.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return jsonx.FormatFloat(x)
	case json.Number:
		return x.String()
	}
	b, _ := jsonx.Marshal(v)
	return string(b)
}

// truthy is Python's truthiness of a decoded JSON value.
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

// pyStrOr is str(v or "").
func pyStrOr(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
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

// pyEqual is == between decoded JSON scalars (443 == 443.0).
func pyEqual(a, b any) bool {
	if x, ok := number(a); ok {
		if y, ok := number(b); ok {
			return x == y
		}
		return false
	}
	return a == b
}

func containsAny(list []any, v any) bool {
	for _, e := range list {
		if pyEqual(e, v) {
			return true
		}
	}
	return false
}

// typeKey is str(type(v)) of the Python value v decodes to.
func typeKey(v any) string {
	switch v.(type) {
	case nil:
		return "<class 'NoneType'>"
	case bool:
		return "<class 'bool'>"
	case string:
		return "<class 'str'>"
	case int, int64:
		return "<class 'int'>"
	case float64:
		return "<class 'float'>"
	}
	return "<class 'object'>"
}

// pyLess orders ports like sorted(key=lambda p: (str(type(p)), p)).
func pyLess(a, b any) bool {
	ta, tb := typeKey(a), typeKey(b)
	if ta != tb {
		return ta < tb
	}
	if x, ok := number(a); ok {
		y, _ := number(b)
		return x < y
	}
	if x, ok := a.(string); ok {
		y, _ := b.(string)
		return x < y
	}
	return false
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
