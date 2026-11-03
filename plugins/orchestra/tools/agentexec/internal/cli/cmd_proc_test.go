package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/proctree"
)

// procGuardIsolate points the state directory at a temporary one, where the
// record directory lives.
func procGuardIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

type procGuardStartCall struct {
	exe  string
	args []string
}

// procGuardRecordStarts replaces the start seam with a recorder that returns err.
func procGuardRecordStarts(t *testing.T, err error) *[]procGuardStartCall {
	t.Helper()
	var calls []procGuardStartCall
	old := procGuardStart
	procGuardStart = func(exe string, args []string) error {
		calls = append(calls, procGuardStartCall{exe, args})
		return err
	}
	t.Cleanup(func() { procGuardStart = old })
	return &calls
}

type procGuardKillCall struct {
	root        int
	includeRoot bool
}

// procGuardRecordKills replaces the kill seam with a recorder that calls check
// first and then the real KillTree when real is set.
func procGuardRecordKills(t *testing.T, check func(), real bool) *[]procGuardKillCall {
	t.Helper()
	var calls []procGuardKillCall
	old := procGuardKill
	procGuardKill = func(root int, includeRoot bool) (int, error) {
		calls = append(calls, procGuardKillCall{root, includeRoot})
		if check != nil {
			check()
		}
		if real {
			return old(root, includeRoot)
		}
		return 0, nil
	}
	t.Cleanup(func() { procGuardKill = old })
	return &calls
}

func procGuardSetLimits(t *testing.T, l proctree.Limits) {
	t.Helper()
	old := procGuardLimits
	procGuardLimits = func() proctree.Limits { return l }
	t.Cleanup(func() { procGuardLimits = old })
}

func TestProcGuardValidation(t *testing.T) {
	procGuardIsolate(t)
	long := strings.Repeat("a", 65)
	cases := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"root 1", []string{"proc", "guard", "--root", "1", "--token", "abcdefgh"},
			"agent-exec: proc guard: --root must be a pid > 1\n"},
		{"root 0", []string{"proc", "guard", "--root", "0", "--token", "abcdefgh"},
			"agent-exec: proc guard: --root must be a pid > 1\n"},
		{"no root", []string{"proc", "guard", "--token", "abcdefgh"},
			"agent-exec: proc guard: --root must be a pid > 1\n"},
		{"path token", []string{"proc", "guard", "--root", "4242", "--token", "abc/../x"}, procGuardTokenErr("proc guard")},
		{"short token", []string{"proc", "guard", "--root", "4242", "--token", "abcdefg"}, procGuardTokenErr("proc guard")},
		{"long token", []string{"proc", "guard", "--root", "4242", "--token", long}, procGuardTokenErr("proc guard")},
		{"empty token", []string{"proc", "guard", "--root", "4242", "--token", ""}, procGuardTokenErr("proc guard")},
		{"watch with bad token", []string{"proc", "guard", "--root", "4242", "--token", "abc/../x", "--watch"}, procGuardTokenErr("proc guard")},
		{"result path token", []string{"proc", "guard-result", "--token", "../../x1"}, procGuardTokenErr("proc guard-result")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			starts := procGuardRecordStarts(t, nil)
			var out, errb bytes.Buffer
			code := run(c.args, &out, &errb)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (stderr %q)", code, errb.String())
			}
			if errb.String() != c.stderr {
				t.Fatalf("stderr %q, want %q", errb.String(), c.stderr)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout %q, want none", out.String())
			}
			if len(*starts) != 0 {
				t.Fatalf("watcher started %d times, want 0", len(*starts))
			}
			if _, err := os.Stat(procGuardDir()); !os.IsNotExist(err) {
				t.Fatalf("record directory exists after a refused command: %v", err)
			}
		})
	}
}

func procGuardTokenErr(name string) string {
	return "agent-exec: " + name + ": --token must match ^[A-Za-z0-9_-]{8,64}$\n"
}

