package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// Probe is one backend's availability on this host.
type Probe struct {
	OK     bool
	Detail string
	Path   string // bwrap: the binary that passed the probe
	ABI    int    // landlock only
}

// ProbeReport is every backend's probe, as doctor and `sandbox probe`
// print it.
type ProbeReport struct {
	Seatbelt, Bwrap, Landlock Probe
}

// MarshalJSON keeps Python's shape: {"ok", "detail"} plus "path" for a
// passing bwrap and "abi" for landlock.
func (r ProbeReport) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(r.Object())
}

// Object is the report as an ordered JSON object.
func (r ProbeReport) Object() *jsonx.Object {
	one := func(p Probe, landlock bool) *jsonx.Object {
		o := jsonx.NewObject().Set("ok", p.OK).Set("detail", p.Detail)
		if p.Path != "" {
			o.Set("path", p.Path)
		}
		if landlock {
			o.Set("abi", int64(p.ABI))
		}
		return o
	}
	return jsonx.NewObject().
		Set("seatbelt", one(r.Seatbelt, false)).
		Set("bwrap", one(r.Bwrap, false)).
		Set("landlock", one(r.Landlock, true))
}

var (
	probeOnce   sync.Once
	probeCached ProbeReport
)

// Probes reports backend availability. Probed once per process; the
// ForceEnv hook is re-read on every call.
func Probes() ProbeReport {
	if os.Getenv(ForceEnv) == "none" {
		off := Probe{Detail: "forced unavailable by " + ForceEnv + "=none"}
		return ProbeReport{Seatbelt: off, Bwrap: off, Landlock: off}
	}
	probeOnce.Do(func() {
		probeCached = ProbeReport{
			Seatbelt: probeSeatbelt(),
			Bwrap:    probeBwrap(),
			Landlock: probeLandlock(),
		}
	})
	return probeCached
}

func runProbe(argv ...string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), stderr.String(), nil
	}
	return 0, stderr.String(), err
}

// truncRunes is Python's s[:n] on a str.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func probeSeatbelt() Probe {
	if runtime.GOOS != "darwin" {
		return Probe{Detail: "not macOS"}
	}
	if !exists(SandboxExec) {
		return Probe{Detail: SandboxExec + " not found"}
	}
	code, stderr, err := runProbe(SandboxExec, "-p", "(version 1)(allow default)", "--", "/usr/bin/true")
	if err != nil {
		return Probe{Detail: "probe failed: " + err.Error()}
	}
	if code != 0 {
		// e.g. agent-exec is itself already inside a sandbox
		return Probe{Detail: fmt.Sprintf("probe exited %d: %s", code, truncRunes(strings.TrimSpace(stderr), 200))}
	}
	return Probe{OK: true, Detail: "sandbox-exec probe ok"}
}

func probeBwrap() Probe {
	if runtime.GOOS != "linux" {
		return Probe{Detail: "not Linux"}
	}
	exe, err := exec.LookPath("bwrap")
	if err != nil {
		return Probe{Detail: "bwrap not on PATH"}
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	code, stderr, err := runProbe(exe, "--ro-bind", "/", "/", "true")
	if err != nil {
		return Probe{Detail: "probe failed: " + err.Error()}
	}
	if code != 0 {
		// Ubuntu >= 24.04: AppArmor restricts unprivileged user namespaces.
		return Probe{Detail: fmt.Sprintf("probe exited %d: %s", code, truncRunes(strings.TrimSpace(stderr), 200))}
	}
	return Probe{OK: true, Detail: "bwrap probe ok", Path: exe}
}

func probeLandlock() Probe {
	if runtime.GOOS != "linux" {
		return Probe{Detail: "not Linux"}
	}
	abi := LandlockABI()
	if abi <= 0 {
		return Probe{Detail: "kernel reports no Landlock support"}
	}
	return Probe{OK: true, ABI: abi, Detail: fmt.Sprintf("Landlock ABI %d", abi)}
}

// backendDetail is "seatbelt: ...; bwrap: ...[; landlock: ...]".
func backendDetail(r ProbeReport, withLandlock bool) string {
	parts := []string{"seatbelt: " + r.Seatbelt.Detail, "bwrap: " + r.Bwrap.Detail}
	if withLandlock {
		parts = append(parts, "landlock: "+r.Landlock.Detail)
	}
	return strings.Join(parts, "; ")
}
