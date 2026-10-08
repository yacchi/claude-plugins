#!/usr/bin/env python3
"""In-container driver for run.sh: sandbox scenarios against one backend, or
the plugin's pytest suite. Runs as `worker`; /repo is the read-only mount and
is only ever copied from.

    scenarios.py bwrap|landlock     sandbox scenarios, expecting that backend
    scenarios.py suite              full pytest suite of the copied plugin
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

HOME = os.path.expanduser("~")
SRC = os.path.join(HOME, "src")
TOOLS = os.path.join(SRC, "plugins", "orchestra", "tools")
TMPDIR = os.path.join(HOME, "tmpdir")
SECRET = "SECRET-KEY-MATERIAL"

RESULTS = []


def check(backend, name, ok, info=""):
    RESULTS.append(ok)
    line = "%s %-8s %s" % ("PASS" if ok else "FAIL", backend, name)
    if info:
        line += "  (%s)" % info
    print(line, flush=True)


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(text)


def read(path):
    try:
        with open(path, encoding="utf-8") as fh:
            return fh.read()
    except OSError:
        return None


def copy_repo():
    os.makedirs(SRC, exist_ok=True)
    shutil.copytree("/repo/plugins", os.path.join(SRC, "plugins"), symlinks=True,
                    ignore=shutil.ignore_patterns(".git", "__pycache__", ".pytest_cache"))


def setup():
    """An ordinary pi user's machine: agent-exec shim on PATH, the permission
    rule, pi's state dir, a secret key, a user config forcing the sandbox."""
    copy_repo()
    write(os.path.join(HOME, ".local/bin/agent-exec"),
          '#!/bin/sh\nexec "%s" "$@"\n' % os.path.join(TOOLS, "agent-exec"))
    os.chmod(os.path.join(HOME, ".local/bin/agent-exec"), 0o755)
    write(os.path.join(HOME, ".claude/settings.json"),
          json.dumps({"permissions": {"allow": ["Bash(agent-exec:*)"]}}))
    write(os.path.join(HOME, ".claude/orchestra.yaml"), "sandbox:\n  mode: required\n")
    write(os.path.join(HOME, ".pi/agent/settings.json"), "{}\n")
    os.makedirs(os.path.join(HOME, ".pi/agent/extensions"), exist_ok=True)
    write(os.path.join(HOME, ".ssh/id_test"), SECRET + "\n")
    os.chmod(os.path.join(HOME, ".ssh"), 0o700)
    os.makedirs(TMPDIR, exist_ok=True)


def git(*args, cwd=None):
    return subprocess.run(["git"] + list(args), cwd=cwd, capture_output=True,
                          text=True)


def new_repo(name):
    repo = os.path.join(HOME, "repos", name)
    os.makedirs(repo)
    write(os.path.join(repo, "README"), "x\n")
    git("init", "-q", cwd=repo)
    git("add", "-A", cwd=repo)
    git("commit", "-qm", "init", cwd=repo)
    return repo


def dispatch(name, commands, env_extra=None, before=None):
    """One `agent-exec dispatch` of the fake pi in a fresh repo; returns
    (result, task_tree, main_repo)."""
    repo = new_repo(name)
    if before:
        before(repo)
    prompt = os.path.join(HOME, "prompts", name + ".txt")
    write(prompt, "Run these.\nCOMMANDS:\n" + "\n".join(commands) + "\n")
    env = dict(os.environ, TMPDIR=TMPDIR)
    env.update(env_extra or {})
    proc = subprocess.run(
        ["agent-exec", "dispatch", "--class", "standard", "--isolate", "always",
         "--task", name, "--workdir", repo, "--prompt-file", prompt,
         "--capture", "--no-cooldown"],
        capture_output=True, text=True, env=env, cwd=repo, timeout=300)
    try:
        result = json.loads(proc.stdout)
    except ValueError:
        result = {"status": "unparseable", "stdout": proc.stdout[-500:],
                  "stderr": proc.stderr[-500:]}
    tree = (result.get("isolation") or {}).get("path") or repo
    return result, tree, repo


def sb(result):
    return result.get("sandbox") or {}


def denied_paths(result):
    return [d.get("path") for d in sb(result).get("denials") or []]


def sleeps(arg):
    proc = subprocess.run(["pgrep", "-u", str(os.getuid()), "-f", "^sleep %s$" % arg],
                          capture_output=True, text=True)
    return proc.stdout.split()


