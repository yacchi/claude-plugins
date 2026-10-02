#!/bin/bash
# orchestra plugin - PostToolUse turn-size tripwire.
#
# WHY. The router asks the model to classify each request into EXPRESS or
# ORCHESTRATED *before acting*. Turn-level transcript analysis says that check
# is not where the failures happen: across instructor sessions with the router
# injected, orchestra was invoked in 1 of the 102 turns that went on to
# hand-edit 10+ files (and in 5 of 279 turns with 5+ edits). The prompts that
# produced those turns look like this:
#
#   [55 edits] "https://github.com/.../pull/204 指摘対応"
#   [45 edits] "作業を続けて"
#   [36 edits] "実装して。その他、環境変数も読めると良いかもしれません。"
#   [27 edits] "y"
#
# None of these read as ORCHESTRATED at classification time. They are small
# requests that turn out large, and the size is only knowable once the work is
# underway -- by which point the lane was chosen and nothing revisits it. An
# earlier version of the router had an abort rule ("if scope moves mid-EXPRESS,
# stop and re-route") but it was prose, and it was lost in a later edit.
#
# This hook is the mechanical version: count the main thread's edits within one
# turn, and once they cross a threshold, put a re-route request in front of the
# model. It never blocks an edit -- a genuinely EXPRESS turn that happens to be
# large is legitimate, and the model can say so and continue.
#
# TWO STAGES. A single nudge was not enough: in a 30-day, two-machine review
# (plugins/orchestra/feedback/2026-10-03-usage-review.md) the nudge fired in
# 34 turns with 8+ edits, and the model answered "EXPRESS 継続 (仕様は手元に
# あります)" and carried on -- up to 37 edits in one turn, with an Agent
# started in only 3 of them. The "I already hold the spec" exemption is
# self-declared, so stage 1 (at the threshold) cannot stop it. Stage 2 fires at
# twice the threshold and says that exemption has expired: holding the spec
# makes the delegation packet cheap to write, it does not make hand edits at
# instructor-model prices cheap. Each stage fires once per turn.
#
# SCOPE.
#   - Main thread only. A subagent making 30 edits is the delegation working
#     as intended; that is the outcome this whole plugin is chasing.
#   - Instructor sessions only, reusing the SessionStart verdict that
#     inject-router.sh already persists (instructor/cheap/unknown). A cheap
#     session has no orchestra lane to re-route into. "unknown" nudges, which
#     mirrors inject-router.sh's own fallback.
#   - Per turn, keyed on `prompt_id` (a documented common input field). No
#     prompt_id means no way to tell one turn from the next, so we do nothing.
#
# ESCAPE HATCHES: `ORCHESTRA_TURN_GUARD=off`; `enforcement.turn_edits: off` (or
# a different integer to move the threshold); and fail-open on anything
# unexpected. Output is advisory context only -- there is no decision field
# here, so even a malformed nudge cannot block a tool call.

if [ "${ORCHESTRA_TURN_GUARD:-}" = "off" ]; then
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
    print(s(data.get("tool_name")))
    print(re.sub(r"[^A-Za-z0-9_-]", "", s(data.get("session_id"))))
    print(re.sub(r"[^A-Za-z0-9_-]", "", s(data.get("prompt_id"))))
    print("1" if s(data.get("agent_id")) else "0")
except Exception:
    pass
' 2>/dev/null) || PARSED=""

TOOL_NAME=$(printf '%s' "$PARSED" | sed -n '1p')
SESSION_ID=$(printf '%s' "$PARSED" | sed -n '2p')
PROMPT_ID=$(printf '%s' "$PARSED" | sed -n '3p')
IS_SUBAGENT=$(printf '%s' "$PARSED" | sed -n '4p')

case "$TOOL_NAME" in
    Edit|Write|MultiEdit|NotebookEdit) ;;
    *) exit 0 ;;
esac

[ "$IS_SUBAGENT" = "0" ] || exit 0
[ -n "$PROMPT_ID" ] || exit 0

# --- session verdict, written by inject-router.sh ---------------------------
STATE="unknown"
if [ -n "$SESSION_ID" ]; then
    STATE_FILE="${TMPDIR:-/tmp}/orchestra-router-state-${SESSION_ID}"
    if [ -r "$STATE_FILE" ]; then
        STATE=$(cat "$STATE_FILE" 2>/dev/null) || STATE="unknown"
    fi
