package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/proctree"
)

// treeGuardSeams lowers the limits and the interval for one test and restores
// every seam with t.Cleanup. It skips where the process table cannot be read.
func treeGuardSeams(t *testing.T) {
	t.Helper()
	if proctree.Snapshot == nil {
		t.Skip("process table not readable on this platform")
	}
	limits, interval, kill := treeGuardLimits, treeGuardInterval, treeGuardKill
	t.Cleanup(func() {
		treeGuardLimits, treeGuardInterval, treeGuardKill = limits, interval, kill
	})
	treeGuardLimits = func() proctree.Limits { return proctree.Limits{MaxProcs: 4} }
	treeGuardInterval = 50 * time.Millisecond
}

// treeGuardFake returns the argv of a bounded fake worker: prefix, then eight
// sleeps recorded in the returned file, then a wait. The cleanup SIGKILLs the
// recorded pids when the test failed.
func treeGuardFake(t *testing.T, prefix string) ([]string, string) {
	t.Helper()
	pids := filepath.Join(t.TempDir(), "pids")
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, pid := range treeGuardPIDs(pids) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	script := prefix + "for i in 1 2 3 4 5 6 7 8; do sleep 30 & echo $! >> " + pids + "; done; wait"
	return []string{"sh", "-c", script}, pids
}

// treeGuardPIDs reads the pids a fake worker recorded.
func treeGuardPIDs(file string) []int {
	raw, _ := os.ReadFile(file)
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// treeGuardAssertGone fails unless every recorded sleep is gone within 2 s.
func treeGuardAssertGone(t *testing.T, pidsFile string) {
	t.Helper()
	pids := treeGuardPIDs(pidsFile)
	if len(pids) == 0 {
		t.Fatal("the fake worker recorded no sleep")
	}
	for _, pid := range pids {
		gone := false
		for i := 0; i < 100; i++ {
			if syscall.Kill(pid, 0) != nil {
				gone = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !gone {
			t.Fatalf("sleep %d survived", pid)
		}
	}
}

func TestTreeGuardKillsRunawayTree(t *testing.T) {
	treeGuardSeams(t)
	argv, pids := treeGuardFake(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := spawn(ctx, spawnOptions{Argv: argv})
	if err != nil {
		t.Fatal(err)
	}
	if res.Runaway == nil || res.Runaway.Reason != "process-tree" {
		t.Fatalf("runaway %+v", res.Runaway)
	}
	detail := regexp.MustCompile(`^process guard: \d+ processes under pid \d+ \(limit 4\); killed the executor's process tree \(\d+ processes\)$`)
	if !detail.MatchString(res.Runaway.Detail) {
		t.Fatalf("detail %q", res.Runaway.Detail)
	}
	if res.Runaway.ElapsedS != res.Runaway.RuntimeS || res.Runaway.ElapsedS <= 0 || res.Runaway.ElapsedS >= 10 {
		t.Fatalf("elapsed %v runtime %v", res.Runaway.ElapsedS, res.Runaway.RuntimeS)
	}
	if res.ExitCode != -int(syscall.SIGKILL) {
		t.Fatalf("exit %d", res.ExitCode)
	}
	obj := res.Runaway.Object()
	if obj["detail"] != res.Runaway.Detail || obj["last_tool"] != nil || obj["repeat_count"] != int64(0) {
		t.Fatalf("object %v", obj)
	}
	treeGuardAssertGone(t, pids)
}

func TestTreeGuardKillErrorSuffix(t *testing.T) {
	treeGuardSeams(t)
	cases := []struct {
		name   string
		killed int
		err    error
		suffix string
	}{
		{
			name:   "partial failure",
			killed: 9,
			err:    errors.New("proctree: could not kill 1 of 10 processes: operation not permitted"),
			suffix: "(9 processes); proctree: could not kill 1 of 10 processes: operation not permitted",
		},
		{
			name:   "root already gone",
			killed: 0,
			err:    proctree.ErrNoRoot,
			suffix: "(0 processes)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			treeGuardSeams(t)
			treeGuardKill = func(pid int, includeRoot bool) (int, error) {
				// The real kill still runs, so the fake worker dies either way.
				_, _ = proctree.KillTree(pid, includeRoot)
				return tc.killed, tc.err
			}
			argv, pids := treeGuardFake(t, "")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := spawn(ctx, spawnOptions{Argv: argv})
			if err != nil {
				t.Fatal(err)
			}
			if res.Runaway == nil || !strings.HasSuffix(res.Runaway.Detail, tc.suffix) {
				t.Fatalf("runaway %+v, want detail ending %q", res.Runaway, tc.suffix)
			}
			treeGuardAssertGone(t, pids)
		})
	}
}

func TestTreeGuardWatchdogRunawayWins(t *testing.T) {
	treeGuardSeams(t)
	// The child ignores SIGTERM, so the wall trip (first) stays in its grace
	// period and the tree trip (second) is the one that kills it.
	argv, pids := treeGuardFake(t, `trap "" TERM; sleep 0.3; `)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := spawn(ctx, spawnOptions{
		Argv:     argv,
		Watchdog: guard(10*time.Second, 10*time.Second, 100*time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Runaway == nil || res.Runaway.Reason != "wall" || res.Runaway.Detail != "" {
		t.Fatalf("runaway %+v", res.Runaway)
	}
	treeGuardAssertGone(t, pids)
}

func TestTreeGuardUnderLimit(t *testing.T) {
	treeGuardSeams(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := spawn(ctx, spawnOptions{Argv: []string{"sh", "-c", "sleep 0.3"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Runaway != nil || res.ExitCode != 0 {
		t.Fatalf("runaway %+v exit %d", res.Runaway, res.ExitCode)
	}
}
