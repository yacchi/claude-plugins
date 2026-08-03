#!/bin/bash
# Self-contained test suite for guard-worker-vcs.sh.
#
# Requires only bash + python3 + git. Stubs `agent-exec` via a temp dir
# prepended to PATH so these tests never depend on a real orchestra install.
#
# What this guards (see the hook header): a *worker* running `git checkout --`
# / `git restore` / `git reset --hard` against the user's shared working tree.
# Transcript analysis found 35 such commands across 7 weeks, from every model
# tier, despite three escalating generations of prose prohibition in the worker
# prompts. The tests below pin the four things that make a mechanism usable
# where prose failed: it must catch the real accident, it must never touch the
# main thread, it must let an isolated worker manage its own worktree, and it
# must have escape hatches that work.
#
# Usage: bash test-guard-worker-vcs.sh

set -u

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
HOOK="$SCRIPT_DIR/guard-worker-vcs.sh"

PASS=0
FAIL=0

STUB_DIR=$(mktemp -d "${TMPDIR:-/tmp}/guard-vcs-stub.XXXXXX")
cat > "$STUB_DIR/agent-exec" <<'STUBEOF'
#!/bin/bash
echo "$*" >> "${STUB_LOG:-/dev/null}"
case "$1" in
    config)
        printf '{"enforcement":{"worker_vcs":"%s"}}\n' "${STUB_WORKER_VCS:-block}"
        exit 0
        ;;
    *)
        exit 1
        ;;
esac
STUBEOF
chmod +x "$STUB_DIR/agent-exec"

# --- repos ------------------------------------------------------------------
# SHARED_REPO stands in for the user's tree; ISO_REPO for a worktree orchestra
# created (branch under orchestra/), where a worker cleaning up is harmless.
REPO_BASE=$(mktemp -d "${TMPDIR:-/tmp}/guard-vcs-repo.XXXXXX")
SHARED_REPO="$REPO_BASE/shared"
mkdir -p "$SHARED_REPO"
(
    cd "$SHARED_REPO" || exit 1
    git init -q -b main . && git config user.email t@e.com && git config user.name T
    echo hi > README.md && git add -A && git commit -q -m init
    git worktree add -q -b orchestra/t1 "$REPO_BASE/iso" HEAD
) >/dev/null 2>&1
ISO_REPO="$REPO_BASE/iso"
NON_REPO=$(mktemp -d "${TMPDIR:-/tmp}/guard-vcs-norepo.XXXXXX")

# --- payload builder ---------------------------------------------------------
# agent_id/agent_type are the documented PreToolUse fields that identify a
# subagent call; passing an empty agent_id omits it, i.e. the main thread.
make_payload() {
    python3 -c '
import sys, json
agent_id, agent_type, cwd, command, description = sys.argv[1:6]
payload = {
    "session_id": "s1",
    "hook_event_name": "PreToolUse",
    "tool_name": sys.argv[6] if len(sys.argv) > 6 else "Bash",
    "cwd": cwd,
    "tool_input": {"command": command, "description": description},
}
if agent_id:
    payload["agent_id"] = agent_id
    payload["agent_type"] = agent_type
sys.stdout.write(json.dumps(payload))
' "$@"
}

