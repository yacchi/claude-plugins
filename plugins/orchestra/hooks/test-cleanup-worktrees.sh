#!/bin/bash
# Self-contained test suite for cleanup-worktrees.sh.
#
# Requires only bash + python3. Stubs `agent-exec` via a temp dir prepended to
# PATH so these tests never depend on a real orchestra install or real git
# worktrees.
#
# What this guards (see the hook header): a SessionEnd hook that removes the
# finishing session's clean orchestra worktrees while never discarding one
# that still holds uncollected work, never blocking session teardown, and
# never emitting anything when there is nothing to say.
#
# Usage: bash test-cleanup-worktrees.sh

set -u

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
HOOK="$SCRIPT_DIR/cleanup-worktrees.sh"

PASS=0
FAIL=0

STUB_DIR=$(mktemp -d "${TMPDIR:-/tmp}/cleanup-wt-stub.XXXXXX")
cat > "$STUB_DIR/agent-exec" <<'STUBEOF'
#!/bin/bash
echo "$*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    config)
        printf '{"enforcement":{"session_cleanup":"%s"}}\n' "${STUB_SESSION_CLEANUP:-on}"
        exit 0
        ;;
    isolate)
        if [ "$2" = "remove" ]; then
            : > "${STUB_REMOVE_MARKER:-/dev/null}"
            printf '%s' "${STUB_REMOVE_JSON:-}"
            exit "${STUB_REMOVE_EXIT:-0}"
        fi
        exit 1
        ;;
    *)
        exit 1
        ;;
esac
STUBEOF
chmod +x "$STUB_DIR/agent-exec"

# --- payload builder ---------------------------------------------------------
make_payload() {
    python3 -c '
import sys, json
session_id, cwd = sys.argv[1], sys.argv[2]
payload = {"hook_event_name": "SessionEnd", "session_id": session_id, "cwd": cwd}
sys.stdout.write(json.dumps(payload))
' "$@"
}

run_hook() {
    local payload="$1"
    shift
    local tmpdir
    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/cleanup-wt-run.XXXXXX")
    OUT=$(printf '%s' "$payload" | env -i \
        PATH="$STUB_DIR:/usr/bin:/bin:/usr/local/bin" \
        TMPDIR="$tmpdir" HOME="$HOME" \
        STUB_LOG="$tmpdir/agent-exec.log" \
        STUB_REMOVE_MARKER="$tmpdir/removed.marker" \
        "$@" bash "$HOOK" 2>&1)
    RC=$?
    LOG_FILE="$tmpdir/agent-exec.log"
    REMOVE_MARKER="$tmpdir/removed.marker"
}

json_removed() {
    python3 -c '
import json, sys
entries = json.loads(sys.argv[1])
sys.stdout.write(json.dumps({"status": "removed", "removed": entries}))
' "$1"
}

assert_rc0() {
    local label="$1"
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: $label -- expected exit 0, got $RC (output: $OUT)"; FAIL=$((FAIL + 1)); return 1
    fi
    return 0
}

