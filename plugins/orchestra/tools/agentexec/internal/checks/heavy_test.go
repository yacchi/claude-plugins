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
	was := heavyPool
	heavyPool = func() slot.Pool { return pool }
	t.Cleanup(func() { heavyPool = was })
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
