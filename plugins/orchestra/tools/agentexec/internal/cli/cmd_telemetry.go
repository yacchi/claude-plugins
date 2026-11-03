package cli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/telemetry"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// telemetryRunRemoved is what `telemetry record` says now: records are derived
// from a run, never handed in.
const telemetryRunRemoved = "telemetry record was removed in orchestra 0.93.0: a run's summary comes from 'agent-exec run-end --run <id>' (or 'agent-exec telemetry run-summary --run <id>'), and dispatch records are written by dispatch itself"

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

	// record is removed: any call, with any arguments or flags, gets the removal
	// message (flags are not parsed, so cobra cannot reject them first).
	record := sub("record", "removed in orchestra 0.93.0 (use telemetry run-summary)")
	record.Args = cobra.ArbitraryArgs
	record.DisableFlagParsing = true
	record.RunE = func(*cobra.Command, []string) error { return exitf(2, "%s", telemetryRunRemoved) }

	rs := sub("run-summary", "derive and record the run_summary telemetry record of one run")
	runID := rs.Flags().String("run", "", "run id")
	_ = rs.MarkFlagRequired("run")
	rs.RunE = func(cmd *cobra.Command, _ []string) error { return telemetryRunSummary(cmd, *runID) }

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

// telemetryRunSummary derives the run_summary record of run id from its wave
// state, its plan and its ledger dispatch records, and appends it. Exactly one
// JSON line goes to stdout on every path past the id check.
func telemetryRunSummary(cmd *cobra.Command, id string) error {
	if !ledger.RunIDPattern.MatchString(id) {
		return exitf(2, "telemetry run-summary: invalid --run: %s", id)
	}
	cfg := configOrDefaults()
	var dispatches []map[string]any
	for _, path := range ledger.SelectedPaths(ledger.Dir(cfg), []string{id}, nil) {
		dispatches = append(dispatches, ledger.ReadFile(path)...)
	}
	tasks, err := telemetryRunTasks(paths.PlanDir(id))
	if err != nil {
		return telemetryRunFail(cmd, err.Error())
	}
	if len(tasks) == 0 && len(dispatches) == 0 {
		return telemetryRunFail(cmd, "run "+id+" has no dispatch records and no wave state")
	}
	rec, err := telemetry.Write(telemetry.RunSummary(tasks, dispatches), cfg)
	switch {
	case err == nil:
		return writeJSONLine(cmd.OutOrStdout(), map[string]any{"recorded": true, "record": rec})
	case errors.Is(err, telemetry.ErrDisabled):
		return writeJSONLine(cmd.OutOrStdout(), map[string]any{
			"recorded": false,
			"reason":   "telemetry is off (telemetry.enabled is not true)",
		})
	default:
		return telemetryRunFail(cmd, err.Error())
	}
}

// telemetryRunFail prints the not-recorded line and exits 1.
func telemetryRunFail(cmd *cobra.Command, reason string) error {
	if err := writeJSONLine(cmd.OutOrStdout(), map[string]any{"recorded": false, "reason": reason}); err != nil {
		return err
	}
	return &ExitError{Code: 1}
}

// telemetryRunTasks reads the wave under dir (plan.json and state.json). A
// directory without both files is no wave: nil tasks, no error.
func telemetryRunTasks(dir string) ([]telemetry.RunTask, error) {
	planPath := filepath.Join(dir, "plan.json")
	statePath := filepath.Join(dir, "state.json")
	for _, p := range []string{planPath, statePath} {
		if _, err := os.Stat(p); err != nil {
			return nil, nil
		}
	}
	p, err := plan.Load(planPath)
	if err != nil {
		return nil, err
	}
	store, err := state.Open(statePath)
	if err != nil {
		return nil, err
	}
	snap, err := store.Load()
	if err != nil {
		return nil, err
	}
	tasks := make([]telemetry.RunTask, 0, len(p.Packages))
	for _, pkg := range p.Packages {
		task := telemetry.RunTask{Cls: pkg.Cls}
		if sp, ok := snap.Packages[pkg.ID]; ok && sp != nil {
			task.Status = sp.Status
			task.Attempts = sp.Attempts
			if sp.Executor != nil {
				task.Executor = *sp.Executor
			}
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
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
