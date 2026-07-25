#!/bin/bash
# Self-contained test suite for enforce-router.sh.
#
# Requires only bash + python3 (the same dependencies the hook itself
# requires). Stubs `agent-exec` via a temp dir prepended to PATH -- these
# tests must NOT depend on a real orchestra installation, since
# tools/agent_exec.py is being written concurrently with this hook.
#
# Each test runs the hook in complete isolation: its own TMPDIR (so session
# state files never leak between tests) and, unless the test explicitly
# wants to reuse state (the deny-cap tests below), its own session id.
#
# Tests 13-15 specifically guard the per-session DENY CAP (see the header
# comment in enforce-router.sh, hatch 1): the hook must deny at most once
# per session, full stop -- even when the prompt text mutates round to
# round (orchestra's own retry convention appends new feedback text every
# round, which changes the fingerprint) or when two genuinely unrelated
# tasks share a session. A fingerprint-only anti-lockout design would fail
# these.
#
# Usage: bash test-enforce-router.sh

set -u

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
HOOK="$SCRIPT_DIR/enforce-router.sh"

PASS=0
FAIL=0

# --- stub agent-exec -------------------------------------------------------
#
# A single stub directory whose behavior is driven entirely by env vars, so
# every test can reuse the same stub binary while varying what it returns.
# Every invocation is appended (one line: the joined argv) to
# $STUB_LOG so tests can assert exactly what was and wasn't called --
# in particular, that `route` is never invoked when `light_class=off`, and
# that neither `config` nor `route` is invoked once the session cache is
# warm or when an escape hatch fires before the cache is ever consulted.
STUB_DIR=$(mktemp -d "${TMPDIR:-/tmp}/enforce-router-stub.XXXXXX")

cat > "$STUB_DIR/agent-exec" <<'STUBEOF'
#!/bin/bash
echo "$*" >> "$STUB_LOG"
case "$1" in
    config)
        if [ "${STUB_CONFIG_FAIL:-0}" = "1" ]; then
            exit 1
        fi
        printf '{"enforcement":{"light_class":"%s"}}\n' "${STUB_LIGHT_CLASS:-off}"
        exit 0
        ;;
    route)
        if [ "${STUB_ROUTE_FAIL:-0}" = "1" ]; then
            exit 1
        fi
        printf '{"executor":"%s","dispatch":"cli","model":"%s","effort":"%s"}\n' \
            "${STUB_ROUTE_EXECUTOR:-copilot}" "${STUB_ROUTE_MODEL:-gpt-5.6-luna}" "${STUB_ROUTE_EFFORT:-medium}"
        exit 0
        ;;
    *)
        exit 1
        ;;
esac
STUBEOF
chmod +x "$STUB_DIR/agent-exec"

# --- payload builder --------------------------------------------------------
#
# Builds the PreToolUse stdin JSON via python3 (not hand-quoted strings) so
# prompt text containing brackets/quotes/colons can never break the JSON.
# Extra tool_input keys (e.g. tool-restriction keys) are passed as
# KEY=VALUE pairs after the fixed positional args.
make_payload() {
    session_id="$1"; tool_name="$2"; subagent_type="$3"; model="$4"; prompt="$5"; description="$6"
    shift 6
    python3 -c '
import sys, json
session_id, tool_name, subagent_type, model, prompt, description = sys.argv[1:7]
extra = sys.argv[7:]
tool_input = {
    "description": description,
    "prompt": prompt,
    "subagent_type": subagent_type,
    "model": model,
}
for kv in extra:
    k, _, v = kv.partition("=")
    tool_input[k] = v.split(",") if v else []
payload = {
    "session_id": session_id,
    "hook_event_name": "PreToolUse",
    "tool_name": tool_name,
    "tool_input": tool_input,
}
sys.stdout.write(json.dumps(payload))
' "$session_id" "$tool_name" "$subagent_type" "$model" "$prompt" "$description" "$@"
}

