# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for the Orca executor of `agent-exec wave run` (agent_exec_orca.py).

The real Orca is never called: each test writes a FAKE `orca` executable (a
small Python script keeping its state in a temp dir) that implements the
subset of the CLI the executor uses, with real `git worktree add`. A
scenario file tells the fake how the "Claude" in each terminal behaves:
which files a prompt writes, which result it reports, after how many idle
waits, whether the trust dialog shows, whether startup never finishes.

Run with: uv run test_agent_exec_orca.py
"""

import json
import contextlib
import io
import os
import shlex
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402
import agent_exec_orca  # noqa: E402
import agent_exec_wave  # noqa: E402
import agent_exec_wave_run  # noqa: E402

# Heartbeats are machine-shared (`~/.claude/orchestra/alive`); redirect this
# whole suite into a throwaway directory.
_ALIVE_TMP = tempfile.mkdtemp(prefix="orch-alive-")
os.environ["ORCHESTRA_ALIVE_DIR"] = _ALIVE_TMP
agent_exec._heartbeat_dir_cache = _ALIVE_TMP

_REGISTRY_TMP = tempfile.mkdtemp(prefix="orch-wave-registry-")
os.environ["ORCHESTRA_WAVE_REGISTRY"] = os.path.join(_REGISTRY_TMP, "waves.jsonl")
os.environ["GIT_CONFIG_GLOBAL"] = "/dev/null"
os.environ["GIT_CONFIG_SYSTEM"] = "/dev/null"

_BASE_LINES = ["line %02d\n" % n for n in range(1, 21)]

_NO_BAD_CHECK = (
    "checks:\n"
    "  items:\n"
    "    - name: nobad\n"
    "      run: \"! grep -rqs BAD --include=*.txt .\"\n"
)

# Claude Code's real folder-trust dialog: unnumbered options, "No, exit"
# preselected. The fake renders the cursor from its own selection state.
_TRUST_SCREEN = [
    " Accessing workspace:",
    " Quick safety check: Is this a project you created or one you trust? (Like your own code)",
    " ❯ No, exit",
    "   Yes, I trust this folder",
    " Enter to confirm · Esc to cancel",
]

_FAKE_ORCA = r'''#!%(python)s
import fcntl, json, os, re, subprocess, sys, uuid

ROOT = os.environ["FAKE_ORCA_DIR"]
STATE = os.path.join(ROOT, "state.json")
TRUST = %(trust)r
READY = ["Claude Code", "", "❯ "]


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def out(result=None, ok=True, error=None):
    payload = {"ok": ok}
    if ok:
        payload["result"] = result or {}
    else:
        payload["error"] = {"message": error or "failed"}
    print(json.dumps(payload))
    sys.exit(0 if ok else 1)


def git(cwd, *args):
    return subprocess.run(["git"] + list(args), cwd=cwd, capture_output=True, text=True)


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as fh:
        fh.write(text)


def main(state, scenario, args):
    verb = " ".join(args[:2])
    if args[:1] == ["status"]:
        if scenario.get("status_ok", True):
            out({"runtime": "up"})
        out(ok=False, error="runtime not running")
    if verb == "worktree create":
        repo = opt(args, "--repo")[len("path:"):]
        name = opt(args, "--name")
        base = opt(args, "--base-branch")
        path = os.path.join(ROOT, "workspaces", os.path.basename(repo), name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        proc = git(repo, "worktree", "add", "-q", "-b", name, path, base)
        if proc.returncode != 0:
            out(ok=False, error=proc.stderr)
        state["worktrees"][path] = {"repo": repo, "branch": name}
        out({"worktree": {"id": "wt-" + name, "path": path, "branch": name}})
    if verb == "worktree rm":
        path = opt(args, "--worktree")[len("path:"):]
        info = state["worktrees"].pop(path, None)
        if info is None:
            out(ok=False, error="unknown worktree")
        git(info["repo"], "worktree", "remove", "--force", path)
        git(info["repo"], "branch", "-D", info["branch"])
        out({"removed": True})
    if verb == "terminal create":
        path = opt(args, "--worktree")[len("path:"):]
        if path not in state["worktrees"]:
            out(ok=False, error="Timed out waiting for terminal handle")
        state["next"] += 1
        handle = "term-%%d" %% state["next"]
        state["terminals"][handle] = {
            "worktree": path, "command": opt(args, "--command"), "closed": False,
            "trusted": not scenario.get("trust", False), "pending": None,
            "pkg": os.path.basename(path).rsplit("-", 1)[-1],
        }
        out({"terminal": {"handle": handle}})
    handle = opt(args, "--terminal")
    term = state["terminals"].get(handle)
    if term is None or term["closed"]:
        out(ok=False, error="no such terminal: %%s" %% handle)
    if verb == "terminal close":
        term["closed"] = True
        out({"closed": True})
    if verb == "terminal read":
        if term.get("exited"):
            screen = ["user@host ~/wt (branch) [1]>"]
        elif not term["trusted"]:
            yes = term.get("selection", 0) == 1
            screen = [line.replace("❯ No", "  No").replace("  Yes", "❯ Yes") if yes else line
                      for line in TRUST]
        elif scenario.get("never_ready"):
            screen = ["Starting Claude Code..."]
        elif term["pending"] and term["pending"].get("dialog"):
            screen = ["Do you want to make this edit to a.txt?", "❯ 1. Yes", "  2. No"]
        else:
            screen = READY
        out({"terminal": {"tail": screen}})
    if verb == "terminal wait":
        state["waits"] += 1
        pending = term["pending"]
        if pending is not None and pending.get("result") is not None:
            pending["waits"] += 1
            if pending["waits"] >= pending.get("after_waits", 0):
                write(pending["result_path"], json.dumps(pending["result"]))
                term["pending"] = None
        out({"wait": {"satisfied": True}})
    if verb == "terminal send":
        text = opt(args, "--text", "")
        if not term["trusted"] and not term.get("exited"):
            # Mirror the real TUI: an Enter in the same send as the arrow is
            # handled before the cursor moves, so it confirms "No, exit".
            if "--enter" in args:
                if text.startswith("\x1b[B") or term.get("selection", 0) == 0:
                    term["exited"] = True
                else:
                    term["trusted"] = True
            elif text.startswith("\x1b[B"):
                term["selection"] = 1
            out({"send": {}})
        match = re.search(r"write (\S+) containing JSON", text)
        if match is None:
            out({"send": {}})
        retry = opt(args, "--retry-request")
        if retry is not None:
            out({"send": {"prompt": {"requestId": retry}}})
        index = state["prompt_index"].get(term["pkg"], 0)
        state["prompt_index"][term["pkg"]] = index + 1
        steps = scenario.get("prompts", {}).get(term["pkg"], [])
        step = dict(steps[index]) if index < len(steps) else {}
        for rel, body in (step.get("files") or {}).items():
            write(os.path.join(term["worktree"], rel), body)
        step.update({"result_path": match.group(1), "waits": 0})
        term["pending"] = step
        if step.get("result") is not None and step.get("after_waits", 0) == 0:
            write(step["result_path"], json.dumps(step["result"]))
            term["pending"] = None
        out({"send": {"prompt": {"requestId": str(uuid.uuid4())}}})
    out(ok=False, error="unsupported: %%s" %% " ".join(args))


args = [a for a in sys.argv[1:] if a != "--json"]
with open(os.path.join(ROOT, "lock"), "a") as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    with open(os.path.join(ROOT, "calls.jsonl"), "a") as fh:
        fh.write(json.dumps(args) + "\n")
    with open(os.path.join(ROOT, "scenario.json")) as fh:
        scenario = json.load(fh)
    try:
        with open(STATE) as fh:
            state = json.load(fh)
    except (OSError, ValueError):
        state = {"worktrees": {}, "terminals": {}, "next": 0, "prompt_index": {}, "waits": 0}
    try:
        main(state, scenario, args)
    finally:
        with open(STATE, "w") as fh:
            json.dump(state, fh)
'''


def _git(cwd, *args):
    return subprocess.run(
        ["git"] + list(args), cwd=cwd, capture_output=True, text=True, check=True,
    )


def _write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as fh:
        fh.write(text)


def _read(path):
    with open(path) as fh:
        return fh.read()


# --- fake CLI executor: every package routes to claude -------------------------


class DelegateExecutor(object):
    """Stands in for CliExecutor: the dispatch creates the orchestra task tree
    (as `dispatch --isolate always` does) and answers `delegate`."""

    def __init__(self, pkgs=None):
        self.pkgs = pkgs
        self.tokens = {}
        self.calls = []

    def prepare(self, prompt_files, cls, workdir, task, run_id):
        token = "dsp-%012d" % len(self.tokens)
        self.tokens[token] = {"prompt_files": list(prompt_files), "workdir": workdir,
                              "task": task, "class": cls}
        return token

    def dispatch(self, token, cls=None, exhausted=(), no_resume=False):
        spec = self.tokens[token]
        pid = spec["task"][len("pkg-"):]
        self.calls.append(pid)
        if self.pkgs is not None and pid not in self.pkgs:
            raise AssertionError("unexpected CLI dispatch for %s" % pid)
        created = agent_exec.isolate_create(spec["workdir"], spec["task"], backend="git")
        return {
            "status": "delegate", "executor": "claude", "model": "opus",
            "effort": None, "agent_type": None,
            "isolation": {"isolate": True, "path": created["path"],
                          "workdir": created["path"]},
        }


class EditExecutor(DelegateExecutor):
    """A non-Claude package: edits its orchestra tree and answers ok."""

    def __init__(self, files, delegate_pkgs):
        DelegateExecutor.__init__(self)
        self.files = files
        self.delegate_pkgs = delegate_pkgs

    def dispatch(self, token, cls=None, exhausted=(), no_resume=False):
        spec = self.tokens[token]
        pid = spec["task"][len("pkg-"):]
        if pid in self.delegate_pkgs:
            return DelegateExecutor.dispatch(self, token, cls, exhausted, no_resume)
        self.calls.append(pid)
        created = agent_exec.isolate_create(spec["workdir"], spec["task"], backend="git")
        for rel, text in self.files[pid].items():
            _write(os.path.join(created["path"], rel), text)
        return {"status": "ok", "answer": "done", "executor": "pi",
                "isolation": {"isolate": True, "path": created["path"],
                              "workdir": created["path"]}}


class NoDispatch(DelegateExecutor):
    def __init__(self):
        DelegateExecutor.__init__(self, pkgs=())


# --- fixture ------------------------------------------------------------------------


class _OrcaRepo(unittest.TestCase):
    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = os.path.realpath(tempfile.mkdtemp(prefix="orch-orca-"))
        self.repo = os.path.join(self.tmp, "repo")
        os.makedirs(self.repo)
        _git(self.repo, "init", "-q", "-b", "main", ".")
        _git(self.repo, "config", "user.email", "t@example.com")
        _git(self.repo, "config", "user.name", "T")
        with open(os.path.join(self.repo, "shared.txt"), "w") as fh:
            fh.writelines(_BASE_LINES)
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "init")

        self.home = os.path.join(self.tmp, "home")
        os.makedirs(os.path.join(self.home, ".claude"))
        self._orig_home = os.environ.get("HOME")
        os.environ["HOME"] = self.home
        self._orig_cwd = os.getcwd()
        os.chdir(self.repo)

        self.orca_dir = os.path.join(self.tmp, "orca")
        os.makedirs(self.orca_dir)
        os.environ["FAKE_ORCA_DIR"] = self.orca_dir
        self.orca_bin = os.path.join(self.orca_dir, "orca")
        with open(self.orca_bin, "w") as fh:
            fh.write(_FAKE_ORCA % {"python": sys.executable, "trust": _TRUST_SCREEN})
        os.chmod(self.orca_bin, 0o755)
        self.scenario({})

        self.wave_dir = os.path.join(self.tmp, "wave")
        os.makedirs(os.path.join(self.wave_dir, "specs"))
        self.state_path = os.path.join(self.wave_dir, "state.json")
        self.plan_path = os.path.join(self.wave_dir, "plan.json")
        _write(os.path.join(self.wave_dir, "preamble.md"), "preamble\n")
        self.config("")

    def tearDown(self):
        os.chdir(self._orig_cwd)
        if self._orig_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = self._orig_home
        os.environ.pop("FAKE_ORCA_DIR", None)
        shutil.rmtree(self.tmp, ignore_errors=True)
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def scenario(self, data):
        with open(os.path.join(self.orca_dir, "scenario.json"), "w") as fh:
            json.dump(data, fh)

    def config(self, orca_yaml, checks=""):
        text = checks + "orca:\n  startup_timeout: 5\n  task_timeout: 5\n" + textwrap.indent(
            textwrap.dedent(orca_yaml), "  ")
        _write(os.path.join(self.home, ".claude", "orchestra.yaml"), text)

    def plan(self, ids, cls="standard"):
        entries = []
        for pid in ids:
            spec = os.path.join(self.wave_dir, "specs", pid + ".md")
            _write(spec, "spec for %s\n" % pid)
            entries.append({"id": pid, "spec": spec, "cls": cls, "depends_on": [],
                            "files_owned": [pid.lower() + ".txt"]})
        with open(self.plan_path, "w") as fh:
            json.dump({"preamble": [os.path.join(self.wave_dir, "preamble.md")],
                       "packages": entries}, fh)

    def orca(self, binary=None):
        return agent_exec_orca.OrcaExecutor.from_config(
            binary=binary or self.orca_bin, poll=0)

    def run_wave(self, executor, orca=None, **overrides):
        opts = agent_exec_wave_run.default_opts()
        opts.update({"plan": self.plan_path, "state": self.state_path,
                     "into": "wave-int", "repo": self.repo, "run_id": "r1234567"})
        opts.update(overrides)
        report = agent_exec_wave_run.run_wave(
            opts, executor=executor, orca=orca if orca is not None else self.orca())
        return report, agent_exec_wave_run.exit_code_for(report)

    # -- inspection --

    def calls(self, verb=None):
        try:
            with open(os.path.join(self.orca_dir, "calls.jsonl")) as fh:
                rows = [json.loads(line) for line in fh if line.strip()]
        except OSError:
            return []
        return [r for r in rows if verb is None or " ".join(r[:2]) == verb]

    def prompts(self):
        return [r for r in self.calls("terminal send")
                if "--text" in r and "Read the worker prompt from" in r[r.index("--text") + 1]]

    def opt(self, row, name):
        return row[row.index(name) + 1]

    def state(self):
        return agent_exec_wave.StateStore(self.state_path).load()

    def status(self, pid):
        return self.state()["packages"][pid]["status"]

    def needs(self):
        return [(n["id"], n["kind"]) for n in self.state()["needs"]]

    def need_detail(self, pid):
        return json.loads([n for n in self.state()["needs"] if n["id"] == pid][-1]["detail"])

    def int_file(self, rel):
        path = os.path.join(self.state()["integration"]["path"], rel)
        return _read(path) if os.path.exists(path) else None

    def events(self, name):
        path = os.path.join(self.wave_dir, "state.events.jsonl")
        with open(path) as fh:
            rows = [json.loads(line) for line in fh if line.strip()]
        return [r for r in rows if r["event"] == name]

    def registry(self):
        try:
            with open(os.path.join(self.wave_dir, "orca-sessions.json")) as fh:
                return json.load(fh)
        except OSError:
            return {}


def _ok(files=None, summary="done", after_waits=0):
    return {"files": files or {}, "result": {"status": "ok", "summary": summary},
            "after_waits": after_waits}


# --- tests ------------------------------------------------------------------------------


class HappyPathTests(_OrcaRepo):
    def test_claude_package_runs_in_orca_and_is_integrated(self):
        self.plan(["A"])
        self.scenario({"prompts": {"a": [_ok({"a.txt": "from orca\n"})]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.int_file("a.txt"), "from orca\n")

        create = self.calls("worktree create")
        self.assertEqual(len(create), 1)
        int_branch = _git(self.state()["integration"]["path"],
                          "rev-parse", "--abbrev-ref", "HEAD").stdout.strip()
        self.assertEqual(self.opt(create[0], "--base-branch"), int_branch)
        self.assertEqual(self.opt(create[0], "--name"), "wave-r1234567-a")
        self.assertEqual(len(self.prompts()), 1)
        terminal = self.calls("terminal create")[0]
        command = self.opt(terminal, "--command")
        self.assertIn("claude --model opus", command)
        # Seen live: spec/preamble and the result file sit outside the
        # worktree, and without --add-dir the session stalls on a dialog.
        state_dir = os.path.dirname(os.path.abspath(self.state_path))
        self.assertIn("--add-dir " + shlex.quote(state_dir), command)
        self.assertIn("--add-dir " + shlex.quote(os.path.join(self.wave_dir, "specs")), command)

        pkg = self.state()["packages"]["A"]
        self.assertEqual(pkg["executor"], "orca")
        self.assertTrue(pkg["session"])
        self.assertTrue(pkg["tree"].endswith("wave-r1234567-a"))
        # Worktree and terminal are gone afterwards; the dispatch's own tree too.
        self.assertEqual(len(self.calls("worktree rm")), 1)
        self.assertEqual(len(self.calls("terminal close")), 1)
        self.assertFalse(os.path.exists(pkg["tree"]))
        self.assertEqual(self.registry(), {})
        branches = _git(self.repo, "branch", "--list", "orchestra/pkg-A").stdout.strip()
        self.assertEqual(branches, "")
        # The prompt names the Orca worktree as WORKING TREE.
        context = _read(os.path.join(self.wave_dir, "context", "A.md"))
        self.assertIn("WORKING TREE: %s" % pkg["tree"], context)
        ends = [json.loads(e["detail"]) for e in self.events("dispatch-end")]
        self.assertEqual(ends[0]["executor"], "orca")
        actions = [json.loads(e["detail"])["action"] for e in self.events("orca-session")]
        self.assertEqual(actions, ["start", "close"])

    def test_add_dirs_and_permission_mode_are_rendered(self):
        configured = os.path.join(self.repo, "configured")
        os.makedirs(configured)
        self.config("permission_mode: auto\nadd_dirs: [configured, ~/orca-extra]\n")
        self.plan(["A"])
        self.scenario({"prompts": {"a": [_ok({"a.txt": "a\n"})]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 0, report)
        command = self.opt(self.calls("terminal create")[0], "--command")
        self.assertIn("--permission-mode auto", command)
        self.assertIn("--add-dir " + shlex.quote(self.repo), command)
        self.assertIn("--add-dir " + shlex.quote(configured), command)
        self.assertIn("--add-dir " + shlex.quote(os.path.join(self.home, "orca-extra")), command)

    def test_setup_is_passed_and_carry_event_is_emitted(self):
        self.config("setup: inherit\n")
        _write(os.path.join(self.repo, "CLAUDE.local.md"), "local\n")
        self.plan(["A"])
        self.scenario({"prompts": {"a": [_ok({"a.txt": "a\n"})]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 0, report)
        create = self.calls("worktree create")[0]
        self.assertEqual(self.opt(create, "--setup"), "inherit")
        carry = [json.loads(e["detail"]) for e in self.events("orca-session")
                 if json.loads(e["detail"])["action"] == "carry"]
        self.assertEqual(carry[0]["files"], 1)

    def test_invalid_setup_is_rejected(self):
        self.config("setup: nope\n")
        self.plan(["A"])
        self.scenario({"prompts": {"a": [_ok({"a.txt": "a\n"})]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.status("A"), "needs")
        self.assertIn("invalid orca.setup", self.need_detail("A")["detail"])

    def test_invalid_and_bypass_permission_modes_are_rejected(self):
        for value in ("nope", "bypassPermissions"):
            with self.subTest(value=value):
                self.config("permission_mode: %s\n" % value)
                self.plan(["A"])
                report, rc = self.run_wave(DelegateExecutor())
                self.assertEqual(rc, 1, report)
                self.assertEqual(self.need_detail("A")["orca"], "error")

    def test_adopted_while_running(self):
        self.plan(["A"])
        seen = {}
        orca = self.orca()
        original = orca.prompt

        def prompt(session, files, result_path, timeout):
            _, entry = agent_exec._resolve_worktree(self.repo, "pkg-A")
            seen["path"] = entry and entry.get("path")
            seen["files"] = list(files)
            return original(session, files, result_path, timeout)

        orca.prompt = prompt
        self.scenario({"prompts": {"a": [_ok({"a.txt": "x\n"})]}})
        report, rc = self.run_wave(DelegateExecutor(), orca=orca)
        self.assertEqual(rc, 0, report)
        self.assertTrue(seen["path"].endswith("wave-r1234567-a"))
        self.assertEqual(seen["files"][-1], os.path.join(self.wave_dir, "specs", "A.md"))

    def test_self_verify_failure_goes_to_the_same_terminal(self):
        self.config("", checks=_NO_BAD_CHECK)
        self.plan(["A"])
        self.scenario({"prompts": {"a": [_ok({"a.txt": "BAD\n"}), _ok({"a.txt": "good\n"})]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 0, report)
        self.assertEqual(self.int_file("a.txt"), "good\n")
        self.assertEqual(len(self.calls("terminal create")), 1)
        prompts = self.prompts()
        self.assertEqual(len(prompts), 2)
        self.assertEqual(self.opt(prompts[0], "--terminal"), self.opt(prompts[1], "--terminal"))
        correction = os.path.join(self.wave_dir, "corrections", "A.md")
        second = self.opt(prompts[1], "--text")
        self.assertIn("from %s and carry it out" % correction, second)
        self.assertNotIn("preamble.md", second)

    def test_escalate_via_result_file(self):
        self.plan(["A"])
        self.scenario({"prompts": {"a": [{"result": {"status": "escalate",
                                                     "summary": "spec contradicts code"}}]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.needs(), [("A", "escalate")])
        self.assertIn("ESCALATE: spec contradicts code",
                      self.state()["needs"][0]["detail"])
        # The session stays alive for the instructor / a later re-dispatch.
        self.assertIn("A", self.registry())
        self.assertEqual(self.calls("worktree rm"), [])


class StartupTests(_OrcaRepo):
    def test_trust_dialog_without_auto_trust_is_a_need(self):
        self.plan(["A"])
        self.scenario({"trust": True})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.needs(), [("A", "delegate")])
        detail = self.need_detail("A")
        self.assertEqual(detail["orca"], "trust")
        self.assertTrue(detail["token"].startswith("dsp-"))
        self.assertEqual(self.calls("terminal send"), [])

    def test_trust_dialog_with_auto_trust_is_accepted(self):
        self.config("auto_trust: true\n")
        self.plan(["A"])
        self.scenario({"trust": True, "prompts": {"a": [_ok({"a.txt": "a\n"})]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 0, report)
        sends = self.calls("terminal send")
        # Arrow alone first, then Enter alone once "Yes" shows selected: the
        # fake, like the real TUI, exits on an arrow+Enter in one send.
        self.assertEqual(self.opt(sends[0], "--text"), "\x1b[B")
        self.assertNotIn("--enter", sends[0])
        self.assertIn("--enter", sends[1])
        self.assertNotIn("--text", sends[1])
        self.assertEqual(len(self.prompts()), 1)
        actions = [json.loads(e["detail"])["action"] for e in self.events("orca-session")]
        self.assertIn("trust", actions)

    def test_startup_timeout_is_stalled(self):
        self.config("startup_timeout: 1\n")
        self.plan(["A"])
        self.scenario({"never_ready": True})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.need_detail("A")["orca"], "stalled")
        self.assertEqual(self.prompts(), [])


class PromptWaitTests(_OrcaRepo):
    def test_task_timeout(self):
        self.config("task_timeout: 1\n")
        self.plan(["A"])
        self.scenario({"prompts": {"a": [{"files": {"a.txt": "half\n"}}]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.needs(), [("A", "delegate")])
        self.assertEqual(self.need_detail("A")["orca"], "timeout")

    def test_premature_idle_keeps_waiting(self):
        self.plan(["A"])
        self.scenario({"prompts": {"a": [_ok({"a.txt": "slow\n"}, after_waits=3)]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 0, report)
        self.assertGreaterEqual(len(self.calls("terminal wait")), 3)
        self.assertEqual(self.int_file("a.txt"), "slow\n")

    def test_choice_dialog_is_stalled(self):
        self.plan(["A"])
        self.scenario({"prompts": {"a": [{"dialog": True}]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.need_detail("A")["orca"], "stalled")
        detail = self.need_detail("A")["detail"]
        self.assertIn("Do you want to make this edit", detail)
        self.assertIn("terminal term-1", detail)
        self.assertIn("result:", detail)
        self.assertIn("approve it in Orca (terminal term-1), then run:", detail)
        self.assertIn("agent-exec wave mark --state", detail)
        self.assertIn("--await A", detail)
        self.assertIn("orca.add_dirs", detail)


class ResumeTests(_OrcaRepo):
    def test_mark_await_resumes_without_resending_prompt(self):
        self.plan(["A"])
        self.scenario({"prompts": {"a": [{"dialog": True,
                                           "files": {"a.txt": "approved\n"},
                                           "result": {"status": "ok", "summary": "done"},
                                           "after_waits": 1}]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 1, report)
        sends_before = len(self.prompts())
        fake_state_path = os.path.join(self.orca_dir, "state.json")
        fake = json.loads(_read(fake_state_path))
        fake["terminals"]["term-1"]["pending"]["dialog"] = False
        with open(fake_state_path, "w") as fh:
            json.dump(fake, fh)
        with contextlib.redirect_stdout(io.StringIO()):
            marked = agent_exec_wave_run.cmd_wave_mark(
                ["--state", self.state_path, "--await", "A"])
        self.assertEqual(marked, 0)
        report, rc = self.run_wave(NoDispatch())
        self.assertEqual(rc, 0, report)
        self.assertEqual(len(self.prompts()), sends_before)
        self.assertEqual(self.int_file("a.txt"), "approved\n")

    def test_mark_await_without_live_session_exits_two(self):
        self.plan(["A"])
        store = agent_exec_wave.StateStore(self.state_path)
        store.init(self.plan_path, ["A"], "wave-int")
        store.add_need("A", "delegate", json.dumps({"orca": "stalled"}))
        with contextlib.redirect_stderr(io.StringIO()):
            rc = agent_exec_wave_run.cmd_wave_mark(
                ["--state", self.state_path, "--await", "A"])
        self.assertEqual(rc, 2)

    def test_resume_reuses_live_terminal_and_refreshes_in_place(self):
        self.plan(["A", "B"])
        self.scenario({"prompts": {"a": [
            {"files": {"a.txt": "first\n"},
             "result": {"status": "escalate", "summary": "need a decision"}},
            _ok({"a.txt": "second\n"}),
        ]}})
        report, rc = self.run_wave(EditExecutor({"B": {"b.txt": "b\n"}}, ("A",)))
        self.assertEqual(rc, 1, report)
        self.assertEqual(report["integrated"], ["B"])
        session = self.registry()["A"]
        handle, path = session["terminal"], session["worktree"]
        baseline_before = agent_exec._read_baseline(path)

        store = agent_exec_wave.StateStore(self.state_path)
        store.set_status("A", "pending", detail="decided")
        store.clear_need("A")

        # A new runner process: only the registry knows about the session.
        report, rc = self.run_wave(NoDispatch())
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A", "B"])
        self.assertEqual(self.int_file("a.txt"), "second\n")
        self.assertEqual(self.int_file("b.txt"), "b\n")
        self.assertEqual(len(self.calls("terminal create")), 1)
        prompts = self.prompts()
        self.assertEqual([self.opt(p, "--terminal") for p in prompts], [handle, handle])
        # Refreshed in place: same path, new baseline, note in the context file.
        refresh = [json.loads(e["detail"]) for e in self.events("refresh")]
        self.assertEqual(refresh[-1]["status"], "ok")
        self.assertEqual(self.state()["packages"]["A"]["tree"], path)
        self.assertIn("b.txt", _read(os.path.join(self.wave_dir, "context", "A.md")))
        self.assertNotEqual(baseline_before, None)
        actions = [json.loads(e["detail"])["action"] for e in self.events("orca-session")]
        self.assertIn("reuse", actions)

    def test_terminal_whose_claude_exited_is_not_reused(self):
        # Seen live: the terminal outlives Claude (a bare shell is left), so
        # "terminal read works" must not count as a reusable session.
        self.plan(["A"])
        self.scenario({"prompts": {"a": [
            {"files": {"a.txt": "first\n"},
             "result": {"status": "escalate", "summary": "need a decision"}},
            _ok({"a.txt": "second\n"}),
        ]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 1, report)
        old = self.registry()["A"]["terminal"]
        fake_state_path = os.path.join(self.orca_dir, "state.json")
        fake = json.loads(_read(fake_state_path))
        fake["terminals"][old]["exited"] = True
        with open(fake_state_path, "w") as fh:
            json.dump(fake, fh)

        store = agent_exec_wave.StateStore(self.state_path)
        store.set_status("A", "pending", detail="decided")
        store.clear_need("A")
        report, rc = self.run_wave(NoDispatch())
        self.assertEqual(rc, 0, report)
        self.assertEqual(self.int_file("a.txt"), "second\n")
        self.assertEqual(len(self.calls("terminal create")), 2)
        prompts = self.prompts()
        self.assertEqual(self.opt(prompts[0], "--terminal"), old)
        self.assertNotEqual(self.opt(prompts[1], "--terminal"), old)

    def test_dead_terminal_is_replaced_on_the_same_worktree(self):
        self.config("keep_sessions: false\n", checks=_NO_BAD_CHECK)
        self.plan(["A"])
        self.scenario({"prompts": {"a": [_ok({"a.txt": "BAD\n"}), _ok({"a.txt": "ok\n"})]}})
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(rc, 0, report)
        creates = self.calls("terminal create")
        self.assertEqual(len(creates), 2)
        self.assertEqual(self.opt(creates[0], "--worktree"), self.opt(creates[1], "--worktree"))
        self.assertEqual(len(self.calls("worktree create")), 1)


class AvailabilityTests(_OrcaRepo):
    def test_orca_absent_is_todays_delegate_need(self):
        self.plan(["A"])
        missing = os.path.join(self.tmp, "no-such-orca")
        report, rc = self.run_wave(DelegateExecutor(), orca=self.orca(binary=missing))
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.needs(), [("A", "delegate")])
        detail = self.need_detail("A")
        self.assertEqual(sorted(detail), ["agent_type", "effort", "model", "token", "tree"])
        self.assertEqual(self.calls(), [])

    def test_enabled_true_and_absent_stops(self):
        self.config("enabled: true\n")
        self.plan(["A"])
        missing = os.path.join(self.tmp, "no-such-orca")
        report, rc = self.run_wave(DelegateExecutor(pkgs=()), orca=self.orca(binary=missing))
        self.assertEqual(rc, 5, report)
        self.assertTrue(report["reason"].startswith("orca unavailable:"), report)

    def test_enabled_false_never_calls_orca(self):
        self.config("enabled: false\n")
        self.plan(["A"])
        report, rc = self.run_wave(DelegateExecutor())
        self.assertEqual(self.needs(), [("A", "delegate")])
        self.assertEqual(self.calls(), [])

    def test_status_not_ok_is_unavailable(self):
        self.scenario({"status_ok": False})
        ok, reason = self.orca().available()
        self.assertFalse(ok)
        self.assertIn("orca status not ok", reason)

    def test_injected_executor_without_orca_never_reads_config(self):
        self.plan(["A"])
        opts = agent_exec_wave_run.default_opts()
        opts.update({"plan": self.plan_path, "state": self.state_path,
                     "into": "wave-int", "repo": self.repo})
        report = agent_exec_wave_run.run_wave(opts, executor=DelegateExecutor())
        self.assertEqual(report["needs"], [{"id": "A", "kind": "delegate"}])
        self.assertEqual(self.calls(), [])


class UnitTests(unittest.TestCase):
    def test_normalize_enabled(self):
        self.assertIs(agent_exec_orca.normalize_enabled(True), True)
        self.assertIs(agent_exec_orca.normalize_enabled("false"), False)
        self.assertEqual(agent_exec_orca.normalize_enabled("auto"), "auto")
        self.assertEqual(agent_exec_orca.normalize_enabled(None), "auto")

    def test_defaults_come_from_agent_exec(self):
        cfg = agent_exec_orca.merged_config({"models": {"light": "haiku"}})
        self.assertEqual(cfg["models"], {"light": "haiku", "standard": "opus", "deep": "opus"})
        self.assertEqual(cfg["task_timeout"], agent_exec.DEFAULTS["orca"]["task_timeout"])

    def test_screen_classifiers(self):
        self.assertTrue(agent_exec_orca._is_trust(_TRUST_SCREEN))
        self.assertTrue(agent_exec_orca._has_prompt_box(["", "❯ "]))
        self.assertFalse(agent_exec_orca._is_dialog(["", "❯ "]))
        self.assertTrue(agent_exec_orca._is_dialog(["❯ 1. Yes", "  2. No"]))
        self.assertTrue(agent_exec_orca._is_dialog(["❯ No, exit"]))
        for screen in (["  No matches found for pattern"],
                       ["● Done", "  No issues found", "❯ "],
                       ["  Yes, the file exists"],
                       ["Cancel the build if needed"],
                       ["Allow list updated"]):
            self.assertFalse(agent_exec_orca._is_dialog(screen), screen)


if __name__ == "__main__":
    unittest.main()
