package approvals

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// DecisionError is a decision that was not applied. Code is the CLI exit
// status: 1 refused by the sandbox rules, 2 nothing to decide or the
// dialog could not be answered.
type DecisionError struct {
	Code int
	Msg  string
}

func (e *DecisionError) Error() string { return e.Msg }

// DialogHost answers a permission dialog in a herdr terminal.
type DialogHost interface {
	// Session is the live session of pkg (its terminal "" when the agent
	// is gone).
	Session(pkg, stateDir string) (*host.Session, bool)
	// SendKeys sends key to terminal in the named herdr session ("" =
	// the host's own).
	SendKeys(terminal, key, sessionName string) error
}

// Env is what Decide needs besides its arguments.
type Env struct {
	Cfg *config.Resolved
	// Dialog builds the herdr host for dialog items; nil uses herdr.New.
	Dialog func() DialogHost
}

type herdrDialog struct{ e *herdr.Executor }

func (h herdrDialog) Session(pkg, stateDir string) (*host.Session, bool) {
	return h.e.Session(pkg, stateDir)
}

func (h herdrDialog) SendKeys(terminal, key, sname string) error {
	if sname != "" {
		h.e.SetSessionName(sname)
	}
	_, err := h.e.Call("agent", "send-keys", terminal, key)
	return err
}

// Decide applies one decision under the resolved config. The result is the
// audit record plus "resumed" (and "error"); a decision that was not
// applied also returns a *DecisionError.
func Decide(statePath, id, decision, reason, by string) (map[string]any, error) {
	return DecideIn(Env{Cfg: resolveConfig()}, statePath, id, decision, reason, by)
}

// DecideIn is Decide with an explicit environment.
func DecideIn(env Env, statePath, id, decision, reason, by string) (map[string]any, error) {
	if env.Cfg == nil {
		env.Cfg = resolveConfig()
	}
	items, err := PendingIn(statePath, env.Cfg)
	if err != nil {
		return nil, err
	}
	var item *Item
	for i := range items {
		if items[i].ID == id {
			item = &items[i]
			break
		}
	}
	if item == nil {
		msg := "no pending approval " + id
		return map[string]any{"item": id, "applied": false, "error": msg}, &DecisionError{Code: 2, Msg: msg}
	}
	var (
		subject  any
		packages []string
		resumed  []string
		failure  *DecisionError
	)
	switch item.Kind {
	case "net":
		resumed, failure = decideNet(env.Cfg, statePath, item, decision, reason, by)
		subject = item.Host
		for _, p := range item.Packages {
			packages = append(packages, p.ID)
		}
	case "path":
		resumed, failure = decidePath(env.Cfg, statePath, item, decision, reason, by)
		subject = item.GrantCandidate
		if !truthy(subject) {
			subject = item.Path
		}
		packages = []string{item.Pkg}
	default:
		resumed, failure = decideDialog(env, statePath, item, decision)
		subject = item.Dialog
		packages = []string{item.Pkg}
	}
	if resumed == nil {
		resumed = []string{}
	}
	rec := map[string]any{
		"ts": fsutil.NowISOZ(), "state": absOf(statePath), "item": id, "kind": item.Kind,
		"subject": subject, "decision": decision, "by": by, "reason": reason,
		"packages": packages, "applied": failure == nil,
	}
	if err := audit(rec); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(rec)+2)
	for k, v := range rec {
		out[k] = v
	}
	out["resumed"] = resumed
	if failure != nil {
		out["error"] = failure.Msg
		return out, failure
	}
	return out, nil
}

