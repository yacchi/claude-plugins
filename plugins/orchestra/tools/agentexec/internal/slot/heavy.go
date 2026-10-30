package slot

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Configured is the machine-wide pool name (Heavy or Test) sized by
// config.PoolSize (cfg nil: the `auto` size), for the CLI and for every
// holder in this process.
func Configured(cfg *config.Resolved, name string) Pool {
	return New(name, config.PoolSize(cfg, name))
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
