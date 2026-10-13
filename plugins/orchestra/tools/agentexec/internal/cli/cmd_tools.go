package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

const toolsUsage = "usage: agent-exec tools [status] [--json|--text] | " +
	"install [NAME ...] [--installer auto|mise|npm] [--force] " +
	"[--json|--text]\n"

// newToolsCmd: bare `tools` is `tools status`. Output is text unless --json
// is given without --text.
func newToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "orchestra-managed executor binaries",
		Args:  cobra.ArbitraryArgs,
	}
	format := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			fmt.Fprint(cmd.ErrOrStderr(), "agent-exec: tools: unknown subcommand: "+args[0]+"\n"+toolsUsage)
			return &ExitError{Code: 2}
		}
		return runToolsStatus(cmd, format)
	}

	status := withPrefix(&cobra.Command{
		Use:   "status",
		Short: "where each tool resolves from",
		Args:  cobra.ArbitraryArgs,
	}, "tools")
	statusFormat := formatFlags(status, "json", "text")
	status.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			fmt.Fprint(cmd.ErrOrStderr(), toolsUsage)
			return &ExitError{Code: 2}
		}
		return runToolsStatus(cmd, statusFormat)
	}

	install := withPrefix(&cobra.Command{
		Use:   "install [NAME...]",
		Short: "install or update the pinned tools",
		Args:  cobra.ArbitraryArgs,
	}, "tools")
	installer := strFlag(install, "installer", "auto|mise|npm")
	force := boolFlag(install, "force", "reinstall even when current")
	installFormat := formatFlags(install, "json", "text")
	install.RunE = func(cmd *cobra.Command, args []string) error {
		if v, ok := installer.Get(); ok && v != "auto" && v != "mise" && v != "npm" {
			return exitf(2, "tools: unknown installer: %s", v)
		}
		cfg := configOrDefaults()
		valid := map[string]bool{}
		for _, n := range tools.KnownNames(cfg) {
			valid[n] = true
		}
		for _, n := range args {
			if !valid[n] {
				return exitf(2, "tools: unknown tool: %s", n)
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
		results := tools.Install(cfg, names, installer.Or(""), force.On())
		out := cmd.OutOrStdout()
		if asJSON(installFormat) {
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
	return withPrefix(cmd, "tools")
}

func asJSON(f *format) bool { return f.Given("json") && !f.Given("text") }

func runToolsStatus(cmd *cobra.Command, f *format) error {
	items := tools.Status(configOrDefaults())
	if asJSON(f) {
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
