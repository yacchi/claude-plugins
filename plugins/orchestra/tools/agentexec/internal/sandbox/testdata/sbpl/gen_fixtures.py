"""Generate fixtures.json: what the Python sandbox makes of each case.

    uv run --no-project python internal/sandbox/testdata/sbpl/gen_fixtures.py \
        > internal/sandbox/testdata/sbpl/fixtures.json

Run on macOS (the Seatbelt cases are what the darwin test replays). Each
case builds a file-system layout under a fresh temp root (the `layout` ops,
replayed verbatim by the Go test), then records the wrapped argv a
`dispatch` would spawn -- the SBPL text and its -D params for Seatbelt, the
srt argv and settings for srt -- with the root spelled `<ROOT>`. A
`learned` case records the exact bytes of the grant store after a fixed
sequence of grants (timestamps pinned).
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
TOOLS = os.path.normpath(os.path.join(HERE, "..", "..", "..", "..", ".."))
sys.path.insert(0, TOOLS)
import agent_exec_sandbox as sb  # noqa: E402

FIXED_NOW = "2026-01-02T03:04:05Z"
SEATBELT = {"seatbelt": {"ok": True, "detail": "ok"},
            "bwrap": {"ok": False, "detail": "not Linux"},
            "landlock": {"ok": False, "detail": "not Linux", "abi": 0}}
GIT_ENV = {"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
           "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t",
           "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
           "GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z"}


def repo_ops(path):
    return [
        {"op": "mkdir", "path": path},
        {"op": "git", "cwd": path, "args": ["init", "-q", "-b", "main"]},
        {"op": "write", "path": path + "/a.txt", "data": "a\n"},
        {"op": "git", "cwd": path, "args": ["add", "a.txt"]},
        {"op": "git", "cwd": path, "args": ["commit", "-qm", "init"]},
    ]


def store(grants, domains=None):
    data = {"version": 1, "grants": grants}
    if domains is not None:
        data["domains"] = domains
    return {"op": "write", "path": "home/.local/state/claude-orchestra/sandbox-learned.json",
            "data": json.dumps(data)}


ARGV = ["pi", "-p", "--mode", "json", "--model", "openai-codex/gpt-5.6-luna",
        "it's a \"prompt\" with $vars"]

CASES = [
    {"name": "workdir-only", "kind": "seatbelt",
     "layout": [{"op": "mkdir", "path": "home"}, {"op": "mkdir", "path": "tmp"},
                {"op": "mkdir", "path": "work/tree"}],
     "tree": "work/tree", "cwd": "work/tree", "cfg": {}, "env": {"TMPDIR": "<ROOT>/tmp/"}},
    {"name": "linked-worktree", "kind": "seatbelt",
     "layout": [{"op": "mkdir", "path": "home"}, {"op": "mkdir", "path": "tmp"}]
     + repo_ops("repo")
     + [{"op": "git", "cwd": "repo", "args": ["worktree", "add", "-q", "-b", "t1", "<ROOT>/repo-orchestra/t1"]},
        {"op": "mkdir", "path": "repo-orchestra/t1/pkg/.claude"}],
     "tree": "repo-orchestra/t1", "cwd": "repo-orchestra/t1/pkg", "cfg": {},
     "env": {"TMPDIR": "<ROOT>/tmp"}},
    {"name": "checkout-carve-out", "kind": "seatbelt",
     "layout": [{"op": "mkdir", "path": "home"}] + repo_ops("repo"),
     "tree": "repo", "cwd": "repo", "cfg": {"allow_write": ["<ROOT>/repo/.git/hooks-x"]},
     "env": {}},
    {"name": "grants-and-config", "kind": "seatbelt",
     "layout": [{"op": "mkdir", "path": "home/.cache/uv"}, {"op": "mkdir", "path": "tmp"},
                {"op": "mkdir", "path": "home/real-pi"},
                {"op": "write", "path": "home/real-pi/auth.json", "data": "{}"},
                {"op": "mkdir", "path": "home/.local/state/claude-orchestra/pi-agent"},
                {"op": "symlink", "path": "home/.local/state/claude-orchestra/pi-agent/auth.json",
                 "target": "<ROOT>/home/real-pi/auth.json"},
                {"op": "mkdir", "path": "home/sp ace"},
                {"op": "symlink", "path": "home/linked", "target": "<ROOT>/home/sp ace"},
                store([
                    {"path": "<ROOT>/home/.cache/uv", "source": "auto", "added": "x"},
                    {"path": "<ROOT>/home/.pub-cache/v3", "source": "auto"},
                    {"path": "<ROOT>/home/granted", "source": "user", "reason": "r"},
                    {"path": "<ROOT>/home/.ssh/k", "source": "user"},
                    {"path": "<ROOT>/home/.config/x", "source": "auto"},
                    {"path": "<ROOT>/home/approved", "source": "instructor"},
                ]),
                {"op": "mkdir", "path": "work/tree"}],
     "tree": "work/tree", "cwd": "work/tree",
     "cfg": {"mode": "required", "allow_write": ["~/extra", "~/linked/x", "~/extra/", "~/q\"uote"],
             "deny_read": ["~/notes", "~/.ssh", "/etc/secret"]},
     "env": {"TMPDIR": "<ROOT>/tmp",
             "PI_CODING_AGENT_DIR": "<ROOT>/home/.local/state/claude-orchestra/pi-agent"}},
    {"name": "denied-paths-metachars", "kind": "seatbelt",
     "layout": [{"op": "mkdir", "path": "home"}] + repo_ops("re+po (x)[1]{2}.^$|"),
     "tree": "re+po (x)[1]{2}.^$|", "cwd": "re+po (x)[1]{2}.^$|",
     "cfg": {"deny_read": ["~/.config/other", "~/sec ret"]}, "env": {}},
    {"name": "denied-paths-quote", "kind": "seatbelt",
     "layout": [{"op": "mkdir", "path": "home"}] + repo_ops("we\"ird"),
     "tree": "we\"ird", "cwd": "we\"ird", "cfg": {}, "env": {}},
    {"name": "deep-tree", "kind": "seatbelt",
     "layout": [{"op": "mkdir", "path": "home"}, {"op": "mkdir", "path": "tmp"}]
     + repo_ops("deep/repo")
     + [{"op": "mkdir", "path": "deep/repo/a/b/c/d/e"},
        {"op": "symlink", "path": "shortcut", "target": "deep/repo/a/b"}],
     "tree": "shortcut/c/../c/d", "cwd": "shortcut/c/d/e", "cfg": {"allow_write": ["~/x/../y"]},
     "env": {"TMPDIR": "<ROOT>/shortcut/c"}},
    {"name": "mode-off", "kind": "seatbelt",
     "layout": [{"op": "mkdir", "path": "home"}, {"op": "mkdir", "path": "work"}],
     "tree": "work", "cwd": "work", "cfg": {"mode": "off"}, "env": {}},
    {"name": "network-srt", "kind": "srt",
     "layout": [{"op": "mkdir", "path": "home"}, {"op": "mkdir", "path": "tmp"}]
     + repo_ops("repo")
     + [{"op": "mkdir", "path": "repo/.git/info/x"}],
     "tree": "repo", "cwd": "repo", "cfg": {"network": {"denied_domains": ["Bad.example.org"]}},
     "env": {"TMPDIR": "<ROOT>/tmp"},
     "network": {"srt": "/opt/srt bin/srt", "reason": "", "id": "task-1", "session": "task-1",
                 "domains": ["api.anthropic.com", "registry.npmjs.org", "api.anthropic.com"]}},
    {"name": "network-off", "kind": "srt",
     "layout": [{"op": "mkdir", "path": "home"}, {"op": "mkdir", "path": "work"}],
     "tree": "work", "cwd": "work", "cfg": {"network": {"mode": "off"}}, "env": {},
     "network": {"srt": "/opt/srt", "reason": "", "id": "t", "session": "t", "domains": []}},
    {"name": "network-srt-missing", "kind": "srt",
     "layout": [{"op": "mkdir", "path": "home"}, {"op": "mkdir", "path": "work"}],
     "tree": "work", "cwd": "work", "cfg": {}, "env": {},
     "network": {"srt": None, "reason": "srt not found (not-installed)", "id": "t",
                 "session": "t", "domains": []}},
    {"name": "herdr-host", "kind": "host",
     "layout": [{"op": "mkdir", "path": "home/.config/shared/projects"},
                {"op": "mkdir", "path": "home/.config/shared/skills"},
                {"op": "mkdir", "path": "home/.claude/plugins/data/other"},
                {"op": "mkdir", "path": "home/.claude/plugins/cache"},
                {"op": "write", "path": "home/.config/shared/history.jsonl", "data": ""},
                {"op": "symlink", "path": "home/.claude/projects", "target": "<ROOT>/home/.config/shared/projects"},
                {"op": "symlink", "path": "home/.claude/skills", "target": "<ROOT>/home/.config/shared/skills"},
                {"op": "symlink", "path": "home/.claude/history.jsonl",
                 "target": "<ROOT>/home/.config/shared/history.jsonl"},
                {"op": "mkdir", "path": "tmp"}]
     + repo_ops("repo"),
     "tree": "repo", "cwd": "repo", "cfg": {"allow_write": ["~/extra"], "deny_read": ["~/notes"]},
     "env": {"TMPDIR": "<ROOT>/tmp"},
     "host": {"repo": "repo", "worktree_base": "repo-orchestra", "herdr_home": "home/hx",
              "extra_writable": ["<ROOT>/home/.local/state/claude-orchestra/pi-agent"]}},
]


def sub(value, root):
    if isinstance(value, str):
        return value.replace("<ROOT>", root)
    if isinstance(value, list):
        return [sub(v, root) for v in value]
    if isinstance(value, dict):
        return {k: sub(v, root) for k, v in value.items()}
    return value


def unsub(value, root):
    if isinstance(value, str):
        return value.replace(root, "<ROOT>")
    if isinstance(value, list):
        return [unsub(v, root) for v in value]
    if isinstance(value, dict):
        return {k: unsub(v, root) for k, v in value.items()}
    return value


def build(layout, root):
    for op in layout:
        path = os.path.join(root, op.get("path", ""))
        if op["op"] == "mkdir":
            os.makedirs(path, exist_ok=True)
        elif op["op"] == "write":
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w") as fh:
                fh.write(sub(op["data"], root))
        elif op["op"] == "symlink":
            os.makedirs(os.path.dirname(path), exist_ok=True)
            os.symlink(sub(op["target"], root), path)
        elif op["op"] == "git":
            env = dict(os.environ, **GIT_ENV)
            subprocess.run(["git"] + sub(op["args"], root), cwd=os.path.join(root, op["cwd"]),
                           env=env, check=True, capture_output=True)


def run_case(case):
    root = os.path.realpath(tempfile.mkdtemp(prefix="sbpl-"))
    try:
        os.environ["HOME"] = os.path.join(root, "home")
        os.environ.pop("XDG_STATE_HOME", None)
        build(case["layout"], root)
        env = sub(case["env"], root)
        cfg = sub(case["cfg"], root)
        tree = os.path.join(root, case["tree"])
        cwd = os.path.join(root, case["cwd"])
        out = {}
        if case["kind"] == "host":
            h = case["host"]
            spec = sb.prepare_host(cfg, os.path.join(root, h["repo"]),
                                   os.path.join(root, h["worktree_base"]),
                                   os.path.join(root, h["herdr_home"]),
                                   extra_writable=sub(h["extra_writable"], root),
                                   probe_results=SEATBELT, env=env, platform="darwin")
            out["srt_settings"] = sb.srt_settings(spec["policy"], ["/tmp/x.sock"],
                                                  ["api.anthropic.com"], ["bad.example.org"])
        else:
            network = sub(case.get("network"), root)
            spec = sb.prepare("pi", tree, cfg, probe_results=SEATBELT, env=env, network=network)
            if spec.get("srt"):
                out["srt_settings"] = spec["srt"]["settings"]
                out["srt_settings_path"] = spec["srt"]["settings_path"]
        out["backend"] = spec["backend"]
        out["reason"] = spec["reason"]
        out["limits"] = spec["limits"]
        out["refuse"] = spec["refuse"]
        out["argv"] = sb.wrap_argv(ARGV, cwd, spec)
        return unsub(out, root)
    finally:
        shutil.rmtree(root, ignore_errors=True)


def learned_case():
    """The store's bytes after a fixed sequence of grants."""
    root = os.path.realpath(tempfile.mkdtemp(prefix="learned-"))
    try:
        os.environ["HOME"] = os.path.join(root, "home")
        os.environ.pop("XDG_STATE_HOME", None)
        os.makedirs(os.path.join(root, "home"))
        sb._now_iso = lambda: FIXED_NOW
        home = os.environ["HOME"]
        steps = []

        def snap(label):
            with open(sb.learned_path(), "rb") as fh:
                steps.append({"after": label,
                              "content": fh.read().decode("utf-8").replace(root, "<ROOT>")})
        sb.add_grant(os.path.join(home, ".cache/uv/archive/x"), "auto",
                     os.path.join(home, ".cache/uv/archive/x"))
        snap("auto")
        sb.add_grant(os.path.join(home, "Données ü"), "user", reason="naïve – ünïcode")
        snap("user")
        sb.add_domain(" Registry.NPMJS.org ", reason=" npm ")
        snap("domain")
        sb.add_grant(os.path.join(home, "approved"), "instructor", reason="approved")
        sb.forget_grant(os.path.join(home, ".cache/uv/archive/x"))
        snap("forget")
        sb.forget_domain("registry.npmjs.org")
        snap("forget-domain")
        return steps
    finally:
        shutil.rmtree(root, ignore_errors=True)


def main():
    if sys.platform != "darwin":
        sys.exit("generate the fixtures on macOS")
    cases = []
    for case in CASES:
        cases.append(dict(case, want=run_case(case)))
    json.dump({"argv": ARGV, "now": FIXED_NOW, "cases": cases, "learned": learned_case()},
              sys.stdout, indent=1, ensure_ascii=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