# --- test harness ------------------------------------------------------------
#
# run_hook: execute the hook with an isolated TMPDIR + PATH (stub dir
# prepended unless NO_STUB=1) and the given stdin. Sets globals OUT / RC.
run_hook() {
    local stdin_payload="$1"
    local tmpdir="$2"
    local extra_env=("${@:3}")
    if [ "${NO_STUB:-0}" = "1" ]; then
        OUT=$(printf '%s' "$stdin_payload" | env -i PATH="/usr/bin:/bin" TMPDIR="$tmpdir" HOME="$HOME" "${extra_env[@]}" bash "$HOOK" 2>&1)
    else
        OUT=$(printf '%s' "$stdin_payload" | env -i PATH="$STUB_DIR:/usr/bin:/bin" TMPDIR="$tmpdir" HOME="$HOME" STUB_LOG="$tmpdir/agent-exec.log" "${extra_env[@]}" bash "$HOOK" 2>&1)
    fi
    RC=$?
    touch "$tmpdir/agent-exec.log" 2>/dev/null || true
}

fresh_tmpdir() {
    mktemp -d "${TMPDIR:-/tmp}/enforce-router-test.XXXXXX"
}

log_lines() {
    # number of agent-exec invocations logged so far in this test's tmpdir
    wc -l < "$1/agent-exec.log" 2>/dev/null | tr -d ' '
}

assert_allow() {
    local label="$1"
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: $label -- expected exit 0, got $RC (output: $OUT)"
        FAIL=$((FAIL + 1))
        return
    fi
    if [ -n "$OUT" ]; then
        echo "FAIL: $label -- expected empty stdout (allow), got: $OUT"
        FAIL=$((FAIL + 1))
        return
    fi
    echo "PASS: $label"
    PASS=$((PASS + 1))
}

assert_deny() {
    local label="$1"
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: $label -- expected exit 0, got $RC (output: $OUT)"
        FAIL=$((FAIL + 1))
        return
    fi
    if ! printf '%s' "$OUT" | python3 -c '
import sys, json
try:
    data = json.load(sys.stdin)
    hso = data["hookSpecificOutput"]
    assert hso["hookEventName"] == "PreToolUse"
    assert hso["permissionDecision"] == "deny"
    assert isinstance(hso["permissionDecisionReason"], str) and len(hso["permissionDecisionReason"]) > 0
except Exception:
    sys.exit(1)
' 2>/dev/null; then
        echo "FAIL: $label -- expected a valid deny JSON object, got: $OUT"
        FAIL=$((FAIL + 1))
        return
    fi
    echo "PASS: $label"
    PASS=$((PASS + 1))
}

assert_eq() {
    local label="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "PASS: $label"
        PASS=$((PASS + 1))
    else
        echo "FAIL: $label -- expected [$expected], got [$actual]"
        FAIL=$((FAIL + 1))
    fi
}

# =============================================================================
# 1. Kill switch env var -> allow (before any work, even with an otherwise
#    fully-qualifying deny payload).
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-killswitch" "Agent" "orchestra-light" "haiku" "Implement X" "impl task")
run_hook "$P" "$T" ORCHESTRA_ENFORCEMENT=off STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "1. kill switch ORCHESTRA_ENFORCEMENT=off -> allow"
assert_eq "1b. kill switch -> zero agent-exec invocations" "0" "$(log_lines "$T")"

# =============================================================================
# 2. enforcement: off -> allow, and `agent-exec route` must never be spawned.
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-off" "Agent" "orchestra-light" "haiku" "Implement Y" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=off
assert_allow "2. enforcement.light_class=off -> allow"
if grep -q '^route' "$T/agent-exec.log" 2>/dev/null; then
    echo "FAIL: 2b. route must not be invoked when light_class=off"
    FAIL=$((FAIL + 1))
else
    echo "PASS: 2b. route not invoked when light_class=off"
    PASS=$((PASS + 1))
fi

# =============================================================================
# 3 & 4. haiku + block + copilot available -> deny exactly once; the SAME
#    input a second time -> allow (anti-lockout guarantee), with zero new
#    agent-exec invocations (proves the session cache is actually reused).
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-denyonce" "Agent" "orchestra-light" "haiku" "Implement formatBytes" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot STUB_ROUTE_MODEL=gpt-5.6-luna STUB_ROUTE_EFFORT=medium
assert_deny "3. haiku + block + copilot available -> deny"
case "$OUT" in
    *agent-exec\ dispatch*) echo "PASS: 3b. deny reason names agent-exec dispatch"; PASS=$((PASS + 1)) ;;
    *) echo "FAIL: 3b. deny reason should name agent-exec dispatch, got: $OUT"; FAIL=$((FAIL + 1)) ;;
esac
LINES_AFTER_FIRST=$(log_lines "$T")

