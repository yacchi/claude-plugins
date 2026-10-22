package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func readAll(t *testing.T, dir string) []Record {
	t.Helper()
	var out []Record
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var r Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("line %q is not a record: %v", sc.Text(), err)
			}
			out = append(out, r)
		}
		f.Close()
	}
	return out
}

func TestBeginWritesOneRecordWithItsInterval(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORCHESTRA_TRACE_DIR", dir)
	t.Setenv("ORCHESTRA_TRACE", "")
	end := Begin(KindGit, "git worktree add", Attrs{Pkg: "A1", Cwd: "/repo", Detail: "x"})
	time.Sleep(20 * time.Millisecond)
	end(3)
	end(0) // a second end is ignored
	recs := readAll(t, dir)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Kind != KindGit || r.Op != "git worktree add" || r.Pkg != "A1" || r.Cwd != "/repo" || r.Exit != 3 {
		t.Fatalf("unexpected record %+v", r)
	}
	if r.MS < 20 || r.End <= r.At || r.PID != os.Getpid() {
		t.Fatalf("bad timing %+v", r)
	}
}

func TestConcurrentWritesStayWholeLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORCHESTRA_TRACE_DIR", dir)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Begin(KindFS, "rm -rf", Attrs{Detail: string(make([]byte, maxDetail+50))})(0)
		}()
	}
	wg.Wait()
	recs := readAll(t, dir)
	if len(recs) != 50 {
		t.Fatalf("got %d records, want 50", len(recs))
	}
	if len(recs[0].Detail) != maxDetail {
		t.Fatalf("detail not clipped: %d", len(recs[0].Detail))
	}
}

func TestOffWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORCHESTRA_TRACE_DIR", dir)
	t.Setenv("ORCHESTRA_TRACE", "off")
	Begin(KindExec, "x", Attrs{})(0)
	if recs := readAll(t, dir); len(recs) != 0 {
		t.Fatalf("got %d records with tracing off", len(recs))
	}
}

func TestPruneKeepsRecentDays(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORCHESTRA_TRACE_DIR", dir)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	old := now.AddDate(0, 0, -RetentionDays-1).Format("2006-01-02") + ".jsonl"
	kept := now.AddDate(0, 0, -RetentionDays).Format("2006-01-02") + ".jsonl"
	for _, n := range []string{old, kept, "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prune(now)
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Fatalf("%s survived", old)
	}
	for _, n := range []string{kept, "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s was removed", n)
		}
	}
}

func TestLoad1IsReadOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sysctl layout is darwin's")
	}
	if v := load1(); v <= 0 || v > 10000 {
		t.Fatalf("load1 = %v", v)
	}
}

func TestTestBinaryWritesOnlyToAnExplicitDir(t *testing.T) {
	t.Setenv("ORCHESTRA_TRACE", "")
	t.Setenv("ORCHESTRA_TRACE_DIR", "")
	if Enabled() {
		t.Fatal("a test binary without ORCHESTRA_TRACE_DIR would write to the real log")
	}
	t.Setenv("ORCHESTRA_TRACE_DIR", t.TempDir())
	if !Enabled() {
		t.Fatal("an explicit ORCHESTRA_TRACE_DIR must enable tracing in tests")
	}
}
