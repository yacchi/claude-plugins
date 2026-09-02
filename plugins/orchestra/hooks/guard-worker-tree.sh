#!/bin/bash
# orchestra plugin - PreToolUse guard: a worker subagent must not write into
# the user's MAIN working tree while it has an isolated worktree of its own.
#
# WHY. A Sonnet worker was spawned with its prompt beginning:
#
#   WORKING TREE: <path>
#   cd there FIRST and do every read, edit, test, and command inside it.
#   Never touch ... files outside that path
#
# It ignored the line entirely and ran Edit/Write against the SAME files in the
# user's main tree. Its own worktree existed and was writable -- the correction
# round used it correctly -- so this was not a setup failure. The rejected
# round-1 output sat in the user's tree and would have been what the user kept.
#
# Prose does not constrain a worker; only a mechanism does. Claude Code's own
# `sandbox.enabled` cannot fix this either: it is session-global (subagents
# share the parent's sandbox config) and it covers only the Bash tool, not
# Edit/Write. Hence a PreToolUse hook, the same shape as guard-worker-vcs.sh.
#
# The rule is narrow on purpose: a worker has no legitimate reason to write to
# the main tree. That is the instructor's job, and the instructor has no
# `agent_id`.
#
# SCOPE -- deny only when ALL hold:
#   1. `agent_id` is non-empty (the call comes from inside a subagent). The main
#      thread is never touched.
#   2. `agent_type` is not `orchestra-delegate` -- the supervising layer owns
#      collection and integration into the main tree by design.
#   3. The session has at least one orchestra worktree: a `git worktree list
#      --porcelain` entry (run against the hook input's `cwd`) whose branch
#      matches `orchestra/<session8>/*`, `<session8>` being the first 8
#      characters of `session_id`, lowercased. With no worktree there is nowhere
#      else for the worker to write, and the express lane is untouched.
#   4. The write target's realpath lies INSIDE the main working tree (the FIRST
#      entry `git worktree list --porcelain` prints) and inside no orchestra
#      worktree. Comparison is by path components after realpath, so a symlink,
#      a `..` escape, and the sibling-prefix trap (`/repo-worktrees/x` is NOT
#      inside `/repo`) all resolve correctly.
#
# WRITE-TARGET COVERAGE IS DELIBERATELY ASYMMETRIC.
#   Structured tools (Edit/Write/MultiEdit/NotebookEdit) -- full coverage, zero
#   parsing: `tool_input.file_path`, plus `tool_input.notebook_path` for
#   NotebookEdit. A relative path resolves against `cwd`. This is where the
#   observed incident happened and it is airtight.
#   Bash -- narrow ON PURPOSE. Parsing arbitrary shell for write targets has an
#   unacceptable false-positive cost, so only three high-signal shapes are
#   covered, after the heredoc/quoted-string stripping guard-worker-vcs.sh uses:
#     - output redirection: `> PATH`, `>> PATH`
#     - `tee [flags] PATH...`
#     - `sed -i ... PATH...` (GNU and BSD `-i ''` forms; only tokens that
#       already exist as files count, which is what separates a path from an
#       unquoted `s/a/b/` script)
#   EVERYTHING ELSE IN A BASH COMMAND FAILS OPEN BY DESIGN. `cp`, `mv`, `rm`,
#   `mkdir`, `python -c`, and every other interpreter invocation are NOT
#   analyzed. This is not an oversight -- do not "fix" it. A token carrying an
#   unexpanded `$VAR`, a backtick, or a glob is skipped for the same reason:
#   guessing what it expands to would deny legitimate work.
#
# ESCAPE HATCHES, all independent:
#   1. `[orchestra:allow-tree]` or `[orchestra:allow-tree: <reason>]` anywhere in
#      the command or its description (Bash-shaped calls; structured tools carry
#      no such field, so use 2 or 3 there).
#   2. `ORCHESTRA_TREE_GUARD=off` in the environment.
#   3. `enforcement.worker_tree: "off"` in orchestra config.
#   4. Fail-open on everything unexpected: no python3, no git, unparseable
#      stdin, `git worktree list` failing, `cwd` not in a repo, an unresolvable
#      path, a config lookup that fails or times out. A guard that blocks
#      legitimate work is worse than one that misses a case, so every branch
#      below exits 0 and nothing is ever written to stderr on the allow path.

if [ "${ORCHESTRA_TREE_GUARD:-}" = "off" ]; then
    exit 0
fi

INPUT=$(cat 2>/dev/null || true)

if ! command -v python3 >/dev/null 2>&1; then
    exit 0
fi

