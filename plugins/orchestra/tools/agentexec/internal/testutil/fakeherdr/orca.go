package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Orca prompt scenario keys (per package, in "prompts"): "result", "after_waits",
// "files", "dialog" (a choice dialog stays on screen), and "login" (the TUI
// shows Claude Code's "Not logged in" reply on every read).

// orcaTrust is Claude Code's real folder-trust dialog: unnumbered options,
// "No, exit" preselected.
var orcaTrust = []string{
	" Accessing workspace:",
	" Quick safety check: Is this a project you created or one you trust? (Like your own code)",
	" ❯ No, exit",
	"   Yes, I trust this folder",
	" Enter to confirm · Esc to cancel",
}

var orcaReady = []string{"Claude Code", "", "❯ "}

var writeRE = regexp.MustCompile(`write (\S+) containing JSON`)

func (c *call) orcaOut(result map[string]any) int {
	if result == nil {
		result = map[string]any{}
	}
	c.emit(map[string]any{"ok": true, "result": result})
	return 0
}

func (c *call) orcaFail(msg string) int {
	c.emit(map[string]any{"ok": false, "error": map[string]any{"message": msg}})
	return 1
}

func git(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (c *call) orca(root string) int {
	args := c.args
	verb := strings.Join(args[:min(2, len(args))], " ")
	st, sc := c.state, c.scenario
	worktrees, _ := st["worktrees"].(map[string]any)
	terminals, _ := st["terminals"].(map[string]any)
	if len(args) > 0 && args[0] == "status" {
		if ok, set := sc["status_ok"].(bool); !set || ok {
			return c.orcaOut(map[string]any{"runtime": "up"})
		}
		return c.orcaFail("runtime not running")
	}
	switch verb {
	case "worktree create":
		repoArg, _ := opt(args, "--repo")
		repo := strings.TrimPrefix(repoArg, "path:")
		name, _ := opt(args, "--name")
		base, _ := opt(args, "--base-branch")
		path := filepath.Join(root, "workspaces", filepath.Base(repo), name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if out, err := git(repo, "worktree", "add", "-q", "-b", name, path, base); err != nil {
			return c.orcaFail(out)
		}
		worktrees[path] = map[string]any{"repo": repo, "branch": name}
		return c.orcaOut(map[string]any{"worktree": map[string]any{"id": "wt-" + name, "path": path, "branch": name}})
	case "worktree rm":
		wtArg, _ := opt(args, "--worktree")
		path := strings.TrimPrefix(wtArg, "path:")
		info, ok := worktrees[path].(map[string]any)
		if !ok {
			return c.orcaFail("unknown worktree")
		}
		delete(worktrees, path)
		_, _ = git(str(info["repo"]), "worktree", "remove", "--force", path)
		_, _ = git(str(info["repo"]), "branch", "-D", str(info["branch"]))
		return c.orcaOut(map[string]any{"removed": true})
	case "terminal create":
		wtArg, _ := opt(args, "--worktree")
		path := strings.TrimPrefix(wtArg, "path:")
		if _, ok := worktrees[path]; !ok {
			return c.orcaFail("Timed out waiting for terminal handle")
		}
		n, _ := st["next"].(float64)
		n++
		st["next"] = n
		handle := fmt.Sprintf("term-%d", int(n))
		command, _ := opt(args, "--command")
		base := filepath.Base(path)
		terminals[handle] = map[string]any{
			"worktree": path, "command": command, "closed": false,
			"trusted": !truthy(sc["trust"]), "pending": nil,
			"pkg": base[strings.LastIndex(base, "-")+1:],
		}
		return c.orcaOut(map[string]any{"terminal": map[string]any{"handle": handle}})
	}
	handle, _ := opt(args, "--terminal")
	term, ok := terminals[handle].(map[string]any)
	if !ok || truthy(term["closed"]) {
		return c.orcaFail("no such terminal: " + handle)
	}
	switch verb {
	case "terminal close":
		term["closed"] = true
		return c.orcaOut(map[string]any{"closed": true})
	case "terminal read":
		var screen []string
		pending, _ := term["pending"].(map[string]any)
		switch {
		case truthy(term["exited"]):
			screen = []string{"user@host ~/wt (branch) [1]>"}
		case !truthy(term["trusted"]):
			yes := term["selection"] == 1.0
			for _, line := range orcaTrust {
				if yes {
					line = strings.Replace(strings.Replace(line, "❯ No", "  No", 1), "  Yes", "❯ Yes", 1)
				}
				screen = append(screen, line)
			}
		case truthy(sc["never_ready"]):
			screen = []string{"Starting Claude Code..."}
		case pending != nil && truthy(pending["dialog"]):
			screen = []string{"Do you want to make this edit to a.txt?", "❯ 1. Yes", "  2. No"}
		case pending != nil && truthy(pending["login"]):
			screen = []string{"❯ implement", "  ⎿  Not logged in · Please run /login", "✻ Worked for 0s"}
		default:
			screen = orcaReady
		}
		return c.orcaOut(map[string]any{"terminal": map[string]any{"tail": screen}})
	case "terminal wait":
		w, _ := st["waits"].(float64)
		st["waits"] = w + 1
		if pending, _ := term["pending"].(map[string]any); pending != nil && pending["result"] != nil {
			pw, _ := pending["waits"].(float64)
			pending["waits"] = pw + 1
			after, _ := pending["after_waits"].(float64)
			if pw+1 >= after {
				b, _ := json.Marshal(pending["result"])
				writeFile(str(pending["result_path"]), string(b))
				term["pending"] = nil
			}
		}
		return c.orcaOut(map[string]any{"wait": map[string]any{"satisfied": true}})
	case "terminal send":
		text, _ := opt(args, "--text")
		if !truthy(term["trusted"]) && !truthy(term["exited"]) {
			// Mirror the real TUI: an Enter in the same send as the arrow is
			// handled before the cursor moves, so it confirms "No, exit".
			if has(args, "--enter") {
				if strings.HasPrefix(text, "\x1b[B") || term["selection"] != 1.0 {
					term["exited"] = true
				} else {
					term["trusted"] = true
				}
			} else if strings.HasPrefix(text, "\x1b[B") {
				term["selection"] = 1.0
			}
			return c.orcaOut(map[string]any{"send": map[string]any{}})
		}
		m := writeRE.FindStringSubmatch(text)
		if m == nil {
			return c.orcaOut(map[string]any{"send": map[string]any{}})
		}
		if retry, ok := opt(args, "--retry-request"); ok {
			return c.orcaOut(map[string]any{"send": map[string]any{"prompt": map[string]any{"requestId": retry}}})
		}
		index := c.promptIndex(str(term["pkg"]))
		step := map[string]any{}
		if steps, _ := sc["prompts"].(map[string]any)[str(term["pkg"])].([]any); index < len(steps) {
			if s, ok := steps[index].(map[string]any); ok {
				for k, v := range s {
					step[k] = v
				}
			}
		}
		if files, ok := step["files"].(map[string]any); ok {
			for rel, body := range files {
				writeFile(filepath.Join(str(term["worktree"]), rel), str(body))
			}
		}
		step["result_path"] = m[1]
		step["waits"] = 0.0
		term["pending"] = step
		after, _ := step["after_waits"].(float64)
		if step["result"] != nil && after == 0 {
			b, _ := json.Marshal(step["result"])
			writeFile(m[1], string(b))
			term["pending"] = nil
		}
		return c.orcaOut(map[string]any{"send": map[string]any{"prompt": map[string]any{"requestId": uuid4()}}})
	}
	return c.orcaFail("unsupported: " + strings.Join(args, " "))
}

func (c *call) promptIndex(pkg string) int {
	idx, _ := c.state["prompt_index"].(map[string]any)
	n, _ := idx[pkg].(float64)
	idx[pkg] = n + 1
	return int(n)
}
