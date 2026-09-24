#!/bin/bash
# orchestra plugin - PreToolUse/SubagentStop guard: one writer per worktree.
#
# WHY. A worktree gives a task a tree of its own, but nothing stops a SECOND
# agent from being pointed at the same tree while the first is still in it.
# Observed in a programme-scale workflow run: the instructor sent a message to
# a package engineer that a Workflow had spawned; the message resumed a *copy*
# of that engineer, and the copy and the original both edited and committed
# in the same worktree -- the original's commit landed two seconds before the
# copy's commit of the same files. The same shape is available to any retry of
# an agent that looked stalled but was still running. Neither agent can tell
# from inside the tree that the other exists.
#
# MECHANISM. A lease per linked worktree, stored in that worktree's own git
# dir (`.git/worktrees/<name>/orchestra-lease.json`, never in the tree, never
# committed). The first subagent to WRITE into a linked worktree takes the
# lease; every later write by the same agent refreshes it. A write by a
# DIFFERENT subagent is denied while the holder was active within the lease
# window (default 120s). The holder's lease is dropped when it stops
# (SubagentStop, `--release`), so a correction round that starts right after
# the previous round finished is never blocked; the window is the fallback for
# a holder that died without a SubagentStop.
#
# WHAT COUNTS AS A WRITE: Edit/Write/MultiEdit/NotebookEdit targets, and Bash
# commands that run a state-changing git verb (commit, add, reset, checkout,
# merge, ...) or redirect output into a file. Reading, testing and `git diff`
# never take or need the lease, so a reviewer inspecting a worker's tree is
# unaffected. The main worktree is never leased: same-tree parallel work with
# disjoint ownership is a supported pattern there.
#
# SCOPE -- deny only when ALL hold: the call comes from a subagent (`agent_id`
# present); `agent_type` is not `orchestra-delegate` (the supervising layer);
# the target is inside a linked worktree whose lease another agent holds and
# refreshed within the window; no escape hatch fired.
#
# ESCAPE HATCHES, all independent:
#   1. `[orchestra:allow-lease]` / `[orchestra:allow-lease: <reason>]` in the
#      command or description.
#   2. `ORCHESTRA_LEASE_GUARD=off` in the environment.
#   3. `enforcement.worktree_lease: "off"` in orchestra config.
#   4. Fail-open on everything unexpected. Every path exits 0.
#
# Window: `ORCHESTRA_LEASE_WINDOW` (seconds) or
# `enforcement.worktree_lease_window` (seconds); default 120.

if [ "${ORCHESTRA_LEASE_GUARD:-}" = "off" ]; then
    exit 0
fi

INPUT=$(cat 2>/dev/null || true)
command -v python3 >/dev/null 2>&1 || exit 0
command -v git >/dev/null 2>&1 || exit 0

MODE="${1:-check}"

ORCH_INPUT="$INPUT" ORCH_MODE="$MODE" python3 - <<'PYEOF' 2>/dev/null
import json, os, re, subprocess, sys, time

def s(v):
    return v if isinstance(v, str) else ""

def tmpdir():
    return os.environ.get("TMPDIR") or "/tmp"

def index_path(agent_id):
    return os.path.join(tmpdir(), "orchestra-leases-" + re.sub(r"[^A-Za-z0-9_-]", "", agent_id))

def read_json(path):
    try:
        with open(path) as f:
            v = json.load(f)
        return v if isinstance(v, dict) else None
    except Exception:
        return None

def git(cwd, *args):
    try:
        r = subprocess.run(["git", "-C", cwd] + list(args), capture_output=True, text=True, timeout=5)
        return r.stdout if r.returncode == 0 else None
    except Exception:
        return None

try:
    data = json.loads(os.environ.get("ORCH_INPUT", ""))
except Exception:
    sys.exit(0)
if not isinstance(data, dict):
    sys.exit(0)

agent_id = s(data.get("agent_id"))
if not agent_id:
    sys.exit(0)                     # main thread: not our business

mode = os.environ.get("ORCH_MODE", "check")

# --- SubagentStop: drop every lease this agent still holds --------------------
if mode == "--release":
    idx = index_path(agent_id)
    try:
        with open(idx) as f:
            paths = [l.strip() for l in f if l.strip()]
    except Exception:
        paths = []
    for p in set(paths):
        lease = read_json(p)
        if lease and lease.get("agent_id") == agent_id:
            try:
                os.remove(p)
            except OSError:
                pass
    try:
        os.remove(idx)
    except OSError:
        pass
    sys.exit(0)

# --- PreToolUse ------------------------------------------------------------------
agent_type = s(data.get("agent_type"))
if "orchestra-delegate" in agent_type.lower():
    sys.exit(0)

tool_name = s(data.get("tool_name"))
tool_input = data.get("tool_input") if isinstance(data.get("tool_input"), dict) else {}
command = s(tool_input.get("command"))
description = s(tool_input.get("description"))
cwd = s(data.get("cwd")) or os.getcwd()
if re.search(r"\[orchestra:allow-lease(?::[^\]]*)?\]", (command + "\n" + description).lower()):
    sys.exit(0)

