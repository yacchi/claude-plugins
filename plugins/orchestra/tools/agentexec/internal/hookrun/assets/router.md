Classify EACH user request before acting.

EXPRESS — ALL of: one self-contained change (or conversational/read-only/a
question); no decomposition, no design decisions, no cross-task coordination;
small expected context bloat. File count is NOT a criterion.
→ Do it yourself, or via cheap subagents (haiku/sonnet), in parallel when the
  parts are independent. No verifier pipeline, no workflow. Hand the test/build run to a cheap subagent and read
  back only its verdict. Name the tier: a subagent call that leaves `model`
  out inherits YOUR model (Opus/Fable), so pass `model: "sonnet"` (or
  "haiku") explicitly for investigation, search, and test runs.
The gate is context bloat, NOT your model tier. Before judging size, ask "do I
already hold this content?" — if writing the delegation prompt would mean
re-authoring a spec you just wrote, delegating buys nothing. That exemption is
about content you already hold, NOT about volume: it does not license an
open-ended build, and it expires the moment the work outgrows what you held.

ORCHESTRATED — anything else, and WHENEVER IN DOUBT.
→ Load the `orchestra:run` skill and follow it: decompose, write contracts,
  delegate to cheap workers with adversarial verifiers, take back only
  structured verdicts.
→ Decomposition beyond a handful of tasks: planner first. Do not plan it
  yourself; spawn `orchestra:orchestra-planner` with the `model`/`effort` of
  `tiers.planner` (`agent-exec config --json`), then run the plan it writes.

WORKFLOW OPT-IN — judge it from the context alone; never read settings files.
ORCHESTRATED work, the Workflow tool included, needs an opt-in the context
shows: a system-reminder that ultracode is on or that the user's prompt carried
the ultracode keyword; the user asking for a workflow in their own words; or
the user having typed /orchestra:run (or /run) themselves. A skill the router
loaded does not count. EXPRESS work never becomes a workflow, even with
ultracode on. ORCHESTRATED work with no opt-in visible: do not hand it to
orchestra agents or Agent-tool workers yourself — reply in one line asking the
user to type `/orchestra:run <the request>` or add "workflow" to the request,
then stop. A hook enforces this: until the user answers, it denies orchestra
agents, and generic ones once `orchestra:run` is loaded this turn.
Never inject or rewrite a prompt to manufacture an opt-in.

A lane never persists — re-classify every request and at every phase shift.
Forced ORCHESTRATED, even mid-conversation: you are about to hand-write
substantive code you have not already worked out in this conversation; a
design discussion just turned into
"implement it" / 「PRを出しましょう」/「直しておきましょう」; or you are planning your
own multi-step implement-then-verify sequence. Then STOP and load
`orchestra:run` first.

ABORT RULE — the lane is wrong more often mid-turn than at the start, because
the prompts that produce the biggest turns are the smallest ones ("作業を続けて",
"実装して", "y"). So re-judge while working, not only when asked: if a turn you
began as EXPRESS keeps sprouting new subtasks or is many files deep, STOP.
Finishing the edit already open is fine; starting the next chunk by hand is
not — re-route it through `orchestra:run` instead.
</orchestra-router>
