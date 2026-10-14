package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// newToolsCmd: bare `tools` is `tools status`. Output is text unless --json
// is given.
func newToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "orchestra-managed executor binaries",
		Args:  cobra.ArbitraryArgs,
	}
	format := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return exitf(2, "tools: unknown subcommand: %s", args[0])
		}
		return runToolsStatus(cmd, format.is("json"))
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "where each tool resolves from",
		Args:  cobra.NoArgs,
	}
	statusFormat := formatFlags(status, "json", "text")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		return runToolsStatus(cmd, statusFormat.is("json"))
	}

	install := &cobra.Command{
		Use:   "install [NAME...]",
		Short: "install or update the pinned tools",
		Args:  cobra.ArbitraryArgs,
	}
	installer := install.Flags().String("installer", "", "auto|mise|npm (default: the configured installer)")
	force := install.Flags().Bool("force", false, "reinstall even when current")
	installFormat := formatFlags(install, "json", "text")
	install.RunE = func(cmd *cobra.Command, args []string) error {
		switch *installer {
		case "auto", "mise", "npm":
		default:
			if cmd.Flags().Changed("installer") {
				return exitf(2, "tools install: unknown installer: %s", *installer)
			}
		}
		cfg := configOrDefaults()
		valid := map[string]bool{}
		for _, n := range tools.KnownNames(cfg) {
			valid[n] = true
		}
		for _, n := range args {
			if !valid[n] {
				return exitf(2, "tools install: unknown tool: %s", n)
			}
		}
		names := args
		if len(names) == 0 {
			for _, n := range tools.Names(cfg) {
				if tools.ConfigFor(n, cfg, "").Spec != "" {
					names = append(names, n)
				}
			}
		}
		results := tools.Install(cfg, names, *installer, *force)
		out := cmd.OutOrStdout()
		if installFormat.is("json") {
			if err := writeJSON(out, results); err != nil {
				return err
			}
		} else {
			printLines(out, results, tools.InstallLine)
		}
		if tools.Failed(results) {
			return &ExitError{Code: 1}
		}
		return nil
	}

	cmd.AddCommand(status, install)
	return cmd
}

func runToolsStatus(cmd *cobra.Command, asJSON bool) error {
	items := tools.Status(configOrDefaults())
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), items)
	}
	printLines(cmd.OutOrStdout(), items, tools.StatusLine)
	return nil
}

func printLines(w io.Writer, items []tools.Item, line func(tools.Item) string) {
	for _, it := range items {
		fmt.Fprintln(w, line(it))
	}
}