run_hook() {
    local payload="$1"
    shift
    local tmpdir
    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/guard-vcs-run.XXXXXX")
    OUT=$(printf '%s' "$payload" | env -i \
        PATH="$STUB_DIR:/usr/bin:/bin:/usr/local/bin" \
        TMPDIR="$tmpdir" HOME="$HOME" STUB_LOG="$tmpdir/agent-exec.log" \
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

worker() { make_payload "ag_1" "orchestra:orchestra-light" "$SHARED_REPO" "$1" "${2:-do work}"; }

# --- the accident this exists to prevent ------------------------------------
run_hook "$(worker 'git checkout -- src/app.ts')"
assert_deny "1. worker: git checkout -- <path> on the shared tree"

run_hook "$(worker 'git restore src/a.ts src/b.ts')"
assert_deny "2. worker: git restore on the shared tree"

run_hook "$(worker 'git reset --hard HEAD')"
assert_deny "3. worker: git reset --hard on the shared tree"

run_hook "$(worker 'git checkout .')"
assert_deny "4. worker: git checkout ."

run_hook "$(worker 'git clean -fd')"
assert_deny "5. worker: git clean -fd"

run_hook "$(worker 'git stash')"
assert_deny "6. worker: git stash (rolls the tree back)"

run_hook "$(worker 'git checkout --ours -- pkg/go.mod')"
assert_deny "7. worker: git checkout --ours"

run_hook "$(worker 'cd /tmp && npm test; git checkout -- .')"
assert_deny "8. worker: destructive command later in a compound line"

run_hook "$(worker 'git -C '"$SHARED_REPO"' restore src/a.ts')"
assert_deny "9. worker: git -C <path> restore"

run_hook "$(worker 'git reset --keep origin/main')"
assert_deny "10. worker: git reset --keep"

# --- must never touch the main thread ---------------------------------------
run_hook "$(make_payload "" "" "$SHARED_REPO" 'git checkout -- src/app.ts' 'instructor fixing a conflict')"
assert_allow "11. main thread: destructive git is the instructor's business"

run_hook "$(make_payload "" "" "$SHARED_REPO" 'git reset --hard backup/pre-rebase' 'rebase recovery')"
assert_allow "12. main thread: reset --hard allowed"

# --- an isolated worker owns its own tree ------------------------------------
run_hook "$(make_payload "ag_1" "orchestra:orchestra-light" "$ISO_REPO" 'git checkout -- README.md' 'undo my own edit')"
assert_allow "13. worker inside an orchestra worktree may manage it"

# --- the supervising layer is allowed to snapshot/roll back ------------------
run_hook "$(make_payload "ag_2" "orchestra:orchestra-delegate" "$SHARED_REPO" 'git reset --hard HEAD' 'roll back a failed attempt')"
assert_allow "14. orchestra-delegate is the supervisor, not a worker"

# --- non-destructive git must not be caught ---------------------------------
run_hook "$(worker 'git status --porcelain')"
assert_allow "15. worker: git status"

run_hook "$(worker 'git diff HEAD')"
assert_allow "16. worker: git diff"

run_hook "$(worker 'git checkout -b feature/x')"
assert_allow "17. worker: git checkout -b (creates, destroys nothing)"

run_hook "$(worker 'git reset HEAD src/a.ts')"
assert_allow "18. worker: mixed reset touches the index only"

run_hook "$(worker 'git restore --staged src/a.ts')"
assert_allow "19. worker: restore --staged touches the index only"

run_hook "$(worker 'git stash list')"
assert_allow "20. worker: git stash list is read-only"

run_hook "$(worker 'git stash pop')"
assert_allow "21. worker: git stash pop restores rather than discards"

run_hook "$(worker 'npm test -- --watch=false')"
assert_allow "22. worker: an unrelated command containing --"

# --- prompt text that merely mentions the commands ---------------------------
run_hook "$(worker "$(printf 'cat > /tmp/task.txt <<EOF\nNever run git checkout -- or git reset --hard.\nEOF')")"
assert_allow "23. worker: heredoc prompt text is not an invocation"

run_hook "$(worker 'echo "do not use git reset --hard"')"
assert_allow "24. worker: echoing the command is not running it"

# --- escape hatches ----------------------------------------------------------
run_hook "$(worker 'git checkout -- src/app.ts [orchestra:allow-vcs]')"
assert_allow "25. escape marker in the command"

run_hook "$(make_payload "ag_1" "orchestra:orchestra-light" "$SHARED_REPO" 'git reset --hard HEAD' 'cleanup [orchestra:allow-vcs: user asked]')"
assert_allow "26. escape marker with a reason in the description"

run_hook "$(worker 'git reset --hard HEAD')" ORCHESTRA_VCS_GUARD=off
assert_allow "27. kill switch: ORCHESTRA_VCS_GUARD=off"

run_hook "$(worker 'git reset --hard HEAD')" STUB_WORKER_VCS=off
assert_allow "28. config: enforcement.worker_vcs=off"

# --- fail-open ---------------------------------------------------------------
run_hook ""
assert_allow "29. empty stdin fails open"

run_hook "not json at all"
assert_allow "30. unparseable stdin fails open"

run_hook "$(make_payload "ag_1" "orchestra:orchestra-light" "$NON_REPO" 'git reset --hard HEAD' 'x')"
assert_allow "31. outside a repository there is nothing to protect"

run_hook "$(make_payload "ag_1" "orchestra:orchestra-light" "$SHARED_REPO" 'git reset --hard HEAD' 'x' "Read")"
assert_allow "32. non-Bash tools are out of scope"

# --- other subagents are covered too (the 7/23-7/28 accidents were generic) --
run_hook "$(make_payload "ag_9" "general-purpose" "$SHARED_REPO" 'git checkout -- go.sum' 'tidy up')"
assert_deny "33. a generic subagent is still a worker"

echo "----------------------------------------"
echo "PASS: $PASS  FAIL: $FAIL"
rm -rf "$STUB_DIR" "$REPO_BASE" "$NON_REPO"
[ "$FAIL" -eq 0 ]
