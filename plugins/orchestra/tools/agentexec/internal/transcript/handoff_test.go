package transcript

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// handoffFile writes lines (one JSON object each) to a fresh transcript and
// returns its path.
func handoffFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-a1.jsonl")
	transcriptWrite(t, path, strings.Join(lines, "\n")+"\n")
	return path
}

func handoffBashUse(id, command string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"` + command + `"}}]}}`
}

func handoffBashFail(id, result string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","is_error":true,"content":` + result + `}]}}`
}

func TestHandoffReadsGoalAndAnswer(t *testing.T) {
	path := handoffFile(t,
		`{"type":"user","message":{"content":"Do X"}}`,
		`{"type":"api-request-blob","message":{"content":"Do Y"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"A"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"none","content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"B"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"C"}]}}`,
		`not json`,
	)

	h, ok := ReadHandoff(path)
	if !ok {
		t.Fatal("ReadHandoff ok = false for a readable file")
	}
	if h.Goal != "Do X" {
		t.Errorf("Goal = %q; want %q", h.Goal, "Do X")
	}
	if h.Answer != "B\nC" {
		t.Errorf("Answer = %q; want %q", h.Answer, "B\nC")
	}
}

func TestHandoffGoalFromTextBlocks(t *testing.T) {
	path := handoffFile(t,
		`{"type":"user","message":{"content":[{"type":"text","text":"first"},{"type":"text","text":"part"}]}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"later"}]}}`,
	)

	h, _ := ReadHandoff(path)
	if h.Goal != "first\npart" {
		t.Errorf("Goal = %q; want the first line's texts joined", h.Goal)
	}
}

func TestHandoffFailedCommands(t *testing.T) {
	lines := []string{}
	for i := 1; i <= 7; i++ {
		id := "c" + string(rune('0'+i))
		lines = append(lines, handoffBashUse(id, "cmd"+string(rune('0'+i))))
		lines = append(lines, handoffBashFail(id, `"boom"`))
	}
	// A failure of a non-Bash tool and a successful Bash call add nothing.
	lines = append(lines,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"x"}}]}}`,
		handoffBashFail("r1", `"boom"`),
		handoffBashUse("ok", "true"),
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"ok","is_error":false,"content":"fine"}]}}`,
	)
	h, _ := ReadHandoff(handoffFile(t, lines...))

	want := []FailedCommand{
		{"cmd3", "boom"}, {"cmd4", "boom"}, {"cmd5", "boom"}, {"cmd6", "boom"}, {"cmd7", "boom"},
	}
	if !reflect.DeepEqual(h.Failed, want) {
		t.Errorf("Failed = %+v; want the last 5 oldest first: %+v", h.Failed, want)
	}
}

func TestHandoffFailedErrorIsFirstNonEmptyLine(t *testing.T) {
	path := handoffFile(t,
		handoffBashUse("t1", `go test\n-run X`),
		handoffBashFail("t1", `[{"type":"text","text":"\n  exit 1\nmore"}]`),
	)

	h, _ := ReadHandoff(path)
	want := []FailedCommand{{"go test\n-run X", "exit 1"}}
	if !reflect.DeepEqual(h.Failed, want) {
		t.Errorf("Failed = %+v; want %+v", h.Failed, want)
	}
}

func TestHandoffChangedAndRead(t *testing.T) {
	path := handoffFile(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"Read","input":{"file_path":"a.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"2","name":"Edit","input":{"file_path":"a.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"3","name":"Read","input":{"file_path":"b.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"4","name":"Write","input":{"file_path":"c.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"5","name":"Read","input":{"file_path":"b.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"6","name":"Edit","input":{"file_path":"c.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"7","name":"MultiEdit","input":{"file_path":"d.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"8","name":"NotebookEdit","input":{"notebook_path":"n.ipynb"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"9","name":"NotebookEdit","input":{"file_path":"skip.ipynb"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"10","name":"Read","input":{"file_path":"n.ipynb"}}]}}`,
	)

	h, _ := ReadHandoff(path)
	if want := []string{"a.go", "c.go", "d.go", "n.ipynb"}; !reflect.DeepEqual(h.Changed, want) {
		t.Errorf("Changed = %v; want %v", h.Changed, want)
	}
	if want := []string{"b.go"}; !reflect.DeepEqual(h.Read, want) {
		t.Errorf("Read = %v; want %v (a changed file is not listed as read)", h.Read, want)
	}
}

func TestHandoffKeepsLongLine(t *testing.T) {
	long := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"` + strings.Repeat("y", 100000) + `"}]}}`
	path := handoffFile(t,
		`{"type":"user","message":{"content":"Do X"}}`,
		long,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"after"}]}}`,
	)

	h, ok := ReadHandoff(path)
	if !ok || h.Answer != "after" {
		t.Errorf("ReadHandoff = %q, %v; want the answer after a long line", h.Answer, ok)
	}
}

