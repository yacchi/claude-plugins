package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/telemetry"
)

func newTelemetryCmd() *cobra.Command {
	tel := groupCommand("telemetry", "anonymized, opt-in telemetry")
	tel.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return exitf(2, "telemetry: unknown subcommand: %s", args[0])
		}
		return exitf(2, "telemetry: missing subcommand")
	}
	sub := func(name, short string, run func(*cobra.Command) error) *cobra.Command {
		cmd := withPrefix(&cobra.Command{
			Use:   name,
			Short: short,
			Args:  noArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return run(cmd) },
		}, "telemetry")
		tel.AddCommand(cmd)
		return cmd
	}

	toggle := func(name string, enabled bool) {
		var scopes *[]string
		cmd := sub(name, "set telemetry.enabled: "+fmt.Sprint(enabled)+" in a scope's orchestra.yaml",
			func(cmd *cobra.Command) error { return runToggle(cmd, *scopes, enabled) })
		scopes = listFlag(cmd, "scope", "user|project|local")
	}
	toggle("enable", true)
	toggle("disable", false)

	record := sub("record", "append an anonymized telemetry record", nil)
	jsonFlag := strFlag(record, "json", "the record as a JSON string")
	fileFlag := strFlag(record, "file", "a file holding the record")
	record.RunE = func(cmd *cobra.Command, _ []string) error { return runRecord(cmd, jsonFlag, fileFlag) }

	// show and clear ignore anything they do not know, as the Python parser
	// did (it only looked for --json).
	show := sub("show", "print recorded telemetry", nil)
	showJSON := boolFlag(show, "json", "raw JSONL")
	lenient(show)
	show.RunE = func(cmd *cobra.Command, _ []string) error { return runShow(cmd, showJSON.On()) }

	archive := sub("archive", "tar.gz the telemetry directory", nil)
	out := strFlag(archive, "out", "archive path")
	archive.RunE = func(cmd *cobra.Command, _ []string) error { return runArchive(cmd, out) }

	clearCmd := sub("clear", "delete all recorded telemetry", func(cmd *cobra.Command) error {
		_ = os.Remove(telemetry.RecordsPath(configOrDefaults()))
		return nil
	})
	lenient(clearCmd)
	return tel
}

// lenient makes cmd accept unknown flags and positional arguments.
func lenient(cmd *cobra.Command) {
	cmd.Args = cobra.ArbitraryArgs
	cmd.FParseErrWhitelist.UnknownFlags = true
}

func runToggle(cmd *cobra.Command, scopes []string, enabled bool) error {
	scope := "user"
	for _, s := range scopes {
		if s != "user" && s != "project" && s != "local" {
			return exitf(2, "telemetry: invalid --scope: %s", s)
		}
		scope = s
	}
	path, err := telemetry.Toggle(scope, enabled)
	if err != nil {
		return exitf(1, "telemetry: %v", err)
	}
	word := "disabled"
	if enabled {
		word = "enabled"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "telemetry %s -> %s\n", word, path)
	return nil
}

func runRecord(cmd *cobra.Command, jsonFlag, fileFlag *optString) error {
	jsonStr, hasJSON := jsonFlag.Get()
	filePath, hasFile := fileFlag.Get()
	if !hasJSON && !hasFile {
		return exitf(2, "telemetry: record requires --json or --file")
	}
	data := []byte(jsonStr)
	if hasFile {
		b, err := os.ReadFile(filePath)
		if err != nil {
			return exitf(2, "telemetry: invalid JSON input")
		}
		data = b
	}
	v, err := jsonx.Decode(data)
	if err != nil {
		return exitf(2, "telemetry: invalid JSON input")
	}
	rec, ok := v.(*jsonx.Object)
	if ev, _ := rec.Get("event"); !ok || (ev != "run_summary" && ev != "dispatch") {
		return exitf(2, "telemetry: record missing valid 'event'")
	}
	telemetry.Append(rec, configOrDefaults())
	return nil
}

func runShow(cmd *cobra.Command, asJSON bool) error {
	records := telemetry.ReadRecords(telemetry.RecordsPath(configOrDefaults()))
	w := cmd.OutOrStdout()
	if asJSON {
		for _, r := range records {
			if err := writeJSONLine(w, r); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := io.WriteString(w, telemetry.Summary(records))
	return err
}

func runArchive(cmd *cobra.Command, outFlag *optString) error {
	dir := telemetry.Dir(configOrDefaults())
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return exitf(1, "telemetry: directory does not exist: %s", dir)
	}
	out, given := outFlag.Get()
	if !given {
		out = filepath.Join(filepath.Dir(telemetry.AbsPath(dir)), "orchestra-telemetry-archive.tgz")
	}
	out = telemetry.AbsPath(out)
	if err := writeTarGz(out, dir); err != nil {
		return exitf(1, "telemetry: %v", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), out)
	return nil
}

// writeTarGz archives dir under its own base name.
func writeTarGz(out, dir string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	base := filepath.Base(strings.TrimRight(dir, "/"))
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		name := base
		if rel != "." {
			name = filepath.ToSlash(filepath.Join(base, rel))
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	for _, c := range []io.Closer{tw, gz, f} {
		if err := c.Close(); err != nil && walkErr == nil {
			walkErr = err
		}
	}
	return walkErr
}
