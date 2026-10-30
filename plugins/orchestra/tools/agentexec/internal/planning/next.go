package planning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
)

const (
	MaxSkeletonSteps = 3
	MaxRevisions     = 2
	MaxSpecAttempts  = 2
	WriterTimeout    = 30 * time.Minute
)

type NextInput struct {
	ID, Repo, Title string   // stored in the state when non-empty (Repo made absolute)
	Returned        []string // package ids whose spec writers have returned
	Answers         string   // file holding the user's answers to the planner's open questions
}

type Writer struct {
	ID          string `json:"id"`
	Cls         string `json:"cls"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	Spec        string `json:"spec"`        // absolute
	Description string `json:"description"` // "spec <id>"
	Prompt      string `json:"prompt"`      // "Plan: <abs plan>\nPackage: <id>\nSpec: <abs spec>" + "\nFix: <reason>" per reason
}

type Step struct {
	Step        string   `json:"step"`             // skeleton | specs | wait | done | stuck
	Plan        string   `json:"plan"`             // absolute
	Agent       string   `json:"agent,omitempty"`  // skeleton: PlannerAgent; specs: WriterAgent
	Model       string   `json:"model,omitempty"`  // skeleton
	Effort      string   `json:"effort,omitempty"` // skeleton
	Round       int      `json:"round,omitempty"`  // skeleton
	Note        string   `json:"note,omitempty"`   // skeleton: the instructor appends it verbatim to the planner prompt
	Writers     []Writer `json:"writers,omitempty"`
	Reasons     []string `json:"reasons"`            // never null
	Packages    int      `json:"packages,omitempty"` // specs, wait, done
	EstTotal    int      `json:"est_total,omitempty"`
	EstCritical int      `json:"est_critical,omitempty"`
	Warnings    []string `json:"warnings,omitempty"` // done: plan.Lint messages as "<pkg>: <msg>" or "<msg>"
}

// EntryHash is the sha256 of everything a spec writer works from, so a spec
// written for an older entry (or an older interface file) is rewritten.
func EntryHash(p *plan.Plan, pkg plan.Package) string {
	type ifaceText struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	ifaces := make([]ifaceText, 0, len(pkg.Interfaces))
	for _, id := range pkg.Interfaces {
		var text string
		if iface, ok := p.Interface(id); ok {
			// An unreadable file hashes as empty; LoadSkeleton has checked it exists.
			raw, _ := os.ReadFile(iface.Path)
			text = string(raw)
		}
		ifaces = append(ifaces, ifaceText{id, text})
	}
	entry := struct {
		Cls          string      `json:"cls"`
		DependsOn    []string    `json:"depends_on"`
		FilesOwned   []string    `json:"files_owned"`
		EstSeconds   float64     `json:"est_seconds"`
		Verification string      `json:"verification"`
		Brief        string      `json:"brief"`
		Risks        []string    `json:"risks"`
		Interfaces   []ifaceText `json:"interfaces"`
	}{pkg.Cls, pkg.DependsOn, pkg.FilesOwned, pkg.EstSeconds, pkg.Verification, pkg.Brief, pkg.Risks, ifaces}
	data, _ := json.Marshal(entry)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Next decides the one next planning step from the files beside planPath and
// records what it issued in the planning state.
func Next(cfg *config.Resolved, planPath string, in NextInput, now time.Time) (Step, State, error) {
	var step Step
	var st State
	err := Locked(planPath, func() error {
		var err error
		st, err = Load(planPath)
		if err != nil {
			return err
		}
		step, err = nextDecide(cfg, planPath, in, now, &st)
		if err != nil {
			return err
		}
		return Save(planPath, st)
	})
	if err != nil {
		return Step{}, State{}, err
	}
	return step, st, nil
}

func nextDecide(cfg *config.Resolved, planPath string, in NextInput, now time.Time, st *State) (Step, error) {
	abs, err := filepath.Abs(planPath)
	if err != nil {
		return Step{}, err
	}
	if in.ID != "" {
		st.ID = in.ID
	}
	if in.Title != "" {
		st.Title = in.Title
	}
	if in.Repo != "" {
		repo, err := filepath.Abs(in.Repo)
		if err != nil {
			return Step{}, err
		}
		st.Repo = repo
	}
	for _, id := range in.Returned {
		if rec, ok := st.Specs[id]; ok {
			rec.InFlight = false
			st.Specs[id] = rec
		}
	}
	P := config.Planning(cfg)

	var answersNote string
	if in.Answers != "" {
		answers, err := filepath.Abs(in.Answers)
		if err != nil {
			return Step{}, err
		}
		answersNote = "The user answered your open questions in " + answers + "; read it first. Continue the skeleton at " +
			abs + " in place, then run agent-exec plan review."
	}

	if st.Round == 0 {
		st.Round, st.Model, st.Effort, st.SkeletonSteps = 1, P.Skeleton.Model, P.Skeleton.Effort, 1
		return nextSkeleton(abs, st, []string{}, answersNote), nil
	}
	continueNote := fmt.Sprintf("Continue the round-%d skeleton at %s in place: fix what reasons lists, then finish with agent-exec plan review.",
		st.Round, abs)
	if in.Answers != "" {
		// The answers may change the split, so the round is reviewed again.
		if st.Review != nil && st.Review.Round == st.Round {
			st.Review = nil
		}
		return nextContinueSkeleton(abs, st, "the user answered the open questions", answersNote), nil
	}

	p, err := plan.LoadSkeleton(planPath)
	if err != nil {
		return nextContinueSkeleton(abs, st, err.Error(), continueNote), nil
	}

	review := st.Review
	if review == nil || review.Round != st.Round || review.Status == "running" {
		return nextContinueSkeleton(abs, st, fmt.Sprintf("the round-%d skeleton has not been reviewed", st.Round), continueNote), nil
	}
	if review.Decision == "redo" && st.Round == 1 {
		st.Round, st.Model, st.Effort, st.SkeletonSteps = 2, P.Redo.Model, P.Redo.Effort, 1
		note := fmt.Sprintf("Round 2 at %s/%s: the round-1 skeleton tripped the redo gate (see reasons). Rework the skeleton at %s in place; "+
			"the round-1 review is %s. Then run agent-exec plan review again; this round is not redone.",
			st.Model, st.Effort, abs, ReviewPath(abs))
		return nextSkeleton(abs, st, append([]string{}, review.Reasons...), note), nil
	}
	if !review.Resolved {
		return nextContinueSkeleton(abs, st, fmt.Sprintf("the round-%d review findings are not resolved", st.Round), continueNote), nil
	}

	// Writers past WriterTimeout are taken as returned.
	inFlight := map[string]bool{}
	for id, rec := range st.Specs {
		if !rec.InFlight {
			continue
		}
		if now.Sub(time.Unix(rec.IssuedAt, 0)) < WriterTimeout {
			inFlight[id] = true
		} else {
			rec.InFlight = false
			st.Specs[id] = rec
		}
	}

	if step, ok, err := nextEscalations(abs, p, st, inFlight); ok || err != nil {
		return step, err
	}

	type need struct {
		pkg      plan.Package
		hash     string
		attempts int
		reasons  []string
	}
	var needs []need
	stuck := []string{}
	var running []string
	for _, pkg := range p.Packages {
		if inFlight[pkg.ID] {
			running = append(running, pkg.ID)
			continue
		}
		h := EntryHash(p, pkg)
		rec, ok := st.Specs[pkg.ID]
		n := need{pkg: pkg, hash: h, reasons: []string{}}
		switch {
		case !ok:
		case rec.Hash != h:
			n.reasons = []string{"the package changed since its spec was written"}
		default:
			n.attempts = rec.Attempts
			if _, err := os.Stat(pkg.Spec); err != nil {
				n.reasons = []string{"spec not written"}
			} else if warnings := plan.SpecWarnings(p, pkg); len(warnings) > 0 {
				for _, w := range warnings {
					n.reasons = append(n.reasons, w.Message)
				}
			} else {
				continue
			}
		}
		if n.attempts >= MaxSpecAttempts {
			stuck = append(stuck, fmt.Sprintf("%s: %s after %d attempts", pkg.ID, strings.Join(n.reasons, "; "), n.attempts))
			continue
		}
		needs = append(needs, n)
	}
	if len(stuck) > 0 {
		return Step{Step: "stuck", Plan: abs, Reasons: stuck}, nil
	}

	total, critical := plan.Estimate(p)
	limit := len(needs)
	if P.WritersConcurrent > 0 {
		limit = min(limit, max(0, P.WritersConcurrent-len(running)))
	}
	var writers []Writer
	for _, n := range needs[:limit] {
		st.Specs[n.pkg.ID] = SpecRecord{Hash: n.hash, Attempts: n.attempts + 1, InFlight: true, IssuedAt: now.Unix()}
		tier := P.SpecWriters[n.pkg.Cls]
		prompt := "Plan: " + abs + "\nPackage: " + n.pkg.ID + "\nSpec: " + n.pkg.Spec
		for _, r := range n.reasons {
			prompt += "\nFix: " + r
		}
		writers = append(writers, Writer{
			ID:          n.pkg.ID,
			Cls:         n.pkg.Cls,
			Model:       tier.Model,
			Effort:      tier.Effort,
			Spec:        n.pkg.Spec,
			Description: "spec " + n.pkg.ID,
			Prompt:      prompt,
		})
	}
	if len(writers) > 0 {
		return Step{Step: "specs", Plan: abs, Agent: WriterAgent, Writers: writers, Reasons: []string{},
			Packages: len(p.Packages), EstTotal: total, EstCritical: critical}, nil
	}
	if len(running) > 0 {
		return Step{Step: "wait", Plan: abs,
			Reasons:  []string{fmt.Sprintf("%d spec writers still running: %s", len(running), strings.Join(running, ", "))},
			Packages: len(p.Packages), EstTotal: total, EstCritical: critical}, nil
	}

	full, err := plan.Load(planPath)
	if err != nil {
		return Step{Step: "stuck", Plan: abs, Reasons: []string{err.Error()}}, nil
	}
	// The repo is the one the plan was made for (st.Repo; "" is the working directory).
	repoRoot, _ := gitx.RepoRoot(st.Repo)
	if err := plan.CheckTsJest(full, repoRoot, false); err != nil {
		return nextContinueSkeleton(abs, st, err.Error(), continueNote), nil
	}
	var warnings []string
	for _, w := range plan.Lint(full, "") {
		if w.Pkg != nil {
			warnings = append(warnings, *w.Pkg+": "+w.Message)
		} else {
			warnings = append(warnings, w.Message)
		}
	}
	st.Done = true
	total, critical = plan.Estimate(full)
	return Step{Step: "done", Plan: abs, Reasons: []string{}, Warnings: warnings,
		Packages: len(full.Packages), EstTotal: total, EstCritical: critical}, nil
}

// nextEscalations handles <spec stem>.escalate.md files left by spec writers;
// ok is false when there are none.
func nextEscalations(abs string, p *plan.Plan, st *State, inFlight map[string]bool) (Step, bool, error) {
	type escalation struct{ id, path, stem string }
	var found []escalation
	for _, pkg := range p.Packages {
		stem := strings.TrimSuffix(pkg.Spec, ".md")
		path := stem + ".escalate.md"
		if _, err := os.Stat(path); err == nil {
			found = append(found, escalation{pkg.ID, path, stem})
		}
	}
	if len(found) == 0 {
		return Step{}, false, nil
	}
	if len(inFlight) > 0 {
		return Step{Step: "wait", Plan: abs,
			Reasons: []string{fmt.Sprintf("waiting for %d spec writers before revising for escalations", len(inFlight))}}, true, nil
	}
	if st.Revisions >= MaxRevisions {
		reasons := make([]string, 0, len(found))
		for _, e := range found {
			reasons = append(reasons, e.id+": escalated again: "+e.path)
		}
		return Step{Step: "stuck", Plan: abs, Reasons: reasons}, true, nil
	}
	st.Revisions++
	reasons := make([]string, 0, len(found))
	for _, e := range found {
		renamed := fmt.Sprintf("%s.escalated-%d.md", e.stem, st.Revisions)
		if err := os.Rename(e.path, renamed); err != nil {
			return Step{}, true, err
		}
		reasons = append(reasons, e.id+": "+renamed)
	}
	note := "Spec writers escalated the packages in reasons; each file says why. Revise the skeleton at " + abs +
		" in place for them. Do not run agent-exec plan review again."
	return nextSkeleton(abs, st, reasons, note), true, nil
}

// nextContinueSkeleton reissues the planner within the round, at most
// MaxSkeletonSteps times.
func nextContinueSkeleton(abs string, st *State, reason, note string) Step {
	if st.SkeletonSteps >= MaxSkeletonSteps {
		return Step{Step: "stuck", Plan: abs, Reasons: []string{fmt.Sprintf(
			"the round-%d skeleton is still unfinished after %d planner runs: %s", st.Round, st.SkeletonSteps, reason)}}
	}
	st.SkeletonSteps++
	return nextSkeleton(abs, st, []string{reason}, note)
}

func nextSkeleton(abs string, st *State, reasons []string, note string) Step {
	return Step{Step: "skeleton", Plan: abs, Agent: PlannerAgent, Model: st.Model, Effort: st.Effort,
		Round: st.Round, Note: note, Reasons: reasons}
}
