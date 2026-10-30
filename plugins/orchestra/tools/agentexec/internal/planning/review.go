package planning

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
)

//go:embed review_prompt.md
var reviewPromptTemplate string

// ReviewTimeout is how long a started review may run before planning goes on
// without it.
const ReviewTimeout = 45 * time.Minute

type ReviewOptions struct {
	Repo    string        // the repo the reviewer reads; "" = State.Repo, else the working directory
	MaxWait time.Duration // how long this call waits for a running review (CLI default 540 s)
}

// RefusedError is a plan review step refused in the current state; the CLI
// exits 1 with its message.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

type GateInput struct {
	Structural     int            // findings with severity "structural"
	Deep           int            // packages with cls "deep"
	InterfaceUsers map[string]int // interface id -> packages listing it
	RedoPossible   bool           // round 1 and P.Redo is stronger than the round's tier
}

// Test seams: tests replace these so no pi is ever spawned.
var (
	// startReview prepares a token for spec and detaches req under it.
	startReview = func(cfg *config.Resolved, spec dispatch.PrepareSpec, req dispatch.Request) (string, error) {
		token, err := dispatch.Prepare(cfg, spec)
		if err != nil {
			return "", err
		}
		if cfg.Root != "" {
			// The detached child resolves the same project layers.
			os.Setenv("ORCHESTRA_CONFIG_ROOT", cfg.Root)
		}
		res := dispatch.Detach(cfg, token, req.Args())
		switch res["status"] {
		case "detached", "running", "done":
			return token, nil
		}
		if raw, _ := res["raw"].(string); raw != "" {
			return "", errors.New(raw)
		}
		return "", fmt.Errorf("could not start the review: %v", res["status"])
	}
	waitReview = func(cfg *config.Resolved, token string, maxWait time.Duration) map[string]any {
		res, _ := dispatch.Wait(cfg, token, maxWait, false)
		return res
	}
	removeReviewTree = func(root, task string) {
		_, _ = isolate.Remove(root, task, true, "")
	}
)

// reviewFinding is one normalized Codex finding.
type reviewFinding struct {
	Severity string   `json:"severity"`
	Kind     string   `json:"kind"`
	Packages []string `json:"packages"`
	Evidence string   `json:"evidence"`
	Detail   string   `json:"detail"`
	Fix      string   `json:"fix"`
}

// reviewFile is <plan stem>.review.json.
type reviewFile struct {
	Round    int             `json:"round"`
	Status   string          `json:"status"`
	Note     string          `json:"note"`
	Executor string          `json:"executor"`
	Model    string          `json:"model"`
	Decision string          `json:"decision"`
	Reasons  []string        `json:"reasons"`
	Findings []reviewFinding `json:"findings"`
}

// reviewOutcome is how a review ended, before the gate.
type reviewOutcome struct {
	status, note, executor, model string
	findings                      []reviewFinding
}

var reviewKinds = []string{"split", "dependency", "interface", "ownership", "other"}

// Gate decides mechanically whether the skeleton is redone at the stronger
// tier. A threshold of 0 never fires; reasons are returned even when the redo
// is not possible.
func Gate(in GateInput, when config.RedoWhen) (decision string, reasons []string) {
	reasons = []string{}
	if when.StructuralFindings > 0 && in.Structural >= when.StructuralFindings {
		reasons = append(reasons, fmt.Sprintf("structural review findings: %d (redo at %d)", in.Structural, when.StructuralFindings))
	}
	if when.DeepPackages > 0 && in.Deep >= when.DeepPackages {
		reasons = append(reasons, fmt.Sprintf("deep packages: %d (redo at %d)", in.Deep, when.DeepPackages))
	}
	if when.InterfaceUsers > 0 {
		ids := make([]string, 0, len(in.InterfaceUsers))
		for id := range in.InterfaceUsers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if n := in.InterfaceUsers[id]; n >= when.InterfaceUsers {
				reasons = append(reasons, fmt.Sprintf("interface %s is used by %d packages (redo at %d)", id, n, when.InterfaceUsers))
			}
		}
	}
	if in.RedoPossible && len(reasons) > 0 {
		return "redo", reasons
	}
	return "accept", reasons
}

