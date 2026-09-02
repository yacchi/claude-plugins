#!/bin/bash
# Self-contained test suite for guard-worker-tree.sh.
#
# Requires only bash + python3 + git. Stubs `agent-exec` via a temp dir
# prepended to PATH so these tests never depend on a real orchestra install.
#
# What this guards (see the hook header): a *worker* subagent that has its own
# orchestra worktree writing into the user's MAIN working tree anyway. It has
# been observed: a Sonnet worker whose prompt opened with "WORKING TREE: <path>
# / cd there FIRST ... Never touch files outside that path" ran Edit/Write
# against the same files in the user's tree instead, and its rejected round-1
# output is what the user would have kept. The tests below pin the four things
# that make a mechanism usable where prose failed: it must catch the real
# accident (structured tools, airtight), it must never touch the main thread,
# it must let a worker write freely inside its own worktree, and every
# uncertainty must fail open.
#
# Usage: bash test-guard-worker-tree.sh

set -u

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
HOOK="$SCRIPT_DIR/guard-worker-tree.sh"

PASS=0
FAIL=0

STUB_DIR=$(mktemp -d "${TMPDIR:-/tmp}/guard-tree-stub.XXXXXX")
cat > "$STUB_DIR/agent-exec" <<'STUBEOF'
#!/bin/bash
case "$1" in
    config)
        if [ "${STUB_CONFIG_FAIL:-0}" = "1" ]; then
            echo "agent-exec: config lookup failed" >&2
            exit 1
        fi
        printf '{"enforcement":{"worker_tree":"%s"}}\n' "${STUB_WORKER_TREE:-block}"
        exit 0
        ;;
    *)
        exit 1
        ;;
esac
STUBEOF
chmod +x "$STUB_DIR/agent-exec"

# --- repos ------------------------------------------------------------------
# MAIN stands in for the user's tree; ISO for the worktree orchestra created
# for session 'abcdefgh'. SIBLING is the prefix trap: a directory whose path
# starts with MAIN's path but which is not inside it. SOLO is a repo with no
# orchestra worktree at all (the express lane).
REPO_BASE=$(mktemp -d "${TMPDIR:-/tmp}/guard-tree-repo.XXXXXX")
REPO_BASE=$(CDPATH= cd -- "$REPO_BASE" && pwd -P)
MAIN="$REPO_BASE/repo"
ISO="$REPO_BASE/iso"
SIBLING="$REPO_BASE/repo-worktrees/x"
mkdir -p "$MAIN" "$SIBLING"
(
    cd "$MAIN" || exit 1
    git init -q -b main . && git config user.email t@e.com && git config user.name T
    mkdir -p src && echo hi > README.md && echo one > src/app.ts
    git add -A && git commit -q -m init
    git worktree add -q -b orchestra/abcdefgh/t1 "$ISO" HEAD
) >/dev/null 2>&1
ln -s "$MAIN" "$REPO_BASE/link"

SOLO="$REPO_BASE/solo"
mkdir -p "$SOLO"
(
    cd "$SOLO" || exit 1
    git init -q -b main . && git config user.email t@e.com && git config user.name T
    echo hi > README.md && git add -A && git commit -q -m init
) >/dev/null 2>&1

NON_REPO=$(mktemp -d "${TMPDIR:-/tmp}/guard-tree-norepo.XXXXXX")

# --- payload builder ---------------------------------------------------------
# agent_id/agent_type are the documented PreToolUse fields that identify a
# subagent call; passing an empty agent_id omits it, i.e. the main thread.
# Args: agent_id agent_type cwd tool_name file_path command description session
make_payload() {
    python3 -c '
import sys, json
agent_id, agent_type, cwd, tool, path, command, description, session = sys.argv[1:9]
ti = {}
if tool == "Bash":
    ti["command"] = command
    ti["description"] = description
else:
    if path:
        ti["notebook_path" if tool == "NotebookEdit" else "file_path"] = path
    if description:
        ti["description"] = description
payload = {
    "session_id": session,
    "hook_event_name": "PreToolUse",
    "tool_name": tool,
    "cwd": cwd,
    "tool_input": ti,
}
if agent_id:
    payload["agent_id"] = agent_id
    payload["agent_type"] = agent_type
sys.stdout.write(json.dumps(payload))
' "$@"
}

