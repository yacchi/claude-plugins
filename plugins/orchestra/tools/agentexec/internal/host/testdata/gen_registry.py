# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Writes the host registry fixtures with the Python hosts' own code.

Run from anywhere (cutover parity fixtures; re-run only when the Python
registry format changes):

    uv run plugins/orchestra/tools/agentexec/internal/host/testdata/gen_registry.py

Every fixture is the registry file a Python HerdrExecutor / OrcaExecutor
wrote through `_save`, `_forget` and `_warm_put`; the Go tests read each one
and must write it back byte for byte.
"""

import os
import shutil
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
TOOLS = os.path.normpath(os.path.join(HERE, "..", "..", "..", ".."))
sys.path.insert(0, TOOLS)

import agent_exec_herdr  # noqa: E402
import agent_exec_orca  # noqa: E402

OUT = os.path.join(HERE, "registry")


def herdr_session(state, pkg, **extra):
    session = {
        "pkg": pkg, "worktree": "/repo/.claude/worktrees/pkg-" + pkg,
        "branch": "orchestra/abcdef12/pkg-" + pkg, "terminal": None, "workspace": None,
        "pane": None, "repo": "/repo", "model": "opus", "cls": "standard",
        "state_dir": state, "created": True, "herdr_session": "orchestra-abcdef12",
        "add_dirs": ["/repo", state], "net_mark": 0,
    }
    session.update(extra)
    return session


def main():
    if os.path.isdir(OUT):
        shutil.rmtree(OUT)
    os.makedirs(OUT)
    tmp = tempfile.mkdtemp(prefix="host-fixtures-")
    cwd = os.getcwd()
    os.chdir(tmp)  # relative state dirs keep the fixtures free of tmp paths

    def herdr_dir(name):
        d = name
        os.makedirs(d)
        ex = agent_exec_herdr.HerdrExecutor(cfg={}, binary="/bin/false", env={})
        ex.state_dir = d
        return ex, d

    def keep(d, name, registry):
        shutil.copy(os.path.join(d, registry), os.path.join(OUT, name + ".json"))

    # one package right after start (no agent yet), then with its agent
    ex, d = herdr_dir("herdr-start")
    ex._save(herdr_session(d, "a"))
    keep(d, "herdr-start", agent_exec_herdr.REGISTRY_NAME)
    ex._save(herdr_session(d, "a", terminal="p-a", workspace="w1", pane="w1:p1",
                           warm_session="1b4e28ba-2fa1-41d2-883f-0016d3cca427",
                           warm_worktree="/repo/.claude/worktrees/warm-opus"))
    ex._save(herdr_session(d, "b-ünï", model="sonnet", cls="light", created=False,
                           net_mark=3, fork_first=True))
    keep(d, "herdr-two", agent_exec_herdr.REGISTRY_NAME)

    # warm records: floats from time.time(), whole floats, the list fields
    ex, d = herdr_dir("herdr-warm")
    ex._save(herdr_session(d, "a"))
    ex._warm_put(d, "opus", {
        "session_id": "1b4e28ba-2fa1-41d2-883f-0016d3cca427",
        "worktree": "/repo/.claude/worktrees/warm-opus", "base_ref": "main",
        "base_rev": "0123456789abcdef0123456789abcdef01234567",
        "files": ["README.md", "docs/plan.md"], "requested": ["README.md", "docs/plan.md", "gone.md"],
        "repo": "/repo", "task": "warm-opus",
        "created_at": 1760000000.123456, "last_used": 100000.0,
    })
    ex._warm_put(d, "sonnet", {"session_id": "x", "worktree": "/w", "invalid": True,
                               "created_at": 0.5, "last_used": 1e-05, "files": [], "requested": []})
    keep(d, "herdr-warm", agent_exec_herdr.REGISTRY_NAME)
    ex._warm_put(d, "sonnet", None)
    ex._forget({"pkg": "a", "state_dir": d})
    keep(d, "herdr-warm-only", agent_exec_herdr.REGISTRY_NAME)

    # orca: request ids accumulate
    d = "orca"
    os.makedirs(d)
    ox = agent_exec_orca.OrcaExecutor(cfg={})
    ox.state_dir = d
    ox._save({"pkg": "A", "worktree": "/orca/workspaces/repo/wave-run1-a",
              "branch": "wave-run1-a", "terminal": "term-1", "request_ids": ["r1", "r2"],
              "repo": "/repo", "model": "opus", "cls": "standard", "state_dir": d,
              "add_dirs": ["/repo", "/plans <x>&\"q\""]})
    ox._save({"pkg": "B", "worktree": "/orca/workspaces/repo/wave-run1-b",
              "branch": None, "terminal": None, "request_ids": [], "repo": "/repo",
              "model": "sonnet", "cls": "light", "state_dir": d, "add_dirs": []})
    keep(d, "orca-two", agent_exec_orca.REGISTRY_NAME)
    os.chdir(cwd)
    shutil.rmtree(tmp)


if __name__ == "__main__":
    main()
