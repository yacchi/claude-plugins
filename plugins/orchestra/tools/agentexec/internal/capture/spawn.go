package capture

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/watchdog"
)

// srtTick is the interval of the network ticks (blocked-host tailing,
// learned-domain push) of a CLI run under srt.
const srtTick = time.Second

// spawnOptions are one capture spawn's inputs.
type spawnOptions struct {
	Argv  []string
	Cwd   string   // "" inherits
	Env   []string // nil inherits
	Input string
	Spec  sandbox.Spec
	// Watchdog observes stdout and ends the process group on runaway
	// work; nil runs unguarded.
	Watchdog *watchdog.Watchdog
	// OnLine receives every stdout record as it arrives.
	OnLine func(string)
	// Label names the srt run in its stderr notes.
	Label string
}

// spawnResult is what a capture spawn returns. Network is set (possibly
// empty) only for an srt run.
type spawnResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Runaway  *watchdog.Runaway
	Network  []srt.NetDenial
}

// spawn runs o.Argv wrapped for o.Spec, feeds o.Input on stdin and returns
// the exit code (-N for a child killed by signal N) and decoded output.
//
// stdin is written from its own goroutine and stderr drained from another,
// so a child that reads and writes more than a pipe buffer at once cannot
// deadlock. stdout is read on the calling goroutine one record per "\n"
// (a trailing "\r" stripped; U+2028/U+2029 never split), with no limit on
// a record's length. The child leads its own session: the watchdog and a
// cancelled ctx end its whole process group, grandchildren included.
//
// Under backend srt the child gets srt's control pipe as fd 3, srt's debug
// lines are split out of the shared stderr into the run's log (never into
// the returned stderr), a ticker tails that log and pushes learned-domain
// changes while the child lives, and the blocked hosts (deduped, quiet
// domains left out) are returned.
func spawn(ctx context.Context, o spawnOptions) (spawnResult, error) {
	run, err := srt.CLIRun(o.Spec, o.Label)
	if err != nil {
		return spawnResult{}, err
	}
	finished := false
	if run != nil {
		defer func() {
			if !finished {
				run.Close()
			}
		}()
	}
	if len(o.Argv) == 0 {
		return spawnResult{}, errNoArgv
	}
	argv := sandbox.WrapArgv(o.Argv, o.Cwd, o.Spec)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = o.Cwd
	cmd.Env = o.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return spawnResult{}, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW)
		return spawnResult{}, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		return spawnResult{}, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW

	var split *srt.StderrSplit
	if run != nil {
		if _, err := run.OpenLog(); err != nil {
			closeAll(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
			return spawnResult{}, err
		}
		split = srt.NewStderrSplit(run)
		err = run.Spawn(cmd)
	} else {
		err = cmd.Start()
	}
	closeAll(stdinR, stdoutW, stderrW) // the child's ends
	if err != nil {
		closeAll(stdinW, stdoutR, stderrR)
		return spawnResult{}, err
	}
	pgid := cmd.Process.Pid

	exited := make(chan struct{})
	var state *os.ProcessState
	go func() {
		_ = cmd.Wait()
		state = cmd.ProcessState
		close(exited)
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // stdin
		defer wg.Done()
		_, _ = io.WriteString(stdinW, o.Input) // the child may exit without reading it all
		stdinW.Close()
	}()
	var stderrBuf bytes.Buffer
	go func() { // stderr
		defer wg.Done()
		defer stderrR.Close()
		buf := make([]byte, 65536)
		for {
			n, err := stderrR.Read(buf)
			if n > 0 {
				if split != nil {
					stderrBuf.Write(split.Feed(buf[:n]))
				} else {
					stderrBuf.Write(buf[:n])
				}
			}
			if err != nil {
				break
			}
		}
		if split != nil {
			stderrBuf.Write(split.Flush())
		}
	}()

	stopTicks := make(chan struct{})
	ticksDone := make(chan struct{})
	if run != nil {
		go func() {
			defer close(ticksDone)
			t := time.NewTicker(srtTick)
			defer t.Stop()
			for {
				select {
				case <-stopTicks:
					return
				case <-t.C:
					run.Tick()
				}
			}
		}()
	} else {
		close(ticksDone)
	}

	if o.Watchdog != nil {
		o.Watchdog.Start(func() { watchdog.TerminateGroup(pgid, exited) })
	}
	cancelled := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		case <-exited:
		}
		close(cancelled)
	}()

	var stdout bytes.Buffer
	reader := bufio.NewReaderSize(stdoutR, 65536)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			stdout.Write(raw)
			record := bytes.TrimSuffix(raw, []byte("\n"))
			record = bytes.TrimSuffix(record, []byte("\r"))
			if o.Watchdog != nil || o.OnLine != nil {
				line := decodeReplace(record)
				if o.Watchdog != nil {
					o.Watchdog.OnLine(line)
				}
				if o.OnLine != nil {
					o.OnLine(line)
				}
			}
		}
		if err != nil {
			break
		}
	}
	stdoutR.Close()
	<-exited
	<-cancelled
	wg.Wait()
	if o.Watchdog != nil {
		o.Watchdog.Stop()
	}
	close(stopTicks)
	<-ticksDone

	if err := ctx.Err(); err != nil {
		return spawnResult{}, err
	}
	res := spawnResult{
		ExitCode: exitCode(state),
		Stdout:   decodeReplace(stdout.Bytes()),
		Stderr:   decodeReplace(stderrBuf.Bytes()),
	}
	if o.Watchdog != nil {
		res.Runaway = o.Watchdog.Result()
	}
	if run != nil {
		finished = true
		run.Close()
		run.Tick() // the lines written since the last tick
		res.Network = run.Denials(0, nil)
	}
	return res, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// exitCode is Python's Popen.returncode: the exit status, or -N when the
// child was killed by signal N.
func exitCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return state.ExitCode()
}

// decodeReplace is bytes.decode("utf-8", errors="replace"): every invalid
// byte becomes U+FFFD.
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out bytes.Buffer
	out.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out.WriteRune(utf8.RuneError)
		} else {
			out.Write(b[:size])
		}
		b = b[size:]
	}
	return out.String()
}

// errNoArgv guards a spawn without a command.
var errNoArgv = errors.New("capture: empty argv")
