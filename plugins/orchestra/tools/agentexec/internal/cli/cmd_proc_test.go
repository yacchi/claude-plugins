package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// procCmdRunErr runs the command line and returns stdout, stderr and the exit code.
func procCmdRunErr(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return out.String(), errb.String(), code
}

// procCmdSetOwner makes the watcher believe its Claude Code process is pid.
func procCmdSetOwner(t *testing.T, pid int) {
	t.Helper()
	old := procCmdOwner
	procCmdOwner = func() int { return pid }
	t.Cleanup(func() { procCmdOwner = old })
}

// procCmdSetHeld answers the lock probe without touching the real lock files.
func procCmdSetHeld(t *testing.T, held bool) {
	t.Helper()
	old := procCmdHeld
	procCmdHeld = func(int) bool { return held }
	t.Cleanup(func() { procCmdHeld = old })
}

type procCmdStartCall struct {
	exe  string
	args []string
}

// procCmdRecordStarts replaces the start seam with a recorder that returns err.
func procCmdRecordStarts(t *testing.T, err error) *[]procCmdStartCall {
	t.Helper()
	var calls []procCmdStartCall
	old := procCmdStart
	procCmdStart = func(exe string, args []string) error {
		calls = append(calls, procCmdStartCall{exe, args})
		return err
	}
	t.Cleanup(func() { procCmdStart = old })
	return &calls
}

// procCmdRecordServe replaces the serve seam with a recorder that returns err.
func procCmdRecordServe(t *testing.T, err error) *[]int {
	t.Helper()
	var owners []int
	old := procCmdServe
	procCmdServe = func(owner int) error {
		owners = append(owners, owner)
		return err
	}
	t.Cleanup(func() { procCmdServe = old })
	return &owners
}

func procCmdEqualArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProcCmdWatchStarts(t *testing.T) {
	hookCmdIsolate(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		owner     int
		held      bool
		startErr  error
		wantStart bool
	}{
		{"not held", 4242, false, nil, true},
		{"held", 4242, true, nil, false},
		{"owner 1", 1, false, nil, false},
		{"owner 0", 0, false, nil, false},
		{"start fails", 4242, false, errors.New("boom"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			procCmdSetOwner(t, c.owner)
			procCmdSetHeld(t, c.held)
			starts := procCmdRecordStarts(t, c.startErr)
			out, errb, code := procCmdRunErr(t, "proc", "watch")
			if code != 0 || out != "" || errb != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and nothing", code, out, errb)
			}
			if !c.wantStart {
				if len(*starts) != 0 {
					t.Fatalf("starts %+v, want none", *starts)
				}
				return
			}
			want := []string{"proc", "watch", "--serve", "--owner", strconv.Itoa(c.owner)}
			if len(*starts) != 1 || (*starts)[0].exe != exe || !procCmdEqualArgs((*starts)[0].args, want) {
				t.Fatalf("starts %+v, want one start of %s %v", *starts, exe, want)
			}
		})
	}
}

func TestProcCmdWatchServe(t *testing.T) {
	hookCmdIsolate(t)
	t.Run("serves the owner", func(t *testing.T) {
		starts := procCmdRecordStarts(t, nil)
		owners := procCmdRecordServe(t, nil)
		out, code := hookCmdRun(t, "", "proc", "watch", "--serve", "--owner", "4242")
		if code != 0 || out != "" {
			t.Fatalf("exit %d, stdout %q; want 0 and nothing", code, out)
		}
		if len(*owners) != 1 || (*owners)[0] != 4242 {
			t.Fatalf("serve calls %v, want [4242]", *owners)
		}
		if len(*starts) != 0 {
			t.Fatalf("serve started %d processes, want 0", len(*starts))
		}
	})
	t.Run("serve error is still exit 0", func(t *testing.T) {
		procCmdRecordServe(t, errors.New("boom"))
		out, code := hookCmdRun(t, "", "proc", "watch", "--serve", "--owner", "4242")
		if code != 0 || out != "" {
			t.Fatalf("exit %d, stdout %q; want 0 and nothing", code, out)
		}
	})
	t.Run("owner 0 does not serve", func(t *testing.T) {
		owners := procCmdRecordServe(t, nil)
		out, code := hookCmdRun(t, "", "proc", "watch", "--serve", "--owner", "0")
		if code != 0 || out != "" {
			t.Fatalf("exit %d, stdout %q; want 0 and nothing", code, out)
		}
		if len(*owners) != 0 {
			t.Fatalf("serve calls %v, want none", *owners)
		}
	})
}