func TestProcGuardStart(t *testing.T) {
	procGuardIsolate(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"abcdefgh", strings.Repeat("a", 64)} {
		starts := procGuardRecordStarts(t, nil)
		out, code := hookCmdRun(t, "", "proc", "guard", "--root", "4242", "--token", tok)
		if code != 0 || out != "" {
			t.Fatalf("token %q: exit %d, stdout %q; want 0 and nothing", tok, code, out)
		}
		want := []string{"proc", "guard", "--root", "4242", "--token", tok, "--watch"}
		if len(*starts) != 1 || (*starts)[0].exe != exe || !procGuardEqualArgs((*starts)[0].args, want) {
			t.Fatalf("starts %+v, want one start of %s %v", *starts, exe, want)
		}
	}

	procGuardRecordStarts(t, errors.New("boom"))
	out, code := hookCmdRun(t, "", "proc", "guard", "--root", "4242", "--token", "abcdefgh")
	if code != 0 || out != "" {
		t.Fatalf("failed start: exit %d, stdout %q; want 0 and nothing", code, out)
	}
}

func procGuardEqualArgs(a, b []string) bool {
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

func TestProcGuardWatcherStdio(t *testing.T) {
	c := procGuardWatcher("x", nil)
	if c.SysProcAttr == nil || !c.SysProcAttr.Setsid {
		t.Fatalf("watcher SysProcAttr %+v, want Setsid", c.SysProcAttr)
	}
	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Fatal("watcher holds the shell's stdio; want nil (/dev/null)")
	}
}

