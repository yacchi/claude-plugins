package fsutil

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// traceRecords reads the day file of the records written so far.
func traceRecords(t *testing.T) []trace.Record {
	t.Helper()
	f, err := os.Open(trace.FileFor(time.Now()))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []trace.Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r trace.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestUncontendedLockWritesNoTraceRecord(t *testing.T) {
	t.Setenv("ORCHESTRA_TRACE_DIR", t.TempDir())
	unlock, err := Lock(filepath.Join(t.TempDir(), "x.lock"), Exclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if recs := traceRecords(t); len(recs) != 0 {
		t.Fatalf("uncontended lock wrote %d records: %+v", len(recs), recs)
	}
}

func TestShortBusyWaitWritesNoTraceRecord(t *testing.T) {
	t.Setenv("ORCHESTRA_TRACE_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(path, Exclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Lock(path, Exclusive, 0); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if recs := traceRecords(t); len(recs) != 0 {
		t.Fatalf("a busy probe under 50ms wrote %d records: %+v", len(recs), recs)
	}
}

func TestBusyLockTimeoutIsTracedAsFailedWait(t *testing.T) {
	t.Setenv("ORCHESTRA_TRACE_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(path, Exclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Lock(path, Exclusive, 150*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	recs := traceRecords(t)
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want one", recs)
	}
	if r := recs[0]; r.Kind != trace.KindLock || r.Exit != 1 || r.Detail != path || r.MS < 0 {
		t.Fatalf("record = %+v", r)
	}
}
