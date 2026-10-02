# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for `agent-exec wave run` / `wave mark` (agent_exec_wave_run.py).

Real temp git repos throughout; the executor is a fake that creates the task
worktree the way `dispatch --isolate always --workdir <integration path>
--task pkg-<id>` does and edits files in it.

Run with: uv run test_agent_exec_wave_run.py
"""

import contextlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402
import agent_exec_wave  # noqa: E402
import agent_exec_wave_run  # noqa: E402
import agent_exec_ui  # noqa: E402

# Heartbeats are machine-shared (`~/.claude/orchestra/alive`); redirect this
# whole suite -- including every CLI subprocess it spawns -- into a throwaway
# directory so a test run never writes into the real user's home.
_ALIVE_TMP = tempfile.mkdtemp(prefix="orch-alive-")
os.environ["ORCHESTRA_ALIVE_DIR"] = _ALIVE_TMP
agent_exec._heartbeat_dir_cache = _ALIVE_TMP

# `wave run` appends to the machine-shared wave registry; keep it out of $HOME.
_REGISTRY_TMP = tempfile.mkdtemp(prefix="orch-wave-registry-")
os.environ["ORCHESTRA_WAVE_REGISTRY"] = os.path.join(_REGISTRY_TMP, "waves.jsonl")

_GIT_ENV = dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")

_BASE_LINES = ["line %02d\n" % n for n in range(1, 21)]

# A check that fails whenever any tracked .txt file contains BAD.
_NO_BAD_CHECK = (
    "checks:\n"
    "  items:\n"
    "    - name: nobad\n"
    "      run: \"! grep -rqs BAD --include=*.txt .\"\n"
)


def _git(cwd, *args):
    return subprocess.run(
        ["git"] + list(args), cwd=cwd, capture_output=True, text=True,
        check=True, env=_GIT_ENV,
    )


def _write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as fh:
        fh.write(text)


def _read(path):
    with open(path) as fh:
        return fh.read()


# --- fake executor -----------------------------------------------------------


def edit(files, answer="done", session=None, executor="copilot"):
    """A round that creates/reuses the task tree and writes `files` into it."""
    def round_fn(spec, exhausted):
        created = agent_exec.isolate_create(spec["workdir"], spec["task"], backend="git")
        path = created["path"]
        for rel, text in files.items():
            _write(os.path.join(path, rel), text)
        return {
            "status": "ok", "answer": answer, "session_id": session,
            "executor": executor,
            "isolation": {"isolate": True, "path": path, "workdir": path},
        }
    return round_fn


def result(payload, files=None):
    """A round returning `payload` verbatim (after optional edits)."""
    def round_fn(spec, exhausted):
        out = dict(payload)
        if files is not None:
            created = agent_exec.isolate_create(spec["workdir"], spec["task"], backend="git")
            path = created["path"]
            for rel, text in files.items():
                _write(os.path.join(path, rel), text)
            out["isolation"] = {"isolate": True, "path": path, "workdir": path}
        return out
    return round_fn


class FakeExecutor(object):
    def __init__(self, rounds, on_dispatch=None):
        self.rounds = {pid: list(fns) for pid, fns in rounds.items()}
        self.tokens = {}
        self.calls = []
        self.on_dispatch = on_dispatch

    def prepare(self, prompt_files, cls, workdir, task, run_id):
        token = "tok-%d" % len(self.tokens)
        self.tokens[token] = {
            "prompt_files": list(prompt_files), "class": cls, "workdir": workdir,
            "task": task, "run_id": run_id,
        }
        return token

    def dispatch(self, token, cls=None, exhausted=(), no_resume=False):
        spec = self.tokens[token]
        pid = spec["task"][len("pkg-"):]
        self.calls.append({
            "pkg": pid, "token": token, "exhausted": list(exhausted),
            "no_resume": no_resume, "prompt_files": list(spec["prompt_files"]),
            "workdir_head": _git(spec["workdir"], "rev-parse", "HEAD").stdout.strip(),
        })
        if self.on_dispatch is not None:
            self.on_dispatch(pid, spec)
        fns = self.rounds.get(pid) or []
        if not fns:
            raise AssertionError("unexpected dispatch for %s" % pid)
        return fns.pop(0)(spec, exhausted)

    def pkgs_dispatched(self):
        return [c["pkg"] for c in self.calls]


class NoDispatch(FakeExecutor):
    def __init__(self):
        FakeExecutor.__init__(self, {})


# --- fixture ------------------------------------------------------------------


class _WaveRepo(unittest.TestCase):
    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = os.path.realpath(tempfile.mkdtemp(prefix="orch-wave-run-"))
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
        self.ui_patch = mock.patch.object(
            agent_exec_ui, "start_or_reuse", return_value={"url": "http://127.0.0.1:1/"})
        self.ui_mock = self.ui_patch.start()

        self.wave_dir = os.path.join(self.tmp, "wave")
        os.makedirs(os.path.join(self.wave_dir, "specs"))
        self.state_path = os.path.join(self.wave_dir, "state.json")
        self.plan_path = os.path.join(self.wave_dir, "plan.json")
        _write(os.path.join(self.wave_dir, "preamble.md"), "preamble\n")

    def tearDown(self):
        self.ui_patch.stop()
        os.chdir(self._orig_cwd)
        if self._orig_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = self._orig_home
        shutil.rmtree(self.tmp, ignore_errors=True)
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def checks(self, text):
        _write(os.path.join(self.home, ".claude", "orchestra.yaml"), text)

    def plan(self, packages):
        entries = []
        for pkg in packages:
            spec = os.path.join(self.wave_dir, "specs", pkg["id"] + ".md")
            _write(spec, "spec for %s\n" % pkg["id"])
            entry = {"id": pkg["id"], "spec": spec, "cls": pkg.get("cls", "light"),
                     "depends_on": pkg.get("depends_on", []),
                     "files_owned": pkg.get("files_owned", [pkg["id"].lower() + ".txt"])}
            entries.append(entry)
        with open(self.plan_path, "w") as fh:
            json.dump({"preamble": [os.path.join(self.wave_dir, "preamble.md")],
                       "packages": entries}, fh)

    def opts(self, **overrides):
        opts = agent_exec_wave_run.default_opts()
        opts.update({"plan": self.plan_path, "state": self.state_path,
                     "into": "wave-int", "repo": self.repo})
        opts.update(overrides)
        return opts

    def run_wave(self, executor, **overrides):
        report = agent_exec_wave_run.run_wave(self.opts(**overrides), executor=executor)
        return report, agent_exec_wave_run.exit_code_for(report)

    def state(self):
        return agent_exec_wave.StateStore(self.state_path).load()

    def status(self, pid):
        return self.state()["packages"][pid]["status"]

    def int_path(self):
        return self.state()["integration"]["path"]

    def int_file(self, rel):
        path = os.path.join(self.int_path(), rel)
        return _read(path) if os.path.exists(path) else None

    def need_kinds(self):
        return [(n["id"], n["kind"]) for n in self.state()["needs"]]


# --- tests --------------------------------------------------------------------


class HappyPathTests(_WaveRepo):
    def test_plan_warnings_are_emitted_and_run_continues(self):
        self.plan([{"id": "A", "files_owned": ["a.txt"]}])
        _write(os.path.join(self.wave_dir, "specs", "A.md"), "削除 old implementation\n")
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertIn("plan-warning: A:", stderr.getvalue())
        events = agent_exec_wave.read_events(self.state_path, 100)
        warnings = [event for event in events if event["event"] == "plan-warning"]
        self.assertEqual(len(warnings), 1)
        self.assertEqual(warnings[0]["pkg"], "A")
        self.assertIn("owns only literal paths", warnings[0]["detail"])

    def test_full_cleanup_keeps_next_wave_package_baseline_at_integration_head(self):
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        ex = FakeExecutor({
            "A": [edit({"a.txt": "a\n"})],
            "B": [edit({"b.txt": "b\n"})],
        })
        report, rc = self.run_wave(
            ex, full="echo rewritten > a.txt", full_every=1,
        )
        self.assertEqual(rc, 0, report)
        self.assertEqual(ex.pkgs_dispatched(), ["A", "B"])
        b_call = next(call for call in ex.calls if call["pkg"] == "B")
        b_spec = ex.tokens[b_call["token"]]
        self.assertEqual(b_spec["workdir"], self.int_path())
        b_tree = self.state()["packages"]["B"]["tree"]
        baseline = agent_exec._read_baseline(b_tree)
        self.assertEqual(
            _git(b_tree, "rev-parse", "%s^" % baseline).stdout.strip(),
            b_call["workdir_head"],
        )
        self.assertEqual(
            _git(b_tree, "diff", "--quiet", "%s^" % baseline, baseline).returncode,
            0,
        )
        self.assertEqual(self.int_file("a.txt"), "a\n")

    def test_two_independent_packages_one_wave(self):
        self.plan([{"id": "A"}, {"id": "B"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["status"], "done")
        self.assertEqual(report["waves"], 1)
        self.assertEqual(sorted(report["integrated"]), ["A", "B"])
        self.assertEqual(self.int_file("a.txt"), "a\n")
        self.assertEqual(self.int_file("b.txt"), "b\n")
        self.assertTrue(self.state()["packages"]["A"]["commit"])
        # The user's tree is untouched.
        self.assertFalse(os.path.exists(os.path.join(self.repo, "a.txt")))
        # Full prompt = preamble + context + spec.
        files = ex.calls[0]["prompt_files"]
        self.assertEqual(len(files), 3)
        self.assertTrue(files[0].endswith("preamble.md"))
        self.assertIn(os.path.join("context", ex.calls[0]["pkg"] + ".md"), files[1])
        self.assertTrue(files[2].endswith(".md") and "specs" in files[2])
        context = _read(files[1])
        self.assertIn("ESCALATE", context)
        self.assertIn("shelf", context)
        self.assertIn("carry", context)

    def test_dependency_chain_two_waves(self):
        self.checks(_NO_BAD_CHECK)
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])

        def b_round(spec, exhausted):
            created = agent_exec.isolate_create(spec["workdir"], spec["task"], backend="git")
            # B starts from A's integrated code.
            self.assertEqual(_read(os.path.join(created["path"], "a.txt")), "a\n")
            return edit({"b.txt": "b\n"})(spec, exhausted)

        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [b_round]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["waves"], 2)
        self.assertEqual(ex.pkgs_dispatched(), ["A", "B"])
        self.assertEqual(self.int_file("b.txt"), "b\n")

    def test_overlapping_files_owned_serialize(self):
        self.plan([{"id": "A", "files_owned": ["src/**"]},
                   {"id": "B", "files_owned": ["src/b.txt"]}])
        ex = FakeExecutor({"A": [edit({"src/a.txt": "a\n"})],
                           "B": [edit({"src/b.txt": "b\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["waves"], 2)
        self.assertEqual(ex.pkgs_dispatched(), ["A", "B"])

    def test_max_packages_respected(self):
        self.plan([{"id": "A"}, {"id": "B"}, {"id": "C"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})],
                           "C": [edit({"c.txt": "c\n"})]})
        report, rc = self.run_wave(ex, max_packages=2)
        self.assertEqual(rc, 5, report)
        self.assertEqual(sorted(ex.pkgs_dispatched()), ["A", "B"])
        self.assertEqual(report["pending"], ["C"])
        self.assertEqual(sorted(report["integrated"]), ["A", "B"])


class SelfVerifyTests(_WaveRepo):
    def test_fix_via_correction_delta_with_session(self):
        self.checks(_NO_BAD_CHECK)
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "BAD\n"}, session="s-1"),
                                 edit({"a.txt": "good\n"}, session="s-1")]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertEqual(len(ex.calls), 2)
        correction = ex.calls[1]["prompt_files"]
        self.assertEqual(len(correction), 1)
        self.assertTrue(correction[0].endswith(os.path.join("corrections", "A.md")))
        text = _read(correction[0])
        self.assertIn("nobad", text)
        self.assertIn("do not commit", text)
        self.assertEqual(self.state()["packages"]["A"]["attempts"], 1)
        self.assertEqual(self.int_file("a.txt"), "good\n")

    def test_fix_via_correction_full_without_session(self):
        self.checks(_NO_BAD_CHECK)
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "BAD\n"}), edit({"a.txt": "good\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        files = ex.calls[1]["prompt_files"]
        self.assertEqual(len(files), 4)
        self.assertTrue(files[-1].endswith(os.path.join("corrections", "A.md")))
        self.assertTrue(ex.calls[1]["no_resume"])

    def test_two_failures_need_self_verify_and_block_dependents(self):
        self.checks(_NO_BAD_CHECK)
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        ex = FakeExecutor({"A": [edit({"a.txt": "BAD\n"}), edit({"a.txt": "BAD again\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(report["status"], "done")
        self.assertEqual(report["needs"], [{"id": "A", "kind": "self-verify"}])
        self.assertEqual(report["blocked"], ["B"])
        self.assertEqual(ex.pkgs_dispatched(), ["A", "A"])


class NeedsTests(_WaveRepo):
    def test_escalate(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"}, answer="\n  ESCALATE: spec unclear\n")]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.need_kinds(), [("A", "escalate")])
        self.assertIn("spec unclear", self.state()["needs"][0]["detail"])

    def test_delegate_then_mark_ready_integrates(self):
        self.plan([{"id": "A"}])
        payload = {"status": "delegate", "executor": "claude", "model": "sonnet",
                   "effort": "high", "agent_type": "general-purpose"}
        ex = FakeExecutor({"A": [result(payload, files={"a.txt": "by instructor\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.need_kinds(), [("A", "delegate")])
        detail = json.loads(self.state()["needs"][0]["detail"])
        self.assertEqual(detail["token"], "tok-0")
        self.assertEqual(detail["agent_type"], "general-purpose")
        self.assertTrue(detail["tree"])

        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            mark_rc = agent_exec_wave.cmd_wave(
                ["mark", "--state", self.state_path, "--pkg", "A", "--status", "ready"])
        self.assertEqual(mark_rc, 0)
        self.assertEqual(self.state()["needs"], [])
        self.assertEqual(self.status("A"), "verifying")

        report, rc = self.run_wave(NoDispatch())
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.int_file("a.txt"), "by instructor\n")

    def test_mark_ready_runs_self_verify_before_integration(self):
        self.checks(_NO_BAD_CHECK)
        self.plan([{"id": "A"}])
        delegate = {"status": "delegate", "executor": "claude", "model": "sonnet",
                    "effort": "high", "agent_type": "general-purpose"}
        first = FakeExecutor({"A": [result(delegate, files={"a.txt": "BAD\n"})]})
        report, rc = self.run_wave(first)
        self.assertEqual(rc, 1, report)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(agent_exec_wave.cmd_wave([
                "mark", "--state", self.state_path, "--pkg", "A", "--status", "ready",
            ]), 0)
        second = FakeExecutor({"A": [edit({"a.txt": "BAD again\n"})]})
        report, rc = self.run_wave(second)
        self.assertEqual(rc, 1, report)
        self.assertEqual(report["needs"], [{"id": "A", "kind": "self-verify"}])
        events = agent_exec_wave.read_events(self.state_path, None)
        self.assertTrue(any(e["event"] == "check-start" and e["pkg"] == "A"
                            for e in events))

    def test_mark_rejects_unknown_status(self):
        self.plan([{"id": "A"}])
        agent_exec_wave.StateStore(self.state_path).init(self.plan_path, ["A"], "wave-int")
        with contextlib.redirect_stderr(io.StringIO()):
            rc = agent_exec_wave.cmd_wave(
                ["mark", "--state", self.state_path, "--pkg", "A", "--status", "integrated"])
        self.assertEqual(rc, 2)

    def test_unavailable_retries_with_exhausted(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [result({"status": "unavailable", "executor": "copilot"}),
                                 edit({"a.txt": "a\n"}, executor="codex")]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertEqual(ex.calls[0]["exhausted"], [])
        self.assertEqual(ex.calls[1]["exhausted"], ["copilot"])
        self.assertEqual(self.state()["packages"]["A"]["executor"], "codex")

    def test_all_unavailable_stops_with_package_pending(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [result({"status": "unavailable", "executor": "copilot"}),
                                 result({"status": "unroutable", "route": {}})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 5, report)
        self.assertEqual(report["status"], "stopped")
        self.assertIn("executor unavailable", report["reason"])
        self.assertEqual(self.status("A"), "pending")
        self.assertEqual(report["pending"], ["A"])


class IntegrationTests(_WaveRepo):
    def test_conflict_in_one_wave_rolls_back_later(self):
        self.plan([{"id": "A"}, {"id": "B"}])
        lines_a = list(_BASE_LINES)
        lines_a[4] = "A's line\n"
        lines_b = list(_BASE_LINES)
        lines_b[4] = "B's line\n"
        ex = FakeExecutor({"A": [edit({"shared.txt": "".join(lines_a)})],
                           "B": [edit({"shared.txt": "".join(lines_b)})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.need_kinds(), [("B", "conflict")])
        detail = json.loads(self.state()["needs"][0]["detail"])
        self.assertEqual(detail["stage"], "integrate")
        self.assertIn("shared.txt", detail["files"])
        self.assertIn("A's line", self.int_file("shared.txt"))
        self.assertNotIn("<<<<<<<", self.int_file("shared.txt"))

    def test_post_integration_failure_reverts_culprit(self):
        self.plan([{"id": "A"}, {"id": "B"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})],
                           "B": [edit({"b.txt": "POISON\n"})]})
        report, rc = self.run_wave(ex, gate="! grep -rqs POISON --include=*.txt .")
        self.assertEqual(rc, 1, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.need_kinds(), [("B", "post-integration")])
        self.assertEqual(self.int_file("a.txt"), "a\n")
        self.assertIsNone(self.int_file("b.txt"))

    def test_default_gate_runs_check_over_the_wave(self):
        # A check that is fine in each package's tree on its own but red once
        # both land: the default gate must catch it and bisect to B.
        self.checks(
            "checks:\n"
            "  items:\n"
            "    - name: notboth\n"
            "      run: \"! ( test -f a.txt && test -f b.txt )\"\n"
        )
        self.plan([{"id": "A"}, {"id": "B"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.need_kinds(), [("B", "post-integration")])

    def test_full_red_stops_and_on_green_skipped(self):
        marker = os.path.join(self.tmp, "on-green")
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        report, rc = self.run_wave(ex, full="false", on_green="touch '%s'" % marker)
        self.assertEqual(rc, 5, report)
        # Red even at the last green SHA: nothing to blame, nothing reverted.
        self.assertEqual(report["reason"], "full verification red at the last green SHA")
        self.assertEqual(ex.pkgs_dispatched(), ["A"])
        self.assertFalse(os.path.exists(marker))

    def test_full_green_runs_on_green(self):
        marker = os.path.join(self.tmp, "on-green")
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, full="test -f a.txt", on_green="echo x >> '%s'" % marker)
        self.assertEqual(rc, 0, report)
        self.assertEqual(_read(marker), "x\n")
        events = agent_exec_wave.read_events(self.state_path, None)
        self.assertIn("on-green", [e["event"] for e in events])


class StageEventTests(_WaveRepo):
    def events(self):
        return agent_exec_wave.read_events(self.state_path, None)

    def details(self, name, pkg="__any__"):
        return [json.loads(e["detail"]) for e in self.events()
                if e["event"] == name and (pkg == "__any__" or e["pkg"] == pkg)]

    def test_happy_path_emits_stage_events(self):
        self.plan([{"id": "A"}, {"id": "B"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        report, rc = self.run_wave(ex, gate="true")
        self.assertEqual(rc, 0, report)
        names = [e["event"] for e in self.events()]
        for pid in ("A", "B"):
            self.assertEqual(self.details("dispatch-start", pid), [
                {"attempt": 1, "cls": "light", "kind": "implement"}])
            end = self.details("dispatch-end", pid)
            self.assertEqual(len(end), 1)
            self.assertEqual(end[0]["status"], "ok")
            self.assertEqual(end[0]["executor"], "copilot")
            self.assertIsInstance(end[0]["seconds"], float)
            self.assertEqual(len(self.details("check-start", pid)), 1)
            check_end = self.details("check-end", pid)
            self.assertEqual(len(check_end), 1)
            self.assertIn(check_end[0]["status"], ("pass", "no-checks"))
        self.assertLess(names.index("dispatch-start"), names.index("check-start"))
        self.assertEqual(self.details("integrate-start"), [{"tasks": ["A", "B"]}])
        self.assertEqual(sorted((e["pkg"], json.loads(e["detail"])["status"])
                                for e in self.events() if e["event"] == "integrate-task"),
                         [("A", "applied"), ("B", "applied")])
        self.assertEqual(self.details("integrate-end"), [{"status": "ok"}])
        self.assertEqual(len(self.details("verify-start")), 1)
        self.assertEqual(self.details("verify-end")[0]["status"], "pass")
        # Every mechanical event's detail is a sorted-key JSON string.
        for e in self.events():
            if e["event"] in ("dispatch-start", "integrate-end"):
                self.assertEqual(e["detail"], json.dumps(
                    json.loads(e["detail"]), ensure_ascii=False, sort_keys=True))

    def test_correction_dispatch_is_labelled(self):
        self.checks(_NO_BAD_CHECK)
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [
            edit({"a.txt": "BAD\n"}), edit({"a.txt": "good\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        starts = self.details("dispatch-start", "A")
        self.assertEqual([(d["kind"], d["attempt"]) for d in starts], [
            ("implement", 1), ("correction", 2)])
        self.assertEqual([d["status"] for d in self.details("check-end", "A")],
                         ["fail", "pass"])

    def test_post_integration_failure_emits_bisect_and_revert_with_package_id(self):
        self.checks(
            "checks:\n"
            "  items:\n"
            "    - name: notboth\n"
            "      run: \"! ( test -f a.txt && test -f b.txt )\"\n"
        )
        self.plan([{"id": "A"}, {"id": "B"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        events = self.events()
        probes = [e for e in events if e["event"] == "bisect-probe"]
        self.assertTrue(probes)
        self.assertTrue(all(e["pkg"] in ("A", "B") for e in probes))
        reverts = [e for e in events if e["event"] == "revert"]
        self.assertEqual([(e["pkg"], json.loads(e["detail"])) for e in reverts],
                         [("B", {"result": "reverted"})])
        self.assertFalse(any((e["pkg"] or "").startswith("pkg-") for e in events))
        self.assertEqual(self.details("integrate-start"), [{"tasks": ["A", "B"]}])
        self.assertEqual(self.details("integrate-end"), [{"status": "reverted"}])

    def test_full_and_on_green_events(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        self.run_wave(ex, full="test -f a.txt", on_green="true")
        self.assertEqual(self.details("full-start"), [{}])
        full_end = self.details("full-end")
        self.assertEqual(full_end[0]["status"], "pass")
        self.assertIsInstance(full_end[0]["seconds"], float)
        self.assertEqual(self.details("on-green")[0]["exit"], 0)

    def test_dirty_full_and_pre_dispatch_emit_integration_dirty(self):
        self.plan([{"id": "A"}])
        created = agent_exec.isolate_create(self.repo, "wave-int", backend="git")
        _write(os.path.join(created["path"], "pre-dispatch.txt"), "dirty\n")
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        self.run_wave(ex, full="echo changed > a.txt; echo extra > full.txt")
        events = self.details("integration-dirty")
        self.assertTrue(any(e["after"] == "pre-dispatch" for e in events))
        self.assertTrue(any(e["after"] == "full" for e in events))

    def test_refresh_event(self):
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        # Pre-create B's tree on the old base so wave 2 must refresh it.
        agent_exec.isolate_create(self.repo, "pkg-B", backend="git")
        self.run_wave(ex)
        refreshes = self.details("refresh", "B")
        self.assertTrue(refreshes)
        self.assertEqual(sorted(refreshes[0]), ["files", "status"])
        self.assertIsInstance(refreshes[0]["files"], int)

    def test_registry_entry_written(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        registry = agent_exec_wave.registry_path()
        self.assertTrue(registry.startswith(_REGISTRY_TMP))
        self.run_wave(ex)
        mine = [w for w in agent_exec_wave.list_waves() if w["state"] == self.state_path]
        self.assertEqual(len(mine), 1)
        self.assertEqual(mine[0]["plan"], self.plan_path)
        self.assertEqual(mine[0]["repo"], self.repo)
        self.assertEqual(mine[0]["into"], "wave-int")
        self.assertIsInstance(mine[0]["registered_at"], float)


class StopAndResumeTests(_WaveRepo):
    def test_stop_file_mid_run(self):
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])

        def on_dispatch(pid, spec):
            if pid == "A":
                agent_exec_wave.request_stop(self.state_path, "enough")

        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]},
                          on_dispatch=on_dispatch)
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 5, report)
        self.assertEqual(ex.pkgs_dispatched(), ["A"])
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(report["pending"], ["B"])

    def test_resume_after_crash_verifies_without_redispatch(self):
        self.plan([{"id": "A"}])
        created = agent_exec.isolate_create(self.repo, "wave-int", backend="git",
                                            onto=_git(self.repo, "rev-parse", "HEAD").stdout.strip())
        int_path = created["path"]
        agent_exec._write_role(int_path, "integration")
        pkg = agent_exec.isolate_create(int_path, "pkg-A", backend="git")
        _write(os.path.join(pkg["path"], "a.txt"), "survived\n")
        store = agent_exec_wave.StateStore(self.state_path)
        store.init(self.plan_path, ["A"], "wave-int")
        store.set_status("A", "implementing", tree=pkg["path"])

        report, rc = self.run_wave(NoDispatch())
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.int_file("a.txt"), "survived\n")

    def test_resume_without_changes_goes_back_to_pending(self):
        self.plan([{"id": "A"}])
        store = agent_exec_wave.StateStore(self.state_path)
        store.init(self.plan_path, ["A"], "wave-int")
        store.set_status("A", "implementing")
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertEqual(ex.pkgs_dispatched(), ["A"])

    def test_stale_tree_refreshed_before_dispatch(self):
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        head = _git(self.repo, "rev-parse", "HEAD").stdout.strip()
        created = agent_exec.isolate_create(self.repo, "wave-int", backend="git", onto=head)
        int_path = created["path"]
        agent_exec._write_role(int_path, "integration")
        # A leftover B tree from an earlier attempt, on the old base.
        stale = agent_exec.isolate_create(int_path, "pkg-B", backend="git")
        _write(os.path.join(stale["path"], "b.txt"), "b from before\n")

        def b_round(spec, exhausted):
            created = agent_exec.isolate_create(spec["workdir"], spec["task"], backend="git")
            path = created["path"]
            self.assertEqual(_read(os.path.join(path, "a.txt")), "a\n")
            self.assertEqual(_read(os.path.join(path, "b.txt")), "b from before\n")
            return edit({"b2.txt": "b2\n"})(spec, exhausted)

        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [b_round]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        context = _read(ex.calls[1]["prompt_files"][1])
        self.assertIn("a.txt", context)
        self.assertTrue(ex.calls[1]["no_resume"])
        self.assertEqual(self.int_file("b.txt"), "b from before\n")
        self.assertEqual(self.int_file("b2.txt"), "b2\n")


class _Clock(object):
    def __init__(self, now=1000.0):
        self.now = now
        self.sleeps = []

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class GreenTreeTests(_WaveRepo):
    def events(self, name):
        return [e for e in agent_exec_wave.read_events(self.state_path, None)
                if e["event"] == name]

    def green_path(self):
        return os.path.join(self.wave_dir, "green-tree")

    def test_on_green_runs_in_green_tree_at_integrated_sha(self):
        log = os.path.join(self.tmp, "green.log")
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        cmd = "echo \"{sha} $WAVE_GREEN_SHA $(pwd -P)\" >> '%s'" % log
        report, rc = self.run_wave(ex, full="true", on_green=cmd)
        self.assertEqual(rc, 0, report)
        sha, env_sha, cwd = _read(log).split()
        head = _git(self.int_path(), "rev-parse", "HEAD").stdout.strip()
        self.assertEqual(sha, head)
        self.assertEqual(env_sha, head)
        self.assertEqual(cwd, os.path.realpath(self.green_path()))
        self.assertNotEqual(cwd, os.path.realpath(self.int_path()))

    def test_tree_reused_on_next_green_with_new_sha(self):
        log = os.path.join(self.tmp, "green.log")
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        cmd = "echo \"$WAVE_GREEN_SHA $(pwd -P)\" >> '%s'" % log
        report, rc = self.run_wave(ex, full="true", on_green=cmd)
        self.assertEqual(rc, 0, report)
        lines = [l.split() for l in _read(log).splitlines()]
        self.assertEqual(len(lines), 2)
        self.assertEqual(lines[0][1], lines[1][1])
        self.assertNotEqual(lines[0][0], lines[1][0])

    def test_failing_on_green_does_not_stop(self):
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        report, rc = self.run_wave(ex, full="true", on_green="exit 3")
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A", "B"])
        self.assertEqual([json.loads(e["detail"])["exit"] for e in self.events("on-green")], [3, 3])

    def test_after_green_is_a_usage_error(self):
        opts, err = agent_exec_wave_run.parse_run_args([
            "--plan", "p", "--state", "s", "--into", "i", "--after-green", "true"])
        self.assertIsNone(opts)
        self.assertIn("unknown option: --after-green", err)
        with contextlib.redirect_stderr(io.StringIO()):
            rc = agent_exec_wave.cmd_wave([
                "run", "--plan", self.plan_path, "--state", self.state_path,
                "--into", "i", "--after-green", "true"])
        self.assertEqual(rc, 2)

    def test_green_tree_kept_on_stop(self):
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])

        def on_dispatch(pid, spec):
            if pid == "A":
                agent_exec_wave.request_stop(self.state_path, "enough")

        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]},
                          on_dispatch=on_dispatch)
        report, rc = self.run_wave(ex, full="true", on_green="true")
        self.assertEqual(rc, 5, report)
        self.assertTrue(os.path.isdir(self.green_path()))

    def test_green_tree_removed_on_done(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, full="true", on_green="true")
        self.assertEqual(rc, 0, report)
        self.assertEqual(len(self.events("on-green")), 1)
        self.assertFalse(os.path.exists(self.green_path()))


class ResumeOnResetTests(_WaveRepo):
    def unavailable(self):
        return result({"status": "unavailable", "executor": "copilot"})

    def run_resume(self, ex, clock, until=1100.0, on_sleep=None, **overrides):
        if on_sleep is not None:
            base = clock.sleep

            def sleep(seconds):
                base(seconds)
                on_sleep()
        else:
            sleep = clock.sleep
        with mock.patch.object(agent_exec, "resolve_config", return_value=({}, None)), \
                mock.patch.object(agent_exec, "active_cooldown_expiries",
                                  return_value={"copilot": until}):
            report = agent_exec_wave_run.run_wave(
                self.opts(resume_on_reset=True, **overrides),
                executor=ex, clock=clock, sleep=sleep)
        return report, agent_exec_wave_run.exit_code_for(report)

    def test_waits_until_cooldown_expiry_then_integrates(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [self.unavailable(), self.unavailable(),
                                 edit({"a.txt": "a\n"})]})
        clock = _Clock()
        report, rc = self.run_resume(ex, clock)
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertGreaterEqual(clock.now, 1100.0)
        self.assertAlmostEqual(sum(clock.sleeps), 100.0)
        self.assertTrue(all(0 <= s <= 30 for s in clock.sleeps))
        waits = [e for e in agent_exec_wave.read_events(self.state_path, None)
                 if e["event"] == "resume-wait"]
        self.assertEqual(json.loads(waits[0]["detail"])["until"], 1100.0)

    def test_stop_file_during_wait_stops(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [self.unavailable(), self.unavailable()]})
        clock = _Clock()
        report, rc = self.run_resume(
            ex, clock,
            on_sleep=lambda: agent_exec_wave.request_stop(self.state_path, "enough"))
        self.assertEqual(rc, 5, report)
        self.assertEqual(report["reason"], "stop requested")
        self.assertEqual(len(clock.sleeps), 1)

    def test_stop_at_before_until_stops_without_waiting(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [self.unavailable(), self.unavailable()]})
        clock = _Clock()
        report, rc = self.run_resume(ex, clock, stop_at=1050.0)
        self.assertEqual(rc, 5, report)
        self.assertEqual(report["reason"], "stop-at reached")
        self.assertEqual(clock.sleeps, [])
        self.assertEqual(clock.now, 1000.0)


class UiTests(_WaveRepo):
    def test_ui_url_printed_and_event_written(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertIn("ui: http://127.0.0.1:1/", err.getvalue())
        events = [e for e in agent_exec_wave.read_events(self.state_path, None)
                  if e["event"] == "ui"]
        self.assertEqual(json.loads(events[0]["detail"]), {"url": "http://127.0.0.1:1/"})

    def test_ui_failure_only_warns(self):
        self.ui_mock.side_effect = RuntimeError("boom")
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        self.assertIn("warning: could not start ui: boom", err.getvalue())

    def test_no_ui_skips_start(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, no_ui=True)
        self.assertEqual(rc, 0, report)
        self.ui_mock.assert_not_called()
        opts, err = agent_exec_wave_run.parse_run_args(
            ["--plan", "p", "--state", "s", "--into", "i", "--no-ui"])
        self.assertTrue(opts["no_ui"])


class NotifyTests(_WaveRepo):
    def test_notify_cmd_receives_need_and_done(self):
        log = os.path.join(self.tmp, "notify.log")
        self.plan([{"id": "A"}, {"id": "B"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"}, answer="ESCALATE: no")],
                           "B": [edit({"b.txt": "b\n"})]})
        cmd = ("printf '%%s ' \"$WAVE_EVENT\" >> '%s'; cat >> '%s'; echo >> '%s'"
               % (log, log, log))
        report, rc = self.run_wave(ex, notify_cmd=cmd)
        self.assertEqual(rc, 1, report)
        lines = [l for l in _read(log).splitlines() if l.strip()]
        kinds = [l.split(" ", 1)[0] for l in lines]
        self.assertEqual(sorted(kinds), ["done", "need"])
        need_line = [l for l in lines if l.startswith("need ")][0]
        payload = json.loads(need_line.split(" ", 1)[1])
        self.assertEqual(payload["pkg"], "A")
        self.assertEqual(payload["kind"], "escalate")


class FrozenDispatchTests(_WaveRepo):
    def test_escalate_carries_context_and_marks_scope_for_outside_path(self):
        self.plan([{"id": "A", "files_owned": ["a.txt"]}])

        def carry(pid, spec):
            _write(os.path.join(self.wave_dir, "carry", "A.carry.md"),
                   "please update src/shared.py\n")

        ex = FakeExecutor(
            {"A": [edit({"a.txt": "a\n"},
                        answer="ESCALATE: blocked by src/other.py")]},
            on_dispatch=carry,
        )
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.need_kinds(), [("A", "scope")])
        detail = self.state()["needs"][0]["detail"]
        self.assertIn("--- carry ---", detail)
        self.assertIn("src/shared.py", detail)
        self.assertIn("--- outside files_owned ---", detail)

    def test_escalate_prose_with_dotted_words_is_not_scope(self):
        self.plan([{"id": "A", "files_owned": ["a.txt"]}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"}, answer=(
            "ESCALATE: need a decision on naming. Thanks, e.g. later."
            " Keep a.txt as is."))]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.need_kinds(), [("A", "escalate")])

    def test_mark_widen_writes_override_and_recheck_is_batch(self):
        self.plan([{"id": "A"}, {"id": "B"}])
        delegate = {"status": "delegate", "executor": "claude"}
        ex = FakeExecutor({
            "A": [result(delegate, files={"a.txt": "a\n"})],
            "B": [result(delegate, files={"b.txt": "b\n"})],
        })
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(agent_exec_wave.cmd_wave([
                "mark", "--state", self.state_path, "--pkg", "A",
                "--widen", "src/*.py,docs/*.md",
            ]), 0)
        with open(os.path.join(self.wave_dir, "plan-overrides.json")) as fh:
            self.assertEqual(json.load(fh)["A"]["files_owned_add"],
                             ["src/*.py", "docs/*.md"])
        self.assertEqual(self.status("A"), "pending")
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(agent_exec_wave.cmd_wave([
                "mark", "--state", self.state_path, "--recheck", "B,A",
            ]), 0)
        self.assertEqual(self.status("A"), "verifying")
        self.assertEqual(self.status("B"), "verifying")


    def _stale_b_setup(self, stale_files):
        head = _git(self.repo, "rev-parse", "HEAD").stdout.strip()
        created = agent_exec.isolate_create(self.repo, "wave-int", backend="git", onto=head)
        int_path = created["path"]
        agent_exec._write_role(int_path, "integration")
        stale = agent_exec.isolate_create(int_path, "pkg-B", backend="git")
        for rel, text in stale_files.items():
            _write(os.path.join(stale["path"], rel), text)

    def test_empty_after_refresh_becomes_need(self):
        self.plan([{"id": "A", "files_owned": ["a.txt"]},
                   {"id": "B", "depends_on": ["A"], "files_owned": ["a.txt", "b.txt"]}])
        # B's leftover tree only holds what A is about to integrate, so after
        # the refresh nothing of B's own is left and the worker adds nothing.
        self._stale_b_setup({"a.txt": "a\n"})
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({})]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.need_kinds(), [("B", "empty-after-refresh")])
        self.assertEqual(self.status("A"), "integrated")

    def test_escalate_in_correction_answer_is_a_need_not_a_second_verify(self):
        self.checks(_NO_BAD_CHECK)
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [
            edit({"a.txt": "BAD\n"}),
            edit({"a.txt": "BAD\n"}, answer="ESCALATE: cannot fix this"),
        ]})
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        self.assertEqual(self.need_kinds(), [("A", "escalate")])
        self.assertIn("cannot fix this", self.state()["needs"][0]["detail"])
        starts = [e for e in agent_exec_wave.read_events(self.state_path, None)
                  if e["event"] == "check-start" and e["pkg"] == "A"]
        self.assertEqual(len(starts), 1)

    def test_widen_redispatch_sees_new_files_owned_and_overlap_serializes(self):
        self.plan([{"id": "A", "files_owned": ["a.txt"]},
                   {"id": "B", "files_owned": ["src/b.py"]}])
        first = FakeExecutor({"A": [edit({"a.txt": "a\n"},
                                         answer="ESCALATE: need to change src/shared.py")]})
        report, rc = self.run_wave(first, max_packages=1)
        self.assertEqual(self.need_kinds(), [("A", "scope")], report)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(agent_exec_wave.cmd_wave([
                "mark", "--state", self.state_path, "--pkg", "A", "--widen", "src/*.py",
            ]), 0)
        second = FakeExecutor({"A": [edit({"a.txt": "a\n", "src/shared.py": "s\n"})],
                               "B": [edit({"src/b.py": "b\n"})]})
        report, rc = self.run_wave(second)
        self.assertEqual(rc, 0, report)
        self.assertIn("src/*.py", _read(second.calls[0]["prompt_files"][1]))
        # The widened glob overlaps B's files_owned, so they no longer share a wave.
        self.assertEqual(second.pkgs_dispatched(), ["A", "B"])
        self.assertEqual(report["waves"], 2)

    def test_recheck_batch_dispatches_nothing_and_integrates_next_run(self):
        self.plan([{"id": "A"}, {"id": "B"}])
        delegate = {"status": "delegate", "executor": "claude"}
        ex = FakeExecutor({
            "A": [result(delegate, files={"a.txt": "a\n"})],
            "B": [result(delegate, files={"b.txt": "b\n"})],
        })
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 1, report)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(agent_exec_wave.cmd_wave([
                "mark", "--state", self.state_path, "--recheck", "A,B",
            ]), 0)
        self.assertEqual(self.state()["needs"], [])
        nothing = NoDispatch()
        report, rc = self.run_wave(nothing)
        self.assertEqual(rc, 0, report)
        self.assertEqual(nothing.calls, [])
        self.assertEqual(sorted(report["integrated"]), ["A", "B"])

    def test_recheck_bad_id_exits_2_and_changes_nothing(self):
        self.plan([{"id": "A"}])
        delegate = {"status": "delegate", "executor": "claude"}
        ex = FakeExecutor({"A": [result(delegate, files={"a.txt": "a\n"})]})
        self.run_wave(ex)
        before = self.state()
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            rc = agent_exec_wave.cmd_wave([
                "mark", "--state", self.state_path, "--recheck", "A,ZZ",
            ])
        self.assertEqual(rc, 2)
        self.assertIn("ZZ", err.getvalue())
        after = self.state()
        self.assertEqual(after["packages"]["A"]["status"], before["packages"]["A"]["status"])
        self.assertEqual(after["needs"], before["needs"])

    def _flaky_run(self):
        self.plan([{"id": "A"}, {"id": "B"}, {"id": "C"}])
        flaky_check = {"status": "pass", "files": [], "checks": [], "flaky": ["t_x.py"]}
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})],
                           "C": [edit({"c.txt": "c\n"})]})
        with mock.patch.object(agent_exec_wave_run._Runner, "_check",
                               return_value=flaky_check):
            report, _ = self.run_wave(ex)
        return report

    def test_flaky_event_and_report_default_threshold(self):
        report = self._flaky_run()
        events = [e for e in agent_exec_wave.read_events(self.state_path, None)
                  if e["event"] == "flaky"]
        self.assertEqual(sorted(e["pkg"] for e in events), ["A", "B", "C"])
        self.assertEqual(report["flaky_tests"], [{"file": "t_x.py", "count": 3}])

    def test_flaky_threshold_read_from_project_config(self):
        _write(os.path.join(self.repo, ".claude", "orchestra.yaml"),
               "checks:\n  flaky_threshold: 4\n")
        self.assertEqual(self._flaky_run()["flaky_tests"], [])

    def test_correction_in_same_tree_resumes_while_refreshed_first_dispatch_does_not(self):
        self.checks(_NO_BAD_CHECK)
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        self._stale_b_setup({"b.txt": "b from before\n"})
        ex = FakeExecutor({
            "A": [edit({"a.txt": "a\n"})],
            "B": [edit({"b2.txt": "BAD\n"}, session="s1"),
                  edit({"b2.txt": "fixed\n"}, session="s1")],
        })
        report, rc = self.run_wave(ex)
        self.assertEqual(rc, 0, report)
        b_calls = [c for c in ex.calls if c["pkg"] == "B"]
        self.assertEqual(len(b_calls), 2)
        self.assertTrue(b_calls[0]["no_resume"])
        self.assertFalse(b_calls[1]["no_resume"])


# Red when any tracked .txt holds POISON; names a failing test file like vitest.
_POISON_FULL = (
    "if grep -rqs POISON --include=*.txt .; then "
    "echo ' FAIL  tests/poison.test.ts > poisoned'; exit 1; fi"
)


class RedFullTests(_WaveRepo):
    def events(self, name, pkg="__any__"):
        return [e for e in agent_exec_wave.read_events(self.state_path, None)
                if e["event"] == name and (pkg == "__any__" or e["pkg"] == pkg)]

    def green(self):
        with open(os.path.join(self.wave_dir, "green.json")) as fh:
            return json.load(fh)

    def head(self):
        return _git(self.int_path(), "rev-parse", "HEAD").stdout.strip()

    def assert_int_clean(self):
        path = self.int_path()
        branch = subprocess.run(["git", "symbolic-ref", "-q", "HEAD"], cwd=path,
                                capture_output=True, text=True, env=_GIT_ENV)
        self.assertEqual(branch.returncode, 0, "integration tree is detached")
        self.assertEqual(_git(path, "status", "--porcelain").stdout, "")
        gitdir = _git(path, "rev-parse", "--absolute-git-dir").stdout.strip()
        for name in ("REVERT_HEAD", "sequencer"):
            self.assertFalse(os.path.exists(os.path.join(gitdir, name)), name)

    def test_culprit_reverted_and_redispatched_fresh_with_correction(self):
        self.plan([{"id": "A"}, {"id": "B"}])
        seen = {}
        correction = os.path.join(self.wave_dir, "corrections", "B.md")

        def on_dispatch(pid, spec):
            if pid != "B":
                return
            if "first" not in seen:
                seen["first"] = True
                return
            seen["files"] = list(spec["prompt_files"])
            seen["correction"] = _read(correction)
            tree = self.state()["packages"]["B"]["tree"]
            seen["collected"] = agent_exec._read_collected(tree)
            seen["b_in_int"] = self.int_file("b.txt")

        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})],
                           "B": [edit({"b.txt": "POISON\n"}), edit({"b.txt": "fixed\n"})]},
                          on_dispatch=on_dispatch)
        report, rc = self.run_wave(ex, gate="true", full=_POISON_FULL)
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A", "B"])
        b_calls = [c for c in ex.calls if c["pkg"] == "B"]
        self.assertEqual(len(b_calls), 2)
        self.assertTrue(b_calls[1]["no_resume"])
        self.assertEqual(seen["files"], [
            os.path.join(self.wave_dir, "preamble.md"),
            os.path.join(self.wave_dir, "context", "B.md"),
            os.path.join(self.wave_dir, "specs", "B.md"),
            correction])
        self.assertIn("broke the full verification after integration", seen["correction"])
        self.assertIn("tests/poison.test.ts", seen["correction"])
        self.assertIn("files_owned", seen["correction"])
        self.assertIsNone(seen["collected"])
        self.assertIsNone(seen["b_in_int"])
        # Sent once, then set aside so it is never resent.
        self.assertFalse(os.path.exists(correction))
        self.assertEqual([e["pkg"] for e in self.events("revert")], ["B"])
        probes = self.events("bisect-probe")
        self.assertTrue(probes)
        self.assertTrue(all(e["pkg"] in ("A", "B") for e in probes))
        pending = [e for e in self.events("status", "B") if e["to"] == "pending"]
        self.assertEqual([e["detail"] for e in pending], ["post-full revert"])
        self.assertEqual(self.int_file("a.txt"), "a\n")
        self.assertEqual(self.int_file("b.txt"), "fixed\n")
        self.assertEqual(self.green()["sha"], self.head())
        self.assert_int_clean()

    def test_flaky_full_rescued_by_full_retry(self):
        counter = os.path.join(self.tmp, "runs")
        marker = os.path.join(self.tmp, "on-green")
        full = ("n=$(cat '%s' 2>/dev/null || echo 0); echo $((n+1)) > '%s'; "
                "if [ \"$n\" = 0 ]; then echo ' FAIL  tests/flaky.test.ts'; exit 1; fi"
                % (counter, counter))
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, gate="true", full=full,
                                   full_retry="test -n {failed}",
                                   on_green="touch '%s'" % marker)
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.events("revert"), [])
        self.assertEqual(self.events("bisect-probe"), [])
        flaky = self.events("flaky")
        self.assertEqual(len(flaky), 1)
        self.assertIsNone(flaky[0]["pkg"])
        self.assertEqual(json.loads(flaky[0]["detail"])["files"], ["tests/flaky.test.ts"])
        self.assertEqual(self.green()["sha"], self.head())
        self.assertTrue(os.path.exists(marker))
        self.assert_int_clean()

    def test_no_full_retry_means_no_retry(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "POISON\n"})]})
        self.run_wave(ex, gate="true", full=_POISON_FULL, max_waves=1)
        self.assertEqual(self.events("flaky"), [])
        self.assertEqual([e["pkg"] for e in self.events("revert")], ["A"])
        self.assert_int_clean()

    def test_red_at_last_green_stops_without_revert(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, gate="true", full="false")
        self.assertEqual(rc, 5, report)
        self.assertEqual(report["reason"], "full verification red at the last green SHA")
        self.assertEqual(self.events("revert"), [])
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.int_file("a.txt"), "a\n")
        # Only the run-start base was ever recorded as green.
        self.assertEqual(self.green()["sha"], self.state()["integration"]["base"])
        self.assert_int_clean()

    def _ensure_cfg(self, exists, run):
        self.checks("checks:\n  ensure:\n    - exists: %s\n      run: \"%s\"\n" % (exists, run))

    def test_ensure_runs_before_full_and_creates_missing_file(self):
        self._ensure_cfg("generated.txt", "echo gen > generated.txt")
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, gate="true", full="test -f generated.txt")
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(self.events("revert"), [])
        self.assertEqual(self.need_kinds(), [])
        self.assertEqual(self.green()["sha"], self.head())

    def test_ensure_failure_and_red_at_last_green_is_environment_need(self):
        self._ensure_cfg("generated.txt", "echo ensure-boom; exit 1")
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, gate="true", full="true")
        self.assertEqual(rc, 5, report)
        self.assertIn("environment", report["reason"])
        self.assertEqual(self.need_kinds(), [(None, "environment")])
        self.assertIn("ensure-boom", self.state()["needs"][0]["detail"])
        self.assertEqual(self.events("revert"), [])
        self.assertEqual(self.status("A"), "integrated")
        self.assertEqual(self.int_file("a.txt"), "a\n")
        self.assertIn("environment", self.state()["stopped"]["reason"])
        self.assertEqual(len(self.events("environment")), 1)
        self.assertEqual(self.events("environment")[0]["pkg"], None)
        self.assert_int_clean()

    def test_clear_environment_then_rerun_continues(self):
        self._ensure_cfg("generated.txt", "echo ensure-boom; exit 1")
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, gate="true", full="true")
        self.assertEqual(rc, 5, report)
        self._ensure_cfg("generated.txt", "echo gen > generated.txt")
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = agent_exec_wave.cmd_wave(
                ["mark", "--state", self.state_path, "--clear-environment"])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out.getvalue()), {"environment_cleared": True})
        self.assertEqual(self.need_kinds(), [])
        self.assertIsNone(self.state()["stopped"])
        report, rc = self.run_wave(FakeExecutor({}), gate="true", full="test -f generated.txt")
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["status"], "done")
        self.assertEqual(self.need_kinds(), [])
        self.assertIsNone(self.state()["stopped"])

    def test_second_revert_of_same_package_is_post_integration_need(self):
        self.plan([{"id": "A"}, {"id": "B"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})],
                           "B": [edit({"b.txt": "POISON\n"}),
                                 edit({"b.txt": "POISON again\n"})]})
        report, rc = self.run_wave(ex, gate="true", full=_POISON_FULL)
        self.assertEqual(rc, 1, report)
        self.assertEqual(report["integrated"], ["A"])
        self.assertEqual(ex.pkgs_dispatched().count("B"), 2)
        self.assertEqual(self.need_kinds(), [("B", "post-integration")])
        self.assertIn("tests/poison.test.ts", self.state()["needs"][0]["detail"])
        self.assertEqual([e["pkg"] for e in self.events("revert")], ["B", "B"])
        self.assertIsNone(self.int_file("b.txt"))
        self.assertEqual(self.green()["sha"], self.head())
        self.assert_int_clean()

    def test_two_culprits_found_in_one_red_full_run(self):
        self.plan([{"id": "A"}, {"id": "B"}, {"id": "C"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "POISON\n"}), edit({"a.txt": "a\n"})],
                           "B": [edit({"b.txt": "POISON\n"}), edit({"b.txt": "b\n"})],
                           "C": [edit({"c.txt": "c\n"})]})
        report, rc = self.run_wave(ex, gate="true", full=_POISON_FULL)
        self.assertEqual(rc, 0, report)
        self.assertEqual(report["integrated"], ["A", "B", "C"])
        self.assertEqual([e["pkg"] for e in self.events("revert")], ["A", "B"])
        self.assertEqual(ex.pkgs_dispatched().count("C"), 1)
        self.assertEqual(self.int_file("c.txt"), "c\n")
        self.assertEqual(self.green()["sha"], self.head())
        self.assert_int_clean()

    def test_green_recorded_after_green_full(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, gate="true", full="true")
        self.assertEqual(rc, 0, report)
        self.assertEqual(self.green()["sha"], self.head())
        self.assertIsInstance(self.green()["at"], float)

    def test_without_full_no_green_file(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, gate="true")
        self.assertEqual(rc, 0, report)
        self.assertFalse(os.path.exists(os.path.join(self.wave_dir, "green.json")))

    def test_parse_full_retry(self):
        opts, err = agent_exec_wave_run.parse_run_args([
            "--plan", "p", "--state", "s", "--into", "i", "--full", "f",
            "--full-retry", "npx vitest run {failed}"])
        self.assertIsNone(err)
        self.assertEqual(opts["full_retry"], "npx vitest run {failed}")


class CliTests(_WaveRepo):
    def test_run_requires_plan_state_into(self):
        with contextlib.redirect_stderr(io.StringIO()):
            rc = agent_exec_wave.cmd_wave(["run", "--plan", self.plan_path])
        self.assertEqual(rc, 2)

    def test_parse_opts(self):
        opts, err = agent_exec_wave_run.parse_run_args([
            "--plan", "p", "--state", "s", "--into", "i", "--max-in-flight", "2",
            "--full-every", "3", "--stop-at", "12.5", "--max-waves", "4", "--text",
        ])
        self.assertIsNone(err)
        self.assertEqual(opts["max_in_flight"], 2)
        self.assertEqual(opts["full_every"], 3)
        self.assertEqual(opts["stop_at"], 12.5)
        self.assertEqual(opts["max_waves"], 4)
        self.assertTrue(opts["text"])


if __name__ == "__main__":
    unittest.main()
