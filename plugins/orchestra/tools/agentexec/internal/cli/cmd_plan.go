package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/planning"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
)

func newPlanCmd() *cobra.Command {
	cmd := groupCommand("plan", "the planning stages: skeleton, Codex review, spec writers")
	cmd.AddCommand(newPlanNextCmd(), newPlanReviewCmd(), newPlanLintCmd())
	return cmd
}

func newPlanNextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "next --plan P [--id ID] [--repo DIR] [--title T] [--returned ID]... [--answers FILE]",
		Short: "print the next planning step and record it in the planning state",
		Args:  cobra.NoArgs,
	}
	planPath := cmd.Flags().String("plan", "", "plan.json path")
	id := cmd.Flags().String("id", "", "the plan's id on the status page (letters, digits, - and _; up to 64)")
	repo := cmd.Flags().String("repo", "", "the repository the plan is for")
	title := cmd.Flags().String("title", "", "the plan's title on the status page")
	returned := cmd.Flags().StringArray("returned", nil, "a package whose spec writer has returned (repeatable)")
	answers := cmd.Flags().String("answers", "", "file holding the user's answers to the planner's open questions")
	_ = cmd.MarkFlagRequired("plan")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		name := cmdName(cmd)
		if cmd.Flags().Changed("id") && !statusPlanIDPattern.MatchString(*id) {
			return exitf(2, "%s: --id must be 1-64 letters, digits, - or _", name)
		}
		if cmd.Flags().Changed("answers") {
			if _, err := os.Stat(*answers); err != nil {
				return exitf(2, "%s: --answers: %v", name, err)
			}
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		now := time.Now()
		in := planning.NextInput{ID: *id, Repo: *repo, Title: *title, Returned: *returned, Answers: *answers}
		step, st, err := planning.Next(cfg, *planPath, in, now)
		if err != nil {
			return exitf(1, "%s: %v", name, err)
		}
		if st.ID != "" {
			writePlanStepStatus(cmd, name, planStepStatus(st, step), now)
		}
		return writeJSONLine(cmd.OutOrStdout(), step)
	}
	return cmd
}

// planStepStatus is the status-file entry one step leaves behind. A step that
// does not name a phase keeps the earlier one.
func planStepStatus(st planning.State, step planning.Step) planStatus {
	ps := planStatus{ID: st.ID, Repo: st.Repo, Title: st.Title}
	switch step.Step {
	case "skeleton":
		ps.Status, ps.Phase = "planning", "explore"
	case "specs", "wait":
		ps.Status, ps.Phase = "planning", "specs"
		ps.Packages, ps.EstTotal, ps.EstCritical = intPtr(step.Packages), intPtr(step.EstTotal), intPtr(step.EstCritical)
	case "done":
		ps.Status, ps.Phase = "done", "done"
		ps.Packages, ps.EstTotal, ps.EstCritical = intPtr(step.Packages), intPtr(step.EstTotal), intPtr(step.EstCritical)
		ps.Questions = intPtr(0)
	case "stuck":
		ps.Status = "needs-user"
		ps.Questions = intPtr(len(step.Reasons))
	}
	return ps
}

func newPlanReviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review --plan P [--repo DIR] [--max-wait SECONDS] [--resolved]",
		Short: "run the Codex review of the skeleton, or mark an accepted review resolved",
		Args:  cobra.NoArgs,
	}
	planPath := cmd.Flags().String("plan", "", "plan.json path")
	repo := cmd.Flags().String("repo", "", "the repository the reviewer reads (default: the plan's repo)")
	maxWait := cmd.Flags().Int("max-wait", 540, "seconds to wait for a running review before reporting it running")
	resolved := cmd.Flags().Bool("resolved", false, "mark the accepted review resolved so plan next moves on")
	_ = cmd.MarkFlagRequired("plan")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		name := cmdName(cmd)
		if *maxWait < 0 {
			return exitf(2, "%s: --max-wait must be 0 or more", name)
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		now := time.Now()
		var out map[string]any
		if *resolved {
			out, err = planning.ResolveReview(cfg, *planPath)
		} else {
			opts := planning.ReviewOptions{Repo: *repo, MaxWait: time.Duration(*maxWait) * time.Second}
			out, err = planning.RunReview(cfg, *planPath, opts, now)
		}
		if err != nil {
			var refused *planning.RefusedError
			if errors.As(err, &refused) {
				return &ExitError{Code: 1, Msg: refused.Msg}
			}
			return planFailure(name, err)
		}

		if !*resolved {
			st, err := planning.Load(*planPath)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agent-exec: %s: status: %v\n", name, err)
			} else if st.ID != "" {
				writePlanStepStatus(cmd, name, planStatus{ID: st.ID, Status: "planning", Phase: "review", Repo: st.Repo, Title: st.Title}, now)
			}
		}
		return writeJSONLine(cmd.OutOrStdout(), out)
	}
	return cmd
}

func newPlanLintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lint --plan P [--package ID]",
		Short: "check a planning skeleton, or the spec of one package",
		Args:  cobra.NoArgs,
	}
	planPath := cmd.Flags().String("plan", "", "plan.json path")
	pkgID := cmd.Flags().String("package", "", "check this package's spec instead of the skeleton")
	_ = cmd.MarkFlagRequired("plan")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		name := cmdName(cmd)
		out := cmd.OutOrStdout()
		p, err := plan.LoadSkeleton(*planPath)
		if err != nil {
			return planFailure(name, err)
		}

		if *pkgID == "" {
			warnings := plan.LintSkeleton(p)
			printPlanWarnings(out, warnings)
			var light, standard, deep int
			for _, pkg := range p.Packages {
				switch pkg.Cls {
				case "light":
					light++
				case "standard":
					standard++
				case "deep":
					deep++
				}
			}
			total, critical := plan.Estimate(p)
			fmt.Fprintf(out, "plan: %d packages (%d light, %d standard, %d deep), %ds total, %ds critical path\n",
				len(p.Packages), light, standard, deep, total, critical)
			if len(warnings) > 0 {
				return &ExitError{Code: 1}
			}
			return nil
		}

		pkg, ok := p.Package(*pkgID)
		if !ok {
			return exitf(2, "%s: unknown package %s", name, *pkgID)
		}
		data, err := os.ReadFile(pkg.Spec)
		if os.IsNotExist(err) {
			return exitf(3, "%s: %s: spec not written: %s", name, pkg.ID, pkg.Spec)
		}
		if err != nil {
			return exitf(1, "%s: %v", name, err)
		}
		warnings := plan.SpecWarnings(p, pkg)
		printPlanWarnings(out, warnings)
		if len(warnings) > 0 {
			return &ExitError{Code: 1}
		}
		_, err = fmt.Fprintf(out, "plan: %s spec ok (%d lines)\n", pkg.ID, planLineCount(string(data)))
		return err
	}
	return cmd
}

// planFailure is the exit for a plan the commands cannot work from: 3 for an
// invalid plan, 1 for anything else.
func planFailure(name string, err error) *ExitError {
	var pe *plan.Error
	if errors.As(err, &pe) {
		return exitf(3, "%s: %s", name, pe.Msg)
	}
	return exitf(1, "%s: %v", name, err)
}

// writePlanStepStatus records a plan's status after a step. A failed write is
// reported on stderr and never changes the step's exit code.
func writePlanStepStatus(cmd *cobra.Command, name string, ps planStatus, now time.Time) {
	if _, err := writePlanStatus(ps, now); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "agent-exec: %s: status: %v\n", name, err)
	}
}

func printPlanWarnings(out io.Writer, warnings []plan.Warning) {
	for _, w := range warnings {
		if w.Pkg != nil {
			fmt.Fprintf(out, "plan-warning: %s: %s\n", *w.Pkg, w.Message)
		} else {
			fmt.Fprintf(out, "plan-warning: %s\n", w.Message)
		}
	}
}

// planLineCount counts lines as the plan package does: a final line without a
// trailing newline counts.
func planLineCount(text string) int {
	n := strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}
