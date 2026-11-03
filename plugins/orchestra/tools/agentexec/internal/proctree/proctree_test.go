package proctree

import (
	"context"
	"errors"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"
)

// Made-up pids start at 5000000: real pids stay below 100000 on macOS and
// at most 4194304 on Linux, so none is this process, an ancestor or its
// group.
const coreRoot = 5000001

var coreBoom = errors.New("table unreadable")

func coreTable(kids ...int) Table {
	t := Table{{coreRoot, 5000000, coreRoot, 0}}
	for _, k := range kids {
		t = append(t, Proc{k, coreRoot, coreRoot, 0})
	}
	return t
}

// coreSnapshots makes Snapshot answer step(n) on call n (from 1); the
// returned int counts the calls. step nil sets Snapshot to nil.
func coreSnapshots(t *testing.T, step func(n int) (Table, error)) *int {
	t.Helper()
	old := Snapshot
	t.Cleanup(func() { Snapshot = old })
	calls := new(int)
	if step == nil {
		Snapshot = nil
		return calls
	}
	Snapshot = func() (Table, error) {
		*calls++
		return step(*calls)
	}
	return calls
}

func coreSeq(ts ...Table) func(int) (Table, error) {
	return func(n int) (Table, error) {
		return ts[min(n, len(ts))-1], nil
	}
}

type coreSig struct {
	pid int
	sig syscall.Signal
}

// coreSignals replaces Signal with a recorder, so no test ever signals a
// real process.
func coreSignals(t *testing.T, answers map[coreSig]error) *[]coreSig {
	t.Helper()
	old := Signal
	t.Cleanup(func() { Signal = old })
	calls := &[]coreSig{}
	Signal = func(pid int, sig syscall.Signal) error {
		c := coreSig{pid, sig}
		*calls = append(*calls, c)
		return answers[c]
	}
	return calls
}

func coreNoSignals(t *testing.T, got *[]coreSig) {
	t.Helper()
	if len(*got) != 0 {
		t.Errorf("signals = %v, want none", *got)
	}
}

func coreWantSignals(t *testing.T, got *[]coreSig, want []coreSig) {
	t.Helper()
	if !slices.Equal(*got, want) {
		t.Errorf("signals = %v, want %v", *got, want)
	}
}

func TestCoreMeasureClosure(t *testing.T) {
	sigs := coreSignals(t, nil)
	tbl := Table{
		{5000003, 5000002, 5000002, 20},
		{5000002, coreRoot, 5000002, 10},
		{coreRoot, 5000000, coreRoot, 100},
		{5000004, 1, coreRoot, 30},
		{5000006, 1, 5000005, 50},
		{5000005, coreRoot, 5000005, 40},
		{5000010, 1, 5000010, 1000},
		{1, 0, 1, 7},
	}
	coreSnapshots(t, coreSeq(tbl))
	u, err := Measure(coreRoot)
	if err != nil || u != (Usage{Root: coreRoot, Procs: 5, Mem: 250}) {
		t.Errorf("Measure = %+v, %v", u, err)
	}
	coreNoSignals(t, sigs)
}

func TestCoreMeasureErrors(t *testing.T) {
	sigs := coreSignals(t, nil)
	cases := []struct {
		name string
		step func(int) (Table, error)
		want error
	}{
		{"nil", nil, ErrUnsupported},
		{"boom", func(int) (Table, error) { return nil, coreBoom }, coreBoom},
		{"noroot", coreSeq(Table{{5000002, 1, 5000002, 0}}), ErrNoRoot},
	}
	for _, c := range cases {
		coreSnapshots(t, c.step)
		u, err := Measure(coreRoot)
		if err != c.want || u != (Usage{}) {
			t.Errorf("%s: Measure = %+v, %v; want %v", c.name, u, err, c.want)
		}
	}
	coreNoSignals(t, sigs)
}

func TestCoreExceeded(t *testing.T) {
	def := Limits{MaxProcs: 200, MaxMem: 12 << 30}
	cases := []struct {
		l     Limits
		procs int
		mem   int64
		text  string
		ok    bool
	}{
		{def, 201, 13 << 30, "process guard: 201 processes under pid 4242 (limit 200)", true},
		{def, 200, 12 << 30, "", false},
		{def, 10, 13 << 30, "process guard: 13.0 GiB of memory under pid 4242 (limit 12.0 GiB)", true},
		{Limits{MaxMem: 3 << 29}, 0, 2 << 30, "process guard: 2.0 GiB of memory under pid 4242 (limit 1.5 GiB)", true},
		{Limits{}, 10000, 1 << 40, "", false},
	}
	for _, c := range cases {
		text, ok := c.l.Exceeded(Usage{Root: 4242, Procs: c.procs, Mem: c.mem})
		if text != c.text || ok != c.ok {
			t.Errorf("%+v.Exceeded(%d, %d) = %q, %v; want %q, %v", c.l, c.procs, c.mem, text, ok, c.text, c.ok)
		}
	}
}

