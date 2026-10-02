#!/bin/bash
# Self-contained test suite for nudge-opus-generalist.sh.
#
# WHAT IS BEING PINNED. Instructor sessions spawned general-purpose subagents
# on Opus/Fable (named `model: opus`, or `model` omitted so it inherits). The
# hook denies the first such spawn once per session and says how to proceed;
# everything else must pass through untouched, and it must never trap a session.
#
# Usage: bash test-nudge-opus-generalist.sh

set -u

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
HOOK="$SCRIPT_DIR/nudge-opus-generalist.sh"

PASS=0
FAIL=0

STUB_DIR=$(mktemp -d "${TMPDIR:-/tmp}/opus-nudge-stub.XXXXXX")
cat > "$STUB_DIR/agent-exec" <<'STUBEOF'
#!/bin/bash
case "$1" in
    config)
        printf '{"enforcement":{"opus_generalist":"%s"}}\n' "${STUB_OPUS_GENERALIST:-nudge}"
        exit 0
        ;;
    *) exit 1 ;;
esac
STUBEOF
chmod +x "$STUB_DIR/agent-exec"

fresh_tmpdir() { mktemp -d "${TMPDIR:-/tmp}/opus-nudge-run.XXXXXX"; }
seed_state() { printf '%s' "$2" > "$1/orchestra-router-state-$3"; }

make_payload() {
    # session_id subagent_type model description [prompt] [agent_id]
    python3 -c '
import sys, json
session_id, subagent_type, model, description = sys.argv[1:5]
prompt = sys.argv[5] if len(sys.argv) > 5 else "look around"
agent_id = sys.argv[6] if len(sys.argv) > 6 else ""
tool_input = {"description": description, "prompt": prompt}
if subagent_type:
    tool_input["subagent_type"] = subagent_type
if model:
    tool_input["model"] = model
payload = {"session_id": session_id, "hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_input": tool_input}
if agent_id:
    payload["agent_id"] = agent_id
sys.stdout.write(json.dumps(payload))
' "$@"
}

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

assert_denied() {
    local label="$1"
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: $label -- expected exit 0, got $RC ($OUT)"; FAIL=$((FAIL + 1)); return
    fi
    if ! printf '%s' "$OUT" | grep -q '"permissionDecision": "deny"'; then
        echo "FAIL: $label -- expected a deny, got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    local want
    for want in 'model: \\"sonnet\\"' 'orchestra:run' 'allow-opus'; do
        if ! printf '%s' "$OUT" | grep -q -- "$want"; then
            echo "FAIL: $label -- the deny must mention $want, got: $OUT"; FAIL=$((FAIL + 1)); return
        fi
    done
    echo "PASS: $label"; PASS=$((PASS + 1))
}

# --- the core behaviour --------------------------------------------------------
T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose "" "survey the repo")" "$T"
assert_denied "1. an omitted model on an instructor session is denied"
printf '%s' "$OUT" | grep -q 'inherits' || { echo "FAIL: 1b. the omitted case should say it inherits"; FAIL=$((FAIL + 1)); }

run_hook "$(make_payload s1 general-purpose "" "survey the repo")" "$T"
assert_silent "2. the retry is never blocked: at most one deny per session"

T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose opus "survey the repo")" "$T"
assert_denied "3. an explicit model: opus is the same spawn on purpose"

T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 "" claude-opus-5-5 "survey the repo")" "$T"
assert_denied "4. a full Opus model id, with no subagent_type, is generic and denied"

T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose fable "survey the repo")" "$T"
assert_denied "5. Fable counts as instructor class"

# --- things that must pass through ----------------------------------------------
T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose sonnet "survey the repo")" "$T"
assert_silent "6. an explicit sonnet is the target behaviour"

run_hook "$(make_payload s1 general-purpose haiku "survey the repo")" "$T"
assert_silent "7. an explicit haiku is fine"

run_hook "$(make_payload s1 Explore "" "survey the repo")" "$T"
assert_silent "8. a named subagent_type is the tool-control mechanism, out of scope"

run_hook "$(make_payload s1 orchestra:orchestra-deep "" "design it")" "$T"
assert_silent "9. orchestra's own workers are the destination, not the problem"

T=$(fresh_tmpdir); seed_state "$T" cheap s1
run_hook "$(make_payload s1 general-purpose "" "survey the repo")" "$T"
assert_silent "10. a cheap-model session: omitted model is not an Opus spawn"

T=$(fresh_tmpdir)
run_hook "$(make_payload s1 general-purpose "" "survey the repo")" "$T"
assert_silent "11. no router verdict: cannot tell the session is Opus, so stay out"

T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose "" "survey the repo" "look around" ag_1)" "$T"
assert_silent "12. a subagent's own spawns are not the instructor's choice"

# --- escape hatches ---------------------------------------------------------------
T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose opus "[orchestra:allow-opus: needs the deep model] audit")" "$T"
assert_silent "13. the escape marker lets a deliberate Opus spawn through"
run_hook "$(make_payload s1 general-purpose "" "survey the repo")" "$T"
assert_denied "14. using the marker does not spend the one deny"

T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose "" "relay" "run agent-exec dispatch --class light ...")" "$T"
assert_silent "15. the agent-exec relay is never denied (would deadlock the mechanism)"

T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose "" "survey the repo")" "$T" ORCHESTRA_OPUS_NUDGE=off
assert_silent "16. kill switch: ORCHESTRA_OPUS_NUDGE=off"

T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook "$(make_payload s1 general-purpose "" "survey the repo")" "$T" STUB_OPUS_GENERALIST=off
assert_silent "17. enforcement.opus_generalist=off disables it"

# --- session scoping and fail-open ----------------------------------------------------
T=$(fresh_tmpdir); seed_state "$T" instructor s1; seed_state "$T" instructor s2
run_hook "$(make_payload s1 general-purpose "" "survey the repo")" "$T"
run_hook "$(make_payload s2 general-purpose "" "survey the repo")" "$T"
assert_denied "18. each session gets its own one deny"

T=$(fresh_tmpdir); seed_state "$T" instructor s1
run_hook '{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"ls"}}' "$T"
assert_silent "19. other tools are not its business"

run_hook "" "$T"
assert_silent "20. empty stdin fails open"

run_hook "not json{" "$T"
assert_silent "21. unparseable stdin fails open"

run_hook '{"tool_name":"Agent","tool_input":{"description":"x","prompt":"y"}}' "$T"
assert_silent "22. without a session id there is nothing to scope a cap to"

echo "----------------------------------------"
echo "PASS: $PASS  FAIL: $FAIL"
rm -rf "$STUB_DIR"
[ "$FAIL" -eq 0 ]
