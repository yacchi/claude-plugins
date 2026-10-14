package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
)

func init() {
	// `wave status` shows how many approvals wait for a decision.
	ApprovalsPending = func(statePath string) int {
		items, err := approvals.Pending(statePath)
		if err != nil {
			return 0
		}
		return len(items)
	}
}

// requireStateFile is exit 3 when the wave's state file is missing.
func requireStateFile(cmd *cobra.Command, statePath string) error {
	if st, err := os.Stat(statePath); err != nil || !st.Mode().IsRegular() {
		return exitf(3, "%s: no state file at %s", cmdName(cmd), statePath)
	}
	return nil
}

func newApprovalsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "pending network, path and dialog approvals of a wave",
		Args:  cobra.NoArgs,
	}
	statePath := cmd.Flags().String("state", "", "state.json path")
	asJSON := cmd.Flags().Bool("json", false, "JSON output")
	_ = cmd.MarkFlagRequired("state")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := requireStateFile(cmd, *statePath); err != nil {
			return err
		}
		items, err := approvals.Pending(*statePath)
		if err != nil {
			return exitf(1, "approvals: %v", err)
		}
		out := cmd.OutOrStdout()
		if *asJSON {
			return writeJSONLine(out, items)
		}
		for _, it := range items {
			fmt.Fprintln(out, approvals.FormatLine(it))
		}
		if len(items) > 0 {
			fmt.Fprintf(out, "decide: agent-exec approvals decide --state %s ITEM allow|deny --reason TEXT\n", *statePath)
		} else {
			fmt.Fprintln(out, "no pending approvals")
		}
		return nil
	}
	cmd.AddCommand(newApprovalsDecideCmd())
	return cmd
}

func newApprovalsDecideCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decide ITEM allow|deny",
		Short: "allow or deny a pending approval",
		Args:  cobra.ExactArgs(2),
	}
	statePath := cmd.Flags().String("state", "", "state.json path")
	reason := cmd.Flags().String("reason", "", "why")
	by := cmd.Flags().String("by", "instructor", "instructor|user")
	asJSON := cmd.Flags().Bool("json", false, "JSON output")
	for _, name := range []string{"state", "reason"} {
		_ = cmd.MarkFlagRequired(name)
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		item, decision := args[0], args[1]
		why := strings.TrimSpace(*reason)
		switch {
		case !slices.Contains(approvals.Decisions, decision):
			return exitf(2, "approvals decide: decision must be %s, got %s", strings.Join(approvals.Decisions, "|"), decision)
		case !slices.Contains(approvals.Bys, *by):
			return exitf(2, "approvals decide: --by must be %s, got %s", strings.Join(approvals.Bys, "|"), *by)
		case why == "":
			return exitf(2, "approvals decide: --reason must not be blank")
		}
		if err := requireStateFile(cmd, *statePath); err != nil {
			return err
		}
		res, err := approvals.Decide(*statePath, item, decision, why, *by)
		var failed *approvals.DecisionError
		if err != nil && !errors.As(err, &failed) {
			return exitf(1, "approvals decide: %v", err)
		}
		out := cmd.OutOrStdout()
		if *asJSON {
			if werr := writeJSONLine(out, res); werr != nil {
				return werr
			}
		} else if failed == nil {
			subject := "dialog"
			if res["kind"] != "dialog" {
				subject = fmt.Sprint(res["subject"])
			}
			tail := ""
			if resumed, _ := res["resumed"].([]string); len(resumed) > 0 {
				tail = "  resumed: " + strings.Join(resumed, ", ")
			}
			fmt.Fprintf(out, "%s %s: %s%s\n", decision, item, subject, tail)
		}
		if failed != nil {
			return exitf(failed.Code, "approvals decide: %s", failed.Msg)
		}
		return nil
	}
	return cmd
}