func TestCoreDefaultLimits(t *testing.T) {
	old := PhysicalMemory
	t.Cleanup(func() { PhysicalMemory = old })
	PhysicalMemory = func() int64 { return 48 << 30 }
	if got := DefaultLimits(); got != (Limits{MaxProcs: MaxProcs, MaxMem: 12 << 30}) {
		t.Errorf("48 GiB: DefaultLimits = %+v", got)
	}
	for _, pm := range []func() int64{nil, func() int64 { return 0 }, func() int64 { return -4096 }} {
		PhysicalMemory = pm
		if got := DefaultLimits(); got != (Limits{MaxProcs: MaxProcs}) {
			t.Errorf("unknown memory: DefaultLimits = %+v", got)
		}
	}
}

func TestCoreWait(t *testing.T) {
	sigs := coreSignals(t, nil)
	l := Limits{MaxProcs: 2}
	trip := Usage{Root: coreRoot, Procs: 3}
	const text = "process guard: 3 processes under pid 5000001 (limit 2)"
	big := coreTable(5000002, 5000003, 5000004)
	cases := []struct {
		name  string
		step  func(int) (Table, error)
		every time.Duration
		u     Usage
		text  string
		ok    bool
		calls int
	}{
		{"at once", coreSeq(big), time.Hour, trip, text, true, 1},
		{"grows", coreSeq(coreTable(5000002), big), time.Millisecond, trip, text, true, 2},
		{"skips error", func(n int) (Table, error) {
			if n == 1 {
				return nil, coreBoom
			}
			return big, nil
		}, time.Millisecond, trip, text, true, 2},
		{"noroot", coreSeq(Table{{5000002, 1, 5000002, 0}}), time.Millisecond, Usage{}, "", false, 1},
		{"unsupported", nil, time.Millisecond, Usage{}, "", false, 0},
	}
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		calls := coreSnapshots(t, c.step)
		u, text, ok := Wait(ctx, coreRoot, l, c.every)
		cancel()
		if u != c.u || text != c.text || ok != c.ok || *calls != c.calls {
			t.Errorf("%s: Wait = %+v, %q, %v after %d calls; want %+v, %q, %v after %d",
				c.name, u, text, ok, *calls, c.u, c.text, c.ok, c.calls)
		}
	}

	// A cancel while Wait sleeps between ticks ends it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	calls := coreSnapshots(t, func(int) (Table, error) {
		cancel()
		return coreTable(5000002), nil
	})
	type res struct {
		u    Usage
		text string
		ok   bool
	}
	done := make(chan res, 1)
	go func() {
		u, text, ok := Wait(ctx, coreRoot, l, time.Hour)
		done <- res{u, text, ok}
	}()
	select {
	case r := <-done:
		if r != (res{}) || *calls != 1 {
			t.Errorf("cancelled: Wait = %+v after %d calls", r, *calls)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after its context was cancelled")
	}
	coreNoSignals(t, sigs)
}

func TestCoreKillTreeFreezesFirst(t *testing.T) {
	sigs := coreSignals(t, nil)
	calls := coreSnapshots(t, coreSeq(
		coreTable(5000002),
		append(coreTable(5000002), Proc{5000003, 5000002, coreRoot, 0}),
	))
	n, err := KillTree(coreRoot, true)
	if n != 3 || err != nil || *calls != 3 {
		t.Errorf("KillTree = %d, %v after %d calls", n, err, *calls)
	}
	coreWantSignals(t, sigs, []coreSig{
		{coreRoot, syscall.SIGSTOP}, {5000002, syscall.SIGSTOP}, {5000003, syscall.SIGSTOP},
		{coreRoot, syscall.SIGKILL}, {5000002, syscall.SIGKILL}, {5000003, syscall.SIGKILL},
	})
}

func TestCoreKillTreeSparesRoot(t *testing.T) {
	sigs := coreSignals(t, nil)
	coreSnapshots(t, coreSeq(append(coreTable(5000002), Proc{5000004, 1, coreRoot, 0})))
	n, err := KillTree(coreRoot, false)
	if n != 2 || err != nil {
		t.Errorf("KillTree = %d, %v", n, err)
	}
	coreWantSignals(t, sigs, []coreSig{
		{5000002, syscall.SIGSTOP}, {5000004, syscall.SIGSTOP},
		{5000002, syscall.SIGKILL}, {5000004, syscall.SIGKILL},
	})
}

