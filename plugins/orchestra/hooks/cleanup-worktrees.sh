#!/bin/bash
# orchestra plugin - SessionEnd hook: remove the finishing session's orchestra
# worktrees.
#
# WHY. `agent-exec isolate` creates a git worktree per dispatched task. Nothing
# ever removes them -- the only cleanup instruction is prose in
# `skills/run/references/isolation.md` -- so they accumulate for the life of
# the repo. This hook removes the finishing session's worktrees automatically,
# but only the ones with no uncollected changes: a worktree holding unreviewed
# work is the only record of what a worker tried, so it must be kept and
# surfaced to the user rather than silently discarded.
#
# SCOPE.
#   - Only the session that is ending. `agent-exec isolate remove --session
#     <id>` never touches another session's worktrees.
#   - A worktree with uncollected changes is reported as "dirty" by the
#     underlying command and is never removed by it; this hook just relays
#     that fact.
#   - Legacy `orchestra/<task>` worktrees (no session segment, session: null)
#     are out of scope -- `--session` never matches them.
#
# ESCAPE HATCHES: `ORCHESTRA_SESSION_CLEANUP=off`; `enforcement.session_cleanup:
# "off"` in orchestra config; and fail-open on anything unexpected (no
# agent-exec, no python3, unparseable stdin/output, a hung or failing removal).
# A SessionEnd hook that errors or hangs would disrupt session teardown, so
# every branch below exits 0, and the removal itself runs under a hard
# timeout.
#
# COST. This runs once per session (not per tool call), so unlike the other
# hooks here it does not cache the config lookup to a tmp file -- there is
# nothing to amortize.

set -u

if [ "${ORCHESTRA_SESSION_CLEANUP:-}" = "off" ]; then
    exit 0
fi

INPUT=$(cat 2>/dev/null || true)

if ! command -v python3 >/dev/null 2>&1; then
    exit 0
fi

PARSED=$(printf '%s' "$INPUT" | python3 -c '
import sys, json, re

def s(v):
    return v if isinstance(v, str) else ""

try:
    data = json.load(sys.stdin)
    session_id = re.sub(r"[^A-Za-z0-9_-]", "", s(data.get("session_id")))
    cwd = s(data.get("cwd"))
    print(session_id)
    print(cwd)
except Exception:
    pass
' 2>/dev/null) || PARSED=""

SESSION_ID=$(printf '%s' "$PARSED" | sed -n '1p')
CWD=$(printf '%s' "$PARSED" | sed -n '2p')

[ -n "$SESSION_ID" ] || exit 0

command -v agent-exec >/dev/null 2>&1 || exit 0

# --- config (no cache: this hook runs once per session) ---------------------
SESSION_CLEANUP="on"
CONFIG_JSON=$(python3 -c '
import subprocess, sys
try:
    r = subprocess.run(["agent-exec", "config", "--json"], capture_output=True, timeout=5, text=True)
    if r.returncode == 0:
        sys.stdout.write(r.stdout)
except Exception:
    pass
' 2>/dev/null) || CONFIG_JSON=""
if [ -n "$CONFIG_JSON" ]; then
    CFG=$(printf '%s' "$CONFIG_JSON" | python3 -c '
import sys, json
try:
    e = json.load(sys.stdin).get("enforcement")
    if isinstance(e, dict):
        v = e.get("session_cleanup")
        if isinstance(v, str) and v in ("on", "off"):
            sys.stdout.write(v)
except Exception:
    pass
' 2>/dev/null) || CFG=""
    [ -n "$CFG" ] && SESSION_CLEANUP="$CFG"
fi

[ "$SESSION_CLEANUP" = "off" ] && exit 0

REPO="$CWD"
[ -n "$REPO" ] || REPO="$PWD"

REMOVE_JSON=$(python3 -c '
import subprocess, sys
session_id, repo = sys.argv[1], sys.argv[2]
try:
    r = subprocess.run(
        ["agent-exec", "isolate", "remove", "--session", session_id, "--repo", repo],
        capture_output=True, timeout=20, text=True,
    )
    sys.stdout.write(r.stdout)
except Exception:
    pass
' "$SESSION_ID" "$REPO" 2>/dev/null) || REMOVE_JSON=""

[ -n "$REMOVE_JSON" ] || exit 0

python3 -c '
import sys, json

try:
    data = json.loads(sys.argv[1])
except Exception:
    sys.exit(0)

removed = data.get("removed")
if not isinstance(removed, list):
    sys.exit(0)

removed_count = 0
dirty = []
errors = []

for entry in removed:
    if not isinstance(entry, dict):
        continue
    status = entry.get("status")
    if status == "removed":
        removed_count += 1
    elif status == "dirty":
        dirty.append(entry)
    elif status == "error":
        errors.append(entry)
    # "absent" and anything else: ignored.

if removed_count == 0 and not dirty and not errors:
    sys.exit(0)

lines = []
if removed_count > 0:
    lines.append("orchestra: removed %d session worktree(s)." % removed_count)

if dirty:
    lines.append("orchestra: kept %d worktree(s) with uncollected changes:" % len(dirty))
    for entry in dirty:
        task = entry.get("task")
        path = entry.get("path")
        lines.append("  %s: %s" % (task, path))
    lines.append(
        "  inspect: agent-exec isolate diff --task <id>   |   "
        "discard: agent-exec isolate remove --task <id> --force"
    )

if errors:
    reason = errors[0].get("reason") if isinstance(errors[0], dict) else None
    lines.append("orchestra: %d worktree removal(s) failed: %s" % (len(errors), reason))

sys.stdout.write("\n".join(lines) + "\n")
' "$REMOVE_JSON" 2>/dev/null

exit 0
