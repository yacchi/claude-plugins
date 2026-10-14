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

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/telemetry"
)

func newTelemetryCmd() *cobra.Command {
	tel := groupCommand("telemetry", "anonymized, opt-in telemetry")
	sub := func(name, short string) *cobra.Command {
		cmd := &cobra.Command{Use: name, Short: short, Args: cobra.NoArgs}
		tel.AddCommand(cmd)
		return cmd
	}

	for _, enabled := range []bool{true, false} {
		name := "disable"
		if enabled {
			name = "enable"
		}
		cmd := sub(name, "set telemetry.enabled: "+fmt.Sprint(enabled)+" in a scope's orchestra.yaml")
		scope := cmd.Flags().String("scope", "user", "user|project|local")
		cmd.RunE = func(cmd *cobra.Command, _ []string) error { return runToggle(cmd, *scope, enabled) }
	}

	record := sub("record", "append an anonymized telemetry record")
	jsonFlag := record.Flags().String("json", "", "the record as a JSON string")
	fileFlag := record.Flags().String("file", "", "a file holding the record")
	record.MarkFlagsOneRequired("json", "file")
	record.MarkFlagsMutuallyExclusive("json", "file")
	record.RunE = func(cmd *cobra.Command, _ []string) error {
		data := []byte(*jsonFlag)
		if cmd.Flags().Changed("file") {
			b, err := os.ReadFile(*fileFlag)
			if err != nil {
				return exitf(2, "telemetry record: %v", err)
			}
			data = b
		}
		return runRecord(data)
	}

	show := sub("show", "print recorded telemetry")
	showJSON := show.Flags().Bool("json", false, "raw JSONL")
	show.RunE = func(cmd *cobra.Command, _ []string) error { return runShow(cmd, *showJSON) }

	archive := sub("archive", "tar.gz the telemetry directory")
	out := archive.Flags().String("out", "", "archive path (default: orchestra-telemetry-archive.tgz beside the telemetry directory)")
	archive.RunE = func(cmd *cobra.Command, _ []string) error { return runArchive(cmd, *out) }

	sub("clear", "delete all recorded telemetry").RunE = func(*cobra.Command, []string) error {
		_ = os.Remove(telemetry.RecordsPath(configOrDefaults()))
		return nil
	}
	return tel
}

func runToggle(cmd *cobra.Command, scope string, enabled bool) error {
	if scope != "user" && scope != "project" && scope != "local" {
		return exitf(2, "%s: invalid --scope: %s", cmdName(cmd), scope)
	}
	path, err := telemetry.Toggle(scope, enabled)
	if err != nil {
		return exitf(1, "%s: %v", cmdName(cmd), err)
	}
	word := "disabled"
	if enabled {
		word = "enabled"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "telemetry %s -> %s\n", word, path)
	return nil
}

func runRecord(data []byte) error {
	v, err := fsutil.DecodeJSON(data)
	if err != nil {
		return exitf(2, "telemetry record: invalid JSON input")
	}
	rec, ok := v.(map[string]any)
	if ev := rec["event"]; !ok || (ev != "run_summary" && ev != "dispatch") {
		return exitf(2, `telemetry record: "event" must be "run_summary" or "dispatch"`)
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

func runArchive(cmd *cobra.Command, out string) error {
	dir := telemetry.Dir(configOrDefaults())
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return exitf(1, "telemetry archive: directory does not exist: %s", dir)
	}
	if !cmd.Flags().Changed("out") {
		out = filepath.Join(filepath.Dir(telemetry.AbsPath(dir)), "orchestra-telemetry-archive.tgz")
	}
	out = telemetry.AbsPath(out)
	if err := writeTarGz(out, dir); err != nil {
		return exitf(1, "telemetry archive: %v", err)
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
