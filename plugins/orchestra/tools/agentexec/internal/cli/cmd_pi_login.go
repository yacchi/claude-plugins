package cli

import (
	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/pilogin"
)

func newPiCmd() *cobra.Command {
	pi := groupCommand("pi", "pi helpers")
	login := &cobra.Command{
		Use:   "login",
		Short: "log pi in through orchestra's own herdr",
		Args:  cobra.NoArgs,
	}
	o := pilogin.Options{Mode: pilogin.ModeStart}
	fl := login.Flags()
	fl.StringVar(&o.Provider, "provider", "", "provider id")
	wait := fl.Bool("wait", false, "poll until the login lands")
	fl.StringVar(&o.Paste, "paste", "", "send the final redirect URL")
	cancel := fl.Bool("cancel", false, "cancel a pending login")
	fl.Float64Var(&o.Timeout, "timeout", pilogin.DefaultTimeout, "seconds")
	fl.Bool("json", false, "JSON output (the default and only format)")
	login.MarkFlagsMutuallyExclusive("wait", "paste", "cancel")
	login.RunE = func(cmd *cobra.Command, _ []string) error {
		switch {
		case *wait:
			o.Mode = pilogin.ModeWait
		case *cancel:
			o.Mode = pilogin.ModeCancel
		case cmd.Flags().Changed("paste"):
			o.Mode = pilogin.ModePaste
		}
		if !finiteNonNegative(o.Timeout) {
			return exitf(2, "pi login: --timeout must be a non-negative number of seconds")
		}
		o.Cfg = configOrDefaults()
		result, code := pilogin.Run(o)
		if err := writeJSONLine(cmd.OutOrStdout(), result); err != nil {
			return err
		}
		if code != 0 {
			return &ExitError{Code: code}
		}
		return nil
	}
	pi.AddCommand(login)
	return pi
}
