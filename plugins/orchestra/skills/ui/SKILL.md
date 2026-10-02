---
name: ui
description: Open the orchestra dashboard in the browser - live view of `agent-exec wave` runs (packages, mechanical stages, needs), worktrees, recent dispatches and cooldowns. Invoke with /ui (cross-plugin, `orchestra:ui`).
when_to_use: Use when the user wants to watch or check on orchestra work without asking you for status - "show me the dashboard", "open the UI", "how are the waves doing", ダッシュボードを開いて. Do not use it to read status yourself; use `agent-exec wave status` for that.
---

# ui: the orchestra dashboard

A local, read-mostly web page served by `agent-exec ui` on 127.0.0.1. It reads the files orchestra already writes (wave registry, wave state and events, worktrees, run ledger, cooldown state), and also shows running executors, linked specs/context/corrections, and 24-hour usage; it streams them to the browser. It costs zero tokens by design.

## Do this

1. Run once:

   ```
   agent-exec ui --open --json
   ```

   It starts the server if none is running (or reuses the running one), opens the browser, and prints `{"url", "pid", "reused"}`.
2. Give the user the `url`. That is the whole job.

## Do not

- Do not poll, fetch, or read the dashboard or its `/api/*` endpoints yourself. Watching it would spend the tokens it exists to save.
- Do not paste the URL into shared places: the `t=` token in it is the credential.

## Stopping

`agent-exec ui --stop` ends the server. It also exits by itself after 30 idle minutes (`--idle-minutes N` to change) with no browser tab connected.

The page has a "Stop wave" button per wave; it asks for confirmation, then drops the `STOP` flag the wave runner already honours.