def run_scenarios(backend):
    setup()
    outside = os.path.join(HOME, "outside.txt")

    # 1-4 in one run: backend, writes, temp, secret read.
    res, tree, _repo = dispatch("s1", [
        "echo inside > in.txt",
        "echo out > $HOME/outside.txt",
        "echo t > /tmp/s3-probe",
        "echo t > \"$TMPDIR/s3-probe\"",
        "cat ~/.ssh/id_test > ssh-copy.txt",
    ])
    s = sb(res)
    limits = s.get("limits") or []
    ok1 = s.get("backend") == backend and s.get("enforced") is True
    if backend == "landlock":
        ok1 = ok1 and "deny_read" in limits and "signal" not in limits
    check(backend, "1 backend reported", ok1,
          "backend=%s enforced=%s limits=%s" % (s.get("backend"), s.get("enforced"), limits))
    ok2 = (read(os.path.join(tree, "in.txt")) == "inside\n"
           and not os.path.exists(outside) and outside in denied_paths(res))
    check(backend, "2 write tree ok / $HOME denied", ok2,
          "status=%s reason=%s" % (res.get("status"), res.get("reason")))
    ok3 = (read("/tmp/s3-probe") == "t\n"
           and read(os.path.join(TMPDIR, "s3-probe")) == "t\n")
    check(backend, "3 /tmp and $TMPDIR writable", ok3)
    copied = read(os.path.join(tree, "ssh-copy.txt")) or ""
    if backend == "bwrap":
        ok4 = SECRET not in copied
        info = "secret not readable"
    else:
        ok4 = SECRET in copied and "deny_read" in limits
        info = "readable, reported in limits"
    check(backend, "4 ~/.ssh read", ok4, info)

    # 5 signal confinement.
    victim = subprocess.Popen(["sleep", "300"])
    try:
        res, _tree, _repo = dispatch("s5", ["kill %d; echo rc=$? > kill-rc.txt" % victim.pid])
        time.sleep(0.5)
        alive = victim.poll() is None
        check(backend, "5 kill outside pid fails", alive,
              "status=%s" % res.get("status"))
    finally:
        victim.kill()
        victim.wait()

    # 6 tamper protection.
    targets = {
        "~/.local/state/claude-orchestra/sandbox-learned.json": '{"version": 1, "grants": []}\n',
        "~/.claude/orchestra.yaml": None,
        "~/.pi/agent/settings.json": None,
        "~/.pi/agent/extensions/x.ts": None,
        "~/.local/share/uv/tools/x": None,
    }
    before = {}
    for t, seed in targets.items():
        p = os.path.expanduser(t)
        if seed is not None:
            write(p, seed)
        before[t] = read(p)
    os.makedirs(os.path.expanduser("~/.local/share/uv/tools"), exist_ok=True)
    res, _tree, _repo = dispatch("s6", ["echo pwned > %s" % t for t in targets])
    limits = sb(res).get("limits") or []
    written = [t for t in targets if read(os.path.expanduser(t)) != before[t]]
    pi_cfg = {"~/.pi/agent/settings.json", "~/.pi/agent/extensions/x.ts"}
    if backend == "bwrap":
        ok6 = not written
    else:
        # Landlock cannot deny inside pi's writable state dir: those two are
        # writable by design and must be reported as `pi-config`.
        ok6 = set(written) <= pi_cfg and (not written or "pi-config" in limits)
    check(backend, "6 tamper protection", ok6,
          "written=%s limits=%s" % (sorted(written) or "none", limits))
    for t in targets:  # restore for later scenarios
        p = os.path.expanduser(t)
        if before[t] is None:
            if os.path.isfile(p):
                os.unlink(p)
        else:
            write(p, before[t])

    # 7 auto-grant, UTF-8 locale (coreutils quotes with U+2018/U+2019).
    res, _tree, _repo = dispatch("s7", [
        "mkdir -p ~/.pub-cache/probe && touch ~/.pub-cache/probe/x.txt",
    ], env_extra={"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8"})
    check(backend, "7 auto-grant (UTF-8 mkdir shape)", grant_ok(res, "~/.pub-cache",
                                                                "~/.pub-cache/probe/x.txt"),
          grant_info(res))
    # 7b same in the C locale with the `touch: cannot touch '...'` shape.
    os.makedirs(os.path.expanduser("~/.pnpm-store/v3"), exist_ok=True)
    res, _tree, _repo = dispatch("s7b", ["touch ~/.pnpm-store/v3/x.txt"],
                                 env_extra={"LANG": "C", "LC_ALL": "C"})
    check(backend, "7b auto-grant (C touch shape)", grant_ok(res, "~/.pnpm-store/v3",
                                                             "~/.pnpm-store/v3/x.txt"),
          grant_info(res))

    # 7c a file directly inside a cache root must grant the root, not create
    # the file as a directory.
    os.makedirs(os.path.expanduser("~/.pnpm-store"), exist_ok=True)
    probe = "~/.pnpm-store/probe-%d.txt" % os.getpid()
    probe_path = os.path.expanduser(probe)
    res, _tree, _repo = dispatch("s7c", ["printf x > %s" % probe],
                                 env_extra={"LANG": "C", "LC_ALL": "C"})
    check(backend, "7c auto-grant (file in cache root)",
          grant_ok(res, "~/.pnpm-store", probe) and os.path.isfile(probe_path),
          grant_info(res))

    # 8 watchdog.
    def local_cfg(repo):
        write(os.path.join(repo, ".claude/orchestra.local.yaml"),
              "watchdog:\n  tool_idle_seconds: 3\n  idle_seconds: 2\n")
    started = time.monotonic()
    res, _tree, _repo = dispatch("s8", ["sleep 30"], before=local_cfg)
    elapsed = time.monotonic() - started
    time.sleep(0.5)
    left = sleeps("30")
    check(backend, "8 watchdog runaway", res.get("status") == "runaway" and not left
          and elapsed < 25,
          "status=%s reason=%s elapsed=%.1fs left=%s"
          % (res.get("status"), res.get("reason"), elapsed, left or "none"))

    # 9 git in the worktree vs the main checkout.
    main_head = {}

    def remember(repo):
        main_head["sha"] = git("rev-parse", "HEAD", cwd=repo).stdout.strip()
    res, tree, repo = dispatch("s9", [
        "echo g > g.txt && git add -A && git status --short",
        "git -C %s commit -q --allow-empty -m pwned" % os.path.join(HOME, "repos", "s9"),
    ], before=remember)
    staged = git("status", "--porcelain", cwd=tree).stdout
    ok_wt = "A  g.txt" in staged
    ok_main = git("rev-parse", "HEAD", cwd=repo).stdout.strip() == main_head["sha"]
    check(backend, "9 git add in worktree / main commit denied", ok_wt and ok_main,
          "worktree staged=%s main unchanged=%s" % (ok_wt, ok_main))


