# Isolation, checkpointing, and aggressive parallelism (orchestra `run`)

Load this file when a run needs any of: **a worker to touch a tree that holds uncommitted work**, **rollback on failure**, **a reliable diff for the reviewer**, **parallel work on files that would otherwise collide**, or **competing implementations of the same task**. Introduced in v0.14.0; isolation became the dirty-tree default in v0.15.0.

The premise: version control is effectively free, and an orchestration that can always return to a known-good state is strictly better than one that edits in place and hopes. A run that cannot roll back has to be cautious; a run that can roll back can be aggressive.

## 0. Isolate first: the failure this prevents

Seven weeks of transcripts contain **35 destructive VCS commands run by workers** against the user's shared tree — `git checkout -- <paths>`, `git restore`, `git reset --hard`, `git clean -fd`. They are spread across haiku, sonnet, and opus workers alike, so this is not a cheap-model defect. The instructor's countermeasure was prose, escalated three generations deep in the worker prompts ("Never run git checkout…", then "The ONE exception…", then "THE WORKING TREE IS ALREADY DIRTY AND THAT IS EXPECTED AND CORRECT"), and the accidents continued after each escalation.

The reason prose loses here is structural. A worker's only view of the world is `git status`. Its contract describes one task; the tree contains several tasks' worth of changes plus whatever the user had in flight. Told "the dirty tree is expected", the worker still sees a diff it cannot account for, and cleaning up is a plausible reading of doing good work.

**So do not rely on telling workers to be careful. Give them a tree where the mistake is impossible** — one that contains their own work and nothing else. Then "revert the stray diff" is a no-op rather than an accident.

```bash
# The default. Isolates exactly when the tree has uncommitted work.
agent-exec dispatch --class light --task <task-id> \
    --prompt-file <file> --workdir <repo> --capture
# --isolate always  : isolate even a clean tree (parallel/competing variants)
# --isolate never   : opt out, e.g. a strictly sequential phase on one tree
```

Every dispatch result carries an `isolation` object saying which tree was used and why, so a run always knows where to collect from:

```bash
agent-exec isolate diff   --task <id>            # patch + file list, worker's changes only
agent-exec isolate list                          # orchestra-created worktrees
agent-exec isolate remove --task <id>            # refuses while changes are uncollected
```

What `isolate create` does for you, so a worktree is not a downgrade for the worker:

- **carries the user's uncommitted work in**, then commits it as that worktree's baseline — so a later `isolate diff` shows the worker's changes and nothing else;
- **copies gitignored dependency directories** (`node_modules`, `.venv`, `vendor`, `target`, …) by copy-on-write clone. ~2s for 83MB/9.5k files, versus minutes and a pile of tokens if every worker re-runs its own install;
- **reuses one worktree per `--task`**, so retry rounds accumulate in one place instead of littering;
- **goes through `git gtr`** (git-worktree-runner) when installed, so the user's own `gtr.copy.include` patterns and `gtr.hook.postCreate` setup steps apply here too. Orchestra never *writes* gtr config: what it needs is injected for one subprocess via `GIT_CONFIG_COUNT`/`KEY_n`/`VALUE_n`, leaving `.git/config`, `~/.gitconfig`, and the committable repo-root `.gtrconfig` untouched. Without gtr it falls back to a plain `git worktree`.

A second layer backs this up: `hooks/guard-worker-vcs.sh` denies destructive VCS commands from *subagents* against a non-`orchestra/*` branch (`enforcement.worker_vcs`, on by default). Inside its own worktree a worker may reset freely — there is nothing there but its own work. The main thread is never affected.

**When isolation is genuinely not available** — no repository at all, or a phase that must run in place — say so in the plan, and fall back to §1's discipline: snapshot before the first worker, and never `checkout .`/`reset --hard`/`clean` the user's work.

## 1. Checkpointing: the run always has somewhere to fall back to

**Before the first worker touches anything, establish a baseline.**