func appendNote(stateDir, pid, line string) error {
	path := NotePath(stateDir, pid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if os.IsNotExist(statErr) {
		if _, err := fmt.Fprintf(f, "# Approval decisions for %s\n\n", pid); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(f, "- %s\n", line)
	return err
}

func resume(store *state.Store, pid, detail string) error {
	if err := store.ClearNeed(pid); err != nil {
		return err
	}
	return store.SetStatus(pid, "pending", detail, nil)
}

// networkNeedHosts is the hosts of pid's `network` need (nil: no such need).
func networkNeedHosts(st *state.State, pid string) []string {
	for _, n := range st.Needs {
		if n.ID == nil || *n.ID != pid || n.Kind != "network" {
			continue
		}
		data := needJSON(n)
		list, _ := data["hosts"].([]any)
		hosts := []string{}
		for _, h := range list {
			hosts = append(hosts, strings.ToLower(strings.TrimSpace(pyStr(h))))
		}
		return hosts
	}
	return nil
}

func decideNet(cfg *config.Resolved, statePath string, it *Item, decision, reason, by string) ([]string, *DecisionError) {
	h := it.Host
	stateDir := stateDirOf(statePath)
	if decision == "allow" {
		if _, err := sandbox.AddDomainEntry(h, by, reason); err != nil {
			return nil, &DecisionError{Code: 1, Msg: "refused: " + err.Error()}
		}
	}
	ap := loadApprovals(statePath)
	key := "declined"
	if decision == "allow" {
		key = "allowed"
	}
	ap[key].(map[string]any)[h] = map[string]any{"by": by, "reason": reason}
	if err := saveApprovals(statePath, ap); err != nil {
		return nil, &DecisionError{Code: 1, Msg: err.Error()}
	}
	store, err := state.Open(statePath)
	if err != nil {
		return nil, &DecisionError{Code: 1, Msg: err.Error()}
	}
	st := store.Snapshot()
	stateOf := hostState(sandbox.ConfigOf(cfg).Network, ap)
	var resumed []string
	for _, p := range it.Packages {
		hosts := networkNeedHosts(&st, p.ID)
		if hosts == nil || !contains(hosts, h) {
			continue // a live session already got the domain pushed
		}
		note := fmt.Sprintf("network access to %s was declined: %s. Finish without it or ESCALATE.", h, reason)
		if decision == "allow" {
			note = fmt.Sprintf("network access to %s is now allowed (%s). Continue the task.", h, reason)
		}
		_ = appendNote(stateDir, p.ID, note)
		decidedAll := true
		for _, other := range hosts {
			decidedAll = decidedAll && stateOf(other) != ""
		}
		if decidedAll {
			verb := "declined"
			if decision == "allow" {
				verb = "allowed"
			}
			_ = resume(store, p.ID, fmt.Sprintf("network %s %s", h, verb))
			resumed = append(resumed, p.ID)
		}
	}
	return resumed, nil
}

func decidePath(cfg *config.Resolved, statePath string, it *Item, decision, reason, by string) ([]string, *DecisionError) {
	pid := it.Pkg
	stateDir := stateDirOf(statePath)
	target := it.GrantCandidate
	if !truthy(target) {
		target = it.Path
	}
	if decision == "allow" {
		text, _ := target.(string)
		if !truthy(target) {
			return nil, &DecisionError{Code: 1, Msg: "refused: the denial names no path"}
		}
		if text == "" {
			text = pyStr(target)
		}
		real := sandbox.RealPath(fsutil.ExpandUser(text))
		if r := sandbox.UserGrantRefusal(real, sandbox.DenyRead(cfg)); r != "" {
			return nil, &DecisionError{Code: 1, Msg: "refused: " + r}
		}
		if _, err := sandbox.AddGrantEntry(real, by, "", reason); err != nil {
			return nil, &DecisionError{Code: 1, Msg: "refused: " + err.Error()}
		}
		_ = appendNote(stateDir, pid, fmt.Sprintf("%s is now granted (%s). Continue the task.", real, reason))
	} else {
		_ = appendNote(stateDir, pid, fmt.Sprintf("%s is off-limits: %s", pyStr(it.Path), reason))
	}
	verb := "declined"
	if decision == "allow" {
		verb = "granted"
	}
	store, err := state.Open(statePath)
	if err != nil {
		return nil, &DecisionError{Code: 1, Msg: err.Error()}
	}
	_ = resume(store, pid, fmt.Sprintf("permission %s %s", pyStr(target), verb))
	return []string{pid}, nil
}

func decideDialog(env Env, statePath string, it *Item, decision string) ([]string, *DecisionError) {
	pid := it.Pkg
	terminal := "unknown"
	if t, ok := it.Terminal.(string); ok && t != "" {
		terminal = t
	}
	how := fmt.Sprintf("approve it in %s (terminal %s), then run: agent-exec wave mark --state %s --await %s",
		capitalize(it.HostName), terminal, statePath, pid)
	if it.HostName != "herdr" {
		return nil, &DecisionError{Code: 2, Msg: fmt.Sprintf("no key injection into %s here; %s", capitalize(it.HostName), how)}
	}
	var h DialogHost
	if env.Dialog != nil {
		h = env.Dialog()
	} else {
		h = herdrDialog{herdr.New(env.Cfg)}
	}
	stateDir := stateDirOf(statePath)
	s, ok := h.Session(pid, stateDir)
	if !ok || s == nil || s.Terminal == "" {
		return nil, &DecisionError{Code: 2, Msg: fmt.Sprintf("the herdr terminal of %s is gone; %s", pid, how)}
	}
	key := "escape"
	if decision == "allow" {
		key = "enter"
	}
	if err := h.SendKeys(s.Terminal, key, s.Str("herdr_session")); err != nil {
		return nil, &DecisionError{Code: 2, Msg: fmt.Sprintf("could not send keys to %s (%s); %s", s.Terminal, err, how)}
	}
	if code, msg := AwaitPackage(statePath, pid); code != 0 {
		return nil, &DecisionError{Code: code, Msg: fmt.Sprintf("sent %s to %s but %s", key, s.Terminal, msg)}
	}
	return []string{pid}, nil
}

// capitalize is Python's str.capitalize for the host names.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}
