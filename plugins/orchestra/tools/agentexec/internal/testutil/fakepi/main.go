// Command fakepi is a stand-in `pi` for tests: no model, no auth, and fully
// deterministic, so agent-exec can be driven by it as a binary on PATH.
//
// The prompt (stdin) lists shell commands, one per line after a `COMMANDS:`
// line; each runs with bash in the cwd and the output is pi 1.1.0-shaped
// JSONL. `--session <id>` resumes: the id is echoed, a line is appended to
// ~/.pi/agent/sessions/<id>.jsonl, and a prompt without `COMMANDS:` re-runs
// the commands the session started with. A new session's id is derived from
// the prompt's sha256. Every other flag is ignored.
//
// Directive lines anywhere in the prompt inject behavior:
//
//	FAKEPI-ANSWER: <text>     final assistant text (repeatable, joined by \n; default DONE)
//	FAKEPI-EVENT: <json>      emit this raw JSONL line after the session event
//	FAKEPI-STDERR: <text>     write a line to stderr
//	FAKEPI-EXIT: <n>          exit status
//	FAKEPI-LONGLINE: <n>      emit a tool event whose output text is n bytes
//	FAKEPI-SLEEP: <ms>        sleep before the final message
//	FAKEPI-HANG               block after the session event until killed
//	FAKEPI-FAULT: quota|nonzero
//	                          preset failures: a quota error event + stderr +
//	                          exit 1, or a bare exit 1 with stderr
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type directives struct {
	answers  []string
	events   []string
	stderr   []string
	exit     int
	longLine int
	sleep    time.Duration
	hang     bool
}

func parseDirectives(prompt string) directives {
	var d directives
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, ":")
		val = strings.TrimSpace(val)
		if !ok {
			if line == "FAKEPI-HANG" {
				d.hang = true
			}
			continue
		}
		switch key {
		case "FAKEPI-ANSWER":
			d.answers = append(d.answers, val)
		case "FAKEPI-EVENT":
			d.events = append(d.events, val)
		case "FAKEPI-STDERR":
			d.stderr = append(d.stderr, val)
		case "FAKEPI-EXIT":
			d.exit, _ = strconv.Atoi(val)
		case "FAKEPI-LONGLINE":
			d.longLine, _ = strconv.Atoi(val)
		case "FAKEPI-SLEEP":
			ms, _ := strconv.Atoi(val)
			d.sleep = time.Duration(ms) * time.Millisecond
		case "FAKEPI-FAULT":
			switch val {
			case "quota":
				d.events = append(d.events,
					`{"type": "error", "error": "You have exceeded your usage quota for this period"}`)
				d.stderr = append(d.stderr, "Error: usage quota exceeded")
				d.exit = 1
			case "nonzero":
				d.stderr = append(d.stderr, "Error: something went wrong")
				d.exit = 1
			}
		}
	}
	return d
}

// commandsOf returns the lines after a `COMMANDS:` line, or nil without one.
func commandsOf(prompt string) []string {
	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "COMMANDS:" {
			continue
		}
		out := []string{}
		for _, l := range lines[i+1:] {
			if strings.TrimSpace(l) != "" && !strings.HasPrefix(strings.TrimSpace(l), "FAKEPI-") {
				out = append(out, l)
			}
		}
		return out
	}
	return nil
}

func sessionFromPrompt(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	session := ""
	for i, a := range args {
		if a == "--session" && i+1 < len(args) {
			session = args[i+1]
		}
	}
	raw, _ := io.ReadAll(stdin)
	prompt := string(raw)
	resumed := session != ""
	if !resumed {
		session = sessionFromPrompt(prompt)
	}
	d := parseDirectives(prompt)
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, session+".jsonl")

	commands := commandsOf(prompt)
	if commands == nil && resumed {
		commands = firstCommands(path)
	}
	if commands == nil {
		commands = []string{}
	}
	appendRecord(path, map[string]any{"resumed": resumed, "cwd": cwd, "commands": commands})

	out := bufio.NewWriter(stdout)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	emit(map[string]any{"type": "session", "version": 3, "id": session,
		"timestamp": "2026-01-01T00:00:00.000Z", "cwd": cwd})
	for _, e := range d.events {
		out.WriteString(e + "\n")
		out.Flush()
	}
	for _, line := range d.stderr {
		fmt.Fprintln(stderr, line)
	}
	if d.hang {
		// A bare `select {}` is a deadlock the Go runtime aborts (exit 2):
		// sleep instead, so only a signal ends the process.
		for {
			time.Sleep(time.Hour)
		}
	}
	for n, command := range commands {
		callID := fmt.Sprintf("call_%d", n)
		cmdArgs := map[string]any{"command": command}
		emit(map[string]any{"type": "tool_execution_start", "toolCallId": callID,
			"toolName": "bash", "args": cmdArgs})
		c := exec.Command("bash", "-c", command)
		c.Stdin = nil
		text, err := c.CombinedOutput()
		emit(map[string]any{"type": "tool_execution_end", "toolCallId": callID,
			"toolName": "bash", "args": cmdArgs,
			"result":  map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}},
			"isError": err != nil})
	}
	if d.longLine > 0 {
		emit(map[string]any{"type": "tool_execution_end", "toolCallId": "call_long",
			"toolName": "bash", "args": map[string]any{"command": "long"},
			"result":  map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", d.longLine)}}},
			"isError": false})
	}
	time.Sleep(d.sleep)
	answer := "DONE"
	if len(d.answers) > 0 {
		answer = strings.Join(d.answers, "\n")
	}
	zero := map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}
	usage := map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0,
		"reasoning": 0, "totalTokens": 0, "cost": zero}
	emit(map[string]any{"type": "message_end", "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": answer}},
		"usage": usage, "stopReason": "stop"}})
	return d.exit
}

func firstCommands(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil
	}
	var first struct {
		Commands []string `json:"commands"`
	}
	if json.Unmarshal(line, &first) != nil {
		return nil
	}
	return first.Commands
}

func appendRecord(path string, rec map[string]any) {
	b, _ := json.Marshal(rec)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}