- If the tree is a git repository, record the baseline commit (`git rev-parse HEAD`) and note whether the tree was dirty. If jj is colocated, `jj op log` already gives you an operation to restore to; nothing extra is needed.
- **If there is no repository at all, create one.** `git init` plus one baseline commit costs nothing and buys the entire rollback story. Do this rather than declining to checkpoint — but say so in your final report, because it is a change to the user's directory.
- If the tree was dirty at baseline, treat the user's uncommitted work as sacred: never `checkout .`, `reset --hard`, or `clean` it away. Snapshot it (a `git stash create` object, a jj snapshot, or a baseline commit on an agent branch) before any worker runs, and restore only paths the workers themselves wrote.

**Snapshot between attempts, not just at the end.** After each worker attempt — pass or fail — capture the tree. This is what makes the two most valuable things possible:

1. **A reliable diff for the reviewer.** `git diff <baseline>..<attempt>` (or `jj diff -r <op>`) tells the reviewer *exactly* what changed, instead of the reviewer inferring the worker's footprint from its prose report. A reviewer working from a real diff catches unrelated edits, stray files, and quiet deletions that a spec-only review structurally cannot.
2. **Discardable failures.** When a retry makes things worse, roll that attempt back to the last good snapshot instead of asking the next worker to untangle it. Compounding half-fixes across three retries is one of the main ways a pipeline burns rounds without converging.

