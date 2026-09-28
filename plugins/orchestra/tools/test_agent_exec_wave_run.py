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

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402
import agent_exec_wave  # noqa: E402
import agent_exec_wave_run  # noqa: E402

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

        self.wave_dir = os.path.join(self.tmp, "wave")
        os.makedirs(os.path.join(self.wave_dir, "specs"))
        self.state_path = os.path.join(self.wave_dir, "state.json")
        self.plan_path = os.path.join(self.wave_dir, "plan.json")
        _write(os.path.join(self.wave_dir, "preamble.md"), "preamble\n")

    def tearDown(self):
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

    def test_full_red_stops_and_after_green_skipped(self):
        marker = os.path.join(self.tmp, "after-green")
        self.plan([{"id": "A"}, {"id": "B", "depends_on": ["A"]}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})], "B": [edit({"b.txt": "b\n"})]})
        report, rc = self.run_wave(ex, full="false", after_green="touch '%s'" % marker)
        self.assertEqual(rc, 5, report)
        self.assertEqual(report["reason"], "full verification red")
        self.assertEqual(ex.pkgs_dispatched(), ["A"])
        self.assertFalse(os.path.exists(marker))

    def test_full_green_runs_after_green(self):
        marker = os.path.join(self.tmp, "after-green")
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        report, rc = self.run_wave(ex, full="test -f a.txt", after_green="echo x >> '%s'" % marker)
        self.assertEqual(rc, 0, report)
        self.assertEqual(_read(marker), "x\n")
        events = agent_exec_wave.read_events(self.state_path, None)
        self.assertIn("after-green", [e["event"] for e in events])


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

    def test_full_and_after_green_events(self):
        self.plan([{"id": "A"}])
        ex = FakeExecutor({"A": [edit({"a.txt": "a\n"})]})
        self.run_wave(ex, full="test -f a.txt", after_green="true")
        self.assertEqual(self.details("full-start"), [{}])
        full_end = self.details("full-end")
        self.assertEqual(full_end[0]["status"], "pass")
        self.assertIsInstance(full_end[0]["seconds"], float)
        self.assertEqual(self.details("after-green"), [{"exit": 0}])

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
        self.assertEqual(self.int_file("b.txt"), "b from before\n")
        self.assertEqual(self.int_file("b2.txt"), "b2\n")


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
