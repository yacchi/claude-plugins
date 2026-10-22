package cli

import (
	"encoding/json"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

func newTraceCmd() *cobra.Command {
	tr := groupCommand("trace", "the machine-local operation timing log")

	record := &cobra.Command{Use: "record", Short: "append one timing record (used by the orchestra hooks)", Args: cobra.NoArgs}
	jsonFlag := record.Flags().String("json", "", "the record as a JSON string")
	_ = record.MarkFlagRequired("json")
	record.RunE = func(cmd *cobra.Command, _ []string) error { return runTraceRecord(cmd, *jsonFlag) }
	tr.AddCommand(record)

	report := &cobra.Command{Use: "report", Short: "show which operations slow down as parallelism goes up", Args: cobra.NoArgs}
	since := report.Flags().Duration("since", 24*time.Hour, "window to analyze")
	kinds := report.Flags().StringArray("kind", nil, "only this kind (repeatable)")
	prefix := report.Flags().String("op-prefix", "", "only operations whose name starts with this")
	minCount := report.Flags().Int("min-count", 3, "records a concurrency bucket needs to count toward sensitivity")
	asJSON := report.Flags().Bool("json", false, "machine-readable output")
	report.RunE = func(cmd *cobra.Command, _ []string) error {
		from := time.Now().Add(-*since)
		recs, err := trace.Load(trace.Dir(), from)
		if err != nil {
			return exitf(1, "trace report: %v", err)
		}
		rep := trace.Analyze(recs, from, trace.Options{Kinds: *kinds, OpPrefix: *prefix, MinCount: *minCount})
		if *asJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(rep)
		}
		trace.WriteText(cmd.OutOrStdout(), rep)
		return nil
	}
	tr.AddCommand(report)
	return tr
}

func runTraceRecord(cmd *cobra.Command, raw string) error {
	var r trace.Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return exitf(2, "%s: invalid --json: %v", cmdName(cmd), err)
	}
	if r.Kind == "" || r.Op == "" {
		return exitf(2, "%s: kind and op are required", cmdName(cmd))
	}
	if r.End < r.At {
		return exitf(2, "%s: end is before at", cmdName(cmd))
	}
	if r.MS == 0 {
		r.MS = int64((r.End - r.At) * 1000)
	}
	if r.Load1 == 0 {
		r.Load1 = trace.Load1()
	}
	trace.Write(r)
	return nil
}