# Pass 1 -- textual only. Lines 1-7 are fixed fields; line 8 onward are the
# candidate write targets, unresolved, one per line (a path containing a
# newline is dropped: fail open rather than mis-parse).
PARSED=$(printf '%s' "$INPUT" | python3 -c '
import sys, json, os, re

STRUCTURED = ("Edit", "Write", "MultiEdit", "NotebookEdit")

def s(v):
    return v if isinstance(v, str) else ""

def unexpanded(tok):
    # `$VAR`, backticks and globs would have to be guessed at; skip them.
    return bool(re.search(r"[$`*?\[\]{}]", tok))

def bash_targets(command):
    # Heredoc bodies and quoted strings are data, not invocations -- the worker
    # prompts that *forbid* writing to the main tree are themselves written
    # with `cat > task.md <<EOF ... EOF`, and a naive scan flags every one.
    body = re.sub(r"<<-?\s*[\x27\"]?(\w+)[\x27\"]?[\s\S]*?(?:\n\1\b|$)", " ", command)
    body = re.sub(r"\x27[^\x27]*\x27|\"[^\"]*\"", " ", body)

    out = []
    for seg in re.split(r"(?:\|\||&&|[;&|\n])", body):
        seg = seg.strip()
        if not seg:
            continue

        # 1. output redirection
        for m in re.finditer(r"(?:^|\s)[0-9]*>>?\s*([^\s;&|<>()]+)", seg):
            tok = m.group(1)
            if tok and not tok.startswith("&"):
                out.append("A:" + tok)

        words = seg.split()
        if not words:
            continue

        # 2. tee [flags] PATH...
        for i, w in enumerate(words):
            if os.path.basename(w) == "tee":
                for tok in words[i + 1:]:
                    if tok == "--":
                        continue
                    if tok.startswith("-"):
                        continue
                    out.append("A:" + tok)
                break

        # 3. sed -i ... PATH... -- only tokens that already exist as files, which
        # is what separates a real path from an unquoted `s/a/b/` script.
        for i, w in enumerate(words):
            if os.path.basename(w) == "sed":
                rest = words[i + 1:]
                if not any(t == "-i" or t.startswith("-i") or t == "--in-place"
                           or t.startswith("--in-place") for t in rest):
                    break
                for tok in rest:
                    if tok.startswith("-"):
                        continue
                    out.append("E:" + tok)   # E: must already exist
                break
    return out

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
    raw_session = s(data.get("session_id"))
    session_id = re.sub(r"[^A-Za-z0-9_-]", "", raw_session)
    session8 = re.sub(r"[^a-z0-9-]", "", raw_session.lower()[:8])

    haystack = (command + "\n" + description).lower()
    escape = "1" if re.search(r"\[orchestra:allow-tree(?::[^\]]*)?\]", haystack) else "0"

    targets = []
    if tool_name in STRUCTURED:
        for key in ("file_path", "notebook_path"):
            v = s(tool_input.get(key))
            if v:
                targets.append("A:" + v)
    elif tool_name == "Bash":
        targets = [t for t in bash_targets(command) if not unexpanded(t[2:])]

    print(tool_name)
    print(agent_id)
    print(agent_type)
    print(cwd)
    print(escape)
    print(session8)
    print(session_id)
    for t in targets:
        if "\n" not in t and "\r" not in t:
            print(t)
except Exception:
    pass
' 2>/dev/null) || PARSED=""

TOOL_NAME=$(printf '%s' "$PARSED" | sed -n '1p')
AGENT_ID=$(printf '%s' "$PARSED" | sed -n '2p')
AGENT_TYPE=$(printf '%s' "$PARSED" | sed -n '3p')
CWD=$(printf '%s' "$PARSED" | sed -n '4p')
ESCAPE=$(printf '%s' "$PARSED" | sed -n '5p')
SESSION8=$(printf '%s' "$PARSED" | sed -n '6p')
SESSION_ID=$(printf '%s' "$PARSED" | sed -n '7p')
TARGETS=$(printf '%s' "$PARSED" | sed -n '8,$p')

TARGET_LIST=()
while IFS= read -r _line; do
    [ -n "$_line" ] && TARGET_LIST+=("$_line")
done < <(printf '%s\n' "$TARGETS")

# Fail-open: an unparseable payload leaves every field empty.
[ -n "$TOOL_NAME" ] || exit 0
[ -n "$TARGETS" ] || exit 0            # nothing recognizable to check
[ -n "$AGENT_ID" ] || exit 0           # main thread: the instructor's own tree
[ "$ESCAPE" = "1" ] && exit 0

# The supervising layer collects and integrates worker output into the main
# tree by design (references/isolation.md).
case "$(printf '%s' "$AGENT_TYPE" | tr '[:upper:]' '[:lower:]')" in
    *orchestra-delegate*) exit 0 ;;
esac

command -v git >/dev/null 2>&1 || exit 0
[ -n "$CWD" ] && [ -d "$CWD" ] || exit 0
[ -n "$SESSION8" ] || exit 0

