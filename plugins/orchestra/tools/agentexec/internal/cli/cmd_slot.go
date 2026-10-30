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

	classify := &cobra.Command{
		Use:   "classify --command CMD",
		Short: "what CMD runs: heavy or not, its kind and pool, a full suite or targeted (used by the orchestra hooks)",
		Args:  cobra.NoArgs,
	}
	classified := classify.Flags().String("command", "", "the command line to classify")
	_ = classify.MarkFlagRequired("command")
	classify.RunE = func(cmd *cobra.Command, _ []string) error {
		c, cfg := classifyConfigured(*classified)
		var kind any
		if c.Heavy {
			kind = c.Kind
		}
		out := map[string]any{
			"op": c.Op, "heavy": c.Heavy, "kind": kind, "pool": c.Pool(), "scope": c.Scope,
		}
		if prefix := config.NodeCapShellPrefix(config.PoolSize(cfg, config.PoolTest)); c.Heavy && prefix != "" {
			out["capped_command"] = prefix + *classified
		}
		return writeJSONLine(cmd.OutOrStdout(), out)
	}
	cmd.AddCommand(classify)

	acquire := &cobra.Command{
		Use:   "acquire --command CMD",
		Short: "wait for a slot of CMD's pool if CMD is heavy (used by the orchestra hooks)",
		Args:  cobra.NoArgs,
	}
	command := acquire.Flags().String("command", "", "the command line to classify")
	owner := acquire.Flags().Int("owner-pid", 0, "the pid whose death frees the slot (default: the parent process)")
	ttl := acquire.Flags().Duration("ttl", slot.DefaultTTL, "the longest the slot is held")
	maxWait := acquire.Flags().Duration("max-wait", 0, "give up unadmitted after this long (0 = wait as long as it takes)")
	_ = acquire.MarkFlagRequired("command")
	acquire.RunE = func(cmd *cobra.Command, _ []string) error {
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

	status := &cobra.Command{Use: "status", Short: "each pool's size and held slots", Args: cobra.NoArgs}
	asJSON := status.Flags().Bool("json", false, "machine-readable output")
	status.RunE = func(cmd *cobra.Command, _ []string) error { return runSlotStatus(cmd, *asJSON) }
	cmd.AddCommand(status)
	return cmd
}

// classifyConfigured classifies command with the extra heavy patterns of
// the resolved config (the built-in defaults on a broken layer).
func classifyConfigured(command string) (slot.Class, *config.Resolved) {
	cfg := configOrDefaults()
	return slot.Classify(command, slot.CompileExtra(cfg.StringList("throttle", "heavy_extra"))), cfg
}

func runSlotAcquire(cmd *cobra.Command, command string, req slot.Request, maxWait time.Duration) error {
	out := cmd.OutOrStdout()
	c, cfg := classifyConfigured(command)
	p := slot.Configured(cfg, c.Pool())
	if !c.Heavy || p.Off() {
		return writeJSONLine(out, map[string]any{"admitted": false})
	}
	req.Op = c.Op
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
		"admitted": true, "token": lease.Token, "waited_ms": waited.Milliseconds(), "op": c.Op, "pool": p.Name,
	})
}

func runSlotStatus(cmd *cobra.Command, asJSON bool) error {
	cfg := configOrDefaults()
	now := float64(time.Now().UnixNano()) / 1e9
	w := cmd.OutOrStdout()
	pools := make([]map[string]any, 0, 2)
	for _, name := range []string{slot.Heavy, slot.Test} {
		p := slot.Configured(cfg, name)
		leases, err := p.Leases()
		if err != nil {
			return exitf(1, "%s: %v", cmdName(cmd), err)
		}
		held := make([]map[string]any, 0, len(leases))
		for _, l := range leases {
			held = append(held, map[string]any{
				"pid": l.PID, "age_s": int64(now - l.Start), "expires_in_s": int64(l.Expires - now),
				"op": l.Op, "token": l.Token,
			})
		}
		pools = append(pools, map[string]any{
			"pool": p.Name, "size": p.Size, "off": p.Off(), "dir": p.Dir, "held": held,
		})
		if asJSON {
			continue
		}
		if p.Off() {
			fmt.Fprintf(w, "%s: off (throttle.%s: 0)\n", p.Name, config.SizeKey(name))
		} else {
			fmt.Fprintf(w, "%s: %d/%d held\n", p.Name, len(leases), p.Size)
		}
		for _, h := range held {
			fmt.Fprintf(w, "  pid %-7v %5vs  %v\n", h["pid"], h["age_s"], h["op"])
		}
	}
	if asJSON {
		return writeJSON(w, map[string]any{"pools": pools})
	}
	return nil
}
