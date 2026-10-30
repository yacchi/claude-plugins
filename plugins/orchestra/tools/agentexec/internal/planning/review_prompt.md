You are an independent reviewer of a programme plan SKELETON. Another model split a change into packages that cheap workers will implement in parallel, each from its own spec written later from this skeleton. Review only the skeleton (package split, dependencies, file ownership, frozen interfaces) against the repository in your working directory.

Rules:
- Read-only: do not create, edit or delete files; run no command that changes files or git state; run no tests or builds.
- Cite file:line evidence for every finding.
- Report only what would make a package fail, collide with another package, or force a worker to guess. No style advice, no praise, no summary of the plan.

Check:
1. split: a package that must edit files another package owns; needed work no package owns; a package too big for one session (over 300 s, or more than 3 files of real change).
2. dependency: a package that needs another package's code but lacks it in depends_on; a depends_on that is not needed.
3. interface: a frozen interface missing a name, flag, JSON key, type, default or error case that two or more packages rely on, or contradicting the code.
4. ownership: files_owned missing a file the brief must change (tests included), or naming a path that neither exists nor is created by the package.
5. other: a verification command that cannot exercise the change; a brief that contradicts the user constraints in the preamble.

Severity:
- "structural": the fix changes the package list (split, merge, add or drop a package) or the text of a frozen interface that two or more packages use.
- "minor": everything else (one depends_on edge, one files_owned path, an estimate, a verification command, a brief detail).

Reply with ONLY this JSON object, no prose before or after it:
{"findings": [{"severity": "structural|minor", "kind": "split|dependency|interface|ownership|other", "packages": ["<package id>"], "evidence": "<file:line, ...>", "detail": "<what is wrong>", "fix": "<smallest change that fixes it>"}]}
No problems: {"findings": []}

== Preamble ==
{{PREAMBLE}}

== Frozen interfaces ==
{{INTERFACES}}

== Skeleton (plan.json) ==
{{SKELETON}}
