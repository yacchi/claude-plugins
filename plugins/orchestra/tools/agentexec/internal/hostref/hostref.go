// Package hostref closes a hosted Claude session (herdr workspace, Orca
// terminal) from what the host recorded about it alone, so a worktree
// removal that knows nothing about hosts (isolate) can close the session
// living in the tree before the tree goes. It only shells out to the
// recorded host binary; it never imports a host package.
package hostref

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Hosts of a Ref.
const (
	Herdr = "herdr"
	Orca  = "orca"
)

// sessionPrefix is the herdr session orchestra names its own; any other
// session (the user's own herdr) is never touched.
const sessionPrefix = "orchestra-"

// Ref is the hosted session a host started in a tree. herdr: Session,
// Home (its XDG_CONFIG_HOME), Workspace and Agent; Orca: Terminal. Binary
// is the host CLI the session was started with.
type Ref struct {
	Host      string `json:"host"`
	Binary    string `json:"binary"`
	Session   string `json:"session,omitempty"`
	Home      string `json:"home,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Terminal  string `json:"terminal,omitempty"`
}

// Close closes ref's session within timeout (all calls together) and, for
// herdr, stops the session's server once no workspace is left in it. It
// returns "" when the session is closed or already gone, else why it could
// not be closed; it never fails otherwise.
func Close(ref Ref, timeout time.Duration) string {
	if ref.Binary == "" {
		return "no " + ref.Host + " binary recorded for the hosted session; left open"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch ref.Host {
	case Herdr:
		return closeHerdr(ctx, ref)
	case Orca:
		return closeOrca(ctx, ref)
	}
	return "unknown host " + ref.Host + " recorded for the hosted session; left open"
}

func closeHerdr(ctx context.Context, ref Ref) string {
	if !strings.HasPrefix(ref.Session, sessionPrefix) || ref.Home == "" || ref.Workspace == "" {
		return fmt.Sprintf("herdr session %q is not an orchestra session with a recorded home and workspace; left open",
			ref.Session)
	}
	env := herdrEnv(ref.Home)
	call := func(args ...string) (string, error) {
		return run(ctx, env, ref.Binary, append([]string{"--session", ref.Session}, args...)...)
	}
	_, closeErr := call("workspace", "close", ref.Workspace)
	open, listErr := workspaces(call)
	switch {
	case closeErr != nil && listErr != nil:
		// A server that is gone took the workspace with it.
		if strings.Contains(closeErr.Error(), "not running") {
			return ""
		}
		return fmt.Sprintf("could not close herdr workspace %s of %s: %s", ref.Workspace, ref.Session, closeErr)
	case closeErr != nil && contains(open, ref.Workspace):
		return fmt.Sprintf("could not close herdr workspace %s of %s: %s", ref.Workspace, ref.Session, closeErr)
	case listErr != nil:
		return "" // closed; whether the server is idle is unknown, so it keeps running
	}
	if len(open) == 0 {
		if _, err := call("server", "stop"); err != nil {
			return fmt.Sprintf("closed herdr workspace %s, but could not stop the idle server of %s: %s",
				ref.Workspace, ref.Session, err)
		}
	}
	return ""
}

// workspaces is the workspace ids `workspace list` reports.
func workspaces(call func(args ...string) (string, error)) ([]string, error) {
	out, err := call("workspace", "list")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Result struct {
			Workspaces []struct {
				ID string `json:"workspace_id"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil || payload.Result.Workspaces == nil {
		return nil, errors.New("unreadable herdr workspace list")
	}
	ids := make([]string, 0, len(payload.Result.Workspaces))
	for _, w := range payload.Result.Workspaces {
		ids = append(ids, w.ID)
	}
	return ids, nil
}

func closeOrca(ctx context.Context, ref Ref) string {
	if ref.Terminal == "" {
		return "no Orca terminal recorded for the hosted session; left open"
	}
	out, err := run(ctx, os.Environ(), ref.Binary, "terminal", "close", "--terminal", ref.Terminal, "--tab", "--json")
	if err == nil {
		var payload struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal([]byte(out), &payload) != nil || !payload.OK {
			err = errors.New("orca did not report ok")
		}
	}
	if err != nil {
		return fmt.Sprintf("could not close Orca terminal %s: %s", ref.Terminal, err)
	}
	return ""
}

// herdrEnv is the process env pointed at orchestra's herdr home, with
// nothing of the user's own herdr.
func herdrEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "XDG_CONFIG_HOME" || strings.HasPrefix(k, "HERDR_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "XDG_CONFIG_HOME="+home)
}

// run is one host call: its stdout, or an error carrying its stderr.
func run(ctx context.Context, env []string, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", errors.New("timed out")
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return out.String(), nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