func TestHandoffMissingFile(t *testing.T) {
	if _, ok := ReadHandoff(filepath.Join(t.TempDir(), "absent.jsonl")); ok {
		t.Fatal("ReadHandoff ok = true for a missing file")
	}
}

func TestHandoffRenderExample(t *testing.T) {
	h := Handoff{
		Goal:    "Fix x\n",
		Answer:  "done",
		Failed:  []FailedCommand{{Command: "go test ./...", Error: "FAIL foo"}},
		Changed: []string{"a.go"},
	}
	at := time.Date(2026, 11, 4, 14, 6, 7, 0, time.FixedZone("JST", 9*3600))

	got := RenderHandoff(h, "worker", "a1", at)
	want := "# Handoff: worker a1\n\n" +
		"Written 2026-11-04T05:06:07Z when this agent returned. Start a fresh worker with this file instead of resuming the agent: read it first, then do the next step your prompt gives.\n\n" +
		"## Goal\nFix x\n\n" +
		"## Answer\ndone\n\n" +
		"## Failed commands\n- `go test ./...`: FAIL foo\n\n" +
		"## Files changed\n- a.go\n\n" +
		"## Files read\n(none)\n"
	if got != want {
		t.Errorf("RenderHandoff =\n%s\nwant\n%s", got, want)
	}
}

func TestHandoffRenderCaps(t *testing.T) {
	lines := func(n int) string {
		out := make([]string, n)
		for i := range out {
			out[i] = "l"
		}
		return strings.Join(out, "\n")
	}
	h := Handoff{Goal: lines(45), Answer: lines(61)}
	got := RenderHandoff(h, "worker", "a1", time.Now())

	if !strings.Contains(got, "\n… (5 more lines)\n\n## Answer") {
		t.Errorf("Goal of 45 lines not cut to 40 with a 5-line marker:\n%s", got)
	}
	if !strings.Contains(got, "\n… (1 more lines)\n\n## Failed commands") {
		t.Errorf("Answer of 61 lines not cut to 60 with a 1-line marker:\n%s", got)
	}

	exact := RenderHandoff(Handoff{Answer: lines(60)}, "worker", "a1", time.Now())
	if strings.Contains(exact, "more lines") {
		t.Errorf("Answer of exactly 60 lines got a marker:\n%s", exact)
	}
}

func TestHandoffRenderEmptyAndCuts(t *testing.T) {
	empty := RenderHandoff(Handoff{Goal: "\n  \n"}, "worker", "a1", time.Now())
	for _, section := range []string{"## Goal\n(none)", "## Answer\n(none)", "## Failed commands\n(none)", "## Files changed\n(none)", "## Files read\n(none)"} {
		if !strings.Contains(empty, section) {
			t.Errorf("empty handoff lacks %q:\n%s", section, empty)
		}
	}

	long := strings.Repeat("é", 170)
	h := Handoff{Failed: []FailedCommand{{Command: long + "\nsecond line", Error: strings.Repeat("é", 210)}}}
	got := RenderHandoff(h, "worker", "a1", time.Now())
	wantLine := "- `" + strings.Repeat("é", 160) + "`: " + strings.Repeat("é", 200)
	if !strings.Contains(got, wantLine+"\n") {
		t.Errorf("failed line not cut to 160 and 200 runes:\n%s", got)
	}
}

func TestHandoffPath(t *testing.T) {
	transcriptSetup(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	real, err := filepath.EvalSymlinks(state)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(real, "claude-orchestra", "handoffs", "s1", "a1.md")
	if got := HandoffPath("s1", "a1"); got != want {
		t.Errorf("HandoffPath = %q; want %q", got, want)
	}
}
