package slot

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// SizeKey is the `throttle` key that sizes the pool name: `test_slots` for
// Test, else `heavy_slots`.
func SizeKey(name string) string {
	if name == Test {
		return "test_slots"
	}
	return "heavy_slots"
}

// Configured is the machine-wide pool name (Heavy or Test) sized by its
// SizeKey in cfg (nil: the `auto` size). This is the one place a pool's size
// is read, for the CLI and for every holder in this process.
func Configured(cfg *config.Resolved, name string) Pool {
	auto := AutoSize(runtime.NumCPU())
	if name == Test {
		auto = TestAutoSize(runtime.NumCPU())
	}
	v, _ := cfg.Lookup("throttle", SizeKey(name))
	return New(name, sizeOf(v, auto))
}

var processConfig = sync.OnceValue(func() *config.Resolved {
	cfg, err := config.Resolve()
	if err != nil {
		return nil
	}
	return cfg
})

// HeavyPool and TestPool are the machine-wide pools as this process's config
// sizes them, read once (a test seam: tests swap in pools of their own).
var (
	HeavyPool = sync.OnceValue(func() Pool { return Configured(processConfig(), Heavy) })
	TestPool  = sync.OnceValue(func() Pool { return Configured(processConfig(), Test) })
)

// Hold waits for one slot of the machine-wide pool name (Heavy or Test) for
// op, owned by this process for at most ttl, and returns its release (safe
// to call more than once). Waiting is not an error, and any slot failure
// admits op at once: admission must never fail the work it throttles.
func Hold(name, op string, ttl time.Duration) (release func()) {
	p := HeavyPool()
	if name == Test {
		p = TestPool()
	}
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

// HoldHeavy is Hold on the heavy pool.
func HoldHeavy(op string, ttl time.Duration) (release func()) {
	return Hold(Heavy, op, ttl)
}
