#!/bin/bash
# Self-contained test suite for guard-worktree-lease.sh.
#
# Requires only bash + python3 + git. Stubs `agent-exec` via a temp dir
# prepended to PATH, and points TMPDIR at a scratch dir so lease indexes never
# land in the real temp directory.
#
# What this guards (see the hook header): two agents writing one worktree at
# the same time -- observed when a message to a running workflow agent resumed
# a copy of it and both committed to the same tree seconds apart.
#
# Usage: bash test-guard-worktree-lease.sh

set -u

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
HOOK="$SCRIPT_DIR/guard-worktree-lease.sh"

PASS=0
FAIL=0

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/guard-lease.XXXXXX")
export TMPDIR="$SCRATCH/tmp"
mkdir -p "$TMPDIR" "$SCRATCH/stub"
cat > "$SCRATCH/stub/agent-exec" <<'STUBEOF'
#!/bin/bash
case "$1" in
    config)
        printf '{"enforcement":{"worktree_lease":"%s"}}\n' "${STUB_LEASE:-block}"
        exit 0
        ;;
    *) exit 1 ;;
esac
STUBEOF
chmod +x "$SCRATCH/stub/agent-exec"
export PATH="$SCRATCH/stub:$PATH"

REPO="$SCRATCH/repo"
(
    mkdir -p "$REPO" && cd "$REPO" || exit 1
    git init -q -b main . && git config user.email t@e.com && git config user.name T
    echo hi > README.md && git add -A && git commit -q -m init
    git worktree add -q -b orchestra/s1/t1 "$SCRATCH/wt1" HEAD
    git worktree add -q -b improve/t2 "$SCRATCH/wt2" HEAD
) >/dev/null 2>&1
WT1="$SCRATCH/wt1"
WT2="$SCRATCH/wt2"

payload() {
    # payload <agent_id> <agent_type> <cwd> <tool> <path-or-command> [description]
    python3 -c '
import sys, json
agent_id, agent_type, cwd, tool, arg = sys.argv[1:6]
desc = sys.argv[6] if len(sys.argv) > 6 else ""
p = {"session_id": "s1", "hook_event_name": "PreToolUse", "tool_name": tool, "cwd": cwd}
p["tool_input"] = {"command": arg, "description": desc} if tool == "Bash" else {"file_path": arg}
if agent_id:
    p["agent_id"] = agent_id
    p["agent_type"] = agent_type
print(json.dumps(p))
' "$@"
}

stop_payload() {
    python3 -c '
import sys, json
print(json.dumps({"session_id": "s1", "hook_event_name": "SubagentStop", "agent_id": sys.argv[1], "agent_type": "x", "cwd": sys.argv[2]}))
' "$@"
}

run_hook() {
    local input="$1"; shift
    OUT=$(printf '%s' "$input" | env "$@" bash "$HOOK" 2>/dev/null)
    RC=$?
}

release() {
    printf '%s' "$(stop_payload "$1" "$REPO")" | bash "$HOOK" --release >/dev/null 2>&1
}

assert_allow() {
    if [ "$RC" -ne 0 ]; then echo "FAIL: $1 -- exit $RC"; FAIL=$((FAIL + 1)); return; fi
    if [ -n "$OUT" ]; then echo "FAIL: $1 -- expected allow, got: $OUT"; FAIL=$((FAIL + 1)); return; fi
    echo "PASS: $1"; PASS=$((PASS + 1))
}

assert_deny() {
    if [ "$RC" -ne 0 ]; then echo "FAIL: $1 -- exit $RC"; FAIL=$((FAIL + 1)); return; fi
    if ! printf '%s' "$OUT" | grep -q '"permissionDecision": *"deny"'; then
        echo "FAIL: $1 -- expected deny, got: $OUT"; FAIL=$((FAIL + 1)); return
    fi
    echo "PASS: $1"; PASS=$((PASS + 1))
}

# --- the accident ------------------------------------------------------------
run_hook "$(payload ag_A orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")"
assert_allow "1. first writer takes the lease"

run_hook "$(payload ag_B orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")"
assert_deny "2. a second agent writing the same worktree is denied"
case "$OUT" in
    *DUPLICATE*) echo "PASS: 2b. denial tells a duplicate to stop"; PASS=$((PASS + 1)) ;;
    *) echo "FAIL: 2b. denial tells a duplicate to stop -- got: $OUT"; FAIL=$((FAIL + 1)) ;;