func TestCoreKillTreeSnapshotFailsMidFreeze(t *testing.T) {
	sigs := coreSignals(t, nil)
	calls := coreSnapshots(t, func(n int) (Table, error) {
		if n == 1 {
			return coreTable(5000002, 5000003), nil
		}
		return nil, coreBoom
	})
	n, err := KillTree(coreRoot, true)
	if n != 3 || err != nil || *calls != 2 {
		t.Errorf("KillTree = %d, %v after %d calls", n, err, *calls)
	}
	coreWantSignals(t, sigs, []coreSig{
		{coreRoot, syscall.SIGSTOP}, {5000002, syscall.SIGSTOP}, {5000003, syscall.SIGSTOP},
		{coreRoot, syscall.SIGKILL}, {5000002, syscall.SIGKILL}, {5000003, syscall.SIGKILL},
	})
}

func TestCoreKillTreeFailures(t *testing.T) {
	sigs := coreSignals(t, map[coreSig]error{
		{5000002, syscall.SIGKILL}: syscall.ESRCH,
		{5000003, syscall.SIGKILL}: syscall.EPERM,
		{5000004, syscall.SIGSTOP}: syscall.EPERM,
		{5000004, syscall.SIGKILL}: syscall.EPERM,
	})
	coreSnapshots(t, coreSeq(coreTable(5000002, 5000003, 5000004)))
	n, err := KillTree(coreRoot, true)
	const want = "proctree: could not kill 2 of 4 processes: operation not permitted"
	if n != 1 || err == nil || err.Error() != want {
		t.Errorf("KillTree = %d, %v; want 1, %q", n, err, want)
	}
	coreWantSignals(t, sigs, []coreSig{
		{5000001, syscall.SIGSTOP}, {5000002, syscall.SIGSTOP}, {5000003, syscall.SIGSTOP}, {5000004, syscall.SIGSTOP},
		{5000001, syscall.SIGKILL}, {5000002, syscall.SIGKILL}, {5000003, syscall.SIGKILL}, {5000004, syscall.SIGKILL},
		{5000003, syscall.SIGCONT},
	})
}

func TestCoreKillTreeProtects(t *testing.T) {
	sigs := coreSignals(t, nil)
	coreSnapshots(t, coreSeq(append(coreTable(1, 5000002, 5000004),
		Proc{os.Getpid(), 5000002, coreRoot, 0},
		Proc{5000003, coreRoot, syscall.Getpgrp(), 0},
	)))
	// Root and 5000002 are this process's ancestors here, so root is spared
	// though includeRoot.
	n, err := KillTree(coreRoot, true)
	if n != 1 || err != nil {
		t.Errorf("KillTree = %d, %v", n, err)
	}
	coreWantSignals(t, sigs, []coreSig{{5000004, syscall.SIGSTOP}, {5000004, syscall.SIGKILL}})
}

func TestCoreKillTreeBounded(t *testing.T) {
	sigs := coreSignals(t, nil)
	calls := coreSnapshots(t, func(n int) (Table, error) {
		var kids []int
		for k := 5000002; k <= 5000001+n; k++ {
			kids = append(kids, k)
		}
		return coreTable(kids...), nil
	})
	n, err := KillTree(coreRoot, false)
	if n != 11 || err != nil || *calls != 11 {
		t.Errorf("KillTree = %d, %v after %d calls", n, err, *calls)
	}
	var want []coreSig
	for k := 5000002; k <= 5000011; k++ {
		want = append(want, coreSig{k, syscall.SIGSTOP})
	}
	for k := 5000002; k <= 5000012; k++ {
		want = append(want, coreSig{k, syscall.SIGKILL})
	}
	coreWantSignals(t, sigs, want)
}

func TestCoreKillTreeErrors(t *testing.T) {
	sigs := coreSignals(t, nil)
	cases := []struct {
		name string
		step func(int) (Table, error)
		want error
	}{
		{"nil", nil, ErrUnsupported},
		{"boom", func(int) (Table, error) { return nil, coreBoom }, coreBoom},
		{"noroot", coreSeq(Table{{5000002, 1, 5000002, 0}}), ErrNoRoot},
	}
	for _, c := range cases {
		coreSnapshots(t, c.step)
		n, err := KillTree(coreRoot, true)
		if n != 0 || err != c.want {
			t.Errorf("%s: KillTree = %d, %v; want 0, %v", c.name, n, err, c.want)
		}
	}
	coreNoSignals(t, sigs)
}
