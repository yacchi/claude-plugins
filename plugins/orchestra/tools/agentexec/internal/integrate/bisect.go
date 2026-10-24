package integrate

import (
	"errors"
	"io"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// AppliedCommit is one task's integrate commit.
type AppliedCommit struct {
	Task, SHA string
}

// BisectInput describes a verify failure to bisect in the integration
// worktree Worktree (attached to Branch). Applied are the commits under
// suspicion, in order, all on top of BaseSHA. Run replaces RunVerify (the
// wave runner adds its environment check); nil uses RunVerify. Max <= 0 is
// DefaultBisectMax.
type BisectInput struct {
	Root, Worktree, Branch, BaseSHA string
	Applied                         []AppliedCommit
	Verify                          string
	Timeout                         time.Duration
	Max                             int
	Progress                        io.Writer
	DirtiedInitial                  int
	Run                             func(dir, cmd string, timeout time.Duration) VerifyResult
}

// RevertedTask is a culprit reverted on the integration branch, with the
// excerpt of the probe that convicted it.
type RevertedTask struct {
	Task, Excerpt string
}

// BisectResult is the final verify (Baseline "pass" or "fail" at BaseSHA),
// the culprits reverted in order, the task whose revert conflicted and was
// aborted, and the task whose revert commit failed and was undone ("" if
// none).
type BisectResult struct {
	Verify             VerifyResult
	Reverted           []RevertedTask
	RevertConflicted   string
	RevertCommitFailed string
}

// Bisect binary-searches the first applied commit that makes verify fail,
// reverts it on the branch, and repeats (up to Max rounds) until verify
// passes at the tip. Monotonicity is assumed: once a commit makes verify
// fail, every later one does too. The worktree ends attached to Branch
// however this returns. A failing baseline means no task is to blame. The
// error is only for an input naming no worktree, branch or base.
func Bisect(in BisectInput) (BisectResult, error) {
	if in.Worktree == "" || in.Branch == "" || in.BaseSHA == "" {
		return BisectResult{}, errors.New("bisect needs a worktree, its branch and a base commit")
	}
	run := in.Run
	if run == nil {
		run = RunVerify
	}
	maxRounds := in.Max
	if maxRounds <= 0 {
		maxRounds = DefaultBisectMax
	}
	wt := in.Worktree
	git := func(args ...string) error {
		_, err := gitx.Run(wt, args...)
		return err
	}
	reattach := func() { _ = git("checkout", "-q", in.Branch) }
	defer reattach()

	dirtied := in.DirtiedInitial
	verify := func() VerifyResult {
		v := run(wt, in.Verify, in.Timeout)
		dirtied += v.Dirtied
		v.Dirtied = dirtied
		return v
	}

	var out BisectResult
	_ = git("checkout", "-q", "--detach", in.BaseSHA)
	last := verify()
	if last.Status == "fail" {
		last.Baseline = "fail"
		out.Verify = last
		return out, nil
	}
	last.Baseline = "pass"

	var reverted []string
	loBound := 0
	for round := 0; round < maxRounds; round++ {
		lo, hi := loBound, len(in.Applied)-1
		firstBad := -1
		var badProbe VerifyResult
		for lo <= hi {
			mid := (lo + hi) / 2
			sha := in.Applied[mid].SHA
			_ = git("checkout", "-q", "--detach", sha)
			for _, culprit := range reverted {
				_ = git("revert", "--no-commit", culprit)
			}
			probe := verify()
			emit(in.Progress, "bisect-probe", in.Applied[mid].Task,
				map[string]any{"commit": sha, "result": probe.Status})
			_ = git("reset", "--hard", sha)
			_ = git("clean", "-fdq")
			if probe.Status == "fail" {
				firstBad, badProbe = mid, probe
				hi = mid - 1
			} else {
				lo = mid + 1
			}
		}
		if firstBad < 0 {
			// Impossible under monotonicity (the tip failed); report the
			// real tip state rather than loop.
			reattach()
			last = verify()
			last.Baseline = "pass"
			break
		}
		culprit := in.Applied[firstBad]
		reattach()
		if git("revert", "--no-commit", culprit.SHA) != nil {
			_ = git("revert", "--abort")
			out.RevertConflicted = culprit.Task
			emit(in.Progress, "revert", culprit.Task, map[string]any{"result": "conflicted"})
			badProbe.Baseline = "pass"
			out.Verify = badProbe
			return out, nil
		}
		if err := commitAll(wt, "orchestra revert "+culprit.Task); err != nil {
			_ = git("reset", "-q", "--hard", "HEAD")
			_ = git("clean", "-fdq")
			out.RevertCommitFailed = culprit.Task
			emit(in.Progress, "revert", culprit.Task,
				map[string]any{"result": "commit-failed", "excerpt": commitExcerpt(err)})
			badProbe.Baseline = "pass"
			out.Verify = badProbe
			return out, nil
		}
		out.Reverted = append(out.Reverted, RevertedTask{Task: culprit.Task, Excerpt: badProbe.Excerpt})
		emit(in.Progress, "revert", culprit.Task, map[string]any{"result": "reverted"})
		clearSourceCollected(in.Root, culprit.Task)
		reverted = append(reverted, culprit.SHA)
		last = verify()
		last.Baseline = "pass"
		if last.Status == "pass" {
			break
		}
		loBound = firstBad + 1
	}
	out.Verify = last
	return out, nil
}
