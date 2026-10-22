package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
)

func newSlotCmd() *cobra.Command {
	cmd := groupCommand("slot", "machine-wide admission for heavy commands")

	acquire := &cobra.Command{
		Use:   "acquire --command CMD",
		Short: "wait for a heavy slot if CMD is heavy (used by the orchestra hooks)",
		Args:  cobra.NoArgs,
	}
	pool := acquire.Flags().String("pool", slot.Heavy, "the pool (only heavy)")
	command := acquire.Flags().String("command", "", "the command line to classify")
	owner := acquire.Flags().Int("owner-pid", 0, "the pid whose death frees the slot (default: the parent process)")
	ttl := acquire.Flags().Duration("ttl", slot.DefaultTTL, "the longest the slot is held")
	maxWait := acquire.Flags().Duration("max-wait", 0, "give up unadmitted after this long (0 = wait as long as it takes)")
	_ = acquire.MarkFlagRequired("command")
	acquire.RunE = func(cmd *cobra.Command, _ []string) error {
		if *pool != slot.Heavy {
			return exitf(2, "%s: unknown pool: %s", cmdName(cmd), *pool)
		}
		pid := *owner
		if pid == 0 {
			pid = os.Getppid()
		}
		if pid <= 0 {
			return exitf(2, "%s: --owner-pid must be a positive pid", cmdName(cmd))
		}
		return runSlotAcquire(cmd, *command, slot.Request{Owner: pid, TTL: *ttl}, *maxWait)
	}
	cmd.AddCommand(acquire)

	release := &cobra.Command{
		Use:   "release TOKEN",
		Short: "free a slot (an unknown or released token is fine)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := slot.Release(args[0]); err != nil {
				return exitf(1, "%s: %v", cmdName(cmd), err)
			}
			return nil
		},
	}
	cmd.AddCommand(release)

	status := &cobra.Command{Use: "status", Short: "the heavy pool's size and held slots", Args: cobra.NoArgs}
	asJSON := status.Flags().Bool("json", false, "machine-readable output")
	status.RunE = func(cmd *cobra.Command, _ []string) error { return runSlotStatus(cmd, *asJSON) }
	cmd.AddCommand(status)
	return cmd
}

// heavyConfig is the heavy pool and the extra heavy patterns of the
// resolved config (the built-in defaults on a broken layer).
func heavyConfig() (slot.Pool, *config.Resolved) {
	cfg := configOrDefaults()
	size, _ := cfg.Lookup("throttle", "heavy_slots")
	return slot.New(slot.Heavy, slot.SizeOf(size)), cfg
}

func runSlotAcquire(cmd *cobra.Command, command string, req slot.Request, maxWait time.Duration) error {
	out := cmd.OutOrStdout()
	p, cfg := heavyConfig()
	op, heavy := slot.Classify(command, slot.CompileExtra(cfg.StringList("throttle", "heavy_extra")))
	if !heavy || p.Off() {
		return writeJSONLine(out, map[string]any{"admitted": false})
	}
	req.Op = op
	ctx := context.Background()
	if maxWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxWait)
		defer cancel()
	}
	lease, waited, err := p.Acquire(ctx, req)
	if err != nil {
		// Unadmitted, never blocked: the caller runs the command anyway.
		return writeJSONLine(out, map[string]any{
			"admitted": false, "waited_ms": waited.Milliseconds(), "reason": err.Error(),
		})
	}
	return writeJSONLine(out, map[string]any{
		"admitted": true, "token": lease.Token, "waited_ms": waited.Milliseconds(), "op": op,
	})
}

func runSlotStatus(cmd *cobra.Command, asJSON bool) error {
	p, _ := heavyConfig()
	leases, err := p.Leases()
	if err != nil {
		return exitf(1, "%s: %v", cmdName(cmd), err)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	held := make([]map[string]any, 0, len(leases))
	for _, l := range leases {
		held = append(held, map[string]any{
			"pid": l.PID, "age_s": int64(now - l.Start), "expires_in_s": int64(l.Expires - now),
			"op": l.Op, "token": l.Token,
		})
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), map[string]any{
			"pool": p.Name, "size": p.Size, "off": p.Off(), "dir": p.Dir, "held": held,
		})
	}
	w := cmd.OutOrStdout()
	if p.Off() {
		fmt.Fprintf(w, "%s: off (throttle.heavy_slots: 0)\n", p.Name)
	} else {
		fmt.Fprintf(w, "%s: %d/%d held\n", p.Name, len(leases), p.Size)
	}
	for _, h := range held {
		fmt.Fprintf(w, "  pid %-7v %5vs  %v\n", h["pid"], h["age_s"], h["op"])
	}
	return nil
}
