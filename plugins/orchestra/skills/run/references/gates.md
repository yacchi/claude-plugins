# Gate discipline — rationale (orchestra `run`)

Long form behind `SKILL.md` §11. Load it when a gate rule is actually biting: a rejection looks out of scope, rounds keep closing one sibling at a time, or you are deciding whether a third round is worth it. Introduced in v0.14.0.

The short version of why any of this exists: **a round is a full worker + review cycle, and it is the unit you actually pay in.** Every rule here converts "spend another round" into "spend a better round."

## 1. Why a rejection needs a citation

The most expensive reviewer is not the one that misses a bug — it is the one that invents requirements. An adversarial reviewer told to "trust nothing" and given no boundary will find something on every pass, because there is always a stricter tolerance, a more defensive check, a nicer API. Each of those costs a full round, and the task still ends up marked failed.

So `cited_contract` is a required field: point at the spec text, an enumerated example, or an explicit instructor decision the work violates, or it is not a finding. Genuine improvements outside the contract go to `optional_hardening`, which is reported and never blocks a pass — you decide later, out of band, whether to spend a round on any of them.

The rule cuts both ways. A supervisor inference about casing, normalization, inheritance, or compatibility does not become a product requirement because you wrote it into a correction packet mid-run. If you decide the contract really should be stricter, amend the contract explicitly and say so — don't smuggle it in as a correction.

**Reviewer-side corollary:** a reviewer must never repair what it found. `orchestra-review` may write its own new test files and nothing else. A reviewer that patches the implementation — or quietly adds the missing assertion to the worker's test — destroys the only independent signal the pipeline has, and the next gate is then reviewing its own work.

## 2. Why findings carry a defect family

A *family* is the kind of defect, not its location: rounding/carry, boundary off-by-one, input normalization, error-type mismatch, ordering/precedence, serialization round-trip, freshness/staleness, schema validation, resource cleanup, action binding, localization.

Defects arrive in families because they come from a single mistaken assumption applied at several call sites. Report one instance and you get one fix; the sibling surfaces next round, and the round after that. That is the actual mechanism behind "the pipeline is slow" — three rounds spent on three instances of one misunderstanding.

So the reviewer classifies each finding, then checks the sibling sites in scope for the same defect and reports them together. One round closes the class. When a whole phase turns out to contain several independently testable families (say, storage + one external action + recovery + UI), that is a sign the phase should have been decomposed into sequential accepted units rather than handed to one worker.

## 3. Why gate 2 is incremental, and why `new_family` stops the loop

A second full specification audit re-reads everything the first one already accepted, re-derives the same conclusions, and costs about what the first gate cost — to check a handful of small corrections. So the re-gate gets the previous verdict, the closed finding ids, and the changed paths, and inspects only those plus regressions those specific corrections could cause. Six read-only shell invocations, first line `VERDICT: PASS|FAIL`. `regatePrompt()` in `SKILL.md` §5 builds it.

The interesting case is when the re-gate finds a defect family that predates the correction. That is not "one more bug" — it is evidence that the **first review's sweep was wrong**. Another automatic round would re-run the same under-sweeping review and surface the next sibling of that family, and the one after that. So the loop stops and returns `needsInstructor`. Your move is to re-analyze: audit the family yourself or via a targeted worker, widen the contract or the verification, and re-enter — not to re-issue the same round.

**When a third round *is* right:** the finding is a genuinely new, single, cited defect that the corrections introduced, the family is already swept, and the fix is mechanical. That is a judgment call, which is exactly why the loop hands it to you instead of spending it automatically.

## 4. Escalation: on signal, and ahead of risk

Workers report `ESCALATE` rather than guessing when the packet doesn't settle a needed decision, the required change crosses their assigned file ownership, or a failing test can't be explained by code they own. That bumps `light` → `standard` → `deep` **without consuming a gate**, because a mis-sized packet is not a defect — you gave a mechanical worker a design problem, and it correctly declined. A defect family that fails twice bumps the class for the same reason.

Going the other direction is cheaper still: route authentication, authorization, session lifecycle, concurrency, recovery, and security-boundary work at `standard`/`deep` **from the first round**. Letting a light-class worker fail on those spends a full round to learn something the contract already told you. Conversely, a rejected security phase is not a reason to send every subsequent mechanical correction to the top tier — decompose until the remaining unit is mechanical again.

Escalating the class never authorizes expanding scope. A `deep` worker on an escalated packet still owns exactly the files the packet named.

## 5. Why corrections go to a fresh invocation

Resuming the rejected worker looks cheaper and usually isn't. That instance is anchored on the reasoning that produced the defect, and its context is now full of dead ends you re-pay for on every subsequent turn. A self-contained correction packet to a fresh worker starts from the contract plus precise findings, with none of that weight.

The packet must therefore actually stand alone: the original contract in full, the findings with their `cited_contract`, the `must_not_change` paths (behavior already accepted, which the fix must not disturb), the fact that the previous attempt's files are still on disk and must be read first, and what the next gate will check. `correctionPacket()` in `SKILL.md` §5 assembles this.

Resume the same instance (`SendMessage`) only when the reasoning history is genuinely expensive to reconstruct — a long investigation, not a routine fix. Note the asymmetry with the *supervisor* tier: `orchestra-delegate` should be resumed across rounds, because its accumulated context is the thing of value.

## 6. Invocation budgets

Cap a worker at ~12 shell invocations, a first-pass review at ~12, a re-gate at 6, and require batched reads rather than one-at-a-time.

The budget is a diagnostic, not a punishment. A worker re-reading broad swaths of the repository in circles has an under-specified packet — it is not being careless, it is searching for information you didn't give it. The right response is to interrupt it, preserve its tree, and re-scope the packet smaller, not to let it keep going. Likewise a review that exhausts its budget returns FAIL with `inspection budget exceeded`; an interrupted or out-of-budget review is never a PASS.

And the trap worth naming: **cached input is still consumption.** A run whose token count is mostly cache reads is not free, and a high cache-hit ratio is not evidence that a loop is behaving. If two bounded rounds both show broad rescanning, stop reaching for more rounds and change the decomposition.
