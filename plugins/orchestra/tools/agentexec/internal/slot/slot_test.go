package slot

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPool(t *testing.T, size int) Pool {
	t.Helper()
	t.Setenv("ORCHESTRA_SLOT_DIR", t.TempDir())
	return New(Heavy, size)
}

func acquireWithin(t *testing.T, p Pool, r Request, d time.Duration) (Lease, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	l, _, err := p.Acquire(ctx, r)
	return l, err
}

// A killed owner's slot goes back to the pool at the next acquire.
func TestDeadOwnersLeaseIsReclaimed(t *testing.T) {
	p := testPool(t, 1)
	owner := exec.Command("sleep", "60")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireWithin(t, p, Request{Owner: owner.Process.Pid, Op: "pnpm test"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireWithin(t, p, Request{Owner: os.Getpid()}, 500*time.Millisecond); err == nil {
		t.Fatal("a second slot was handed out while the owner lives")
	}
	_ = owner.Process.Kill()
	_ = owner.Wait()
	if _, err := acquireWithin(t, p, Request{Owner: os.Getpid()}, 3*time.Second); err != nil {
		t.Fatalf("the dead owner's slot was not reclaimed: %v", err)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	p := testPool(t, 1)
	if _, err := acquireWithin(t, p, Request{Owner: os.Getpid(), TTL: 100 * time.Millisecond}, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireWithin(t, p, Request{Owner: os.Getpid()}, 3*time.Second); err != nil {
		t.Fatalf("the expired lease was not reclaimed: %v", err)
	}
}

// N+3 acquirers against N slots: never more than N held at once, and every
// one is admitted in the end.
func TestContentionNeverExceedsTheSize(t *testing.T) {
	const size = 2
	p := testPool(t, size)
	var held, peak, admitted atomic.Int32
	var wg sync.WaitGroup
	for range size + 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := acquireWithin(t, p, Request{Owner: os.Getpid()}, 20*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			n := held.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
			held.Add(-1)
			admitted.Add(1)
			_ = p.Release(l.Token)
		}()
	}
	wg.Wait()
	if peak.Load() > size {
		t.Errorf("%d slots held at once, pool size %d", peak.Load(), size)
	}
	if admitted.Load() != size+3 {
		t.Errorf("admitted %d of %d", admitted.Load(), size+3)
	}
}
