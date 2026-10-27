package cli

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/statusfile"
)

var statusPlanIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func newStatusCmd() *cobra.Command {
	cmd := groupCommand("status", "the machine-wide status file ~/.claude/orchestra/status.json")
	cmd.AddCommand(newStatusShowCmd(), newStatusPlanCmd(), newStatusCloseCmd())
	return cmd
}

func newStatusShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "rebuild status.json from the run and plan files and print it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := statusfile.Default().Rebuild(time.Now())
			if err != nil {
				return exitf(1, "%s: %v", cmdName(cmd), err)
			}
			return writeJSON(cmd.OutOrStdout(), doc)
		},
	}
}

func newStatusPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan --id ID --status planning|done|needs-user|failed",
		Short: "write one planner's plan entry and rebuild status.json",
		Args:  cobra.NoArgs,
	}
	id := cmd.Flags().String("id", "", "the plan id (letters, digits, - and _; up to 64)")
	status := cmd.Flags().String("status", "", "planning, done, needs-user or failed")
	phase := cmd.Flags().String("phase", "", "the planner's phase: "+strings.Join(statusfile.PlanPhases, ", "))
	repo := cmd.Flags().String("repo", "", "the repository the plan is for (default: the working directory)")
	title := cmd.Flags().String("title", "", "the plan's title (kept from the earlier write when omitted)")
	questions := cmd.Flags().Int("questions", 0, "questions waiting on the user")
	packages := cmd.Flags().Int("packages", 0, "packages in the plan")
	estTotal := cmd.Flags().Int("est-total", 0, "estimated total seconds")
	estCritical := cmd.Flags().Int("est-critical", 0, "estimated critical-path seconds")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("status")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		name := cmdName(cmd)
		if !statusPlanIDPattern.MatchString(*id) {
			return exitf(2, "%s: --id must be 1-64 letters, digits, - or _", name)
		}
		if !slices.Contains(statusfile.PlanStatuses, *status) {
			return exitf(2, "%s: --status must be one of %s", name, strings.Join(statusfile.PlanStatuses, ", "))
		}
		if *phase != "" && !slices.Contains(statusfile.PlanPhases, *phase) {
			return exitf(2, "%s: --phase must be one of %s", name, strings.Join(statusfile.PlanPhases, ", "))
		}
		counts := []struct {
			flag  string
			value int
		}{
			{"questions", *questions},
			{"packages", *packages},
			{"est-total", *estTotal},
			{"est-critical", *estCritical},
		}
		for _, c := range counts {
			if cmd.Flags().Changed(c.flag) && c.value < 0 {
				return exitf(2, "%s: --%s must be 0 or more", name, c.flag)
			}
		}

		dir := *repo
		if dir == "" {
			wd, err := os.Getwd()
			if err != nil {
				return exitf(1, "%s: %v", name, err)
			}
			dir = wd
		}

		now := time.Now()
		store := statusfile.Default()
		p, ok := store.ReadPlan(*id)
		if !ok {
			p = statusfile.Plan{PlanID: *id, StartedAt: now.Unix()}
		}
		p.Status = *status
		if *phase != "" {
			p.Phase = *phase
		}
		switch {
		case *title != "":
			p.Title = *title
		case p.Title == "":
			p.Title = *id
		}
		if cmd.Flags().Changed("questions") {
			p.Questions = *questions
		}
		if cmd.Flags().Changed("packages") {
			p.Packages = intPtr(*packages)
		}
		if cmd.Flags().Changed("est-total") {
			p.EstTotalS = intPtr(*estTotal)
		}
		if cmd.Flags().Changed("est-critical") {
			p.EstCriticalS = intPtr(*estCritical)
		}
		p.Project = statusfile.Project(dir)
		p.UpdatedAt = now.Unix()

		if err := store.WritePlan(p); err != nil {
			return exitf(1, "%s: %v", name, err)
		}
		if _, err := store.Rebuild(now); err != nil {
			return exitf(1, "%s: %v", name, err)
		}
		return writeJSONLine(cmd.OutOrStdout(), map[string]any{
			"path": store.Path(), "plan": *id, "status": *status,
		})
	}
	return cmd
}

func newStatusCloseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "close --state PATH --status done|stopped",
		Short: "mark a wave run ended when its runner is gone (a run waiting on the instructor)",
		Args:  cobra.NoArgs,
	}
	state := cmd.Flags().String("state", "", "state.json path of the run")
	status := cmd.Flags().String("status", "", "done or stopped")
	_ = cmd.MarkFlagRequired("state")
	_ = cmd.MarkFlagRequired("status")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		name := cmdName(cmd)
		if *status != "done" && *status != "stopped" {
			return exitf(2, "%s: --status must be done or stopped", name)
		}

		now := time.Now()
		store := statusfile.Default()
		key := statusfile.RunKey(*state)
		r, ok := store.ReadRun(key)
		if !ok {
			return exitf(3, "%s: no status entry for %s", name, *state)
		}
		if r.EndedAt == nil && fsutil.PIDAlive(r.PID) && now.Unix()-r.UpdatedAt <= statusfile.StaleAfterSeconds {
			return exitf(2, "%s: the runner of %s is still running (pid %d): stop it with agent-exec wave stop --state %s",
				name, *state, r.PID, *state)
		}

		r.Status = *status
		r.UpdatedAt = now.Unix()
		if r.EndedAt == nil {
			ended := now.Unix()
			r.EndedAt = &ended
		}
		if err := store.WriteRun(r); err != nil {
			return exitf(1, "%s: %v", name, err)
		}
		if _, err := store.Rebuild(now); err != nil {
			return exitf(1, "%s: %v", name, err)
		}
		return writeJSONLine(cmd.OutOrStdout(), map[string]any{
			"path": store.Path(), "run": key, "status": *status,
		})
	}
	return cmd
}

func intPtr(v int) *int { return &v }
