package runner

import (
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

// loginNeed records a hosted worker whose Claude login expired or is missing
// as a `login` need. Its session is closed with the tree kept, so the retry
// starts a fresh claude that reads the credentials after /login. It reports
// false for any other result.
func (r *runner) loginNeed(pid string, result map[string]any) bool {
	if str(result["status"]) != "_orca_need" || str(result["kind"]) != host.KindLogin {
		return false
	}
	if s := r.hostSession(pid); s != nil {
		_ = r.host.Close(s, false)
	}
	r.need(pid, "login", str(result["detail"])+"\nThen hand the package back: agent-exec wave mark --state "+
		r.statePath+" --pkg "+pid+" --status pending")
	return true
}

// loginHeld reports whether a `login` need is open, and why new starts wait.
// It reads the state file, so a `wave mark` from another process (which
// clears the package's needs) releases the hold at the next scheduling round.
func (r *runner) loginHeld() (bool, string) {
	if r.store == nil {
		return false, ""
	}
	for _, n := range r.store.Snapshot().Needs {
		if n.Kind == "login" && n.ID != nil && *n.ID != "" {
			id := *n.ID
			return true, "Claude login expired or missing (package " + id + "): run /login, then agent-exec wave mark --state " +
				r.statePath + " --pkg " + id + " --status pending"
		}
	}
	return false, ""
}