run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "4. identical call a second time -> allow (anti-lockout)"
assert_eq "4b. no NEW agent-exec invocations on the cached retry" "$LINES_AFTER_FIRST" "$(log_lines "$T")"

# =============================================================================
# 5. prompt containing "agent-exec" -> allow (relay carve-out). Must short
#    circuit before ever consulting the session cache/agent-exec.
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-relay" "Agent" "orchestra-light" "haiku" "Run agent-exec dispatch --class light --prompt-file f --workdir w --capture" "relay call")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "5. prompt mentions agent-exec -> allow (relay carve-out)"
assert_eq "5b. relay carve-out short-circuits before any agent-exec call" "0" "$(log_lines "$T")"

# =============================================================================
# 6. explicit escape marker -> allow, no deny recorded.
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-marker" "Agent" "orchestra-light" "haiku" "Implement Z [orchestra:allow-claude: needs Read-only tools]" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "6. [orchestra:allow-claude: needs Read-only tools] -> allow"
assert_eq "6b. escape marker short-circuits before any agent-exec call" "0" "$(log_lines "$T")"

# =============================================================================
# 7. tool-restriction key present -> allow (fine-grained tool control case).
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-restrict" "Agent" "orchestra-light" "haiku" "Implement W" "impl task" "tools=Read,Grep")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "7. tools restriction key present -> allow"
assert_eq "7b. tool-restriction carve-out short-circuits before any agent-exec call" "0" "$(log_lines "$T")"

# also check the alternate spellings are honored
T=$(fresh_tmpdir)
P=$(make_payload "sess-restrict2" "Agent" "" "haiku" "Implement W2" "impl task" "disallowedTools=Bash")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "7c. disallowedTools restriction key present -> allow"

# =============================================================================
# 8. subagent_type: Explore -> allow (reserved carve-out).
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-explore" "Agent" "Explore" "haiku" "Find where X is defined" "search")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "8. subagent_type=Explore -> allow"
assert_eq "8b. reserved subagent_type carve-out short-circuits before any agent-exec call" "0" "$(log_lines "$T")"

# =============================================================================
# 9. model sonnet/opus, non-light subagent_type -> allow (not light class).
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-sonnet" "Agent" "general-purpose" "sonnet" "Implement V" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "9. model=sonnet, non-light subagent_type -> allow"
assert_eq "9b. non-light call never consults agent-exec" "0" "$(log_lines "$T")"

