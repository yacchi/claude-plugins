package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// parseExecArgs splits `--write W --protect P ... -- argv`.
func parseExecArgs(args []string) (writes, protect, argv []string, err error) {
	for i := 0; i < len(args); {
		switch {
		case args[i] == "--":
			return writes, protect, args[i+1:], nil
		case (args[i] == "--write" || args[i] == "--protect") && i+1 < len(args):
			if args[i] == "--write" {
				writes = append(writes, args[i+1])
			} else {
				protect = append(protect, args[i+1])
			}
			i += 2
		default:
			return nil, nil, nil, fmt.Errorf("unexpected argument: %s", args[i])
		}
	}
	return nil, nil, nil, errors.New("missing -- before the command")
}

// execDeps are ExecMain's effects, swapped out by tests.
type execDeps struct {
	restrict func(writes []string) error
	exec     func(argv []string) error
	stderr   io.Writer
}

func realExec(argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ())
}

// ExecMain is the hidden `_sandbox-exec` command, the Landlock backend's
// re-executed child: it restricts only itself, then execs argv. It fails
// closed: when the restriction cannot be applied (or a writable root
// contains a protected path), the command is not run.
func ExecMain(args []string) int {
	// The restriction and the exec must happen on one OS thread: Landlock
	// and no_new_privs apply to the calling thread only.
	runtime.LockOSThread()
	return execMain(args, execDeps{restrict: landlockRestrict, exec: realExec, stderr: os.Stderr})
}

func execMain(args []string, d execDeps) int {
	writes, protect, argv, err := parseExecArgs(args)
	if err != nil {
		fmt.Fprintf(d.stderr, "agent-exec: _sandbox-exec: %s\n", err)
		return 2
	}
	if len(argv) == 0 {
		fmt.Fprintln(d.stderr, "agent-exec: _sandbox-exec: no command")
		return 2
	}
	// Landlock cannot deny inside an allowed root, so no writable root may
	// contain -- or sit inside -- orchestra's protected state.
	for _, t := range protect {
		for _, w := range writes {
			if overlaps(realPath(t), realPath(w)) {
				fmt.Fprintf(d.stderr, "agent-exec: _sandbox-exec: writable root %s contains protected %s\n", w, t)
				return 126
			}
		}
	}
	if err := d.restrict(writes); err != nil {
		fmt.Fprintf(d.stderr, "agent-exec: _sandbox-exec: %s\n", err)
		return 126
	}
	if err := d.exec(argv); err != nil {
		fmt.Fprintf(d.stderr, "agent-exec: _sandbox-exec: %s: %s\n", argv[0], err)
	}
	return 127
}
