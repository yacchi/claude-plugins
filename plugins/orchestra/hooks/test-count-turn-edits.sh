#!/bin/bash
# Self-contained test suite for count-turn-edits.sh.
#
# WHAT IS BEING PINNED. Turn-level transcript analysis: of 102 turns where the
# instructor hand-edited 10+ files, orchestra was invoked in 1. The router asks
# the model to classify a request *before* acting, but the prompts that produce
# those turns are things like "作業を続けて", "実装して", "y" -- small at
# classification time, huge in hindsight. Nothing existed to notice the turn had
# outgrown its lane once work was underway. This hook is that noticing.
#
# Usage: bash test-count-turn-edits.sh

set -u

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
HOOK="$SCRIPT_DIR/count-turn-edits.sh"

PASS=0
FAIL=0

STUB_DIR=$(mktemp -d "${TMPDIR:-/tmp}/turn-edits-stub.XXXXXX")
cat > "$STUB_DIR/agent-exec" <<'STUBEOF'
#!/bin/bash
case "$1" in
    config)
        printf '{"enforcement":{"turn_edits":%s}}\n' "${STUB_TURN_EDITS:-8}"
        exit 0
        ;;
    *) exit 1 ;;
esac
STUBEOF
chmod +x "$STUB_DIR/agent-exec"

make_payload() {
    # session_id prompt_id tool_name [agent_id]
    python3 -c '
import sys, json
session_id, prompt_id, tool_name = sys.argv[1:4]
agent_id = sys.argv[4] if len(sys.argv) > 4 else ""
payload = {
    "session_id": session_id,
    "hook_event_name": "PostToolUse",
    "tool_name": tool_name,
    "tool_input": {"file_path": "/tmp/x.ts"},
}
if prompt_id:
    payload["prompt_id"] = prompt_id
if agent_id:
    payload["agent_id"] = agent_id
    payload["agent_type"] = "orchestra:orchestra-light"
sys.stdout.write(json.dumps(payload))
' "$@"
}

# One TMPDIR per test scenario: the counter file lives there, so sharing it is
# what makes "the same turn" mean the same turn.
fresh_tmpdir() { mktemp -d "${TMPDIR:-/tmp}/turn-edits-run.XXXXXX"; }

# The router's SessionStart verdict decides whether this hook applies at all.
seed_router_state() { printf '%s' "$2" > "$1/orchestra-router-state-$3"; }

run_hook() {
    local payload="$1" tmpdir="$2"
    shift 2
    OUT=$(printf '%s' "$payload" | env -i \
        PATH="$STUB_DIR:/usr/bin:/bin" TMPDIR="$tmpdir" HOME="$HOME" \
        "$@" bash "$HOOK" 2>&1)
    RC=$?
}

assert_silent() {
    local label="$1"
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: $label -- expected exit 0, got $RC ($OUT)"; FAIL=$((FAIL + 1)); return
    fi
    if [ -n "$OUT" ]; then
        echo "FAIL: $label -- expected no output, got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

assert_nudged() {
    local label="$1"
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: $label -- expected exit 0, got $RC ($OUT)"; FAIL=$((FAIL + 1)); return
    fi
    if ! printf '%s' "$OUT" | grep -q '"additionalContext"'; then
        echo "FAIL: $label -- expected an additionalContext nudge, got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    if ! printf '%s' "$OUT" | grep -q 'orchestra:run'; then
        echo "FAIL: $label -- nudge must name the skill to load, got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

# edit_n: fire the hook n times in one turn, leaving OUT/RC from the last call.
edit_n() {
    local tmpdir="$1" n="$2" session="${3:-s1}" prompt="${4:-p1}"
    local i
    for ((i = 0; i < n; i++)); do
        run_hook "$(make_payload "$session" "$prompt" "Edit")" "$tmpdir"
    done
}

# --- the core behaviour ------------------------------------------------------
T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
edit_n "$T" 7
assert_silent "1. below the threshold, stay out of the way"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
edit_n "$T" 8
assert_nudged "2. at the threshold, ask for a re-route"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
edit_n "$T" 8
run_hook "$(make_payload s1 p1 Edit)" "$T"
assert_silent "3. nudges at most once per turn"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
edit_n "$T" 8
edit_n "$T" 7 s1 p2
assert_silent "4. the counter resets on a new turn"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
edit_n "$T" 8
edit_n "$T" 8 s1 p2
assert_nudged "5. a second oversized turn is nudged again"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
edit_n "$T" 4
run_hook "$(make_payload s1 p1 Write)" "$T"
run_hook "$(make_payload s1 p1 MultiEdit)" "$T"
run_hook "$(make_payload s1 p1 Write)" "$T"
run_hook "$(make_payload s1 p1 Write)" "$T"
assert_nudged "6. Write and MultiEdit count toward the same total"

# --- scope -------------------------------------------------------------------
T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
for i in 1 2 3 4 5 6 7 8 9; do
    run_hook "$(make_payload s1 p1 Edit ag_1)" "$T"
done
assert_silent "7. a subagent's edits are the delegation working, not a problem"

T=$(fresh_tmpdir); seed_router_state "$T" cheap s1
edit_n "$T" 9
assert_silent "8. cheap-model sessions are out of the router's scope"

T=$(fresh_tmpdir)   # no router state written at all
edit_n "$T" 8
assert_nudged "9. unknown session verdict still nudges (matches inject-router fallback)"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
for i in 1 2 3 4 5 6 7 8 9; do
    run_hook "$(make_payload s1 p1 Read)" "$T"
done
assert_silent "10. reads are not edits"

# --- configuration and escape hatches ---------------------------------------
T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
for i in 1 2 3; do run_hook "$(make_payload s1 p1 Edit)" "$T" STUB_TURN_EDITS=3; done
assert_nudged "11. threshold is configurable (enforcement.turn_edits)"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
for i in 1 2 3 4 5 6 7 8 9 10; do
    run_hook "$(make_payload s1 p1 Edit)" "$T" STUB_TURN_EDITS='"off"'
done
assert_silent "12. enforcement.turn_edits=off disables it"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
for i in 1 2 3 4 5 6 7 8 9 10; do
    run_hook "$(make_payload s1 p1 Edit)" "$T" ORCHESTRA_TURN_GUARD=off
done
assert_silent "13. kill switch: ORCHESTRA_TURN_GUARD=off"

# --- fail-open ---------------------------------------------------------------
T=$(fresh_tmpdir)
run_hook "" "$T"
assert_silent "14. empty stdin fails open"

T=$(fresh_tmpdir)
run_hook "not json" "$T"
assert_silent "15. unparseable stdin fails open"

T=$(fresh_tmpdir); seed_router_state "$T" instructor s1
for i in 1 2 3 4 5 6 7 8 9; do
    run_hook "$(make_payload s1 "" Edit)" "$T"
done
assert_silent "16. without a prompt_id there is no turn to count"

# --- separate sessions must not share a counter ------------------------------
T=$(fresh_tmpdir); seed_router_state "$T" instructor s1; seed_router_state "$T" instructor s2
edit_n "$T" 5 s1 p1
edit_n "$T" 4 s2 p1
assert_silent "17. two sessions with the same prompt id are separate turns"

echo "----------------------------------------"
echo "PASS: $PASS  FAIL: $FAIL"
rm -rf "$STUB_DIR"
[ "$FAIL" -eq 0 ]
