// Package host is the contract between `wave run` and the interactive hosts
// that run Claude-tier packages in live Claude Code sessions (herdr and
// Orca, subpackages herdr and orca), plus what both hosts share: the
// session registry file, the result file, screen classifiers, the
// `enabled` tri-state and the isolation flags of a hosted Claude.
//
// A package's session stays open across prompts so corrections go back to
// the SAME Claude. Completion is signalled only by the result file the
// prompt asks Claude to write; a settled TUI never means "done".
package host

import (
	"context"
	"time"
)

// Tri is a host's `enabled`: true, false or auto.
type Tri int

// The values of Tri.
const (
	TriAuto Tri = iota
	TriTrue
	TriFalse
)

func (t Tri) String() string {
	switch t {
	case TriTrue:
		return "true"
	case TriFalse:
		return "false"
	}
	return "auto"
}

// Need kinds: the package cannot continue in the host and the instructor
// has to step in.
const (
	KindTrust   = "trust"   // the folder-trust dialog needs a human once
	KindStalled = "stalled" // a dialog, a stalled prompt, a vanished agent
	KindTimeout = "timeout" // no result file in time
	KindError   = "error"   // anything else that failed
	KindLogin   = "login"   // the profile's Claude login expired or is missing
)

// NeedError is a package the host cannot carry on with. Kind is one of the
// Kind* constants; Detail is a short human-readable reason (often the
// screen tail). Data carries extras: "network_denials" ([]any of
// {"host", "port"} objects) when the session hit the network allowlist.
type NeedError struct {
	Kind, Detail string
	Data         map[string]any
}

func (e *NeedError) Error() string { return e.Kind + ": " + e.Detail }

// Need is a NeedError without extras.
func Need(kind, detail string) *NeedError { return &NeedError{Kind: kind, Detail: detail} }

// LoginNeed is the need of a hosted Claude Code that answered with its /login
// reply: the Claude login of the profile it runs under expired or is missing,
// so every other session of that profile fails the same way until someone
// runs /login. tail is the screen tail.
func LoginNeed(terminal, tail string) *NeedError {
	return Need(KindLogin, "Claude Code in "+terminal+" is not logged in: the Claude login of this profile expired or is missing. Run /login in a Claude Code session of this profile.\n"+tail)
}

// NetworkDenials is the "network_denials" extra, or nil.
func (e *NeedError) NetworkDenials() []any {
	if e == nil || e.Data == nil {
		return nil
	}
	d, _ := e.Data["network_denials"].([]any)
	return d
}

// EventFunc receives the host's observability events (`orca-session` in
// the wave log): pkg is "" for host-wide events; detail always holds
// "action" and "terminal". It must not fail a package: panics are dropped.
type EventFunc func(pkg string, detail map[string]any)

// Host runs packages in interactive Claude Code sessions.
//
// Prompt delivers ONE prompt line (opening a new agent on the session's
// worktree when the old one is gone) and returns once it is submitted;
// WaitResult then waits for the result file. PromptAndWait composes both
// the way a dispatch does.
type Host interface {
	Name() string
	Enabled() Tri
	Available() (bool, string)
	ModelFor(cls string) string
	Session(pkg, stateDir string) (*Session, bool)
	Start(ctx context.Context, pkgID, repo, baseRef, cls, model, runID, stateDir string, addDirs []string) (*Session, error)
	Prompt(ctx context.Context, s *Session, promptFiles []string, resultPath string, timeout time.Duration) error
	WaitResult(ctx context.Context, s *Session, resultPath string, timeout time.Duration) (map[string]any, error)
	Close(s *Session, removeWorktree bool) error
	Shutdown()
}

// Warm is a host that can fork packages from a warm reader session that
// already read the plan's shared files.
type Warm interface {
	ConfigureWarm(paths []string, modelCounts map[string]int)
	EnsureWarm(ctx context.Context, model, baseRef string, paths []string, repo, stateDir string, addDirs []string) error
}

// Network is a host whose sessions run behind the network allowlist.
// NetworkTick tails the blocked-connection log and pushes newly approved
// domains (never fails). NetworkDenials returns the blocked connections
// logged since mark as "host:port" (or "host"), de-duplicated, and the mark
// to pass next time.
type Network interface {
	NetworkTick()
	NetworkDenials(mark int) ([]string, int)
}

// Settings are the host config values the runner needs.
type Settings struct {
	TaskTimeout    time.Duration
	StartupTimeout time.Duration
	KeepSessions   bool
	// AddDirs is the host's `add_dirs` config as written (the runner
	// expands and resolves it against the repo).
	AddDirs []string
}

// Runtime is what both hosts offer the runner besides Host.
type Runtime interface {
	Settings() Settings
	// SetStateDir names the wave's state dir (registry, logs, warm records)
	// before the first Start; Start/Session/EnsureWarm set it too when unset.
	SetStateDir(dir string)
	SetEventFunc(fn EventFunc)
}

// Emit calls fn with {"action", "terminal", extra...}; a nil fn or a
// panicking one is ignored (observability never takes a package down).
func Emit(fn EventFunc, pkg, action, terminal string, extra map[string]any) {
	if fn == nil {
		return
	}
	defer func() { _ = recover() }()
	detail := map[string]any{"action": action, "terminal": nil}
	if terminal != "" {
		detail["terminal"] = terminal
	}
	for k, v := range extra {
		detail[k] = v
	}
	fn(pkg, detail)
}

// sleepCtx sleeps d unless ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Clock is the injectable time source of a host's wait loops.
type Clock struct {
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// RealClock is the wall clock.
func RealClock() Clock { return Clock{Now: time.Now, Sleep: sleepCtx} }

// Seconds renders a duration as seconds without trailing zeros ("60", "1.5").
func Seconds(d time.Duration) string { return formatSeconds(d.Seconds()) }
