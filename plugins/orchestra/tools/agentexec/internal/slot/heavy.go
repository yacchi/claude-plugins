package slot

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// HeavyPool is the machine-wide heavy pool sized by `throttle.heavy_slots`,
// read once per process (a test seam: tests swap in a pool of their own).
var HeavyPool = sync.OnceValue(func() Pool {
	var size any
	if cfg, err := config.Resolve(); err == nil {
		size, _ = cfg.Lookup("throttle", "heavy_slots")
	}
	return New(Heavy, SizeOf(size))
})

// HoldHeavy waits for one slot of HeavyPool for op, owned by this process
// for at most ttl, and returns its release (safe to call more than once).
// Waiting is not an error, and any slot failure admits op at once:
// admission must never fail the work it throttles.
func HoldHeavy(op string, ttl time.Duration) (release func()) {
	p := HeavyPool()
	if p.Off() {
		return func() {}
	}
	lease, _, err := p.Acquire(context.Background(), Request{Owner: os.Getpid(), TTL: ttl, Op: op})
	if err != nil {
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(func() { _ = p.Release(lease.Token) }) }
}