SESSION="abcdefgh-1111-2222-3333-444455556666"

# worker_edit <cwd> <file_path> [tool]
worker_edit() {
    make_payload "ag_1" "orchestra:orchestra-standard" "$1" "${3:-Edit}" "$2" "" "" "$SESSION"
}
# worker_bash <cwd> <command> [description]
worker_bash() {
    make_payload "ag_1" "orchestra:orchestra-standard" "$1" "Bash" "" "$2" "${3:-do work}" "$SESSION"
}

run_hook() {
    local payload="$1"
    shift
    local tmpdir
    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/guard-tree-run.XXXXXX")
    OUT=$(printf '%s' "$payload" | env -i \
        PATH="$STUB_DIR:/usr/bin:/bin:/usr/local/bin" \
        TMPDIR="$tmpdir" HOME="$HOME" \
        "$@" bash "$HOOK" 2>&1)
    RC=$?
}

assert_allow() {
    local label="$1"
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: $label -- expected exit 0, got $RC (output: $OUT)"; FAIL=$((FAIL + 1)); return
    fi
    if [ -n "$OUT" ]; then
        echo "FAIL: $label -- expected empty stdout (allow), got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

assert_deny() {
    local label="$1"
    if [ "$RC" -ne 0 ]; then
        echo "FAIL: $label -- expected exit 0, got $RC (output: $OUT)"; FAIL=$((FAIL + 1)); return
    fi
    if ! printf '%s' "$OUT" | grep -q '"permissionDecision": *"deny"'; then
        echo "FAIL: $label -- expected a deny decision, got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $label"; PASS=$((PASS + 1))
}

# === edge case 3: the observed incident =====================================
run_hook "$(worker_edit "$ISO" "$MAIN/src/app.ts")"
assert_deny "3a. worker Edit into the main tree while its worktree exists"

run_hook "$(worker_edit "$ISO" "$MAIN/src/new.ts" "Write")"
assert_deny "3b. worker Write into the main tree"

run_hook "$(worker_edit "$ISO" "$MAIN/src/app.ts" "MultiEdit")"
assert_deny "3c. worker MultiEdit into the main tree"

run_hook "$(worker_edit "$ISO" "$MAIN/nb.ipynb" "NotebookEdit")"
assert_deny "3d. worker NotebookEdit (notebook_path) into the main tree"

run_hook "$(worker_edit "$MAIN" "$MAIN/src/app.ts")"
assert_deny "3e. worker whose cwd is itself the main tree"

# === edge case 1: the main thread is never touched ==========================
run_hook "$(make_payload "" "" "$MAIN" "Edit" "$MAIN/src/app.ts" "" "" "$SESSION")"
assert_allow "1a. main thread Edit in the main tree"

run_hook "$(make_payload "" "" "$MAIN" "Bash" "" 'echo x > '"$MAIN"'/src/app.ts' "instructor" "$SESSION")"
assert_allow "1b. main thread Bash redirection in the main tree"

# === edge case 2: a worker owns its own worktree ============================
run_hook "$(worker_edit "$ISO" "$ISO/src/app.ts")"
assert_allow "2a. worker Edit inside its own orchestra worktree"

run_hook "$(worker_edit "$ISO" "src/app.ts")"
assert_allow "2b. worker relative Edit resolves inside its own worktree"

# === edge case 4: no orchestra worktree -> express lane =====================
run_hook "$(make_payload "ag_1" "orchestra:orchestra-standard" "$SOLO" "Edit" "$SOLO/README.md" "" "" "$SESSION")"
assert_allow "4a. subagent in a repo with no orchestra worktree"

run_hook "$(make_payload "ag_1" "orchestra:orchestra-standard" "$ISO" "Edit" "$MAIN/src/app.ts" "" "" "zzzzzzzz-9999")"
assert_allow "4b. worktrees exist but none for THIS session"

# === edge case 5: sibling prefix ============================================
run_hook "$(worker_edit "$ISO" "$SIBLING/notes.md")"
assert_allow "5. /repo-worktrees/x is not inside /repo"

# === edge case 6: relative path resolved against cwd ========================
run_hook "$(make_payload "ag_1" "orchestra:orchestra-standard" "$MAIN" "Edit" "src/app.ts" "" "" "$SESSION")"
assert_deny "6. relative file_path resolves against cwd into the main tree"

# === edge case 7: symlink ===================================================
run_hook "$(worker_edit "$ISO" "$REPO_BASE/link/src/app.ts")"
assert_deny "7. symlink whose realpath lands in the main tree"

# === edge case 8: .. escape =================================================
run_hook "$(worker_edit "$ISO" "../repo/src/app.ts")"
assert_deny "8. relative path with .. escaping back into the main tree"

# === edge case 9: not a repo / no git =======================================
run_hook "$(make_payload "ag_1" "orchestra:orchestra-standard" "$NON_REPO" "Edit" "$NON_REPO/a.txt" "" "" "$SESSION")"
assert_allow "9a. cwd outside any git repo fails open"

# An empty PATH strips python3 (and cat, and git) entirely; bash is invoked by
# absolute path so the hook still starts.
BASH_BIN=$(command -v bash)
OUT=$(printf '%s' "$(worker_edit "$ISO" "$MAIN/src/app.ts")" | env -i \
    PATH="$STUB_DIR" TMPDIR="${TMPDIR:-/tmp}" HOME="$HOME" "$BASH_BIN" "$HOOK" 2>&1)
RC=$?
if [ "$RC" -eq 0 ] && [ -z "$OUT" ]; then
    echo "PASS: 9b. no python3 on PATH fails open silently"; PASS=$((PASS + 1))
else
    echo "FAIL: 9b. no python3 on PATH -- rc=$RC out=$OUT"; FAIL=$((FAIL + 1))
fi

# git absent but python3 present: the guard cannot locate any worktree.
NOGIT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/guard-tree-nogit.XXXXXX")
ln -s "$(command -v bash)" "$NOGIT_DIR/bash"
ln -s "$(command -v python3)" "$NOGIT_DIR/python3"
ln -s "$(command -v cat)" "$NOGIT_DIR/cat"
ln -s "$(command -v sed)" "$NOGIT_DIR/sed"
ln -s "$(command -v tr)" "$NOGIT_DIR/tr"
run_hook "$(worker_edit "$ISO" "$MAIN/src/app.ts")" PATH="$NOGIT_DIR"
assert_allow "9c. git absent fails open silently"
rm -rf "$NOGIT_DIR"

# === edge case 10/11/12: escape hatches =====================================
run_hook "$(worker_edit "$ISO" "$MAIN/src/app.ts")" ORCHESTRA_TREE_GUARD=off
assert_allow "10. kill switch: ORCHESTRA_TREE_GUARD=off"

run_hook "$(worker_edit "$ISO" "$MAIN/src/app.ts")" STUB_WORKER_TREE=off
assert_allow "11. config: enforcement.worker_tree=off"

run_hook "$(worker_edit "$ISO" "$MAIN/src/app.ts")" STUB_CONFIG_FAIL=1
assert_allow "11b. config lookup fails (agent-exec config nonzero exit) fails open"

run_hook "$(worker_bash "$ISO" 'echo x > '"$MAIN"'/src/app.ts [orchestra:allow-tree: user asked]')"
assert_allow "12a. escape marker in the command"

run_hook "$(worker_bash "$ISO" 'echo x > '"$MAIN"'/src/app.ts' 'patch [orchestra:allow-tree: deliberate]')"
assert_allow "12b. escape marker in the description"

# === edge case 13: Bash write shapes ========================================
run_hook "$(worker_bash "$ISO" 'echo x > '"$MAIN"'/src/app.ts')"
assert_deny "13a. Bash > into the main tree"

run_hook "$(worker_bash "$ISO" 'echo x >> '"$MAIN"'/src/app.ts')"
assert_deny "13b. Bash >> into the main tree"

run_hook "$(worker_bash "$ISO" 'echo x | tee -a '"$MAIN"'/src/app.ts')"
assert_deny "13c. Bash tee into the main tree"

run_hook "$(worker_bash "$ISO" 'sed -i -e s/one/two/ '"$MAIN"'/src/app.ts')"
assert_deny "13d. Bash sed -i on a main-tree file"

run_hook "$(worker_bash "$ISO" 'npm test; echo done > '"$MAIN"'/log.txt')"
assert_deny "13e. redirection later in a compound command"

run_hook "$(worker_bash "$ISO" 'echo x > $WORKTREE/file')"
assert_allow "13f. unexpanded \$VAR target is skipped (fail open)"

run_hook "$(worker_bash "$ISO" 'echo x > '"$ISO"'/src/app.ts')"
assert_allow "13g. redirection into the worker's own worktree"

run_hook "$(worker_bash "$ISO" 'sed -i -e s/one/two/ '"$ISO"'/src/app.ts')"
assert_allow "13h. sed -i inside the worker's own worktree"

run_hook "$(worker_bash "$ISO" 'sed -i s/one/two/ '"$MAIN"'/src/app.ts')"
assert_deny "13i. an unquoted sed script is not mistaken for a path"

# === edge case 14: no write at all ==========================================
run_hook "$(worker_bash "$ISO" 'cat '"$MAIN"'/src/app.ts | grep x')"
assert_allow "14a. a read-only pipeline"

run_hook "$(worker_bash "$ISO" 'cd '"$MAIN"' && npm test 2>&1')"
assert_allow "14b. 2>&1 is not a file target"

run_hook "$(worker_bash "$ISO" 'cp '"$ISO"'/a.ts '"$MAIN"'/a.ts')"
assert_allow "14c. cp/mv/rm are deliberately not analyzed (fail open)"

# === edge case 15: text that merely mentions a redirection ==================
run_hook "$(worker_bash "$ISO" "$(printf 'cat > %s/task.md <<EOF\nWrite the result to %s/out.txt when done.\nEOF' "$ISO" "$MAIN")")"
assert_allow "15a. a main-tree path mentioned inside a heredoc body"

run_hook "$(worker_bash "$ISO" 'echo "never write > '"$MAIN"'/src/app.ts"')"
assert_allow "15b. a quoted string is data, not a redirection"

# === edge case 16: malformed stdin ==========================================
run_hook ""
assert_allow "16a. empty stdin fails open"

run_hook "not json at all"
assert_allow "16b. unparseable stdin fails open"

run_hook '{"tool_name":"Edit","agent_id":'
assert_allow "16c. truncated JSON fails open"

# === edge case 17: no recognizable target ===================================
run_hook "$(make_payload "ag_1" "orchestra:orchestra-standard" "$ISO" "Write" "" "" "" "$SESSION")"
assert_allow "17a. Write with no file_path"

run_hook "$(make_payload "ag_1" "orchestra:orchestra-standard" "$ISO" "Read" "$MAIN/src/app.ts" "" "" "$SESSION")"
assert_allow "17b. a tool outside the matcher is out of scope"

# === the supervising layer is exempt ========================================
run_hook "$(make_payload "ag_2" "orchestra:orchestra-delegate" "$ISO" "Edit" "$MAIN/src/app.ts" "" "" "$SESSION")"
assert_allow "18. orchestra-delegate integrates into the main tree by design"

# === a generic subagent is still a worker ===================================
run_hook "$(make_payload "ag_9" "general-purpose" "$ISO" "Edit" "$MAIN/src/app.ts" "" "" "$SESSION")"
assert_deny "19. a generic subagent is still a worker"

echo "----------------------------------------"
echo "PASS: $PASS  FAIL: $FAIL"
rm -rf "$STUB_DIR" "$REPO_BASE" "$NON_REPO"
[ "$FAIL" -eq 0 ]