WORKTREES=$(cd "$CWD" 2>/dev/null && git worktree list --porcelain 2>/dev/null) || WORKTREES=""
[ -n "$WORKTREES" ] || exit 0          # not a repo, or git refused: nothing to protect

# Pass 2 -- resolve and compare. Prints the offending path, or nothing.
OFFENDER=$(printf '%s' "$WORKTREES" | python3 -c '
import sys, os

session8, cwd = sys.argv[1], sys.argv[2]
targets = sys.argv[3:]

def components(p):
    return os.path.realpath(p).split(os.sep)

def inside(child, parent):
    c, p = components(child), components(parent)
    return len(c) > len(p) and c[:len(p)] == p

try:
    trees = []          # [(path, branch)] in git order; main worktree is first
    path = None
    branch = ""
    for line in sys.stdin.read().splitlines():
        if line.startswith("worktree "):
            if path:
                trees.append((path, branch))
            path, branch = line[len("worktree "):], ""
        elif line.startswith("branch "):
            branch = line[len("branch "):]
    if path:
        trees.append((path, branch))

    if not trees:
        sys.exit(0)

    main_tree = trees[0][0]
    prefix = "refs/heads/orchestra/" + session8 + "/"
    orchestra = [p for p, b in trees[1:] if b.startswith(prefix)]
    if not orchestra:
        sys.exit(0)     # express lane: no isolated worktree for this session

    # Any orchestra worktree shields a path, not just this session-s: one nested
    # under the main tree must never read as a main-tree write.
    shields = [p for p, b in trees[1:] if b.startswith("refs/heads/orchestra/")]

    for raw in targets:
        must_exist = raw.startswith("E:")
        tok = raw[2:]
        if not tok:
            continue
        resolved = tok if os.path.isabs(tok) else os.path.join(cwd, tok)
        if must_exist and not os.path.exists(resolved):
            continue
        if not inside(resolved, main_tree):
            continue
        if any(inside(resolved, w) for w in shields):
            continue
        sys.stdout.write(os.path.realpath(resolved))
        break
except Exception:
    pass
' "$SESSION8" "$CWD" "${TARGET_LIST[@]}" 2>/dev/null) || OFFENDER=""

[ -n "$OFFENDER" ] || exit 0

# --- config, cached per session ---------------------------------------------
WORKER_TREE=""
CACHE_FILE="${TMPDIR:-/tmp}/orchestra-guard-tree-${SESSION_ID:-nosession}"
if [ -r "$CACHE_FILE" ]; then
    WORKER_TREE=$(cat "$CACHE_FILE" 2>/dev/null) || WORKER_TREE=""
fi

if [ -z "$WORKER_TREE" ]; then
    # Default to blocking: this hook only exists inside an orchestra install,
    # and preventing the accident is the point.
    WORKER_TREE="block"
    if command -v agent-exec >/dev/null 2>&1; then
        CONFIG_JSON=$(python3 -c '
import subprocess, sys
try:
    r = subprocess.run(["agent-exec", "config", "--json"], capture_output=True, timeout=5, text=True)
    if r.returncode == 0:
        sys.stdout.write(r.stdout)
        sys.exit(0)
    sys.exit(1)
except Exception:
    sys.exit(1)
' 2>/dev/null)
        CONFIG_STATUS=$?
        if [ "$CONFIG_STATUS" -ne 0 ]; then
            # The lookup itself failed (nonzero exit, timeout, exception) --
            # this is not "no config set", it is "we could not ask". Fail open
            # on this call and do not cache a decision, so a transient failure
            # does not lock the session into blocking for its whole duration.
            exit 0
        fi
        if [ -n "$CONFIG_JSON" ]; then
            CFG=$(printf '%s' "$CONFIG_JSON" | python3 -c '
import sys, json
try:
    e = json.load(sys.stdin).get("enforcement")
    if isinstance(e, dict):
        v = e.get("worker_tree")
        if isinstance(v, str) and v in ("off", "block"):
            sys.stdout.write(v)
except Exception:
    pass
' 2>/dev/null) || CFG=""
            [ -n "$CFG" ] && WORKER_TREE="$CFG"
        fi
    fi
    printf '%s' "$WORKER_TREE" > "$CACHE_FILE" 2>/dev/null || true
fi

[ "$WORKER_TREE" = "block" ] || exit 0

REASON="orchestra: this write targets the user's MAIN working tree ('${OFFENDER}'), and you are a worker with an isolated worktree of your own. Writing here is never a worker's job -- the instructor collects your diff and integrates it. Your prompt named a WORKING TREE path; cd there and redo this write against the same relative path inside it (git worktree list will show it if you lost it). If your task genuinely requires touching the main tree, hand that back to the instructor as part of your verdict instead. To override deliberately on a Bash call, include [orchestra:allow-tree: <reason>] in the command or its description."

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