def grant_ok(res, grant, target):
    s = sb(res)
    grant = os.path.realpath(os.path.expanduser(grant))
    sid = res.get("session_id")
    log = read(os.path.join(HOME, ".pi/agent/sessions/%s.jsonl" % sid)) or ""
    lines = [json.loads(line) for line in log.splitlines() if line.strip()]
    learned = json.loads(read(os.path.join(HOME, ".local/state/claude-orchestra/sandbox-learned.json"))
                         or '{"grants": []}')
    return (res.get("status") == "ok" and s.get("cycles") == 1
            and grant in (s.get("granted") or [])
            and grant in [g.get("path") for g in learned.get("grants") or []]
            and os.path.exists(os.path.expanduser(target))
            # resumed: the grant cycle continued the SAME pi session
            and len(lines) == 2 and lines[1].get("resumed") is True)


def grant_info(res):
    s = sb(res)
    return "status=%s cycles=%s granted=%s denials=%s" % (
        res.get("status"), s.get("cycles"), s.get("granted"),
        [(d.get("path"), d.get("auto")) for d in s.get("denials") or []])


# Order-dependent outside Linux too (also flaky on macOS): it passes alone
# but not after the rest of the suite. Run separately so it still counts.
KNOWN_FLAKY = "test_isolate_sweep.py::SweepCliTests::test_json_output_is_the_default"


def pytest(*args):
    proc = subprocess.run(
        [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider"] + list(args),
        capture_output=True, text=True, cwd=TOOLS)
    tail = [line for line in proc.stdout.splitlines() if line.strip()][-1:] or ["?"]
    if proc.returncode != 0:
        sys.stdout.write("\n".join(line for line in proc.stdout.splitlines()
                                   if line.startswith(("FAILED", "ERROR"))) + "\n")
    return proc.returncode == 0, tail[0]


def run_suite():
    copy_repo()
    ok, tail = pytest("--deselect", KNOWN_FLAKY, TOOLS)
    check("suite", "pytest (linux, default container)", ok, tail)
    ok, tail = pytest(KNOWN_FLAKY)
    check("suite", "known order-dependent test, alone", ok, tail)


def main(argv):
    mode = argv[0] if argv else ""
    if mode in ("bwrap", "landlock"):
        run_scenarios(mode)
    elif mode == "suite":
        run_suite()
    else:
        sys.stderr.write("usage: scenarios.py bwrap|landlock|suite\n")
        return 2
    return 0 if all(RESULTS) else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