func TestProcCmdWatcherStdio(t *testing.T) {
	c := procCmdWatcher("x", nil)
	if c.SysProcAttr == nil || !c.SysProcAttr.Setsid {
		t.Fatalf("watcher SysProcAttr %+v, want Setsid", c.SysProcAttr)
	}
	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Fatal("watcher holds the shell's stdio; want nil (/dev/null)")
	}
}

func TestProcCmdGuardRemoved(t *testing.T) {
	hookCmdIsolate(t)
	const want = "agent-exec: proc guard was removed in orchestra 0.93.0: the process guard no longer runs as a command prefix; the hooks start one watcher per Claude Code process with 'agent-exec proc watch'\n"
	cases := [][]string{
		{"proc", "guard"},
		{"proc", "guard", "--root", "1", "--token", "abcdefgh"},
		{"proc", "guard", "--help"},
		{"proc", "guard", "extra"},
	}
	for _, args := range cases {
		out, errb, code := procCmdRunErr(t, args...)
		if code != 2 || errb != want || out != "" {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want 2, nothing, %q", args, code, out, errb, want)
		}
	}
}

func TestProcCmdResultValidation(t *testing.T) {
	hookCmdIsolate(t)
	procCmdSetOwner(t, 4242)
	t.Run("token is removed", func(t *testing.T) {
		for _, args := range [][]string{
			{"proc", "guard-result", "--token", "abcdefgh"},
			{"proc", "guard-result", "--token", "abcdefgh", "--since", "0"},
		} {
			out, errb, code := procCmdRunErr(t, args...)
			want := "agent-exec: proc guard-result --token was removed in orchestra 0.93.0: ask with --since <epoch ms>\n"
			if code != 2 || out != "" || errb != want {
				t.Fatalf("%v: exit %d, stdout %q, stderr %q; want 2, nothing, %q", args, code, out, errb, want)
			}
		}
	})
	for _, since := range []string{"", "-1", "abc", "1.5"} {
		t.Run("since "+since, func(t *testing.T) {
			args := []string{"proc", "guard-result"}
			if since != "" {
				args = append(args, "--since", since)
			}
			out, _, code := procCmdRunErr(t, args...)
			if code != 2 || out != "" {
				t.Fatalf("exit %d, stdout %q; want 2 and no stdout", code, out)
			}
		})
	}
}

// procCmdWriteRecord writes a raw record file into the watcher directory.
func procCmdWriteRecord(t *testing.T, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(procWatchDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(procWatchDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProcCmdResultNoRecords(t *testing.T) {
	hookCmdIsolate(t)
	procCmdSetOwner(t, 4242)
	out, code := hookCmdRun(t, "", "proc", "guard-result", "--since", "0")
	if code != 0 || out != "{\"killed\":false}\n" {
		t.Fatalf("missing dir: exit %d, stdout %q", code, out)
	}
	if err := os.MkdirAll(procWatchDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code = hookCmdRun(t, "", "proc", "guard-result", "--since", "0")
	if code != 0 || out != "{\"killed\":false}\n" {
		t.Fatalf("empty dir: exit %d, stdout %q", code, out)
	}
}

func TestProcCmdResultPicksNewestOwnRecord(t *testing.T) {
	hookCmdIsolate(t)
	procCmdSetOwner(t, 4242)
	files := map[string]string{
		"4242-100-100.json": `{"at_ms":100,"owner":4242,"root":100,"procs":201,"mem_bytes":1,"reason":"a"}`,
		"4242-200-200.json": `{"at_ms":200,"owner":4242,"root":200,"procs":5,"mem_bytes":1073741824,"reason":"b"}`,
		"999-300-300.json":  `{"at_ms":300,"owner":999,"root":300,"procs":5,"mem_bytes":1,"reason":"other"}`,
		"4242-9-999.json":   `not json`,
		"watch-4242.lock":   ``,
		"abcdefgh.json":     `{"at":1,"mem_bytes":1,"procs":1,"reason":"old token","root":1,"token":"abcdefgh"}`,
	}
	var paths []string
	for name, content := range files {
		paths = append(paths, procCmdWriteRecord(t, name, content))
	}

	killed := `{"killed":true,"mem_bytes":1073741824,"procs":5,"reason":"b","root":200}` + "\n"
	cases := []struct {
		since string
		want  string
	}{
		{"0", killed},
		{"150", killed},
		{"200", killed},
		{"201", "{\"killed\":false}\n"},
	}
	for _, c := range cases {
		out, code := hookCmdRun(t, "", "proc", "guard-result", "--since", c.since)
		if code != 0 || out != c.want {
			t.Fatalf("since %s: exit %d, stdout %q, want %q", c.since, code, out, c.want)
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("guard-result removed %s: %v", p, err)
		}
	}
}
