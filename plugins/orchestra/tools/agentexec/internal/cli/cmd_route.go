package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/readiness"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/route"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

func newRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "resolve a capability class to a concrete executor",
		Args:  noArgs,
	}
	class := strFlag(cmd, "class", "light|standard|deep|review|independent-review")
	archetype := strFlag(cmd, "archetype", "task archetype (default|investigation)")
	exhausted := strFlag(cmd, "exhausted", "comma-separated executors already tried")
	noCooldown := boolFlag(cmd, "no-cooldown", "ignore executor cooldowns")
	format := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cls, ok := class.Get()
		if !ok {
			return exitf(2, "route: missing required option: --class")
		}
		asText := format.Given("text")

		cfg, err := config.Resolve()
		if err != nil {
			if asText {
				fmt.Fprintln(cmd.ErrOrStderr(), "agent-exec: "+err.Error())
			} else if werr := writeJSONLine(cmd.OutOrStdout(), jsonx.NewObject().Set("error", "agent-exec: "+err.Error())); werr != nil {
				return werr
			}
			return &ExitError{Code: 1}
		}
		tools.MarkAvailability(cfg)

		var exhaustedList []string
		for _, e := range strings.Split(exhausted.Or(""), ",") {
			if e != "" {
				exhaustedList = append(exhaustedList, e)
			}
		}
		var active map[string]cooldown.Entry
		if !noCooldown.On() && cooldown.Enabled(cfg) {
			active = cooldown.Active(cfg, time.Now())
		}
		r := route.Resolve(cfg, readiness.Compute(cfg).Ready, cls, archetype.Or("default"), exhaustedList, active)
		if asText {
			_, err := fmt.Fprint(cmd.OutOrStdout(), route.FormatText(r))
			return err
		}
		return writeJSON(cmd.OutOrStdout(), r.Object())
	}
	return cmd
}
