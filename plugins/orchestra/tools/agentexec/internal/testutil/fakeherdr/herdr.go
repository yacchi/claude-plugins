package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	warmPromptRE = regexp.MustCompile(`(?s)Read each of these files .* write (\S+) containing`)
	resultPathRE = regexp.MustCompile(`write (\S+) containing JSON`)
)

func initHerdr(sc map[string]any) map[string]any {
	statuses := strList(sc["statuses"])
	if len(statuses) == 0 {
		statuses = []any{"idle"}
	}
	stalls, _ := sc["stalls"].(float64)
	return map[string]any{
		"running": truthy(sc["running"]), "not_ready": truthy(sc["not_ready"]),
		"screen": str(sc["screen"]), "screens": strList(sc["screens"]), "statuses": statuses, "stalls": stalls,
		"after_keys": strList(sc["after_keys"]), "keys_sent": false, "workspaces": 0.0,
	}
}

func (c *call) herdrOK(payload any) int {
	c.emit(payload)
	return 0
}

// herdrErr is herdr's failure shape: the error object on stderr, stdout
// empty.
func (c *call) herdrErr(code string, rc int) int {
	return c.herdrErrMsg(code, code, rc)
}

// herdrErrMsg is herdrErr with a message of its own.
func (c *call) herdrErrMsg(code, msg string, rc int) int {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg}})
	c.errw.Write(append(b, '\n'))
	return rc
}

func (c *call) herdr() int {
	args := c.args
	// --session S
	if len(args) >= 2 && args[0] == "--session" {
		args = args[2:]
	}
	verb := strings.Join(args[:min(2, len(args))], " ")
	st := c.state
	switch {
	case len(args) > 0 && args[0] == "--version":
		c.out.WriteString("herdr 0.0.0-fake\n")
		return 0
	case len(args) == 1 && args[0] == "server":
		st["running"] = true
		return 0
	case len(args) > 0 && args[0] == "status":
		return c.herdrOK(map[string]any{"result": map[string]any{"server": map[string]any{"running": st["running"]}}})
	case verb == "server stop":
		st["running"] = false
		return c.herdrOK(map[string]any{})
	case verb == "workspace create":
		n, _ := st["workspaces"].(float64)
		n++
		st["workspaces"] = n
		ws := "w" + itoa(int(n))
		return c.herdrOK(map[string]any{"result": map[string]any{
			"workspace": map[string]any{"workspace_id": ws},
			"root_pane": map[string]any{"pane_id": ws + ":p1"}}})
	case verb == "workspace close", verb == "agent wait":
		return c.herdrOK(map[string]any{})
	case verb == "agent start":
		if truthy(c.scenario["fail_warm"]) && len(args) > 2 && strings.HasPrefix(args[2], "p-warm") {
			return c.herdrErr("boom", 1)
		}
		if truthy(c.scenario["start_timeout"]) {
			return c.herdrErrMsg("timeout", "timed out waiting for agent startup", 1)
		}
		if truthy(st["not_ready"]) {
			return c.herdrErr("agent_not_ready", 1)
		}
		return c.herdrOK(map[string]any{})
	case verb == "agent read", verb == "pane read":
		// "screens" is a per-read sequence; its last entry sticks.
		if screens := strList(st["screens"]); len(screens) > 0 {
			st["screen"] = screens[0]
			if len(screens) > 1 {
				st["screens"] = screens[1:]
			}
		}
		keys := strList(st["after_keys"])
		if truthy(st["keys_sent"]) && len(keys) > 0 {
			st["screen"] = keys[0]
			if len(keys) > 1 {
				st["after_keys"] = keys[1:]
			}
		}
		return c.herdrOK(map[string]any{"result": map[string]any{"text": st["screen"]}})
	case verb == "agent send-keys":
		st["keys_sent"] = true
		st["not_ready"] = false
		return c.herdrOK(map[string]any{})
	case verb == "agent get":
		if code := str(c.scenario["get_error"]); code != "" {
			return c.herdrErr(code, 1)
		}
		statuses := strList(st["statuses"])
		status := statuses[0]
		if len(statuses) > 1 {
			st["statuses"] = statuses[1:]
		}
		return c.herdrOK(map[string]any{"result": map[string]any{"agent": map[string]any{"agent_status": status}}})
	case verb == "agent prompt":
		if stalls, _ := st["stalls"].(float64); stalls > 0 {
			st["stalls"] = stalls - 1
			return c.herdrErr("agent_prompt_stalled", 1)
		}
		line := ""
		if len(args) > 3 {
			line = args[3]
		}
		if m := warmPromptRE.FindStringSubmatch(line); m != nil {
			writeFile(m[1], `{"status": "ok"}`)
			return c.herdrOK(map[string]any{})
		}
		if files, ok := c.scenario["prompt_write"].(map[string]any); ok {
			for path, body := range files {
				writeFile(path, str(body))
			}
		}
		if res, ok := c.scenario["prompt_result"].(map[string]any); ok {
			if m := resultPathRE.FindStringSubmatch(line); m != nil {
				b, _ := json.Marshal(res)
				writeFile(m[1], string(b))
			}
		}
		return c.herdrOK(map[string]any{})
	}
	return c.herdrErr("unknown", 2)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