T=$(fresh_tmpdir)
P=$(make_payload "sess-opus" "Agent" "general-purpose" "opus" "Implement U" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "9c. model=opus, non-light subagent_type -> allow"

# =============================================================================
# 10. route returns "claude" -> allow (nothing better to redirect to).
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-routeclaude" "Agent" "orchestra-light" "haiku" "Implement T" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=claude
assert_allow "10. route executor=claude -> allow (nothing to redirect to)"

# =============================================================================
# 11. malformed/empty stdin -> allow, exit 0.
# =============================================================================
T=$(fresh_tmpdir)
run_hook "" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "11a. empty stdin -> allow"
assert_eq "11a-count. empty stdin never consults agent-exec" "0" "$(log_lines "$T")"

T=$(fresh_tmpdir)
run_hook 'not { valid json at all' "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "11b. malformed (non-JSON) stdin -> allow"

# =============================================================================
# 12. agent-exec absent from PATH -> allow, exit 0.
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-noagentexec" "Agent" "orchestra-light" "haiku" "Implement S" "impl task")
NO_STUB=1 run_hook "$P" "$T"
assert_allow "12. agent-exec absent from PATH -> allow"

# =============================================================================
# 13. REGRESSION for defect 1: round 1 deny -> round 2 with a realistic
#    retry-feedback suffix appended (orchestra's own retry convention,
#    skills/run/SKILL.md pipeline(): "This is retry N of MAX ... apply this
#    feedback exactly: " + JSON.stringify(feedback)) -> MUST allow. This
#    mutates the prompt text, so the fingerprint DIFFERS from round 1 -- if
#    the anti-lockout guarantee were still fingerprint-only, this would deny
#    again. It must not: the per-session cap makes round 2 allow regardless.
# =============================================================================
T=$(fresh_tmpdir)
ROUND1_PROMPT="Implement formatBytes(bytes) returning a human-readable string."
P1=$(make_payload "sess-retrysuffix" "Agent" "orchestra-light" "haiku" "$ROUND1_PROMPT" "impl task")
run_hook "$P1" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_deny "13a. round 1 -> deny"

ROUND2_PROMPT="${ROUND1_PROMPT}

This is retry 2 of 3. Your previous attempt already wrote files to disk at the paths you used before. Read those files first, then apply this feedback exactly, changing only what it names:
{\"pass\": false, \"reason\": \"formatBytes(1536) returned '1.5KB' but expected '1.5 KB' (missing space)\"}"
P2=$(make_payload "sess-retrysuffix" "Agent" "orchestra-light" "haiku" "$ROUND2_PROMPT" "impl task retry")
run_hook "$P2" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "13b. round 2 (mutated prompt, different fingerprint) -> allow (defect 1 fix)"

# =============================================================================
# 14. A 3-round orchestra-delegate-style loop (implement -> review FAIL ->
#    retry with feedback -> review FAIL -> retry with different feedback),
#    each round's prompt text genuinely different -> AT MOST ONE deny total
#    across all three rounds.
# =============================================================================
T=$(fresh_tmpdir)
BASE_PROMPT="Implement parseConfig(text) per the attached spec."
DENY_COUNT=0
ROUND_PROMPT="$BASE_PROMPT"
for i in 1 2 3; do
    P=$(make_payload "sess-3round" "Agent" "orchestra-light" "haiku" "$ROUND_PROMPT" "round $i")
    run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: 14. round $i: hook exited nonzero ($RC)"
        FAIL=$((FAIL + 1))
    elif [ -n "$OUT" ]; then
        DENY_COUNT=$((DENY_COUNT + 1))
    fi
    ROUND_PROMPT="${BASE_PROMPT}

This is retry $((i + 1)) of 4. Apply this feedback exactly, changing only what it names:
{\"pass\": false, \"round\": $i, \"reason\": \"distinct feedback text for round $i\"}"
done
assert_eq "14. at most one deny across a 3-round retry loop" "1" "$DENY_COUNT"

# =============================================================================
# 15. Two UNRELATED tasks (different prompt content, different task) in one
#    session -> at most one deny total (the cap is per-SESSION, not
#    per-task -- a second, genuinely different light-class task must also
#    be allowed once the session's one deny has already fired).
# =============================================================================
T=$(fresh_tmpdir)
PA=$(make_payload "sess-twotasks" "Agent" "orchestra-light" "haiku" "Implement a CSV parser" "task A")
run_hook "$PA" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_deny "15a. first (unrelated) task -> deny"

PB=$(make_payload "sess-twotasks" "Agent" "orchestra-light" "haiku" "Implement a completely different rate limiter module" "task B, nothing to do with task A")
run_hook "$PB" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "15b. second unrelated task, same session -> allow (per-session cap, not per-task)"

# =============================================================================
# 16. subagent_type: general-purpose + haiku -> deny (still a generic call,
#    still in scope for enforcement).
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-genpurpose" "Agent" "general-purpose" "haiku" "Implement R" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_deny "16. subagent_type=general-purpose + haiku -> deny"

# =============================================================================
# 17. A named custom subagent_type + haiku -> allow (naming any specific,
#    non-generic agent IS the tool-control / specialized-prompt mechanism;
#    Copilot cannot substitute for it -- this is the FIX B carve-out).
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-customagent" "Agent" "my-custom-worker" "haiku" "Implement Q" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_allow "17. named custom subagent_type + haiku -> allow"

# =============================================================================
# 18. subagent_type: orchestra:orchestra-light (namespaced form) -> still
#    deniable -- the sole named-subagent_type exception, since redirecting
#    it is the entire point of this hook.
# =============================================================================
T=$(fresh_tmpdir)
P=$(make_payload "sess-namespaced" "Agent" "orchestra:orchestra-light" "haiku" "Implement P" "impl task")
run_hook "$P" "$T" STUB_LIGHT_CLASS=block STUB_ROUTE_EXECUTOR=copilot
assert_deny "18. subagent_type=orchestra:orchestra-light -> still deniable"

# =============================================================================
# Summary
# =============================================================================
echo "----------------------------------------"
echo "PASS: $PASS  FAIL: $FAIL"

rm -rf "$STUB_DIR" 2>/dev/null || true

if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
exit 0
