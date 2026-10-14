package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/readiness"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/route"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

func newRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "resolve a capability class to a concrete executor",
		Args:  cobra.NoArgs,
	}
	class := cmd.Flags().String("class", "", "light|standard|deep|review|independent-review")
	archetype := cmd.Flags().String("archetype", "default", "task archetype (default|investigation)")
	exhausted := cmd.Flags().String("exhausted", "", "comma-separated executors already tried")
	noCooldown := cmd.Flags().Bool("no-cooldown", false, "ignore executor cooldowns")
	format := formatFlags(cmd, "json", "text")
	_ = cmd.MarkFlagRequired("class")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		asText := format.is("text")

		cfg, err := config.Resolve()
		if err != nil {
			if asText {
				fmt.Fprintln(cmd.ErrOrStderr(), "agent-exec: "+err.Error())
			} else if werr := writeJSONLine(cmd.OutOrStdout(), map[string]any{"error": "agent-exec: " + err.Error()}); werr != nil {
				return werr
			}
			return &ExitError{Code: 1}
		}
		tools.MarkAvailability(cfg)

		var exhaustedList []string
		for _, e := range strings.Split(*exhausted, ",") {
			if e != "" {
				exhaustedList = append(exhaustedList, e)
			}
		}
		var active map[string]cooldown.Entry
		if !*noCooldown && cooldown.Enabled(cfg) {
			active = cooldown.Active(cfg, time.Now())
		}
		r := route.Resolve(cfg, readiness.Compute(cfg).Ready, *class, *archetype, exhaustedList, active)
		if asText {
			_, err := fmt.Fprint(cmd.OutOrStdout(), route.FormatText(r))
			return err
		}
		return writeJSON(cmd.OutOrStdout(), r.Object())
	}
	return cmd
}