// RunReview starts or attaches to the round's review (state.go already has a type named Review).
func RunReview(cfg *config.Resolved, planPath string, opts ReviewOptions, now time.Time) (map[string]any, error) {
	P := config.Planning(cfg)
	var (
		out     map[string]any
		started Review
		repo    string
	)
	err := Locked(planPath, func() error {
		st, err := Load(planPath)
		if err != nil {
			return err
		}
		if st.Round == 0 {
			st.Round, st.Model, st.Effort, st.SkeletonSteps = 1, P.Skeleton.Model, P.Skeleton.Effort, 1
		}
		p, err := plan.LoadSkeleton(planPath)
		if err != nil {
			return err
		}
		if reviewFinished(st) {
			out = reviewPrinted(st)
			return nil
		}
		if !P.Review {
			out, err = reviewFinish(cfg, planPath, &st, p, reviewOutcome{status: "skipped", note: "planning.review is off"})
			return err
		}
		if st.Review != nil && st.Review.Round == st.Round && st.Review.Status == "running" {
			started = *st.Review
			return nil
		}
		repo, err = reviewRepo(opts.Repo, st.Repo)
		if err != nil {
			return err
		}
		if err := reviewWritePrompt(planPath, p); err != nil {
			return err
		}
		task := fmt.Sprintf("plan-review-%s-r%d", reviewShortHash(st.Plan), st.Round)
		prompts := []string{PromptPath(st.Plan)}
		// No RunID: planning precedes the request. Task keeps pi out of the
		// user's checkout.
		spec := dispatch.PrepareSpec{
			Class: "independent-review", Archetype: "default", Workdir: repo,
			Isolate: dispatch.IsolateAlways, PromptFiles: prompts, Task: &task,
		}
		req := dispatch.Request{
			Class: spec.Class, Archetype: spec.Archetype, Workdir: repo,
			Isolate: spec.Isolate, PromptFiles: prompts, Task: task,
		}
		token, err := startReview(cfg, spec, req)
		if err != nil {
			out, err = reviewFinish(cfg, planPath, &st, p, reviewOutcome{status: "skipped", note: err.Error()})
			return err
		}
		st.Review = &Review{
			Round: st.Round, Status: "running", Token: token, Task: task, Repo: repo,
			StartedAt: now.Unix(), Reasons: []string{},
		}
		started = *st.Review
		return Save(planPath, st)
	})
	if err != nil || out != nil {
		return out, err
	}

	// Outside the lock: a second call must be able to attach meanwhile.
	res := waitReview(cfg, started.Token, opts.MaxWait)
	status, _ := res["status"].(string)
	var outcome reviewOutcome
	switch status {
	case "running":
		waited := reviewInt(res["waited_s"])
		if now.Unix()+int64(waited)-started.StartedAt < int64(ReviewTimeout/time.Second) {
			return map[string]any{"status": "running", "round": started.Round, "token": started.Token, "waited_s": waited}, nil
		}
		outcome = reviewOutcome{status: "skipped", note: "the review did not finish within 45 minutes"}
	case "ok":
		outcome = reviewParse(res)
	default:
		note := status
		for _, key := range []string{"reason", "raw"} {
			if s, _ := res[key].(string); s != "" {
				note += ": " + s
			}
		}
		outcome = reviewOutcome{status: "skipped", note: note}
	}
	outcome.executor, _ = res["executor"].(string)
	outcome.model, _ = res["model"].(string)

	finished := false
	err = Locked(planPath, func() error {
		st, err := Load(planPath)
		if err != nil {
			return err
		}
		if st.Review == nil || st.Review.Token != started.Token || st.Review.Status != "running" {
			// Another call finished (or replaced) this review meanwhile.
			if st.Review == nil {
				return fmt.Errorf("plan review: the round-%d review disappeared while waiting", started.Round)
			}
			out = reviewPrinted(st)
			return nil
		}
		p, err := plan.LoadSkeleton(planPath)
		if err != nil {
			return err
		}
		out, err = reviewFinish(cfg, planPath, &st, p, outcome)
		finished = err == nil
		return err
	})
	if finished && started.Task != "" {
		removeReviewTree(started.Repo, started.Task)
	}
	return out, err
}

// ResolveReview marks an accepted review as dealt with, so plan next moves on
// to the spec writers. A redo decision is refused: the redo is not the
// planner's to skip.
func ResolveReview(cfg *config.Resolved, planPath string) (map[string]any, error) {
	P := config.Planning(cfg)
	var out map[string]any
	err := Locked(planPath, func() error {
		st, err := Load(planPath)
		if err != nil {
			return err
		}
		if _, err := plan.LoadSkeleton(planPath); err != nil {
			return err
		}
		if !reviewFinished(st) {
			return &RefusedError{fmt.Sprintf("plan review: the round-%d review has not finished; run agent-exec plan review --plan %s first", st.Round, planPath)}
		}
		if st.Review.Decision == "redo" {
			return &RefusedError{fmt.Sprintf("plan review: round %d must be redone at %s/%s (%s); return to the instructor now without revising",
				st.Round, P.Redo.Model, P.Redo.Effort, strings.Join(st.Review.Reasons, "; "))}
		}
		st.Review.Resolved = true
		if err := Save(planPath, st); err != nil {
			return err
		}
		out = map[string]any{"status": "resolved", "round": st.Round}
		return nil
	})
	return out, err
}

func reviewFinished(st State) bool {
	return st.Review != nil && st.Review.Round == st.Round && (st.Review.Status == "done" || st.Review.Status == "skipped")
}

func reviewPrinted(st State) map[string]any {
	r := st.Review
	reasons := r.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	return map[string]any{
		"status": r.Status, "round": r.Round, "decision": r.Decision, "reasons": reasons,
		"structural": r.Structural, "findings": r.Findings, "resolved": r.Resolved,
		"review": ReviewPath(st.Plan), "note": r.Note,
	}
}