assert_empty() {
    local label="$1"
    assert_rc0 "$label" || return
    if [ -n "$OUT" ]; then
        echo "FAIL: $label -- expected empty stdout, got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

assert_contains() {
    local label="$1"
    local needle="$2"
    assert_rc0 "$label" || return
    if ! printf '%s' "$OUT" | grep -qF -- "$needle"; then
        echo "FAIL: $label -- expected output to contain '$needle', got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

assert_not_contains() {
    local label="$1"
    local needle="$2"
    assert_rc0 "$label" || return
    if printf '%s' "$OUT" | grep -qF -- "$needle"; then
        echo "FAIL: $label -- expected output NOT to contain '$needle', got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

assert_no_stderr_error() {
    local label="$1"
    assert_rc0 "$label" || return
    if printf '%s' "$OUT" | grep -qiE 'syntax error|traceback|command not found'; then
        echo "FAIL: $label -- unexpected shell/python error text: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

assert_marker_absent() {
    local label="$1"
    assert_rc0 "$label" || return
    if [ -e "$REMOVE_MARKER" ]; then
        echo "FAIL: $label -- expected agent-exec isolate remove NOT to run"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

CWD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/cleanup-wt-cwd.XXXXXX")

REMOVED_3=$(json_removed '[
  {"status":"removed","task":"t1","path":"/tmp/w1","branch":"b1","session":"s1"},
  {"status":"removed","task":"t2","path":"/tmp/w2","branch":"b2","session":"s1"},
  {"status":"removed","task":"t3","path":"/tmp/w3","branch":"b3","session":"s1"}
]')

MIXED_1_1=$(json_removed '[
  {"status":"removed","task":"t1","path":"/tmp/w1","branch":"b1","session":"s1"},
  {"status":"dirty","task":"t2","path":"/tmp/w2","files":["a.txt"],"reason":"uncommitted changes"}
]')

ALL_DIRTY=$(json_removed '[
  {"status":"dirty","task":"t1","path":"/tmp/w1","files":["a.txt"],"reason":"uncommitted changes"},
  {"status":"dirty","task":"t2","path":"/tmp/w2","files":["b.txt"],"reason":"uncommitted changes"}
]')

EMPTY_REMOVED=$(json_removed '[]')

ALL_ABSENT=$(json_removed '[
  {"status":"absent","task":"t1"},
  {"status":"absent","task":"t2"}
]')

WITH_ERROR=$(json_removed '[
  {"status":"removed","task":"t1","path":"/tmp/w1","branch":"b1","session":"s1"},
  {"status":"error","task":"t2","reason":"worktree lock held"}
]')

# 1. All-clean: three removed entries.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$REMOVED_3"
assert_contains "1. all-clean: reports 3 removed" "removed 3 session worktree(s)"
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$REMOVED_3"
assert_not_contains "1b. all-clean: no dirty lines" "uncollected changes"

# 2. Mixed: one removed, one dirty.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$MIXED_1_1"
assert_contains "2. mixed: reports 1 removed" "removed 1 session worktree(s)"
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$MIXED_1_1"
assert_contains "2b. mixed: names the dirty task" "t2"
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$MIXED_1_1"
assert_contains "2c. mixed: names the dirty path" "/tmp/w2"
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$MIXED_1_1"
assert_contains "2d. mixed: contains the --force hint" "--force"

# 3. All dirty: no removed count line, dirty lines present.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$ALL_DIRTY"
assert_not_contains "3. all-dirty: no removed count line" "removed 0 session worktree(s)"
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$ALL_DIRTY"
assert_contains "3b. all-dirty: dirty lines present" "uncollected changes"

# 4. Nothing matched: empty removed list -> empty stdout.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$EMPTY_REMOVED"
assert_empty "4. empty removed list -> silent"

# 5. All absent -> empty stdout.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$ALL_ABSENT"
assert_empty "5. all absent -> silent"

# 6. Nested error entry -> summary mentions it.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$WITH_ERROR"
assert_contains "6. error entry: summary mentions error count/reason" "failed"
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$WITH_ERROR"
assert_contains "6b. error entry: reason text present" "worktree lock held"

# 7. Config off -> remove never invoked, stdout empty.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_SESSION_CLEANUP=off STUB_REMOVE_JSON="$REMOVED_3"
assert_empty "7. config off -> silent"
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_SESSION_CLEANUP=off STUB_REMOVE_JSON="$REMOVED_3"
assert_marker_absent "7b. config off -> remove never invoked"

# 8. Env escape hatch wins even when config says on.
run_hook "$(make_payload s1 "$CWD_DIR")" ORCHESTRA_SESSION_CLEANUP=off STUB_SESSION_CLEANUP=on STUB_REMOVE_JSON="$REMOVED_3"
assert_empty "8. env escape hatch -> silent"
run_hook "$(make_payload s1 "$CWD_DIR")" ORCHESTRA_SESSION_CLEANUP=off STUB_SESSION_CLEANUP=on STUB_REMOVE_JSON="$REMOVED_3"
assert_marker_absent "8b. env escape hatch -> remove never invoked"

# 9. agent-exec absent from PATH -> stdout empty, rc 0.
run_hook_no_agent_exec() {
    local payload="$1"
    local tmpdir
    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/cleanup-wt-run.XXXXXX")
    local emptybin="$tmpdir/emptybin"
    mkdir -p "$emptybin"
    OUT=$(printf '%s' "$payload" | env -i \
        PATH="$emptybin:/usr/bin:/bin" \
        TMPDIR="$tmpdir" HOME="$HOME" \
        bash "$HOOK" 2>&1)
    RC=$?
}
run_hook_no_agent_exec "$(make_payload s1 "$CWD_DIR")"
assert_empty "9. agent-exec absent from PATH -> silent"

# 10. Missing / empty / malformed session_id -> remove never invoked, silent.
run_hook "$(make_payload "" "$CWD_DIR")" STUB_REMOVE_JSON="$REMOVED_3"
assert_empty "10a. empty session_id -> silent"
run_hook "$(make_payload "" "$CWD_DIR")" STUB_REMOVE_JSON="$REMOVED_3"
assert_marker_absent "10b. empty session_id -> remove never invoked"
run_hook "$(python3 -c 'import json,sys; sys.stdout.write(json.dumps({"cwd": sys.argv[1]}))' "$CWD_DIR")" STUB_REMOVE_JSON="$REMOVED_3"
assert_empty "10c. missing session_id field -> silent"
run_hook "$(python3 -c 'import json,sys; sys.stdout.write(json.dumps({"session_id": "!!! ***", "cwd": sys.argv[1]}))' "$CWD_DIR")" STUB_REMOVE_JSON="$REMOVED_3"
assert_marker_absent "10d. session_id sanitizes to empty -> remove never invoked"

# 11. Malformed (non-JSON) stdin -> stdout empty, rc 0, no shell error text.
run_hook "not json at all {{{" STUB_REMOVE_JSON="$REMOVED_3"
assert_empty "11. malformed stdin -> silent"
run_hook "not json at all {{{" STUB_REMOVE_JSON="$REMOVED_3"
assert_no_stderr_error "11b. malformed stdin -> no shell error text"

# 12. agent-exec isolate remove exits non-zero / prints non-JSON.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_EXIT=1 STUB_REMOVE_JSON="not json"
assert_empty "12. remove exits non-zero with non-JSON output -> silent, rc 0"
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="not json"
assert_empty "12b. remove exits 0 but prints non-JSON -> silent, rc 0"

# 13. --session and --repo argv assertions.
run_hook "$(make_payload s1 "$CWD_DIR")" STUB_REMOVE_JSON="$REMOVED_3"
assert_rc0 "13. argv check runs cleanly"
if [ -r "$LOG_FILE" ] && grep -q -- "--session s1" "$LOG_FILE" 2>/dev/null && grep -qF -- "--repo $CWD_DIR" "$LOG_FILE" 2>/dev/null; then
    echo "PASS: 13b. --session and --repo match the payload"; PASS=$((PASS + 1))
else
    echo "FAIL: 13b. --session and --repo match the payload -- log: $(cat "$LOG_FILE" 2>/dev/null)"; FAIL=$((FAIL + 1))
fi

echo "----------------------------------------"
echo "PASS: $PASS  FAIL: $FAIL"
rm -rf "$STUB_DIR" "$CWD_DIR"
[ "$FAIL" -eq 0 ]
