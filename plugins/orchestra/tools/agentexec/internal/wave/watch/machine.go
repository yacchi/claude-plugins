package watch

import (
	"runtime"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sysload"
)

// Machine is the `machine` object of `wave wait`: how loaded the machine
// is. Each field is probed on its own and degrades alone: a load or pool
// count that cannot be read is -1, a memory pressure "unknown", git_procs
// -1.
type Machine struct {
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	CPUs        int     `json:"cpus"`
	MemPressure string  `json:"mem_pressure"`
	Heavy       PoolUse `json:"heavy"`
	Test        PoolUse `json:"test"`
	GitProcs    int     `json:"git_procs"`
}

// PoolUse is one slot pool's live leases (Used, -1 when unreadable) of
// Size (0 = the pool is off).
type PoolUse struct {
	Used int `json:"used"`
	Size int `json:"size"`
}

// probes are the machine reads (a test seam). load reports ok=false when
// the load averages are unreadable, gitProcs -1 when the process table is.
type probes struct {
	load     func() (load1, load5 float64, ok bool)
	pressure func() string
	gitProcs func() int
}

var machineProbes = probes{
	load:     sysload.Load,
	pressure: func() string { return sysload.MemoryPressure().Level },
	gitProcs: sysload.GitProcs,
}

// ProbeMachine reads the machine's load, memory pressure, the use of the
// heavy and test slot pools, and how many git processes run. It never
// shells out (internal/sysload).
func ProbeMachine(heavy, test slot.Pool) Machine {
	return machineProbes.machine(heavy, test)
}

func (p probes) machine(heavy, test slot.Pool) Machine {
	m := Machine{Load1: -1, Load5: -1, CPUs: runtime.NumCPU()}
	if l1, l5, ok := p.load(); ok {
		m.Load1, m.Load5 = l1, l5
	}
	m.MemPressure = p.pressure()
	m.Heavy, m.Test = poolUse(heavy), poolUse(test)
	m.GitProcs = p.gitProcs()
	return m
}

// UnknownMachine is a Machine with nothing probed.
func UnknownMachine() Machine {
	return Machine{
		Load1: -1, Load5: -1, CPUs: runtime.NumCPU(), MemPressure: sysload.Unknown,
		Heavy: PoolUse{Used: -1}, Test: PoolUse{Used: -1}, GitProcs: -1,
	}
}

func poolUse(p slot.Pool) PoolUse {
	u := PoolUse{Size: max(p.Size, 0)}
	leases, err := p.Leases()
	if err != nil {
		u.Used = -1
		return u
	}
	u.Used = len(leases)
	return u
}