// reviewFinish gates the outcome, records it in st and review.json, and
// saves st. The caller holds the lock.
func reviewFinish(cfg *config.Resolved, planPath string, st *State, p *plan.Plan, o reviewOutcome) (map[string]any, error) {
	P := config.Planning(cfg)
	in := GateInput{InterfaceUsers: map[string]int{}}
	for _, f := range o.findings {
		if f.Severity == "structural" {
			in.Structural++
		}
	}
	for _, pkg := range p.Packages {
		if pkg.Cls == "deep" {
			in.Deep++
		}
		seen := map[string]bool{}
		for _, id := range pkg.Interfaces {
			if !seen[id] {
				seen[id] = true
				in.InterfaceUsers[id]++
			}
		}
	}
	in.RedoPossible = st.Round == 1 &&
		(P.Redo.Model != st.Model || config.EffortRank(P.Redo.Effort) > config.EffortRank(st.Effort))
	decision, reasons := Gate(in, P.RedoWhen)

	r := Review{Round: st.Round}
	if st.Review != nil && st.Review.Round == st.Round {
		r = *st.Review
	}
	r.Status, r.Decision, r.Reasons, r.Note = o.status, decision, reasons, o.note
	r.Structural, r.Findings = in.Structural, len(o.findings)
	r.Resolved = decision == "accept" && r.Findings == 0
	st.Review = &r

	findings := o.findings
	if findings == nil {
		findings = []reviewFinding{}
	}
	file := reviewFile{
		Round: r.Round, Status: r.Status, Note: r.Note, Executor: o.executor, Model: o.model,
		Decision: decision, Reasons: reasons, Findings: findings,
	}
	if err := fsutil.AtomicWriteJSON(ReviewPath(st.Plan), file, 0o644); err != nil {
		return nil, err
	}
	if err := Save(planPath, *st); err != nil {
		return nil, err
	}
	return reviewPrinted(*st), nil
}

// reviewParse turns an ok dispatch result into findings, or a skip when the
// answer holds no findings JSON.
func reviewParse(res map[string]any) reviewOutcome {
	answer, _ := res["answer"].(string)
	if answer == "" {
		if path, _ := res["answer_path"].(string); path != "" {
			if b, err := os.ReadFile(path); err == nil {
				answer = string(b)
			}
		}
	}
	obj, ok := dispatch.ExtractJSONObject(answer)
	list, isList := obj["findings"].([]any)
	if !ok || !isList {
		return reviewOutcome{status: "skipped", note: "the review answer had no findings JSON"}
	}
	findings := []reviewFinding{}
	for _, e := range list {
		m, _ := e.(map[string]any)
		f := reviewFinding{
			Severity: strings.ToLower(reviewString(m["severity"])),
			Kind:     reviewString(m["kind"]),
			Packages: []string{},
			Evidence: reviewString(m["evidence"]),
			Detail:   reviewString(m["detail"]),
			Fix:      reviewString(m["fix"]),
		}
		if f.Detail == "" {
			continue
		}
		if f.Severity != "structural" {
			f.Severity = "minor"
		}
		if !slices.Contains(reviewKinds, f.Kind) {
			f.Kind = "other"
		}
		pkgs, _ := m["packages"].([]any)
		for _, pv := range pkgs {
			if s, isStr := pv.(string); isStr {
				f.Packages = append(f.Packages, s)
			}
		}
		findings = append(findings, f)
	}
	return reviewOutcome{status: "done", findings: findings}
}

// reviewWritePrompt fills the review template from the skeleton on disk.
func reviewWritePrompt(planPath string, p *plan.Plan) error {
	skeleton, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	preamble := []string{}
	for _, path := range p.Preamble {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		preamble = append(preamble, fmt.Sprintf("--- %s ---\n%s", path, reviewNewline(string(b))))
	}
	interfaces := []string{}
	for _, iface := range p.Interfaces {
		b, err := os.ReadFile(iface.Path)
		if err != nil {
			return err
		}
		interfaces = append(interfaces, fmt.Sprintf("--- interface %s (%s) ---\n%s", iface.ID, iface.Path, reviewNewline(string(b))))
	}
	prompt := strings.NewReplacer(
		"{{PREAMBLE}}", reviewJoined(preamble),
		"{{INTERFACES}}", reviewJoined(interfaces),
		"{{SKELETON}}", strings.TrimRight(string(skeleton), "\n"),
	).Replace(reviewPromptTemplate)
	return os.WriteFile(PromptPath(planPath), []byte(prompt), 0o644)
}

func reviewJoined(blocks []string) string {
	if len(blocks) == 0 {
		return "(none)"
	}
	return strings.TrimRight(strings.Join(blocks, "\n"), "\n")
}

func reviewNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// reviewRepo is the repo the reviewer reads, absolute.
func reviewRepo(given, recorded string) (string, error) {
	repo := given
	if repo == "" {
		repo = recorded
	}
	if repo == "" {
		return os.Getwd()
	}
	return filepath.Abs(repo)
}

func reviewShortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func reviewString(v any) string {
	s, _ := v.(string)
	return s
}

func reviewInt(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