func TestProcGuardResult(t *testing.T) {
	procGuardIsolate(t)
	if err := os.MkdirAll(procGuardDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := procGuardRecordPath("abcdefgh")
	run1 := func() (string, int) {
		return hookCmdRun(t, "", "proc", "guard-result", "--token", "abcdefgh")
	}

	out, code := run1()
	if code != 0 || out != "{\"killed\":false}\n" {
		t.Fatalf("no record: exit %d, stdout %q", code, out)
	}

	rec := `{"at":1,"mem_bytes":1073741824,"procs":201,"reason":"process guard: 201 processes under pid 4242 (limit 200)","root":4242,"token":"abcdefgh"}`
	if err := os.WriteFile(path, []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = run1()
	want := "{\"killed\":true,\"mem_bytes\":1073741824,\"procs\":201,\"reason\":\"process guard: 201 processes under pid 4242 (limit 200)\",\"root\":4242}\n"
	if code != 0 || out != want {
		t.Fatalf("record: exit %d, stdout %q, want %q", code, out, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("record not removed after it was read: %v", err)
	}
	out, code = run1()
	if code != 0 || out != "{\"killed\":false}\n" {
		t.Fatalf("second read: exit %d, stdout %q", code, out)
	}

	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = run1()
	if code != 0 || out != "{\"killed\":false}\n" {
		t.Fatalf("unparsable record: exit %d, stdout %q", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unparsable record not removed: %v", err)
	}
}

func TestProcGuardTrip(t *testing.T) {
	procGuardIsolate(t)
	u := proctree.Usage{Root: 4242, Procs: 201, Mem: 1 << 30}
	const text = "process guard: 201 processes under pid 4242 (limit 200)"
	const token = "abcdefgh"

	t.Run("record first, then kill", func(t *testing.T) {
		kills := procGuardRecordKills(t, func() {
			data, err := os.ReadFile(procGuardRecordPath(token))
			if err != nil {
				t.Errorf("record missing at the first signal: %v", err)
				return
			}
			var rec procGuardRecord
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Errorf("record does not parse: %v", err)
				return
			}
			if rec.Root != 4242 || rec.Token != token || rec.Procs != 201 || rec.MemBytes != 1<<30 || rec.Reason != text {
				t.Errorf("record %+v does not match the trip", rec)
			}
		}, false)
		procGuardTrip(4242, token, u, text)
		if len(*kills) != 1 || (*kills)[0] != (procGuardKillCall{4242, false}) {
			t.Fatalf("kill calls %+v, want one (4242, false)", *kills)
		}
	})

	t.Run("failed record write still kills", func(t *testing.T) {
		// A regular file where the record directory belongs makes the write fail.
		if err := os.RemoveAll(procGuardDir()); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(procGuardDir()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(procGuardDir(), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(procGuardDir()) })
		kills := procGuardRecordKills(t, nil, false)
		procGuardTrip(4242, token, u, text)
		if len(*kills) != 1 {
			t.Fatalf("kill calls %d, want 1 after a failed record write", len(*kills))
		}
	})
}

func TestProcGuardWatchKills(t *testing.T) {
	if proctree.Snapshot == nil {
		t.Skip("process table not readable on this platform")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	procGuardIsolate(t)
	procGuardSetLimits(t, proctree.Limits{MaxProcs: 2})

	// Three bounded sleeps in the shell's own group: the tree under the shell
	// trips MaxProcs 2 and is killed. The shell itself must survive.
	sh := exec.Command("sh", "-c", "sleep 30 & sleep 30 & sleep 30 & wait; echo survived")
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout bytes.Buffer
	sh.Stdout = &stdout
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	pid := sh.Process.Pid
	const token = "abcdefgh"
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = sh.Wait()
	})

	kills := procGuardRecordKills(t, func() {
		if _, err := os.Stat(procGuardRecordPath(token)); err != nil {
			t.Errorf("record not on disk at the first signal: %v", err)
		}
	}, true)

	out, code := hookCmdRun(t, "", "proc", "guard", "--root", strconv.Itoa(pid), "--token", token, "--watch")
	if code != 0 || out != "" {
		t.Fatalf("watch: exit %d, stdout %q; want 0 and nothing", code, out)
	}
	if err := sh.Wait(); err != nil {
		t.Fatalf("shell did not finish normally: %v", err)
	}
	if stdout.String() != "survived\n" {
		t.Fatalf("shell stdout %q, want survived", stdout.String())
	}
	if len(*kills) != 1 || (*kills)[0] != (procGuardKillCall{pid, false}) {
		t.Fatalf("kill calls %+v, want one (%d, false)", *kills, pid)
	}

	res, code := hookCmdRun(t, "", "proc", "guard-result", "--token", token)
	if code != 0 {
		t.Fatalf("guard-result: exit %d", code)
	}
	var got struct {
		Killed bool `json:"killed"`
		Root   int  `json:"root"`
		Procs  int  `json:"procs"`
	}
	if err := json.Unmarshal([]byte(res), &got); err != nil {
		t.Fatalf("guard-result %q: %v", res, err)
	}
	if !got.Killed || got.Root != pid || got.Procs < 3 {
		t.Fatalf("guard-result %+v, want killed true, root %d, procs >= 3", got, pid)
	}
}

func TestProcGuardWatchExpires(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	procGuardIsolate(t)
	procGuardSetLimits(t, proctree.Limits{MaxProcs: 2})

	// A root that has already exited: Wait ends on ErrNoRoot, so nothing trips.
	done := exec.Command("sh", "-c", "exit 0")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	pid := done.Process.Pid

	if err := os.MkdirAll(procGuardDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(procGuardDir(), "old.json")
	fresh := filepath.Join(procGuardDir(), "new.json")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}

	kills := procGuardRecordKills(t, nil, false)
	out, code := hookCmdRun(t, "", "proc", "guard", "--root", strconv.Itoa(pid), "--token", "abcdefgh", "--watch")
	if code != 0 || out != "" {
		t.Fatalf("watch: exit %d, stdout %q; want 0 and nothing", code, out)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("record older than 24 h not removed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh record removed: %v", err)
	}
	if len(*kills) != 0 {
		t.Fatalf("kill called %d times for a root that is gone, want 0", len(*kills))
	}
}
