// Package proctree measures the process tree under one pid, waits for it to
// outgrow its limits and kills it, so a runaway worker (a fork bomb, a
// memory hog) is stopped before it takes the machine down. It reads the
// process table only through the platform seams and never starts a
// process: under a fork bomb the table is full and a fork fails.
package proctree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"
)

// Proc is one process: its pid, parent pid, process group id, and memory in
// bytes (macOS: phys_footprint, resident plus compressed; Linux: resident
// set size; 0 when unreadable).
type Proc struct {
	PID, PPID, PGID int
	Mem             int64
}

// Table is one snapshot of the machine's process table, in no particular order.
type Table []Proc

// The platform readers, set by the platform file's init(); tests replace them
// and restore them with t.Cleanup. Snapshot nil: no reader on this platform.
// PhysicalMemory nil, or 0: unknown (the memory check is then off).
var (
	Snapshot       func() (Table, error)
	PhysicalMemory func() int64
)

// Signal sends sig to the one process pid. KillTree and KillChildTree send
// every signal through it, so tests replace it with a recorder (restored with
// t.Cleanup) and never signal a made-up pid.
var Signal = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// Usage is the tree under Root: Procs counts Root's descendants (Root itself
// excluded); Mem sums the Mem of Root and of its descendants, in bytes.
type Usage struct {
	Root, Procs int
	Mem         int64
}

// Limits are the trip points; a zero field disables its check.
type Limits struct {
	MaxProcs int
	MaxMem   int64
}

const (
	MaxProcs        = 200             // descendants of the root
	MemoryDivisor   = 4               // MaxMem = physical memory / MemoryDivisor (a quarter)
	DefaultInterval = 2 * time.Second // one measurement per tick
)

var (
	ErrUnsupported = errors.New("proctree: process table not readable on this platform") // Snapshot is nil; callers run unguarded
	ErrNoRoot      = errors.New("proctree: root process is gone")                        // root is not in the table
)

// coreFreezePasses bounds the freeze: a tree that keeps forking faster than
// it is stopped must not keep KillTree from ever reaching the kill.
const coreFreezePasses = 10

// DefaultLimits are MaxProcs descendants and a quarter of physical memory;
// MaxMem is 0 (off) when PhysicalMemory is nil or does not know the size.
func DefaultLimits() Limits {
	l := Limits{MaxProcs: MaxProcs}
	if PhysicalMemory != nil {
		if m := PhysicalMemory(); m > 0 {
			l.MaxMem = m / MemoryDivisor
		}
	}
	return l
}

// Exceeded reports whether u is over l, the process count checked before
// memory, each with a strict ">"; a field <= 0 disables its check. The text
// names the limit that tripped.
func (l Limits) Exceeded(u Usage) (string, bool) {
	if l.MaxProcs > 0 && u.Procs > l.MaxProcs {
		return fmt.Sprintf("process guard: %d processes under pid %d (limit %d)", u.Procs, u.Root, l.MaxProcs), true
	}
	if l.MaxMem > 0 && u.Mem > l.MaxMem {
		return fmt.Sprintf("process guard: %s of memory under pid %d (limit %s)", coreGiB(u.Mem), u.Root, coreGiB(l.MaxMem)), true
	}
	return "", false
}

