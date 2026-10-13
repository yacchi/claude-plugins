package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
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

const approvalsUsage = "usage: agent-exec approvals --state STATE [--json]\n" +
	"       agent-exec approvals decide --state STATE ITEM allow|deny --reason TEXT\n" +
	"                                   [--by instructor|user] [--json]\n"

// The approvals command parses its own arguments like the Python parser:
// `decide` is only a verb when it comes first, positionals are collected in
// any position, and the usage goes to stderr with exit 2.
func newApprovalsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "approvals [decide ITEM allow|deny]",
		Short:              "pending network, path and dialog approvals of a wave",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApprovals(cmd.OutOrStdout(), cmd.ErrOrStderr(), args)
		},
	}
	// Declared for --help only; DisableFlagParsing means they are never parsed.
	strFlag(cmd, "state", "state.json path")
	strFlag(cmd, "reason", "why (decide)")
	strFlag(cmd, "by", "instructor|user (decide)")
	boolFlag(cmd, "json", "JSON output")
	return cmd
}

func runApprovals(out, errw io.Writer, args []string) error {
	deciding := len(args) > 0 && args[0] == "decide"
	rest := args
	if deciding {
		rest = args[1:]
	}
	if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help") {
		_, _ = io.WriteString(out, approvalsUsage)
		return nil
	}
	usage := func() error {
		_, _ = io.WriteString(errw, approvalsUsage)
		return &ExitError{Code: 2}
	}
	values := map[string]string{}
	var positional []string
	asJSON := false
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case tok == "--json":
			asJSON = true
		case tok == "--state" || tok == "--reason" || tok == "--by":
			if i+1 >= len(rest) {
				return exitf(2, "approvals: missing value for %s", tok)
			}
			values[tok[2:]] = rest[i+1]
			i++
		case strings.HasPrefix(tok, "--"):
			return exitf(2, "approvals: unknown option: %s", tok)
		default:
			positional = append(positional, tok)
		}
	}
	statePath := values["state"]
	if statePath == "" {
		return usage()
	}
	if st, err := os.Stat(statePath); err != nil || !st.Mode().IsRegular() {
		return exitf(3, "approvals: no state file at %s", statePath)
	}
	if !deciding {
		_, hasReason := values["reason"]
		_, hasBy := values["by"]
		if len(positional) > 0 || hasReason || hasBy {
			return usage()
		}
		items, err := approvals.Pending(statePath)
		if err != nil {
			return exitf(1, "approvals: %v", err)
		}
		if asJSON {
			return writeJSONLine(out, items)
		}
		for _, it := range items {
			fmt.Fprintln(out, approvals.FormatLine(it))
		}
		if len(items) > 0 {
			fmt.Fprintf(out, "decide: agent-exec approvals decide --state %s ITEM allow|deny --reason TEXT\n", statePath)
		} else {
			fmt.Fprintln(out, "no pending approvals")
		}
		return nil
	}
	by, ok := values["by"]
	if !ok {
		by = "instructor"
	}
	reason := strings.TrimSpace(values["reason"])
	if len(positional) != 2 || !contains(approvals.Decisions, positional[1]) ||
		!contains(approvals.Bys, by) || reason == "" {
		return usage()
	}
	res, err := approvals.Decide(statePath, positional[0], positional[1], reason, by)
	var failed *approvals.DecisionError
	if err != nil && !errors.As(err, &failed) {
		return exitf(1, "approvals decide: %v", err)
	}
	if asJSON {
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
		fmt.Fprintf(out, "%s %s: %s%s\n", positional[1], positional[0], subject, tail)
	}
	if failed != nil {
		return exitf(failed.Code, "approvals decide: %s", failed.Msg)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
