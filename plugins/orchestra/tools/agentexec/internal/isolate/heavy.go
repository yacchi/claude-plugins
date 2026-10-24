package isolate

import (
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
)

// A worktree checkout or deletion writes the whole tree, as heavy as any
// install: on a large repository a dozen at once saturate the disk. So each
// takes one slot of the machine-wide heavy pool (`throttle.heavy_slots`)
// while it runs, and only while it runs: never across carry, adoption or
// anything else that may take a slot itself, which would deadlock a pool of
// one inside one process.

// heavyTTL bounds a lease: a checkout of a large tree on a loaded machine
// can take long, and the owner's death frees the slot anyway.
const heavyTTL = time.Hour

// HoldHeavy waits for one heavy slot for op and returns its release (safe to
// call more than once); see slot.HoldHeavy.
func HoldHeavy(op string) (release func()) { return slot.HoldHeavy(op, heavyTTL) }
