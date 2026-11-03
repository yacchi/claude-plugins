package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
)

// The seams. Tests replace them and restore them with t.Cleanup. procCmdStart
// never starts a watcher from inside a test binary, which would re-run the
// tests instead.
var (
	procCmdOwner = os.Getppid
	procCmdHeld  = procWatchHeld
	procCmdServe = procWatchServe
	procCmdStart = func(exe string, args []string) error {
		if testing.Testing() {
			return errors.New("proc watch: watcher not started under test")
		}
		c := procCmdWatcher(exe, args)
		if err := c.Start(); err != nil {
			return err
		}
		return c.Process.Release()
	}
)

// procCmdRecord is the kill record the watcher writes before its first signal.
// guard-result decodes it by the frozen JSON keys.
type procCmdRecord struct {
	AtMS     int64  `json:"at_ms"`
	Owner    int    `json:"owner"`
	Root     int    `json:"root"`
	Procs    int    `json:"procs"`
	MemBytes int64  `json:"mem_bytes"`
	Reason   string `json:"reason"`
}

func newProcCmd() *cobra.Command {
	cmd := groupCommand("proc", "process-tree watcher (used by the orchestra hooks)")

	watch := &cobra.Command{
		Use:   "watch",
		Short: "start this Claude Code process's process-tree watcher (used by the orchestra hooks)",
		Args:  cobra.NoArgs,
	}
	serve := watch.Flags().Bool("serve", false, "run the watcher in this process (internal)")
	_ = watch.Flags().MarkHidden("serve")
	owner := watch.Flags().Int("owner", 0, "the Claude Code pid the watcher serves (internal)")
	_ = watch.Flags().MarkHidden("owner")
	watch.RunE = func(_ *cobra.Command, _ []string) error {
		if *serve {
			if *owner > 1 {
				_ = procCmdServe(*owner)
			}
			return nil
		}
		o := procCmdOwner()
		if o <= 1 || procCmdHeld(o) {
			return nil
		}
		exe, err := os.Executable()
		if err != nil {
			return nil
		}
		// The start error is dropped on purpose: unguarded beats a failed hook.
		_ = procCmdStart(exe, []string{"proc", "watch", "--serve", "--owner", strconv.Itoa(o)})
		return nil
	}
	cmd.AddCommand(watch)

	result := &cobra.Command{
		Use:   "guard-result --since MS",
		Short: "print whether the watcher killed a child of this Claude Code process since MS (used by the orchestra hooks)",
		Args:  cobra.NoArgs,
	}
	since := result.Flags().String("since", "", "epoch milliseconds; the oldest kill time to report")
	_ = result.Flags().String("token", "", "removed in orchestra 0.93.0")
	_ = result.Flags().MarkHidden("token")
	result.RunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("token") {
			return exitf(2, "proc guard-result --token was removed in orchestra 0.93.0: ask with --since <epoch ms>")
		}
		if *since == "" {
			return exitf(2, "%s: --since is required (epoch milliseconds)", cmdName(cmd))
		}
		ms, err := strconv.ParseInt(*since, 10, 64)
		if err != nil || ms < 0 {
			return exitf(2, "%s: --since must be a non-negative integer of epoch milliseconds", cmdName(cmd))
		}
		procCmdResult(cmd.OutOrStdout(), procCmdOwner(), ms)
		return nil
	}
	cmd.AddCommand(result)

	// Removed in 0.93.0. DisableFlagParsing keeps cobra from rejecting the old
	// flags before the message.
	removed := &cobra.Command{
		Use:                "guard",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitf(2, "proc guard was removed in orchestra 0.93.0: the process guard no longer runs as a command prefix; the hooks start one watcher per Claude Code process with 'agent-exec proc watch'")
		},
	}
	cmd.AddCommand(removed)
	return cmd
}

// procCmdWatcher is the detached watcher process. Its stdio stays nil, so
// it reads and writes /dev/null: it must not hold the shell's pipes, or the
// Bash call would wait for an end-of-file that never comes.
func procCmdWatcher(exe string, args []string) *exec.Cmd {
	c := exec.Command(exe, args...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return c
}

// procCmdResult prints the newest kill record of owner whose at_ms is at or
// after since. Records are never removed: the 24 h expiry of the watcher
// owns their lifetime. Unreadable files are skipped, not reported.
func procCmdResult(out io.Writer, owner int, since int64) {
	entries, err := os.ReadDir(procWatchDir())
	if err != nil {
		_ = writeJSONLine(out, map[string]any{"killed": false})
		return
	}
	prefix := strconv.Itoa(owner) + "-"
	var best procCmdRecord
	found := false
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procWatchDir(), name))
		if err != nil {
			continue
		}
		var rec procCmdRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if rec.Owner != owner || rec.AtMS < since {
			continue
		}
		// ReadDir sorts by name, so on an equal at_ms the later name wins.
		if !found || rec.AtMS >= best.AtMS {
			best = rec
			found = true
		}
	}
	if !found {
		_ = writeJSONLine(out, map[string]any{"killed": false})
		return
	}
	_ = writeJSONLine(out, map[string]any{
		"killed":    true,
		"mem_bytes": best.MemBytes,
		"procs":     best.Procs,
		"reason":    best.Reason,
		"root":      best.Root,
	})
}
