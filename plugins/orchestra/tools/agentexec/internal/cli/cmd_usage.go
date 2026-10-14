package cli

import (
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/usage"
)

func newUsageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "aggregate token usage and estimated cost",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	since := fl.String("since", "24h", "<N>m|<N>h|<N>d or an ISO8601 timestamp")
	runs := fl.StringArray("run", nil, "run id(s), comma-separated (repeatable)")
	sessions := fl.StringArray("session", nil, "session id(s), comma-separated (repeatable)")
	source := fl.StringArray("source", nil, "comma-separated sources (claude,pi)")
	allProjects := fl.Bool("all-projects", false, "do not scope to this working directory")
	listRuns := fl.Bool("list-runs", false, "list the recorded runs")
	format := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return runUsage(cmd, *since, *runs, *sessions, *source, *allProjects, *listRuns, format.is("text"))
	}
	return cmd
}

// selectors splits and validates the values of --run or --session.
func selectors(flag string, raw []string, pattern ledger.Matcher) ([]string, error) {
	var out []string
	for _, v := range raw {
		values := strings.Split(v, ",")
		for _, value := range values {
			if value == "" {
				return nil, exitf(2, "usage: empty value for %s", flag)
			}
		}
		for _, value := range values {
			if !ledger.ValidSelector(value, pattern) {
				return nil, exitf(2, "usage: invalid value for %s", flag)
			}
		}
		out = append(out, values...)
	}
	return out, nil
}

func runUsage(cmd *cobra.Command, since string, runFlags, sessionFlags, sourceFlags []string,
	allProjects, listRuns, asText bool) error {
	sources := append([]string(nil), usage.Sources...)
	for _, raw := range sourceFlags {
		var names []string
		for _, x := range strings.Split(raw, ",") {
			if x = strings.TrimSpace(x); x != "" {
				names = append(names, x)
			}
		}
		var unknown []string
		for _, x := range names {
			if x != "claude" && x != "pi" {
				unknown = append(unknown, x)
			}
		}
		if len(unknown) > 0 || len(names) == 0 {
			shown := strings.Join(unknown, ",")
			if shown == "" {
				shown = "<empty>"
			}
			return exitf(2, "usage: unknown --source: %s (known: %s)", shown, strings.Join(usage.Sources, ","))
		}
		sources = names
	}
	runIDs, err := selectors("--run", runFlags, ledger.RunSelector)
	if err != nil {
		return err
	}
	sessionIDs, err := selectors("--session", sessionFlags, ledger.SessionPattern)
	if err != nil {
		return err
	}
	sinceGiven := cmd.Flags().Changed("since")
	if listRuns && (len(runIDs) > 0 || len(sessionIDs) > 0 || sinceGiven) {
		return exitf(2, "usage: --list-runs cannot be combined with scope or --since")
	}
	if (len(runIDs) > 0 || len(sessionIDs) > 0) && sinceGiven {
		return exitf(2, "usage: --since cannot be combined with --run or --session")
	}

	now := time.Now().UTC()
	cutoff, ok := usage.ParseSince(since, now)
	if !ok {
		return exitf(2, "usage: invalid --since: %s (expected <N>m|<N>h|<N>d or an ISO8601 timestamp)", since)
	}

	cfg := configOrDefaults()
	w := cmd.OutOrStdout()
	if len(runIDs) > 0 {
		dir := ledger.Dir(cfg)
		for i, v := range runIDs {
			if resolved, ok := ledger.ResolveRunSelector(dir, v); ok {
				runIDs[i] = resolved
			}
		}
	}
	if listRuns {
		runs := usage.ListRuns(usage.RunsOptions{Sources: sources, Cfg: cfg, AllProjects: allProjects})
		if asText {
			_, err := io.WriteString(w, usage.FormatRunsText(runs))
			return err
		}
		return writeJSON(w, map[string]any{"runs": runs})
	}
	report := usage.BuildReport(usage.ReportOptions{
		Since: cutoff, Now: now, Sources: sources, AllProjects: allProjects, Cfg: cfg,
		RunIDs: runIDs, SessionIDs: sessionIDs,
	})
	if asText {
		_, err := io.WriteString(w, usage.FormatText(report))
		return err
	}
	return writeJSON(w, report)
}
