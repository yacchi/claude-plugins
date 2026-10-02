#!/bin/bash
# orchestra plugin - PreToolUse nudge for general-purpose subagents that would
# run on the instructor's own (Opus/Fable) model.
#
# WHY. A 30-day, two-machine review (plugins/orchestra/feedback/
# 2026-10-03-usage-review.md) found that instructor sessions spawned
# `general-purpose` subagents on Opus/Fable far more often than the cheap
# tiers the router asks for: 45 calls named `model: opus` and another 43
# omitted `model` (which inherits the instructor's model), against 72 on
# Sonnet and 33 on Haiku. The Opus-class generalists produced about 1.58M
# output tokens. The router's EXPRESS lane says "ONE cheap subagent
# (haiku/sonnet)", but nothing said that leaving `model` out means Opus, and
# `enforce-router.sh` only looks at direct Haiku spawns. This hook says it,
# once.
#
# THIS IS A NUDGE, NEVER A WALL. About half of those calls were read-only
# investigation, where Opus is occasionally the right choice. So the hook
# denies AT MOST ONCE per session, and that one deny tells the model exactly
# how to proceed either way. Escape hatches, all independent:
#   1. the once-per-session cap (a DENIED flag file; after the first deny the
#      hook is inert for the rest of the session);
#   2. `[orchestra:allow-opus]` or `[orchestra:allow-opus: <reason>]` anywhere
#      in the prompt or description;
#   3. carve-outs: the prompt mentions `agent-exec` (the relay), the call names
#      any specific subagent_type other than general-purpose/claude, or it
#      names a model other than Opus/Fable (sonnet, haiku, ...);
#   4. `ORCHESTRA_OPUS_NUDGE=off`, or `enforcement.opus_generalist: off`;
#   5. fail-open on anything unexpected: no python3, unparseable stdin, no
#      session id. This script never exits nonzero.
#
# SCOPE. Only the main thread of a session the SessionStart hook classified as
# "instructor" (Opus/Fable). Cheap and unknown sessions are left alone: on a
# Sonnet session an omitted `model` is not an Opus spawn.
#
# OUTPUT. Deny: one PreToolUse JSON object on stdout, exit 0. Allow: exit 0
# with no stdout (the documented "fall through" signal).

if [ "${ORCHESTRA_OPUS_NUDGE:-}" = "off" ]; then
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

def d(v):
    return v if isinstance(v, dict) else {}

try:
    data = json.load(sys.stdin)
    tool_input = d(data.get("tool_input"))
    haystack = (s(tool_input.get("prompt")) + "\n" + s(tool_input.get("description"))).lower()
    subagent_type = s(tool_input.get("subagent_type")).strip().lower()
    model = s(tool_input.get("model")).strip().lower()

    is_generic = subagent_type in ("", "general-purpose", "claude")
    # Omitted `model` inherits the instructor model; naming Opus/Fable is the
    # same spawn on purpose. Anything else (sonnet, haiku, a full model id of
    # another family) is already a cheaper tier.
    is_instructor_class = model == "" or "opus" in model or "fable" in model
    escape = re.search(r"\[orchestra:allow-opus(?::[^\]]*)?\]", haystack) is not None
    carve_out = "agent-exec" in haystack

    print(s(data.get("tool_name")))
    print(re.sub(r"[^A-Za-z0-9_-]", "", s(data.get("session_id"))))
    print("1" if s(data.get("agent_id")) else "0")
    print("1" if (is_generic and is_instructor_class and not escape and not carve_out) else "0")
    print("omitted" if model == "" else model)
except Exception:
    pass
' 2>/dev/null) || PARSED=""

TOOL_NAME=$(printf '%s' "$PARSED" | sed -n '1p')
SESSION_ID=$(printf '%s' "$PARSED" | sed -n '2p')
IS_SUBAGENT=$(printf '%s' "$PARSED" | sed -n '3p')
ELIGIBLE=$(printf '%s' "$PARSED" | sed -n '4p')
MODEL_SEEN=$(printf '%s' "$PARSED" | sed -n '5p')

case "$TOOL_NAME" in
    Agent|Task) ;;
    *) exit 0 ;;
esac

[ -n "$SESSION_ID" ] || exit 0
[ "$IS_SUBAGENT" = "0" ] || exit 0
[ "$ELIGIBLE" = "1" ] || exit 0

# --- instructor sessions only (verdict written by inject-router.sh) ----------
STATE=""
STATE_FILE="${TMPDIR:-/tmp}/orchestra-router-state-${SESSION_ID}"
if [ -r "$STATE_FILE" ]; then
    STATE=$(cat "$STATE_FILE" 2>/dev/null) || STATE=""
fi
[ "$STATE" = "instructor" ] || exit 0

# --- hatch 1: once per session ------------------------------------------------
DENIED_FILE="${TMPDIR:-/tmp}/orchestra-opus-nudge-denied-${SESSION_ID}"
[ -e "$DENIED_FILE" ] && exit 0

# --- config, cached per session (the only place that may spawn agent-exec) ----
CFG_CACHE="${TMPDIR:-/tmp}/orchestra-opus-nudge-cfg-${SESSION_ID}"
MODE=""
if [ -r "$CFG_CACHE" ]; then
    MODE=$(cat "$CFG_CACHE" 2>/dev/null) || MODE=""
fi
if [ -z "$MODE" ]; then
    MODE="nudge"
    if command -v agent-exec >/dev/null 2>&1; then
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
        v = e.get("opus_generalist")
        if v is False or (isinstance(v, str) and v.strip().lower() == "off"):
            sys.stdout.write("off")
except Exception:
    pass
' 2>/dev/null) || CFG=""
            [ "$CFG" = "off" ] && MODE="off"
        fi
    fi
    printf '%s' "$MODE" > "$CFG_CACHE" 2>/dev/null || true
fi
[ "$MODE" = "off" ] && exit 0

: > "$DENIED_FILE" 2>/dev/null || true

python3 - "$MODEL_SEEN" <<'PYEOF' 2>/dev/null
import sys, json
seen = sys.argv[1]
if seen == "omitted":
    how = "leaves `model` out, so it inherits this session's Opus/Fable model"
else:
    how = "names `model: %s`" % seen
reason = (
    "orchestra: this general-purpose subagent %s. Opus-class generalists were "
    "the largest avoidable cost in past sessions. Re-issue the call as: "
    "(1) read-only investigation, search, test or build runs -> add "
    "`model: \"sonnet\"` (or \"haiku\" for a mechanical lookup); "
    "(2) writing or changing code -> do not use a generalist: load the "
    "`orchestra:run` skill and send it to a worker (`agent-exec dispatch "
    "--class light` / `standard`, or orchestra-deep for design-sensitive "
    "work). If this one really needs the instructor-class model, retry the "
    "same call with `[orchestra:allow-opus: <reason>]` in the description. "
    "This is denied once per session; the retry is never blocked." % how
)
sys.stdout.write(json.dumps({
    "hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "deny",
        "permissionDecisionReason": reason,
    }
}))
PYEOF

exit 0
