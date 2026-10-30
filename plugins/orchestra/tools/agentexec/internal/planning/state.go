// Package planning sequences orchestra's three planning stages (skeleton,
// Codex review, spec writers) from files beside the plan, so the instructor
// follows `agent-exec plan next` instead of judging stages itself.
package planning

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

const (
	PlannerAgent = "orchestra:orchestra-planner"
	WriterAgent  = "orchestra:orchestra-spec-writer"
)

// For /d/plan.json: /d/plan.planning.json, /d/plan.review.json, /d/plan.review-prompt.md.
// The stem is the base name minus one ".json" suffix (a name without it is kept whole).
func StatePath(planPath string) string {
	return sibling(planPath, ".planning.json")
}

func ReviewPath(planPath string) string {
	return sibling(planPath, ".review.json")
}

func PromptPath(planPath string) string {
	return sibling(planPath, ".review-prompt.md")
}

func sibling(planPath, suffix string) string {
	dir, base := filepath.Split(planPath)
	return dir + strings.TrimSuffix(base, ".json") + suffix
}

// Review is the current round's Codex review of the skeleton.
type Review struct {
	Round      int      `json:"round"`
	Status     string   `json:"status"` // running | done | skipped
	Token      string   `json:"token,omitempty"`
	Task       string   `json:"task,omitempty"`
	Repo       string   `json:"repo,omitempty"`
	StartedAt  int64    `json:"started_at"`
	Decision   string   `json:"decision,omitempty"` // accept | redo; "" while running
	Reasons    []string `json:"reasons"`
	Structural int      `json:"structural"`
	Findings   int      `json:"findings"`
	Resolved   bool     `json:"resolved"`
	Note       string   `json:"note,omitempty"`
}

// SpecRecord is what plan next issued for one package's spec.
type SpecRecord struct {
	Hash     string `json:"hash"`      // EntryHash of the package when issued
	Attempts int    `json:"attempts"`  // writers issued for this hash
	InFlight bool   `json:"in_flight"` // issued and not reported back with --returned
	IssuedAt int64  `json:"issued_at"` // unix seconds
}

// State is <plan stem>.planning.json.
type State struct {
	Version       int                   `json:"version"` // 1
	Plan          string                `json:"plan"`    // absolute plan path
	ID            string                `json:"id,omitempty"`
	Repo          string                `json:"repo,omitempty"`
	Title         string                `json:"title,omitempty"`
	Round         int                   `json:"round"` // 0 none yet, 1 skeleton, 2 redo
	Model         string                `json:"model,omitempty"`
	Effort        string                `json:"effort,omitempty"`
	SkeletonSteps int                   `json:"skeleton_steps"` // skeleton steps issued in this round
	Revisions     int                   `json:"revisions"`      // escalation revisions issued
	Review        *Review               `json:"review"`
	Specs         map[string]SpecRecord `json:"specs"`
	Done          bool                  `json:"done"`
}

// Load reads the state of planPath. A missing file is the zero state
// {Version: 1, Plan: <absolute planPath>, Specs: {}}; a file that does not
// decode is an error naming it.
func Load(planPath string) (State, error) {
	abs, err := filepath.Abs(planPath)
	if err != nil {
		return State{}, err
	}
	statePath := StatePath(planPath)
	var s State
	err = fsutil.ReadJSONFile(statePath, &s)
	switch {
	case os.IsNotExist(err):
		return State{Version: 1, Plan: abs, Specs: map[string]SpecRecord{}}, nil
	case err != nil:
		return State{}, fmt.Errorf("read planning state %s: %w", statePath, err)
	}
	if s.Specs == nil {
		s.Specs = map[string]SpecRecord{}
	}
	return s, nil
}

// Save writes s with fsutil.AtomicWriteJSON (0o644); Version is set to 1 and a
// nil Specs is written as {}.
func Save(planPath string, s State) error {
	s.Version = 1
	if s.Specs == nil {
		s.Specs = map[string]SpecRecord{}
	}
	if s.Review != nil && s.Review.Reasons == nil {
		review := *s.Review
		review.Reasons = []string{}
		s.Review = &review
	}
	return fsutil.AtomicWriteJSON(StatePath(planPath), s, 0o644)
}

// Locked runs fn holding an exclusive fsutil.Lock on StatePath(planPath)+".lock"
// (30 s timeout). Never hold it while waiting on a review.
func Locked(planPath string, fn func() error) error {
	unlock, err := fsutil.Lock(StatePath(planPath)+".lock", fsutil.Exclusive, 30*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	return fn()
}
