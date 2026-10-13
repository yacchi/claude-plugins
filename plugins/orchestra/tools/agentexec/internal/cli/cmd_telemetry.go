package cli

import "github.com/spf13/cobra"

func newTelemetryCmd() *cobra.Command {
	tel := groupCommand("telemetry", "anonymized, opt-in telemetry")
	sub := func(name, short string) *cobra.Command {
		cmd := withPrefix(&cobra.Command{
			Use:   name,
			Short: short,
			Args:  noArgs,
			RunE:  notImplemented("telemetry " + name),
		}, "telemetry")
		tel.AddCommand(cmd)
		return cmd
	}
	strFlag(sub("enable", "set telemetry.enabled: true in a scope's orchestra.yaml"), "scope", "user|project|local")
	strFlag(sub("disable", "set telemetry.enabled: false in a scope's orchestra.yaml"), "scope", "user|project|local")
	record := sub("record", "append an anonymized telemetry record")
	strFlag(record, "json", "the record as a JSON string")
	strFlag(record, "file", "a file holding the record")
	boolFlag(sub("show", "print recorded telemetry"), "json", "raw JSONL")
	strFlag(sub("archive", "tar.gz the telemetry directory"), "out", "archive path")
	sub("clear", "delete all recorded telemetry")
	return tel
}
