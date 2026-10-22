package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// herdrTraceRecords reads the herdr call records written so far.
func herdrTraceRecords(t *testing.T) []trace.Record {
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
		if r.Kind == trace.KindHerdr {
			out = append(out, r)
		}
	}
	return out
}

func TestPromptTextIsNotTraced(t *testing.T) {
	t.Setenv("ORCHESTRA_TRACE_DIR", t.TempDir())
	e := newEnv(t)
	x := e.executor("")
	const secret = "SECRET-PROMPT-TEXT"
	_, _, _ = x.call(context.Background(), callTimeout, "", "agent", "prompt", "pkg-a", secret)
	recs := herdrTraceRecords(t)
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want one", recs)
	}
	if r := recs[0]; r.Detail != "pkg-a" || strings.Contains(r.Detail, secret) {
		t.Fatalf("detail = %q, want the agent name only", r.Detail)
	}
}

func TestTimedOutHerdrCallIsTracedAsFailed(t *testing.T) {
	t.Setenv("ORCHESTRA_TRACE_DIR", t.TempDir())
	e := newEnv(t)
	x := e.executor("")
	_, _, err := x.call(context.Background(), time.Nanosecond, "", "status")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	recs := herdrTraceRecords(t)
	if len(recs) != 1 || recs[0].Exit != -1 || recs[0].Op != "herdr status" {
		t.Fatalf("records = %+v, want one herdr status record with exit -1", recs)
	}
}
