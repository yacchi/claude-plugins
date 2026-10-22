package trace

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rec(kind, op string, at, end float64, ms int64) Record {
	return Record{Kind: kind, Op: op, At: at, End: end, MS: ms}
}

func TestConcurrency(t *testing.T) {
	recs := []Record{
		rec(KindGit, "a", 0, 10, 10),
		rec(KindGit, "b", 5, 15, 10),
		rec(KindGit, "c", 12, 20, 8),
		rec(KindLock, "l", 0, 100, 100),
		rec(KindGit, "d", 10, 11, 1), // a ended exactly at 10: not counted
	}
	got := Concurrency(recs)
	// others: a=0, b=1, c=1 (b still running at 12), lock=1 (a is running at 0), d=1 (b only)
	want := []int{0, 1, 1, 1, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d: others = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestPercentile(t *testing.T) {
	if got := percentile([]int64{10, 20, 30, 40}, 50); got != 20 {
		t.Errorf("p50 = %d, want 20", got)
	}
	var hundred []int64
	for i := int64(1); i <= 100; i++ {
		hundred = append(hundred, i)
	}
	if got := percentile(hundred, 95); got != 95 {
		t.Errorf("p95 = %d, want 95", got)
	}
	if got := percentile([]int64{7}, 95); got != 7 {
		t.Errorf("single = %d, want 7", got)
	}
}

func TestSensitivityUsesOnlyFullBuckets(t *testing.T) {
	var recs []Record
	// 3 solo runs at 100ms.
	for i := 0; i < 3; i++ {
		at := float64(1000 + i*10)
		recs = append(recs, rec(KindGit, "op", at, at+0.1, 100))
	}
	// 3 runs with one blanket record running (total 2) at 400ms.
	recs = append(recs, rec(KindExec, "blanket", 2000, 2100, 100000))
	for i := 0; i < 3; i++ {
		at := float64(2010 + i*10)
		recs = append(recs, rec(KindGit, "op", at, at+0.4, 400))
	}
	// 1 run at total 4 (below min-count) at 9000ms must be ignored.
	for i := 0; i < 3; i++ {
		recs = append(recs, rec(KindExec, "blanket", 3000, 3100, 100000))
	}
	recs = append(recs, rec(KindGit, "op", 3010, 3019, 9000))

	rep := Analyze(recs, time.Unix(0, 0), Options{MinCount: 3})
	var g *Group
	for i := range rep.Groups {
		if rep.Groups[i].Op == "op" {
			g = &rep.Groups[i]
		}
	}
	if g == nil || g.Sensitivity == nil {
		t.Fatalf("no sensitivity: %+v", rep.Groups)
	}
	if *g.Sensitivity != 4 {
		t.Errorf("sensitivity = %v, want 4", *g.Sensitivity)
	}
	if g.Buckets["4-6"].Count != 1 {
		t.Errorf("4-6 bucket = %+v", g.Buckets["4-6"])
	}
}

func TestLocksReportedSeparately(t *testing.T) {
	recs := []Record{
		rec(KindGit, "a", 1, 2, 1000),
		rec(KindLock, "mu", 0, 100, 100000),
	}
	rep := Analyze(recs, time.Unix(0, 0), Options{MinCount: 1})
	if len(rep.Groups) != 1 || len(rep.Locks) != 1 || rep.Locks[0].Op != "mu" {
		t.Fatalf("groups=%+v locks=%+v", rep.Groups, rep.Locks)
	}
	if _, ok := rep.Groups[0].Buckets["2-3"]; ok {
		t.Error("lock record raised concurrency")
	}
}

func TestLoadSkipsMalformedAndOldRecords(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	body := fmt.Sprintf(`{"at":%d,"end":%d,"ms":1000,"kind":"git","op":"x"}`, now.Unix(), now.Unix()+1) +
		"\nnot json\n" + `{"at":1,"end":2,"ms":1000,"kind":"git","op":"old"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, now.Format("2006-01-02")+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := Load(dir, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Op != "x" {
		t.Errorf("recs = %+v", recs)
	}
}