MUTATING = {"commit", "add", "rm", "mv", "reset", "checkout", "switch", "restore",
            "merge", "cherry-pick", "rebase", "revert", "am", "apply", "stash", "clean", "pull"}

def resolve(base, p):
    p = os.path.expanduser(p)
    return p if os.path.isabs(p) else os.path.join(base, p)

def bash_targets(cmd, base):
    body = re.sub(r"<<-?\s*[\x27\"]?(\w+)[\x27\"]?[\s\S]*?(?:\n\1\b|$)", " ", cmd)
    body = re.sub(r"\x27[^\x27]*\x27|\"[^\"]*\"", " ", body)
    out = []
    here = base
    for seg in re.split(r"(?:\|\||&&|[;&|\n])", body):
        seg = seg.strip()
        if not seg:
            continue
        m = re.match(r"^cd\s+(\S+)$", seg)
        if m and not re.search(r"[$`*?]", m.group(1)):
            here = resolve(here, m.group(1))
            continue
        for r in re.finditer(r"(?:^|\s)[0-9]*>>?\s*([^\s;&|<>()]+)", seg):
            tok = r.group(1)
            if tok and not tok.startswith("&") and tok != "/dev/null" and not re.search(r"[$`*?]", tok):
                out.append(resolve(here, tok))
        g = re.match(r"^git\b((?:\s+(?:-C\s+\S+|--git-dir=\S+|--work-tree=\S+|-c\s+\S+))*)\s+(\S+)", seg)
        if g and g.group(2) in MUTATING:
            c = re.search(r"-C\s+(\S+)", g.group(1) or "")
            out.append(resolve(here, c.group(1)) if c else here)
    return out

if tool_name in ("Edit", "Write", "MultiEdit", "NotebookEdit"):
    targets = [resolve(cwd, s(tool_input.get(k))) for k in ("file_path", "notebook_path") if s(tool_input.get(k))]
elif tool_name == "Bash":
    targets = bash_targets(command, cwd)
else:
    targets = []
if not targets:
    sys.exit(0)

def lease_file(path):
    d = path
    while d and not os.path.isdir(d):
        parent = os.path.dirname(d)
        if parent == d:
            return None
        d = parent
    out = git(d, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir", "--show-toplevel")
    if not out:
        return None
    lines = out.splitlines()
    if len(lines) < 3:
        return None
    git_dir, common, top = lines[0], lines[1], lines[2]
    if os.path.realpath(git_dir) == os.path.realpath(common):
        return None                 # the main worktree: never leased
    return os.path.join(git_dir, "orchestra-lease.json"), top

def window():
    v = os.environ.get("ORCHESTRA_LEASE_WINDOW")
    if v:
        try:
            return float(v)
        except ValueError:
            pass
    return None

now = time.time()
seen = set()
for t in targets:
    found = lease_file(t)
    if not found or found[0] in seen:
        continue
    path, top = found
    seen.add(path)
    lease = read_json(path)
    if lease and lease.get("agent_id") != agent_id:
        age = now - float(lease.get("last_seen") or 0)
        limit = window()
        if limit is None:
            # Config only matters when we are about to deny: keep the common path cheap.
            cfg = {}
            try:
                r = subprocess.run(["agent-exec", "config", "--json"], capture_output=True, text=True, timeout=5)
                if r.returncode == 0:
                    e = json.loads(r.stdout).get("enforcement")
                    cfg = e if isinstance(e, dict) else {}
            except Exception:
                cfg = {}
            if cfg.get("worktree_lease") == "off":
                continue
            try:
                limit = float(cfg.get("worktree_lease_window") or 120)
            except (TypeError, ValueError):
                limit = 120.0
        if age < limit:
            holder = s(lease.get("agent_type")) or "another agent"
            reason = (
                "orchestra: the worktree %s is leased by %s (agent %s), which wrote to it %ds ago. "
                "Two agents writing one worktree corrupt each other's work: this has happened when a "
                "message to a running workflow agent resumed a COPY of it, and when a retry started "
                "while the original was still running. If you were started for this task while another "
                "agent may still be working on it, you are the duplicate: stop now, make no further "
                "changes anywhere, and report DUPLICATE. If you are certain you are the only agent "
                "assigned here (a new round after the previous agent finished), wait %ds for the "
                "lease to expire and retry once. Override deliberately with "
                "[orchestra:allow-lease: <reason>] in the command or its description."
            ) % (top, holder, s(lease.get("agent_id"))[:12], int(age), int(limit - age) + 1)
            sys.stdout.write(json.dumps({"hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "deny",
                "permissionDecisionReason": reason,
            }}))
            sys.exit(0)
    new = {"agent_id": agent_id, "agent_type": agent_type, "last_seen": now,
           "since": lease.get("since", now) if lease and lease.get("agent_id") == agent_id else now}
    try:
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(new, f)
        os.replace(tmp, path)
        if not lease or lease.get("agent_id") != agent_id:
            with open(index_path(agent_id), "a") as f:
                f.write(path + "\n")
    except OSError:
        pass
sys.exit(0)
PYEOF

exit 0
