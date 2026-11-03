package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/proctree"
)

// procGuardMaxAge bounds both a watcher's life and how long a trip record
// is kept: a command that outlives a day is not a runaway worth killing, and
// a record nobody read in a day belongs to a command that is long gone.
const procGuardMaxAge = 24 * time.Hour

// procGuardTokenPattern keeps the token safe to use in a file name: the
// record path is built from it, so anything with a separator is refused.
var procGuardTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// procGuardRecord is the trip record guard-result reads back. It is written
// before the first signal, so the hook finds it for every command that was
// killed.
type procGuardRecord struct {
	At       int64  `json:"at"`
	MemBytes int64  `json:"mem_bytes"`
	Procs    int    `json:"procs"`
	Reason   string `json:"reason"`
	Root     int    `json:"root"`
	Token    string `json:"token"`
}

// The seams. Tests replace them and restore them with t.Cleanup. procGuardStart
// never starts a watcher from inside a test binary, which would re-run the
// tests instead.
var (
	procGuardStart = func(exe string, args []string) error {
		if testing.Testing() {
			return errors.New("proc guard: watcher not started under test")
		}
		c := procGuardWatcher(exe, args)
		if err := c.Start(); err != nil {
			return err
		}
		return c.Process.Release()
	}
	procGuardLimits = proctree.DefaultLimits
	procGuardKill   = proctree.KillTree
)

func newProcCmd() *cobra.Command {
	cmd := groupCommand("proc", "process-tree guards (used by the orchestra hooks)")

	guard := &cobra.Command{
		Use:   "guard --root PID --token TOKEN",
		Short: "watch the process tree under the shell PID and kill it on a trip (used by the orchestra hooks)",
		Args:  cobra.NoArgs,
	}
	root := guard.Flags().Int("root", 0, "the shell pid whose tree is watched (must be > 1)")
	token := guard.Flags().String("token", "", "the guard token the record is keyed by")
	watch := guard.Flags().Bool("watch", false, "run the watcher in this process (internal)")
	_ = guard.Flags().MarkHidden("watch")
	guard.RunE = func(cmd *cobra.Command, _ []string) error {
		if *root <= 1 {
			return exitf(2, "%s: --root must be a pid > 1", cmdName(cmd))
		}
		if err := procGuardCheckToken(cmd, *token); err != nil {
			return err
		}
		if *watch {
			procGuardWatch(*root, *token)
			return nil
		}
		exe, err := os.Executable()
		if err != nil {
			return nil
		}
		// Unguarded is better than failing the command: the start error is
		// dropped on purpose.
		_ = procGuardStart(exe, []string{"proc", "guard", "--root", strconv.Itoa(*root), "--token", *token, "--watch"})
		return nil
	}
	cmd.AddCommand(guard)

	result := &cobra.Command{
		Use:   "guard-result --token TOKEN",
		Short: "print whether the guard killed the command with this token, and remove its record (used by the orchestra hooks)",
		Args:  cobra.NoArgs,
	}
	resultToken := result.Flags().String("token", "", "the guard token the record is keyed by")
	result.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := procGuardCheckToken(cmd, *resultToken); err != nil {
			return err
		}
		procGuardResult(cmd, *resultToken)
		return nil
	}
	cmd.AddCommand(result)
	return cmd
}

// procGuardCheckToken refuses a bad token before any path is built from it:
// the record path is made from the token, so a separator would escape the
// record directory.
func procGuardCheckToken(cmd *cobra.Command, token string) error {
	if !procGuardTokenPattern.MatchString(token) {
		return exitf(2, "%s: --token must match %s", cmdName(cmd), procGuardTokenPattern)
	}
	return nil
}

func procGuardDir() string {
	return filepath.Join(paths.StateDir(), "procguard")
}

func procGuardRecordPath(token string) string {
	return filepath.Join(procGuardDir(), token+".json")
}

// procGuardWatcher is the detached watcher process. Its stdio stays nil, so
// it reads and writes /dev/null: it must not hold the shell's pipes, or the
// Bash call would wait for an end-of-file that never comes.
func procGuardWatcher(exe string, args []string) *exec.Cmd {
	c := exec.Command(exe, args...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return c
}

// procGuardWatch runs in the watcher process. It prints nothing; the trip
// record is its only output.
func procGuardWatch(root int, token string) {
	procGuardExpire(time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), procGuardMaxAge)
	defer cancel()
	u, text, ok := proctree.Wait(ctx, root, procGuardLimits(), proctree.DefaultInterval)
	if ok {
		procGuardTrip(root, token, u, text)
	}
}

// procGuardExpire removes the records older than procGuardMaxAge. Errors are
// ignored: a stale file that survives one sweep is removed by the next.
func procGuardExpire(now time.Time) {
	entries, err := os.ReadDir(procGuardDir())
	if err != nil {
		return
	}
	cutoff := now.Add(-procGuardMaxAge)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(procGuardDir(), e.Name()))
	}
}

// procGuardTrip writes the record and then kills the tree. The order is the
// point: guard-result runs only after the Bash call returned, which needs the
// command's processes dead, so the record is already there for every kill.
// The shell itself is spared (includeRoot false) so the command's shell
// finishes as usual.
func procGuardTrip(root int, token string, u proctree.Usage, text string) {
	if err := os.MkdirAll(procGuardDir(), 0o755); err == nil {
		rec := procGuardRecord{
			At:       time.Now().Unix(),
			MemBytes: u.Mem,
			Procs:    u.Procs,
			Reason:   text,
			Root:     root,
			Token:    token,
		}
		if data, err := fsutil.MarshalJSON(rec); err == nil {
			_ = fsutil.AtomicWriteFile(procGuardRecordPath(token), data, 0o644)
		}
	}
	// A failed record write does not stop the kill: the trip still happened.
	_, _ = procGuardKill(root, false)
}

// procGuardResult reads and removes the record. It never waits or polls: the
// record either exists by now or never will.
func procGuardResult(cmd *cobra.Command, token string) {
	path := procGuardRecordPath(token)
	out := cmd.OutOrStdout()
	data, err := os.ReadFile(path)
	if err != nil {
		_ = writeJSONLine(out, map[string]any{"killed": false})
		return
	}
	var rec procGuardRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		_ = os.Remove(path)
		_ = writeJSONLine(out, map[string]any{"killed": false})
		return
	}
	_ = writeJSONLine(out, map[string]any{
		"killed":    true,
		"mem_bytes": rec.MemBytes,
		"procs":     rec.Procs,
		"reason":    rec.Reason,
		"root":      rec.Root,
	})
	_ = os.Remove(path)
}
