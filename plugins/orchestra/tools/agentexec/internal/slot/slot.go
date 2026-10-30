// Package slot is the machine-wide admission pools for heavy local commands
// (installs, tests, typechecks, lints, builds). Past about five at once they
// all slow down several times over, so every worker's heavy command and every
// check command takes one slot of a pool while it runs, whatever wave or
// session it belongs to: tests, lints and typechecks on the test pool, so
// they are capped apart from installs and builds on the heavy pool.
//
// A held slot is a lease file in <slot dir>/<pool>/ naming its owner pid,
// start and expiry. Leases are counted and taken under a short flock on the
// pool; a lease whose owner is dead or whose expiry passed is reclaimed by
// the next acquirer, so a killed worker never shrinks the pool for long.
package slot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// The machine-wide pools: Test admits test, lint and typecheck runs, Heavy
// every other heavy command (installs, builds) and agent-exec's worktree I/O.
const (
	Heavy = config.PoolHeavy
	Test  = config.PoolTest
)

// DefaultTTL is how long a lease lives when the acquirer names no TTL.
const DefaultTTL = 15 * time.Minute

const (
	leaseSuffix = ".lease"
	lockTimeout = 10 * time.Second
	pollMin     = 200 * time.Millisecond
	pollJitter  = 200 * time.Millisecond
)

// ErrOff is returned by Acquire when the pool admits everything at once
// (size 0, or slots disabled in a test binary).
var ErrOff = errors.New("slot pool is off")

// ErrOwnerGone is returned by Acquire when the owner died while waiting.
var ErrOwnerGone = errors.New("slot owner is gone")

var tokenRE = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// Root is where the pools live: $ORCHESTRA_SLOT_DIR, else <state dir>/slots
// (machine-wide, shared by every wave and session).
func Root() string {
	if d := os.Getenv("ORCHESTRA_SLOT_DIR"); d != "" {
		return d
	}
	return filepath.Join(paths.StateDir(), "slots")
}

// Enabled is false inside a test binary unless ORCHESTRA_SLOT_DIR names
// where to keep leases: the suite runs thousands of checks that must not
// queue behind (or hold up) the machine's real workers.
func Enabled() bool {
	return !testing.Testing() || os.Getenv("ORCHESTRA_SLOT_DIR") != ""
}

// Pool is one named pool of Size slots (0 = off).
type Pool struct {
	Name string
	Dir  string
	Size int
}

// New is the pool name of size under Root.
func New(name string, size int) Pool {
	return Pool{Name: name, Dir: filepath.Join(Root(), name), Size: size}
}

// Off reports whether Acquire admits at once.
func (p Pool) Off() bool { return p.Size <= 0 || !Enabled() }

// Lease is one held slot. Start and Expires are Unix seconds.
type Lease struct {
	Token   string  `json:"token"`
	PID     int     `json:"pid"`
	Start   float64 `json:"start"`
	Expires float64 `json:"expires"`
	Op      string  `json:"op,omitempty"`
}

// Request is what an acquirer asks for: Owner is the pid whose death frees
// the slot, TTL the longest it may hold it (0 = DefaultTTL), Op what it runs.
type Request struct {
	Owner int
	TTL   time.Duration
	Op    string
}

// Acquire blocks until a slot is held and returns its lease and how long it
// waited. It gives up with ctx's error when ctx ends first, and with
// ErrOwnerGone when the owner dies first. The wait is written to the trace
// as a lock record (op "slot <pool>", detail the request's op).
func (p Pool) Acquire(ctx context.Context, r Request) (Lease, time.Duration, error) {
	if p.Off() {
		return Lease{}, 0, ErrOff
	}
	if r.TTL <= 0 {
		r.TTL = DefaultTTL
	}
	start := time.Now()
	end := trace.Begin(trace.KindLock, "slot "+p.Name, trace.Attrs{Detail: r.Op})
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		end(-1)
		return Lease{}, 0, err
	}
	for {
		lease, ok, err := p.try(r)
		if err != nil {
			end(-1)
			return Lease{}, time.Since(start), err
		}
		if ok {
			end(0)
			return lease, time.Since(start), nil
		}
		if !fsutil.PIDAlive(r.Owner) {
			end(1)
			return Lease{}, time.Since(start), ErrOwnerGone
		}
		select {
		case <-ctx.Done():
			end(1)
			return Lease{}, time.Since(start), ctx.Err()
		case <-time.After(pollMin + jitter()):
		}
	}
}

func jitter() time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(pollJitter)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}

// try takes a slot if one is free, under the pool lock.
func (p Pool) try(r Request) (Lease, bool, error) {
	unlock, err := fsutil.Lock(filepath.Join(p.Dir, ".lock"), fsutil.Exclusive, lockTimeout)
	if err != nil {
		return Lease{}, false, err
	}
	defer func() { _ = unlock() }()
	if len(p.live(time.Now())) >= p.Size {
		return Lease{}, false, nil
	}
	token, err := newToken()
	if err != nil {
		return Lease{}, false, err
	}
	now := time.Now()
	lease := Lease{
		Token:   token,
		PID:     r.Owner,
		Start:   epoch(now),
		Expires: epoch(now.Add(r.TTL)),
		Op:      r.Op,
	}
	data, err := json.Marshal(lease)
	if err != nil {
		return Lease{}, false, err
	}
	// Every reader holds the pool lock, so a plain write is never seen
	// half-done; no fsync: a lease is worthless after a reboot anyway.
	if err := os.WriteFile(filepath.Join(p.Dir, token+leaseSuffix), data, 0o600); err != nil {
		return Lease{}, false, err
	}
	return lease, true, nil
}

// live reads the pool's leases, deleting those whose owner is dead or whose
// expiry passed. The caller holds the pool lock (or only reads).
func (p Pool) live(now time.Time) []Lease {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return nil
	}
	var out []Lease
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, leaseSuffix) || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(p.Dir, name)
		var l Lease
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if json.Unmarshal(data, &l) != nil || l.Expires < epoch(now) || !fsutil.PIDAlive(l.PID) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// Leases are the pool's live leases, oldest first (stale ones reclaimed).
func (p Pool) Leases() ([]Lease, error) {
	if _, err := os.Stat(p.Dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	unlock, err := fsutil.Lock(filepath.Join(p.Dir, ".lock"), fsutil.Exclusive, lockTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	return p.live(time.Now()), nil
}

// Release frees the slot token holds; an unknown or already released token
// is not an error.
func (p Pool) Release(token string) error {
	if !tokenRE.MatchString(token) {
		return nil
	}
	err := os.Remove(filepath.Join(p.Dir, token+leaseSuffix))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Release frees token in whichever pool holds it (see Pool.Release).
func Release(token string) error {
	if !tokenRE.MatchString(token) {
		return nil
	}
	entries, _ := os.ReadDir(Root())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := (Pool{Dir: filepath.Join(Root(), e.Name())}).Release(token); err != nil {
			return err
		}
	}
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func epoch(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}
