package checks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
)

// The retry holds every per-wave check slot but only ONE machine-wide heavy
// slot: with one of two heavy slots taken elsewhere it still runs, and
// while it runs exactly two leases exist.
func TestRerunFailedAloneTakesOneHeavySlot(t *testing.T) {
	t.Setenv("ORCHESTRA_SLOT_DIR", t.TempDir())
	pool := slot.New(slot.Heavy, 2)
	was := slot.HeavyPool
	slot.HeavyPool = func() slot.Pool { return pool }
	t.Cleanup(func() { slot.HeavyPool = was })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := pool.Acquire(ctx, slot.Request{Owner: os.Getpid(), Op: "pnpm test"}); err != nil {
		t.Fatal(err)
	}

	template := fmt.Sprintf(`test "$(ls %s | grep -c '\.lease$')" -eq 2 && test -n {failed}`, pool.Dir)
	done := make(chan string, 1)
	go func() {
		status, _ := RerunFailedAlone(" FAIL  tests/a.test.ts\n", template, t.TempDir(),
			filepath.Join(t.TempDir(), "slots"), 2, 10*time.Second)
		done <- status
	}()
	select {
	case status := <-done:
		if status != "pass" {
			t.Errorf("status %s: the retry did not hold exactly one of the two heavy slots", status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RerunFailedAlone is waiting for more than one heavy slot")
	}
}

// A test command is admitted on the test pool, not the heavy one: with the
// only heavy slot taken elsewhere it still runs, holding one test lease.
func TestTestCommandTakesATestSlot(t *testing.T) {
	t.Setenv("ORCHESTRA_SLOT_DIR", t.TempDir())
	heavy, tests := slot.New(slot.Heavy, 1), slot.New(slot.Test, 1)
	wasHeavy, wasTest := slot.HeavyPool, slot.TestPool
	slot.HeavyPool = func() slot.Pool { return heavy }
	slot.TestPool = func() slot.Pool { return tests }
	t.Cleanup(func() { slot.HeavyPool, slot.TestPool = wasHeavy, wasTest })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := heavy.Acquire(ctx, slot.Request{Owner: os.Getpid(), Op: "pnpm install"}); err != nil {
		t.Fatal(err)
	}

	// `go test` never runs (true ||); it makes the line a test run.
	command := fmt.Sprintf(`true || go test ./x; test "$(ls %s | grep -c '\.lease$')" -eq 1`, tests.Dir)
	done := make(chan shellResult, 1)
	go func() { done <- runShell(command, t.TempDir(), nil, 10*time.Second, t.TempDir(), 0) }()
	select {
	case sh := <-done:
		if sh.exit == nil || *sh.exit != 0 {
			t.Errorf("the test command did not hold one test slot: %s%s", sh.stdout, sh.stderr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the test command is waiting for the heavy pool")
	}
}
