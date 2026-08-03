#!/bin/bash
# orchestra plugin - PreToolUse guard: workers must not run destructive VCS
# commands against the user's shared working tree.
#
# WHY. Seven weeks of transcripts contain 35 destructive VCS commands
# (`git checkout --`, `git restore`, `git reset --hard`, `git clean -fd`) run
# by *workers* -- spread across haiku, sonnet, and opus tiers alike, so this is
# not a "cheap model misunderstands" problem. The instructor's countermeasure
# had been prose, escalated three generations deep in the worker prompts:
#
#   (1) "Never run git checkout, git restore, git stash, git clean or git reset
#        on any path."
#   (2) "... The ONE exception is spelled out ..."
#   (3) "THE WORKING TREE IS ALREADY DIRTY AND THAT IS EXPECTED AND CORRECT.
#        It contains accepted, deliberate changes from other tasks ..."
#
# It kept happening. A worker's only view of the world is `git status`; a diff
# it did not author reads as contamination whatever the prompt says. So the
# real fix is worktree isolation (`agent-exec isolate`), which removes the
# other tasks' changes from view entirely. This hook is the second layer, for
# the cases isolation does not cover: a run that chose not to isolate, a
# non-repo checkout, or a worker that wanders outside its worktree.
#
# SCOPE -- deny only when ALL hold:
#   - tool is Bash, and the call comes from a subagent (`agent_id` present;
#     documented PreToolUse field). The main thread is never touched: an
#     instructor resolving a rebase conflict with `git checkout --ours` is
#     doing its job.
#   - the command actually *invokes* a destructive git operation. Text that
#     merely mentions one (the prohibition prose above, written into a worker
#     prompt via heredoc) is not an invocation.
#   - the cwd is a git repository whose branch is NOT one of orchestra's own
#     worktree branches (`orchestra/*`). Inside its own worktree a worker may
#     freely reset: there is nothing there but its own work.
#   - `agent_type` is not the supervising layer. `orchestra-delegate` owns
#     snapshots and rollbacks by design (references/isolation.md §1).
#   - no escape hatch fired (below).
#
# ESCAPE HATCHES, all independent:
#   1. `[orchestra:allow-vcs]` or `[orchestra:allow-vcs: <reason>]` anywhere in
#      the command or description.
#   2. `ORCHESTRA_VCS_GUARD=off` in the environment.
#   3. `enforcement.worker_vcs: "off"` in orchestra config.
#   4. Fail-open on everything unexpected: no python3, no git, unparseable
#      stdin, a config lookup that fails or times out. A PreToolUse hook that
#      errors would disrupt every Bash call in every session, so every branch
#      below exits 0.
#
# COST. This fires on every Bash call, so the common path must be cheap: one
# python3 parse decides everything textual, and a call that is not a
# destructive git invocation from a subagent returns before git or agent-exec
# is ever consulted. The config lookup is cached per session, same pattern as
# inject-router.sh and enforce-router.sh.

if [ "${ORCHESTRA_VCS_GUARD:-}" = "off" ]; then
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
    tool_name = s(data.get("tool_name"))
    tool_input = data.get("tool_input")
    tool_input = tool_input if isinstance(tool_input, dict) else {}
    command = s(tool_input.get("command"))
    description = s(tool_input.get("description"))
    agent_id = s(data.get("agent_id"))
    agent_type = s(data.get("agent_type"))
    cwd = s(data.get("cwd"))
    session_id = re.sub(r"[^A-Za-z0-9_-]", "", s(data.get("session_id")))

    haystack = (command + "\n" + description).lower()
    escape = "1" if re.search(r"\[orchestra:allow-vcs(?::[^\]]*)?\]", haystack) else "0"

    # Strip heredoc bodies before looking for invocations: the worker prompts
    # that *forbid* these commands are themselves written with
    # `cat > task.txt <<EOF ... EOF`, and a naive scan flags every one of them.
    body = re.sub(r"<<-?\s*[\x27\"]?(\w+)[\x27\"]?[\s\S]*?(?:\n\1\b|$)", " ", command)
    # Quoted strings are data, not invocations (`echo "git reset --hard"`).
    body = re.sub(r"\x27[^\x27]*\x27|\"[^\"]*\"", " ", body)

    def destructive(seg):
        seg = seg.strip()
        # `git`, possibly with global options (-C <path>, --git-dir=..., -c k=v)
        m = re.match(r"^git\b((?:\s+(?:-C\s+\S+|--git-dir=\S+|--work-tree=\S+|-c\s+\S+))*)\s+(\S+)(.*)$", seg)
        if not m:
            return False
        verb, rest = m.group(2), m.group(3)
        words = rest.split()
        if verb == "checkout":
            # -b/-B create a branch; a bare branch switch refuses to clobber.
            # What destroys work is a pathspec restore or a forced switch.
            if any(w in ("-b", "-B") for w in words):
                return False
            return ("--" in words or "." in words
                    or any(w in ("-f", "--force", "--ours", "--theirs") for w in words))
        if verb == "restore":
            # --staged alone only rewrites the index; pairing it with
            # --worktree/-W puts the working tree back in scope.
            if "--staged" in words and not any(w in ("--worktree", "-W") for w in words):
                return False
            return True
        if verb == "reset":
            return any(w in ("--hard", "--merge", "--keep") for w in words)
        if verb == "clean":
            return any(re.match(r"^-[A-Za-z]*f", w) for w in words)
        if verb == "stash":
            sub = words[0] if words else "push"
            return sub in ("push", "save", "create", "store") or sub.startswith("-")
        if verb == "worktree":
            return len(words) >= 1 and words[0] == "remove" and (
                "-f" in words or "--force" in words
            )
        return False

    segments = re.split(r"(?:\|\||&&|[;&|\n])", body)
    hit = "1" if any(destructive(seg) for seg in segments) else "0"

    print(tool_name)
    print(agent_id)
    print(agent_type)
    print(cwd)
    print(escape)
    print(hit)
    print(session_id)