fi
[ "$STATE" = "cheap" ] && exit 0

# --- threshold, cached per session ------------------------------------------
THRESHOLD=""
CFG_CACHE="${TMPDIR:-/tmp}/orchestra-turn-edits-cfg-${SESSION_ID:-nosession}"
if [ -r "$CFG_CACHE" ]; then
    THRESHOLD=$(cat "$CFG_CACHE" 2>/dev/null) || THRESHOLD=""
fi

if [ -z "$THRESHOLD" ]; then
    THRESHOLD="8"
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
    if isinstance(e, dict) and "turn_edits" in e:
        v = e.get("turn_edits")
        # `off` (string, or the YAML 1.1 bareword that loads as False)
        # disables; a positive int moves the threshold. Anything else falls
        # through to the default.
        if v is False or (isinstance(v, str) and v.strip().lower() == "off"):
            sys.stdout.write("off")
        elif isinstance(v, int) and not isinstance(v, bool) and v > 0:
            sys.stdout.write(str(v))
except Exception:
    pass
' 2>/dev/null) || CFG=""
            [ -n "$CFG" ] && THRESHOLD="$CFG"
        fi
    fi
    printf '%s' "$THRESHOLD" > "$CFG_CACHE" 2>/dev/null || true
fi

[ "$THRESHOLD" = "off" ] && exit 0
case "$THRESHOLD" in
    ''|*[!0-9]*) exit 0 ;;   # unusable value -> stay silent
esac

# --- per-turn counter --------------------------------------------------------
COUNT_FILE="${TMPDIR:-/tmp}/orchestra-turn-edits-${SESSION_ID:-nosession}-${PROMPT_ID}"
COUNT=0
if [ -r "$COUNT_FILE" ]; then
    COUNT=$(cat "$COUNT_FILE" 2>/dev/null) || COUNT=0
fi
case "$COUNT" in
    ''|*[!0-9]*) COUNT=0 ;;
esac

COUNT=$((COUNT + 1))
printf '%s' "$COUNT" > "$COUNT_FILE" 2>/dev/null || true

# Each stage is an exact-equality hit, so it fires once per turn without a
# separate "already nudged" marker, and counting continues past stage 1.
if [ "$COUNT" -eq "$THRESHOLD" ]; then
    STAGE=1
elif [ "$COUNT" -eq $((THRESHOLD * 2)) ]; then
    STAGE=2
else
    exit 0
fi

python3 - "$COUNT" "$STAGE" <<'PYEOF' 2>/dev/null
import sys, json
count = sys.argv[1]
stage = sys.argv[2]
stage1 = (
    "orchestra: this turn has now hand-edited %s files in the main thread. "
    "That is past the point where the EXPRESS lane was the right call -- the "
    "request looked small when you classified it, and it is not. Re-classify "
    "NOW rather than at the next user message: if the remaining work is more "
    "than finishing what is already open, stop and load the `orchestra:run` "
    "skill, then delegate the rest with contracts and adversarial review. If "
    "you are deliberately staying in EXPRESS -- you already hold the whole "
    "spec and delegating would mean re-authoring it -- say so in one line and "
    "carry on. This fires once per turn; a firmer request follows if the turn "
    "reaches twice this size." % count
)
stage2 = (
    "orchestra: this turn has now hand-edited %s files in the main thread -- "
    "twice the re-route threshold, and you already saw the first request. "
    "The \"I already hold the spec\" exemption stops applying here: holding "
    "the spec makes a delegation packet cheap to write; it does not make this "
    "many hand edits cheap on the instructor model. Do not just declare "
    "EXPRESS again. Either (a) load the `orchestra:run` skill and hand the "
    "remaining edits to a worker, using the spec you hold as the packet, or "
    "(b) stop at a clean point, say what is left, and let the user's next "
    "message start a fresh turn that is classified from scratch. This fires "
    "once more at most." % count
)
message = stage2 if stage == "2" else stage1
sys.stdout.write(json.dumps({
    "hookSpecificOutput": {
        "hookEventName": "PostToolUse",
        "additionalContext": message,
    }
}))
PYEOF

exit 0