**Who does what.** Workers never touch VCS state — `orchestra-light`/`orchestra-deep` are explicitly forbidden from committing. Snapshots, rollbacks, and merges belong to the supervising layer (the Workflow script's own steps, or `orchestra-delegate`). Keep them on agent-owned branches (`agent/<task>-<attempt>` or similar); **never commit to, rebase, or push the user's branch without an explicit request in the current turn.** Integration into the user's branch is a separate, user-approved step.

**Recovery.** If work is lost anyway, jj's operation log (`jj op log` → `jj op restore`) recovers states that git alone would not, including uncommitted working-copy snapshots. That is precisely why keeping jj colocated is worth it even in a git-first repo.

## 1a. Branch hygiene: messy underneath, clean on top

Create as many descendant branches as the work wants — per task, per attempt, per variant, nested arbitrarily deep. Cheap snapshots are the whole point, and there is no reason to be frugal with them mid-run.

**The branch that becomes the PR is the exception.** It carries the deliverable and nothing else: no per-attempt checkpoints, no rejected variants, no scratch commits, no spec or worker-only files, no revert-of-a-revert churn. Build it by applying the *accepted diff* onto the baseline — squash, or cherry-pick a reconstructed set of focused commits (implementation / tests / docs separately when that helps review) — rather than by merging the working branch's history wholesale. If a task commit mixes accepted deliverables with context-only files or rejected paths, reconstruct it; do not cherry-pick it as-is.

**Clean up when the run ends.** Delete the worktrees and agent branches you created once the deliverable is integrated and the user confirms nothing more needs inspecting — but verify each exact target first, and never use a glob, an unresolved variable, or a recursive delete rooted anywhere near the repository root or the home directory. Keeping the artifacts until that confirmation is deliberate: they are the only record of what was tried.

## 2. Worktrees: isolate so you can be aggressive

`agent()` accepts `isolation: 'worktree'`, which gives that Claude subagent a fresh git worktree; `agent-exec dispatch --isolate` (§0) does the same for CLI executors, which `agent()`'s option cannot reach. Beyond the safety case in §0 — where isolation is simply the default on a dirty tree — isolation also removes the constraint that has been shaping every parallelism decision in this playbook so far.

The base rule elsewhere in the skill (`authoring.md` §1) is *partition file ownership so parallel workers never collide*. That rule is correct for same-tree runs and it is also a real limit: it forbids parallelizing exactly the work that is most tangled, and tangled work is usually the slow work. Worktree isolation lifts the limit — collisions become merge decisions instead of silent corruption.

**Use worktree isolation when:**

- the tree holds uncommitted work — this one is the default and needs no deliberation (§0);
- two or more tasks genuinely need to touch overlapping paths, and splitting them would be artificial;
- a task is exploratory enough that you want its failure to be free (delete the worktree, nothing else moved);
- you are running competing implementations (§3);
- a long-running task should not block quick ones from mutating the same files.

**Don't use it when** the tree is clean *and* the phase is genuinely sequential on one tree, or ownership partitions cleanly and cheaply — then the merge step is pure overhead. Note the ordering: a clean tree is what makes skipping isolation safe, not a tidy partition.

**After the workers finish, the supervisor merges.** Cherry-pick or apply each accepted worktree's diff onto an integration branch, resolve conflicts deliberately, and re-run verification on the merged result. A per-worktree PASS does not imply the merge passes; verification of the integrated tree is a separate, mandatory step.

## 3. Competing implementations: conflict as signal

When a task is high-risk, under-specified in an interesting way, or has more than one defensible approach, dispatch **N workers on the same contract in separate worktrees** and let the supervisor pick.

```javascript
// Same contract, independent worktrees, deliberately no shared state.
const variants = await parallel(
  ['a', 'b', 'c'].map((v) => () =>
    agent(task.workerPrompt, {
      label: task.id + '-variant-' + v,
      agentType: 'orchestra:orchestra-deep',   // or model: 'sonnet' / dispatchClass(...)
      isolation: 'worktree',
    }),
  ),
).then((r) => r.filter(Boolean))
```

Then run the *same* adversarial test suite against every variant and select on evidence, not on prose. What makes this worth the tokens is not just "pick the best one":

- **Divergence localizes risk.** Where independent implementations agree, the spec was clear. Where they disagree, the spec was ambiguous — and that disagreement is a defect report about your *contract*, delivered before the bug ships. Feed it back into the contract rather than just picking a winner.
- **A shared test suite finds bugs no single implementation would reveal.** A case where variant A passes and variant B fails is a boundary the spec did not pin down; a case where all variants fail the same way is usually a bad test or a misread requirement.
- **Merging beats choosing.** Often the right output is variant A's structure with variant B's edge-case handling. Say so explicitly in the correction packet for the final pass instead of silently accepting one whole variant.

**The cost is real: N× the implementation tokens, and worktree setup on top.** Spend it deliberately — on the security-sensitive core, the algorithm nobody is sure about, the interface everything else will depend on. Do not make it the default shape; most tasks have one obvious implementation and competing on them is pure waste. Two variants is usually enough to expose divergence; three is the practical ceiling.

## 4. Verify on a context-free tree

A worker's tree contains things the deliverable does not: the spec you handed it, scratch notes, fixtures it created to try something. That is how a phase passes review while shipping nothing — the check succeeds because a file that only exists in the *worker's* context is satisfying it.

So before accepting a phase, run the full verification (fresh dependency install, build, static checks, tests) on the **integration tree**, which was created from the baseline and contains only the accepted diff — never from the worker's context. Confirm `git status` is clean there and that no generated output or dependency directory leaked into the accepted diff. Omissions that every in-tree check missed show up immediately here.

## 5. Front-load approvals

Workers dispatched to a CLI executor run **non-interactively**. A permission prompt they cannot surface does not become a question — it becomes a stall, and you pay for the whole run before noticing.

So before launching a phase, budget its approvals in one batch: worktree creation at the exact paths you will use, snapshot/commit on `agent/*` branches only, dependency install from an existing lockfile, and whatever test/build commands the contracts name. Keep these at action time regardless: adding or upgrading dependencies, anything that touches the user's branch, pushing or opening a PR, deleting worktrees or branches, and any destructive filesystem or VCS command. If a new approval turns out to be necessary mid-phase, pause at a worker boundary and batch the rest — don't broaden a permission just to keep one worker alive, and don't grant a prefix that would allow more than the operation you actually reviewed.