except Exception:
    pass
' 2>/dev/null) || PARSED=""

TOOL_NAME=$(printf '%s' "$PARSED" | sed -n '1p')
AGENT_ID=$(printf '%s' "$PARSED" | sed -n '2p')
AGENT_TYPE=$(printf '%s' "$PARSED" | sed -n '3p')
CWD=$(printf '%s' "$PARSED" | sed -n '4p')
ESCAPE=$(printf '%s' "$PARSED" | sed -n '5p')
DESTRUCTIVE=$(printf '%s' "$PARSED" | sed -n '6p')
SESSION_ID=$(printf '%s' "$PARSED" | sed -n '7p')

# Fail-open: an unparseable payload leaves every field empty.
[ "$TOOL_NAME" = "Bash" ] || exit 0
[ "$DESTRUCTIVE" = "1" ] || exit 0
[ -n "$AGENT_ID" ] || exit 0          # main thread: not our business
[ "$ESCAPE" = "1" ] && exit 0

# The supervising layer owns snapshots, rollbacks, and merges by design.
case "$(printf '%s' "$AGENT_TYPE" | tr '[:upper:]' '[:lower:]')" in
    *orchestra-delegate*) exit 0 ;;
esac

command -v git >/dev/null 2>&1 || exit 0
[ -n "$CWD" ] && [ -d "$CWD" ] || exit 0

BRANCH=$(cd "$CWD" 2>/dev/null && git rev-parse --abbrev-ref HEAD 2>/dev/null) || BRANCH=""
if [ -z "$BRANCH" ]; then
    # Not a repository (or a detached/unknown state we cannot reason about):
    # there is no shared working tree to protect here.
    exit 0
fi
case "$BRANCH" in
    orchestra/*) exit 0 ;;   # the worker's own isolated worktree
esac

# --- config, cached per session ---------------------------------------------
WORKER_VCS=""
CACHE_FILE="${TMPDIR:-/tmp}/orchestra-guard-vcs-${SESSION_ID:-nosession}"
if [ -r "$CACHE_FILE" ]; then
    WORKER_VCS=$(cat "$CACHE_FILE" 2>/dev/null) || WORKER_VCS=""
fi

if [ -z "$WORKER_VCS" ]; then
    # Default to blocking: this hook only exists inside an orchestra install,
    # and preventing the accident is the point.
    WORKER_VCS="block"
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
        v = e.get("worker_vcs")
        if isinstance(v, str) and v in ("off", "block"):
            sys.stdout.write(v)
except Exception:
    pass
' 2>/dev/null) || CFG=""
            [ -n "$CFG" ] && WORKER_VCS="$CFG"
        fi
    fi
    printf '%s' "$WORKER_VCS" > "$CACHE_FILE" 2>/dev/null || true
fi

[ "$WORKER_VCS" = "block" ] || exit 0

REASON="orchestra: a worker must not run destructive VCS commands against the user's shared working tree (branch '${BRANCH}'). This tree holds work from other tasks and from the user; a diff you did not author is not contamination and must not be reverted, stashed, or reset. If your task genuinely needs to undo something, undo only what YOU wrote, by editing the file back. If you need a tree of your own, ask the supervisor to dispatch you with 'agent-exec dispatch --isolate always --task <id>', which gives you a worktree where this restriction does not apply. To override deliberately, include [orchestra:allow-vcs: <reason>] in the command or its description."

python3 - "$REASON" <<'PYEOF' 2>/dev/null
import sys, json
sys.stdout.write(json.dumps({
    "hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "deny",
        "permissionDecisionReason": sys.argv[1],
    }
}))
PYEOF

exit 0