func coreGiB(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// Measure takes one Snapshot and sums the tree under root. It returns
// ErrUnsupported when there is no reader, ErrNoRoot when root is not in the
// table, and Snapshot's own error as is.
func Measure(root int) (Usage, error) {
	if Snapshot == nil {
		return Usage{}, ErrUnsupported
	}
	t, err := Snapshot()
	if err != nil {
		return Usage{}, err
	}
	u, ok := MeasureIn(t, root)
	if !ok {
		return Usage{}, ErrNoRoot
	}
	return u, nil
}

// MeasureIn sums the tree under root in t exactly as Measure does on a fresh
// Snapshot (Procs: root's descendants; Mem: root plus descendants); ok is
// false when root is not in t. Measure becomes Snapshot + MeasureIn.
func MeasureIn(t Table, root int) (u Usage, ok bool) {
	byPID, members := coreTree(t, root)
	r, ok := byPID[root]
	if !ok {
		return Usage{}, false
	}
	u = Usage{Root: root, Procs: len(members), Mem: r.Mem}
	for _, pid := range members {
		u.Mem += byPID[pid].Mem
	}
	return u, true
}

// Children are the pids in t whose PPID is owner, ascending; owner itself
// and pids <= 1 are left out.
func Children(t Table, owner int) []int {
	var out []int
	for _, p := range t {
		if p.PPID == owner && p.PID != owner && p.PID > 1 {
			out = append(out, p.PID)
		}
	}
	slices.Sort(out)
	return out
}

// Wait measures the tree under root at once, then once every every
// (DefaultInterval when every <= 0), and returns (u, text, true) as soon as
// l trips. It never signals anything: the caller acts on the trip. It
// returns zero values when root is gone, when the table cannot be read on
// this platform, or when ctx is done; any other Measure error skips that
// tick.
func Wait(ctx context.Context, root int, l Limits, every time.Duration) (Usage, string, bool) {
	if every <= 0 {
		every = DefaultInterval
	}
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		if ctx.Err() != nil {
			return Usage{}, "", false
		}
		u, err := Measure(root)
		switch {
		case errors.Is(err, ErrNoRoot), errors.Is(err, ErrUnsupported):
			return Usage{}, "", false
		case err != nil:
			// A transient read failure is no reason to stop guarding.
		default:
			if text, ok := l.Exceeded(u); ok {
				return u, text, true
			}
		}
		select {
		case <-ctx.Done():
			return Usage{}, "", false
		case <-tk.C:
		}
	}
}

// KillTree kills the members of the tree under root, and root too when
// includeRoot, never signalling pid <= 1, this process, its ancestors, its
// own process group, or root unless includeRoot. It freezes the tree with
// SIGSTOP first, kills with SIGKILL and resumes with SIGCONT whatever it
// stopped but could not kill. It returns how many SIGKILLs succeeded; an
// error when a SIGKILL failed other than with ESRCH, or ErrUnsupported,
// ErrNoRoot or Snapshot's error before anything was signalled. KillChildTree
// shares this body.
func KillTree(root int, includeRoot bool) (int, error) {
	return coreKill(root, includeRoot, 0)
}

// KillChildTree kills the tree under child, child itself spared, as
// KillTree(child, false) does (SIGSTOP freeze passes, SIGKILL, SIGCONT for
// what was stopped but not killed), and never signals owner, an ancestor of
// owner, or any other pid whose PPID is owner, in any pass. It returns and
// errors as KillTree: ErrUnsupported, ErrNoRoot when child is not in the
// table, else the count of successful SIGKILLs.
func KillChildTree(owner, child int) (int, error) {
	return coreKill(child, false, owner)
}