esac

run_hook "$(payload ag_B orchestra:orchestra-light "$WT1" Bash 'git commit -qm dup')"
assert_deny "3. ...and so is its commit"

run_hook "$(payload ag_B general-purpose "$REPO" Bash "cd $WT1 && git add -A")"
assert_deny "4. cd into the worktree then git add is still a write there"

run_hook "$(payload ag_A orchestra:orchestra-light "$WT1" Edit "$WT1/b.ts")"
assert_allow "5. the holder keeps writing"

# --- what is not a write ------------------------------------------------------
run_hook "$(payload ag_B orchestra:orchestra-review "$WT1" Bash 'git diff HEAD~1 && npm test')"
assert_allow "6. a reviewer reading and testing needs no lease"

run_hook "$(payload ag_B orchestra:orchestra-light "$WT1" Bash "git -C $WT1 status")"
assert_allow "7. read-only git is not a write"

# --- sequential rounds ----------------------------------------------------------
release ag_A
run_hook "$(payload ag_B orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")"
assert_allow "8. once the holder stops (SubagentStop), the next round writes freely"

run_hook "$(payload ag_C orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")" ORCHESTRA_LEASE_WINDOW=1
assert_deny "9. an active holder blocks within a short window too"
sleep 2
run_hook "$(payload ag_C orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")" ORCHESTRA_LEASE_WINDOW=1
assert_allow "10. a holder silent past the window lost its lease (died without SubagentStop)"

run_hook "$(payload ag_B orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")"
assert_deny "11. ...and the old holder coming back is now the one denied"

# --- scope --------------------------------------------------------------------
run_hook "$(payload ag_D orchestra:orchestra-light "$WT2" Edit "$WT2/x.ts")"
assert_allow "12. a different worktree has its own lease (any branch name)"

run_hook "$(payload ag_E orchestra:orchestra-light "$REPO" Edit "$REPO/a.ts")"
assert_allow "13. main worktree: first agent"
run_hook "$(payload ag_F orchestra:orchestra-light "$REPO" Edit "$REPO/b.ts")"
assert_allow "14. main worktree is never leased (same-tree parallel work is supported)"

run_hook "$(payload '' '' "$WT1" Edit "$WT1/a.ts")"
assert_allow "15. the main thread is never affected"

run_hook "$(payload ag_G orchestra:orchestra-delegate "$WT1" Bash 'git commit -qm integrate')"
assert_allow "16. orchestra-delegate is the supervising layer"

run_hook "$(payload ag_G orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")"
assert_deny "17. another worker is still denied"

run_hook "$(payload ag_G orchestra:orchestra-light "$WT1" Bash 'git commit -qm x' 'take over [orchestra:allow-lease: holder confirmed dead]')"
assert_allow "18. escape hatch"

# --- switches and fail-open --------------------------------------------------
run_hook "$(payload ag_H orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")" ORCHESTRA_LEASE_GUARD=off
assert_allow "19. kill switch: ORCHESTRA_LEASE_GUARD=off"

run_hook "$(payload ag_H orchestra:orchestra-light "$WT1" Edit "$WT1/a.ts")" STUB_LEASE=off
assert_allow "20. config: enforcement.worktree_lease=off"

run_hook "not json"
assert_allow "21. unparseable stdin fails open"

run_hook "$(payload ag_H orchestra:orchestra-light "$SCRATCH" Edit "$SCRATCH/notrepo/x.ts")"
assert_allow "22. outside a repository there is nothing to lease"

release ag_zzz
if [ -z "$(ls "$TMPDIR" | grep orchestra-leases-ag_zzz)" ]; then
    echo "PASS: 23. releasing an agent with no leases is a no-op"; PASS=$((PASS + 1))
else
    echo "FAIL: 23. releasing an agent with no leases is a no-op"; FAIL=$((FAIL + 1))
fi

if [ -z "$(git -C "$WT1" status --porcelain)" ]; then
    echo "PASS: 24. the lease never appears in the worktree"; PASS=$((PASS + 1))
else
    echo "FAIL: 24. the lease never appears in the worktree -- $(git -C "$WT1" status --porcelain)"; FAIL=$((FAIL + 1))
fi

echo "----------------------------------------"
echo "PASS: $PASS  FAIL: $FAIL"
rm -rf "$SCRATCH"
[ "$FAIL" -eq 0 ]
