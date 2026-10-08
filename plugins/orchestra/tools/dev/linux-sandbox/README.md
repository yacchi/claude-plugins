# Linux sandbox check

Repeatable check of agent-exec's Linux sandbox backends (bwrap, Landlock) in
Docker, with a fake `pi` (no model, no auth). Needs Docker; run on any host:

```sh
plugins/orchestra/tools/dev/linux-sandbox/run.sh [bwrap|landlock|suite|all]
```

- `bwrap`: `--privileged` container, user `worker` -- user namespaces work,
  like an ordinary Linux desktop.
- `landlock`: default container, user `worker` -- the bwrap probe fails, like
  Ubuntu >= 24.04 with unprivileged user namespaces blocked.
- `suite`: the plugin's full pytest suite in the default container.

Each scenario runs `agent-exec dispatch --class standard --isolate always`
through the repo's own `tools/agent-exec`: backend/limits report, task-tree vs
`$HOME` writes, `/tmp` and `$TMPDIR`, `~/.ssh` reads, signals to an outside
pid, tamper-protected files, cache auto-grant + resume (UTF-8 and C locale
coreutils messages), the watchdog, and git in the worktree vs the main checkout.

Files: `Dockerfile` (ubuntu:24.04, bubblewrap, uv), `fake_pi.py` (installed as
`/usr/local/bin/pi`; runs the shell lines after `COMMANDS:` in the prompt and
prints pi 1.1.0-shaped JSONL; `--session` re-runs them), `scenarios.py`
(in-container driver), `run.sh` (host side). The repo is mounted read-only
and copied inside; every container is `--rm`. One PASS/FAIL line per check;
exit status is non-zero on any FAIL.