// coreKill is the body of KillTree and KillChildTree; owner > 0 adds the
// owner protection of coreKillable to every scan.
func coreKill(root int, includeRoot bool, owner int) (int, error) {
	if Snapshot == nil {
		return 0, ErrUnsupported
	}
	t, err := Snapshot()
	if err != nil {
		return 0, err
	}
	if !slices.ContainsFunc(t, func(p Proc) bool { return p.PID == root }) {
		return 0, ErrNoRoot
	}
	s := coreKillable(t, root, includeRoot, owner)
	inS := make(map[int]bool, len(s))
	for _, pid := range s {
		inS[pid] = true
	}

	// Freeze before the kill: a stopped process cannot fork, so a fork bomb
	// cannot refill the tree between the scan and the SIGKILLs. Each pass
	// rescans for children forked before their parent was stopped.
	stopped := map[int]bool{}
	sent := 0
	for range coreFreezePasses {
		for _, pid := range s[sent:] {
			if Signal(pid, syscall.SIGSTOP) == nil {
				stopped[pid] = true
			}
		}
		sent = len(s)
		nt, err := Snapshot()
		if err != nil {
			break
		}
		added := false
		for _, pid := range coreKillable(nt, root, false, owner) {
			if !inS[pid] {
				inS[pid] = true
				s = append(s, pid)
				added = true
			}
		}
		if !added {
			break
		}
	}

	killed, failed := 0, 0
	var first error
	var resume []int
	for _, pid := range s {
		err := Signal(pid, syscall.SIGKILL)
		switch {
		case err == nil:
			killed++
		case errors.Is(err, syscall.ESRCH):
			// Already gone: neither killed by us nor a failure.
		default:
			failed++
			if first == nil {
				first = err
			}
			if stopped[pid] {
				resume = append(resume, pid)
			}
		}
	}
	// Nothing is left stopped: a process we froze but could not kill would
	// otherwise hang forever.
	for _, pid := range resume {
		_ = Signal(pid, syscall.SIGCONT)
	}
	if failed > 0 {
		return killed, fmt.Errorf("proctree: could not kill %d of %d processes: %w", failed, len(s), first)
	}
	return killed, nil
}

// coreTree indexes t by pid and returns the members of the tree under root,
// ascending: the closure over children by PPID and over process groups by
// PGID (an orphan reparented to launchd keeps its group), root excluded. A
// breadth-first walk makes the table order irrelevant.
func coreTree(t Table, root int) (byPID map[int]Proc, members []int) {
	byPID = make(map[int]Proc, len(t))
	byPPID := map[int][]int{}
	byPGID := map[int][]int{}
	for _, p := range t {
		byPID[p.PID] = p
		byPPID[p.PPID] = append(byPPID[p.PPID], p.PID)
		byPGID[p.PGID] = append(byPGID[p.PGID], p.PID)
	}
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, idx := range []map[int][]int{byPPID, byPGID} {
			for _, pid := range idx[cur] {
				if !seen[pid] {
					seen[pid] = true
					members = append(members, pid)
					queue = append(queue, pid)
				}
			}
		}
	}
	slices.Sort(members)
	return byPID, members
}

// coreProtectChain marks pid and its ancestors in byPID, walking PPIDs up and
// stopping at a pid not in byPID, at PPID <= 1, or at a PPID already marked.
func coreProtectChain(byPID map[int]Proc, pid int, protected map[int]bool) {
	protected[pid] = true
	for cur := pid; ; {
		p, ok := byPID[cur]
		if !ok || p.PPID <= 1 || protected[p.PPID] {
			break
		}
		protected[p.PPID] = true
		cur = p.PPID
	}
}

// coreKillable is what KillTree may signal in t: the members under root,
// plus root when includeRoot and root is in t, minus the protected pids,
// ascending. Protected are pid <= 1, this process, its ancestors in t, every
// process of this process's group, and root unless includeRoot: killing any
// of them would take the guard itself (or the machine) down. owner > 0
// (KillChildTree) also protects the owner, its ancestors and every child of
// the owner in t, so a tree that shares the child's process group cannot pull
// them in.
func coreKillable(t Table, root int, includeRoot bool, owner int) []int {
	byPID, members := coreTree(t, root)
	protected := map[int]bool{}
	coreProtectChain(byPID, os.Getpid(), protected)
	if owner > 0 {
		coreProtectChain(byPID, owner, protected)
		for _, pid := range Children(t, owner) {
			protected[pid] = true
		}
	}
	pgrp := syscall.Getpgrp()
	for _, p := range t {
		if p.PGID == pgrp {
			protected[p.PID] = true
		}
	}
	cands := members
	if _, ok := byPID[root]; ok && includeRoot {
		cands = append(slices.Clone(members), root)
	} else {
		protected[root] = true
	}
	var out []int
	for _, pid := range cands {
		if pid > 1 && !protected[pid] {
			out = append(out, pid)
		}
	}
	slices.Sort(out)
	return out
}
