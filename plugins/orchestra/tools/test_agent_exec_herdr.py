# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for the herdr executor of `agent-exec wave run` (agent_exec_herdr.py)
and the interactive-host selection in agent_exec_wave_run.

The real herdr is never called: `FakeHerdr` is injected as the executor's
subprocess runner / Popen and records every argv.

Run with: uv run test_agent_exec_herdr.py
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import types
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402
import agent_exec_herdr  # noqa: E402
import agent_exec_orca  # noqa: E402
import agent_exec_wave_run  # noqa: E402

_ALIVE_TMP = tempfile.mkdtemp(prefix="orch-alive-")
os.environ["ORCHESTRA_ALIVE_DIR"] = _ALIVE_TMP
agent_exec._heartbeat_dir_cache = _ALIVE_TMP
os.environ["GIT_CONFIG_GLOBAL"] = "/dev/null"
os.environ["GIT_CONFIG_SYSTEM"] = "/dev/null"

SESSION_ENV = {"CLAUDE_CODE_SESSION_ID": "abcdef12-0000", "CLAUDECODE": "1",
               "CLAUDE_CODE_ENTRYPOINT": "cli", "PATH": "/usr/bin"}
SESSION = "orchestra-abcdef12"

TRUST_SCREEN = ("Accessing workspace\nIs this a project you created or one you trust?\n"
                "> 1. No, exit\n  2. Yes, I trust this folder")
PERMISSION_SCREEN = "Bash command\nDo you want to proceed?\n> 1. Yes\n  2. No"


def _git(cwd, *args):
    return subprocess.run(["git"] + list(args), cwd=cwd, capture_output=True, text=True,
                          check=True)


class FakeHerdr(object):
    """Records every call; `statuses` is the agent_status sequence `agent get`
    walks through (the last value repeats)."""

    def __init__(self, running=False, not_ready=False, screen="", statuses=("idle",),
                 on_prompt=None, stalls=0, fail_warm=False):
        self.calls = []
        self.popens = []
        self.running = running
        self.not_ready = not_ready
        self.screen = screen
        self.statuses = list(statuses)
        self.on_prompt = on_prompt
        self.stalls = stalls  # the next N `agent prompt` calls stall
        self.fail_warm = fail_warm  # the warm reader's agent never starts

    def prompts(self):
        return [c for c in self.calls if c[3:5] == ["agent", "prompt"]]

    def starts(self):
        return [c for c in self.calls if c[3:5] == ["agent", "start"]]

    def verbs(self):
        return [c[3:5] for c in self.calls]

    def has(self, *prefix):
        return any(c[3:3 + len(prefix)] == list(prefix) for c in self.calls)

    def popen(self, argv, **kwargs):
        self.popens.append((argv, kwargs))
        self.running = True

    def __call__(self, argv, **kwargs):
        self.calls.append(list(argv))
        args = argv[3:]
        if args[:1] == ["--version"]:
            return self._ok("")
        if args[:1] == ["status"]:
            return self._ok({"result": {"server": {"running": self.running}}})
        if args[:2] == ["server", "stop"]:
            self.running = False
            return self._ok({})
        if args[:2] == ["workspace", "create"]:
            return self._ok({"result": {"workspace": {"workspace_id": "w1"},
                                        "root_pane": {"pane_id": "w1:p1"}}})
        if args[:2] == ["workspace", "close"]:
            return self._ok({})
        if args[:2] == ["agent", "start"]:
            if self.fail_warm and args[2].startswith("p-warm"):
                return self._err("boom")
            if self.not_ready:
                return self._err("agent_not_ready")
            return self._ok({})
        if args[:2] == ["agent", "read"]:
            return self._ok({"result": {"text": self.screen}})
        if args[:2] == ["agent", "send-keys"]:
            self.not_ready = False
            return self._ok({})
        if args[:2] == ["agent", "wait"]:
            return self._ok({})
        if args[:2] == ["agent", "get"]:
            status = self.statuses.pop(0) if len(self.statuses) > 1 else self.statuses[0]
            return self._ok({"result": {"agent": {"agent_status": status}}})
        if args[:2] == ["agent", "prompt"]:
            if self.stalls > 0:
                self.stalls -= 1
                return self._err("agent_prompt_stalled")
            warm = re.match(r"Read each of these files .* write (\S+) containing", args[3])
            if warm:
                with open(warm.group(1), "w") as fh:
                    json.dump({"status": "ok"}, fh)
            if self.on_prompt:
                self.on_prompt()
            return self._ok({})
        return self._err("unknown", 2)

    @staticmethod
    def _ok(payload):
        return types.SimpleNamespace(
            returncode=0, stdout=json.dumps(payload) if payload != "" else "", stderr="")

    @staticmethod
    def _err(code, rc=1):
        return types.SimpleNamespace(
            # Real herdr: error object on stderr, stdout empty.
            returncode=rc, stdout="",
            stderr=json.dumps({"error": {"code": code, "message": code}}))


class _Base(unittest.TestCase):
    def setUp(self):
        self._saved_session = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = tempfile.mkdtemp(prefix="orch-herdr-")
        self.repo = os.path.join(self.tmp, "repo")
        os.makedirs(self.repo)
        _git(self.repo, "init", "-q", "-b", "main", ".")
        _git(self.repo, "config", "user.email", "t@example.com")
        _git(self.repo, "config", "user.name", "T")
        with open(os.path.join(self.repo, "README.md"), "w") as fh:
            fh.write("hello\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "init")
        self.state = os.path.join(self.tmp, "state")
        os.makedirs(self.state)
        self.result = os.path.join(self.state, "a.result.json")

    def tearDown(self):
        subprocess.run(["git", "worktree", "prune"], cwd=self.repo, capture_output=True)
        shutil.rmtree(self.tmp, ignore_errors=True)
        if self._saved_session is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._saved_session

    def executor(self, fake, cfg=None, **kwargs):
        clock = {"now": 0.0}

        def sleep(seconds):
            clock["now"] += max(seconds, 1.0)

        kwargs.setdefault("env", dict(SESSION_ENV))
        executor = agent_exec_herdr.HerdrExecutor(
            cfg=cfg, binary="/opt/herdr", runner=fake, popen=fake.popen,
            clock=lambda: clock["now"], sleep=sleep, poll=1.0, **kwargs)
        return executor

    def write_result(self, status="ok"):
        with open(self.result, "w") as fh:
            json.dump({"status": status, "summary": "done"}, fh)

    def manual_session(self):
        return {"pkg": "a", "worktree": self.repo, "terminal": "p-a", "workspace": "w1",
                "pane": "w1:p1", "state_dir": self.state, "repo": self.repo}


class SessionIsolationTests(_Base):
    def test_every_call_carries_the_session_and_the_server_env_is_clean(self):
        fake = FakeHerdr(on_prompt=lambda: self.write_result())
        executor = self.executor(fake)
        session = executor.start("a", self.repo, "main", "standard", "opus", "run-1",
                                 self.state)
        out = executor.prompt(session, ["/p/one.md"], self.result, 60)
        executor.close(session, remove_worktree=True)
        executor.shutdown()
        self.assertEqual(out["executor"], "herdr")
        self.assertTrue(fake.calls)
        for argv in fake.calls + [p[0] for p in fake.popens]:
            self.assertEqual(argv[1:3], ["--session", SESSION], argv)
        self.assertEqual(len(fake.popens), 1)
        env = fake.popens[0][1]["env"]
        self.assertFalse([k for k in env if k.startswith("CLAUDE_CODE_") or k == "CLAUDECODE"])
        self.assertEqual(env["PATH"], "/usr/bin")
        self.assertTrue(fake.has("server", "stop"))

    def test_shutdown_keeps_the_server_while_sessions_await_resume(self):
        fake = FakeHerdr()
        executor = self.executor(fake)
        executor.start("a", self.repo, "main", "standard", "opus", "run-1", self.state)
        executor.shutdown()
        self.assertFalse(fake.has("server", "stop"))
        # keep_sessions off: nothing is resumed, so the server goes
        fake = FakeHerdr()
        executor = self.executor(fake, cfg={"keep_sessions": False})
        executor.start("a", self.repo, "main", "standard", "opus", "run-1", self.state)
        executor.shutdown()
        self.assertTrue(fake.has("server", "stop"))

    def test_shutdown_leaves_a_server_it_did_not_start(self):
        fake = FakeHerdr(running=True)
        executor = self.executor(fake)
        executor.start("a", self.repo, "main", "standard", "opus", "run-1", self.state)
        executor.shutdown()
        self.assertEqual(fake.popens, [])
        self.assertFalse(fake.has("server", "stop"))


class WarmTests(_Base):
    """The warm reader session and the packages forked from it."""

    def warm_executor(self, fake, cfg=None, counts=None):
        executor = self.executor(fake, cfg)
        executor.configure_warm(["README.md"], counts or {"opus": 2})
        return executor

    def start(self, executor, pkg="a", model="opus", base="main"):
        return executor.start(pkg, self.repo, base, "standard", model, "run-1", self.state,
                              add_dirs=[self.state])

    def forks(self, fake):
        return [c for c in fake.starts() if "--fork-session" in c]

    def test_warm_reader_has_its_own_worktree_and_packages_fork_from_it(self):
        fake = FakeHerdr(running=True)
        executor = self.warm_executor(fake)
        first = self.start(executor, "a")
        second = self.start(executor, "b")
        cwds = [c[c.index("--cwd") + 1] for c in fake.calls if c[3:5] == ["workspace", "create"]]
        warm_tree = cwds[0]
        self.assertEqual(len(cwds), 3)  # warm reader, a, b
        self.assertNotIn(warm_tree, (os.path.realpath(self.repo), first["worktree"],
                                     second["worktree"]))
        self.assertIn("warm-opus", warm_tree)
        warm_start = fake.starts()[0]
        sid = warm_start[warm_start.index("--session-id") + 1]
        self.assertNotIn("--fork-session", warm_start)
        self.assertEqual(len(self.forks(fake)), 2)
        for argv in self.forks(fake):
            self.assertEqual(argv[argv.index("--resume") + 1], sid)
        self.assertEqual(first["warm_session"], sid)
        self.assertEqual(len(fake.prompts()), 1)  # the reader read once for both

    def test_no_fork_when_warm_does_not_apply(self):
        for label, cfg, counts, model in (
                ("disabled", {"warm": {"enabled": False}}, None, "opus"),
                ("below min_packages", None, {"opus": 1}, "opus"),
                ("other model", None, {"opus": 2}, "sonnet")):
            with self.subTest(label):
                fake = FakeHerdr(running=True)
                executor = self.warm_executor(fake, cfg, counts)
                self.start(executor, "a", model)
                self.assertEqual(self.forks(fake), [])
                self.assertEqual(fake.prompts(), [])
                self.assertNotIn("--session-id", " ".join(fake.starts()[0]))

    def test_expired_ttl_and_changed_warm_file_rebuild_but_other_changes_do_not(self):
        fake = FakeHerdr(running=True)
        executor = self.warm_executor(fake)
        self.start(executor, "a")
        # a change to a file the reader did not read keeps the warm session
        with open(os.path.join(self.repo, "other.txt"), "w") as fh:
            fh.write("x\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "other")
        self.start(executor, "b")
        self.assertEqual(len(fake.prompts()), 1)
        self.assertEqual(len(self.forks(fake)), 2)
        # a change to a warm file does not
        with open(os.path.join(self.repo, "README.md"), "w") as fh:
            fh.write("changed\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "readme")
        self.start(executor, "c")
        self.assertEqual(len(fake.prompts()), 2)
        warm = [c for c in fake.starts() if "--session-id" in c]
        self.assertEqual(len(warm), 2)
        self.assertNotEqual(warm[0][warm[0].index("--session-id") + 1],
                            warm[1][warm[1].index("--session-id") + 1])
        # ttl: the record is stale once last_used is old enough
        executor.clock = lambda: 100000.0
        self.start(executor, "d")
        self.assertEqual(len(fake.prompts()), 3)

    def test_warm_failure_falls_back_to_a_fresh_start(self):
        fake = FakeHerdr(running=True, fail_warm=True)
        executor = self.warm_executor(fake)
        events = []
        executor.on_event = lambda pkg, detail: events.append((pkg, detail))
        first = self.start(executor, "a")
        second = self.start(executor, "b")
        self.assertEqual(self.forks(fake), [])
        self.assertNotIn("warm_session", first)
        self.assertEqual([d["action"] for _, d in events if d["action"] == "warm-failed"],
                         ["warm-failed"])  # reported once, not retried per package
        self.assertEqual(second["pkg"], "b")

    def test_fork_that_dirties_the_warm_tree_is_a_need_and_forces_a_rebuild(self):
        fake = FakeHerdr(running=True)
        executor = self.warm_executor(fake)
        session = self.start(executor, "a")

        def leak():
            with open(os.path.join(session["warm_worktree"], "oops.txt"), "w") as fh:
                fh.write("x\n")
            self.write_result()

        fake.on_prompt = leak
        with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
            executor.prompt(session, ["/p/one.md"], self.result, 60)
        self.assertEqual(ctx.exception.kind, "error")
        self.assertIn("oops.txt", ctx.exception.detail)
        fake.on_prompt = None
        self.start(executor, "b")
        self.assertEqual(len([c for c in fake.starts() if "--session-id" in c]), 2)

    def test_forked_prompt_names_both_trees_once(self):
        fake = FakeHerdr(running=True, on_prompt=lambda: self.write_result())
        executor = self.warm_executor(fake)
        session = self.start(executor, "a")
        executor.prompt(session, ["/p/one.md"], self.result, 60)
        executor.prompt(session, ["/p/two.md"], self.result, 60)
        first, second = fake.prompts()[1][-1], fake.prompts()[2][-1]
        self.assertIn("Your working tree is %s." % session["worktree"], first)
        self.assertIn(session["warm_worktree"], first)
        self.assertNotIn("Your working tree", second)

    def test_warm_record_is_not_a_package_and_does_not_keep_the_server(self):
        fake = FakeHerdr()
        executor = self.warm_executor(fake)
        self.start(executor, "a")
        self.assertIsNone(executor.session("_warm", self.state))
        executor.close(executor.session("a", self.state), remove_worktree=True)
        executor.shutdown()
        self.assertTrue(fake.has("server", "stop"))
        self.assertNotIn("_warm", executor._load_registry())

    def test_bypass_is_refused_on_the_fork_path(self):
        executor = self.executor(FakeHerdr(), {"permission_mode": "bypassPermissions"})
        with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
            executor._native_args({"model": "opus", "warm_session": "sid"}, fork=True)
        self.assertIn("bypass", ctx.exception.detail)
        fake = FakeHerdr(running=True)
        executor = self.warm_executor(fake, {"args": "--dangerously-skip-permissions"})
        with self.assertRaises(agent_exec_orca.OrcaNeed):
            self.start(executor, "a")
        self.assertEqual(fake.starts(), [])


class PromptStallTests(_Base):
    def test_a_stalled_prompt_is_resent_once_and_a_second_stall_is_a_need(self):
        fake = FakeHerdr(running=True, stalls=1, on_prompt=lambda: self.write_result())
        executor = self.executor(fake)
        out = executor.prompt(self.manual_session(), ["/p/one.md"], self.result, 60)
        self.assertEqual(out["status"], "ok")
        self.assertEqual(len(fake.prompts()), 2)
        self.assertEqual(fake.prompts()[0], fake.prompts()[1])
        fake = FakeHerdr(running=True, stalls=5)
        executor = self.executor(fake)
        with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
            executor.prompt(self.manual_session(), ["/p/one.md"], self.result, 60)
        self.assertEqual(ctx.exception.kind, "stalled")
        self.assertEqual(len(fake.prompts()), 2)


class TrustTests(_Base):
    def start(self, fake, cfg, precreate=False):
        if precreate:
            _, sha = agent_exec._git(self.repo, "rev-parse", "HEAD")
            agent_exec.isolate_create(self.repo, "pkg-a", backend="git", onto=sha.strip())
        executor = self.executor(fake, cfg)
        return executor.start("a", self.repo, "main", "standard", "opus", "run-1", self.state)

    def test_dialog_is_reported_unless_auto_trust_and_executor_created_the_tree(self):
        for label, cfg, precreate in (
                ("auto_trust off", {"auto_trust": False}, False),
                ("tree not created here", {"auto_trust": True}, True)):
            with self.subTest(label):
                fake = FakeHerdr(running=True, not_ready=True, screen=TRUST_SCREEN)
                with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
                    self.start(fake, cfg, precreate)
                self.assertEqual(ctx.exception.kind, "trust")
                self.assertFalse(fake.has("agent", "send-keys"))
                subprocess.run(["git", "worktree", "prune"], cwd=self.repo)

    def test_auto_trust_moves_down_before_enter_then_waits(self):
        fake = FakeHerdr(running=True, not_ready=True, screen=TRUST_SCREEN)
        self.start(fake, {"auto_trust": True})
        keys = [c[3:] for c in fake.calls if c[3:5] == ["agent", "send-keys"]]
        self.assertEqual(keys, [["agent", "send-keys", "p-a", "down", "enter"]])
        self.assertLess(fake.verbs().index(["agent", "send-keys"]),
                        fake.verbs().index(["agent", "wait"]))

    def test_other_not_ready_screen_is_an_error(self):
        fake = FakeHerdr(running=True, not_ready=True, screen="login required")
        with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
            self.start(fake, {"auto_trust": True})
        self.assertEqual(ctx.exception.kind, "error")
        self.assertIn("login required", ctx.exception.detail)


class WaitTests(_Base):
    def test_idle_and_done_are_not_completion(self):
        fake = FakeHerdr(running=True, statuses=("idle", "done", "idle"))
        executor = self.executor(fake)
        with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
            executor.wait_result(self.manual_session(), self.result, 10)
        self.assertEqual(ctx.exception.kind, "timeout")
        self.assertGreater(len([c for c in fake.calls if c[3:5] == ["agent", "get"]]), 2)

    def test_blocked_is_a_stall_with_the_dialog(self):
        fake = FakeHerdr(running=True, statuses=("working", "blocked"),
                         screen=PERMISSION_SCREEN)
        executor = self.executor(fake)
        with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
            executor.wait_result(self.manual_session(), self.result, 60)
        self.assertEqual(ctx.exception.kind, "stalled")
        self.assertIn("dialog:", ctx.exception.detail)
        self.assertIn("Do you want to proceed?", ctx.exception.detail)
        self.assertIn("terminal: p-a", ctx.exception.detail)

    def test_vanished_agent_is_a_stall(self):
        fake = FakeHerdr(running=True)
        fake.statuses = ["idle"]
        executor = self.executor(fake)
        original = fake.__call__

        def gone(argv, **kwargs):
            if argv[3:5] == ["agent", "get"]:
                return fake._err("agent_not_found")
            return original(argv, **kwargs)

        executor.runner = gone
        with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
            executor.wait_result(self.manual_session(), self.result, 60)
        self.assertEqual(ctx.exception.kind, "stalled")

    def test_no_prompt_is_sent_to_a_blocked_agent(self):
        fake = FakeHerdr(running=True, statuses=("blocked",), screen=PERMISSION_SCREEN)
        executor = self.executor(fake)
        with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
            executor.prompt(self.manual_session(), ["/p/one.md"], self.result, 60)
        self.assertEqual(ctx.exception.kind, "stalled")
        self.assertFalse(fake.has("agent", "prompt"))


class BypassTests(_Base):
    def test_bypass_is_refused(self):
        for label, cfg in (
                ("mode", {"permission_mode": "bypassPermissions"}),
                ("args", {"args": "--model {model} --dangerously-skip-permissions"}),
                ("args mode", {"args": "--permission-mode bypassPermissions"})):
            with self.subTest(label):
                executor = self.executor(FakeHerdr(), cfg)
                with self.assertRaises(agent_exec_orca.OrcaNeed) as ctx:
                    executor._native_args({"model": "opus"})
                self.assertIn("bypass", ctx.exception.detail)

    def test_add_dirs_follow_the_native_args(self):
        executor = self.executor(FakeHerdr())
        args = executor._native_args({"model": "opus", "add_dirs": ["/x/state"]})
        self.assertEqual(args, ["--model", "opus", "--permission-mode", "acceptEdits",
                                "--add-dir", "/x/state"])


class _Stub(object):
    def __init__(self, name, enabled="auto", ok=True):
        self.host_name = name
        self.enabled = enabled
        self.ok = ok
        self.state_dir = None
        self.on_event = None

    def available(self):
        return self.ok, "" if self.ok else "gone"


class HostSelectionTests(_Base):
    def select(self, mode, herdr, orca):
        runner = agent_exec_wave_run._Runner.__new__(agent_exec_wave_run._Runner)
        runner.opts = {"interactive_host": mode}
        runner.orca, runner.herdr, runner.host_name = orca, herdr, "orca"
        runner.int_path, runner.base, runner.state_dir = self.repo, "main", self.state
        runner.stops = []
        runner._stop = runner.stops.append
        runner._init_orca()
        return runner

    def test_auto_prefers_herdr_then_orca_then_none(self):
        self.assertIs(self.select("auto", _Stub("herdr"), _Stub("orca")).orca.host_name, "herdr")
        runner = self.select("auto", _Stub("herdr", ok=False), _Stub("orca"))
        self.assertEqual((runner.orca.host_name, runner.host_name), ("orca", "orca"))
        self.assertIsNone(self.select("auto", _Stub("herdr", False),
                                      _Stub("orca", False)).orca)
        self.assertIsNone(self.select("none", _Stub("herdr"), _Stub("orca")).orca)

    def test_explicit_mode_uses_only_that_backend(self):
        runner = self.select("orca", _Stub("herdr"), _Stub("orca"))
        self.assertEqual((runner.orca.host_name, runner.host_name), ("orca", "orca"))
        runner = self.select("herdr", _Stub("herdr"), _Stub("orca"))
        self.assertEqual((runner.orca.host_name, runner.host_name), ("herdr", "herdr"))
        # unavailable explicit choice never falls back to the other backend
        runner = self.select("herdr", _Stub("herdr", ok=False), _Stub("orca"))
        self.assertIsNone(runner.orca)
        self.assertEqual(runner.stops, [])

    def test_auto_without_a_host_stops_for_an_enabled_unavailable_orca(self):
        runner = self.select("auto", _Stub("herdr", ok=False), _Stub("orca", True, False))
        self.assertIsNone(runner.orca)
        self.assertEqual(runner.stops, ["orca unavailable: gone"])
        # a selected herdr is not stopped by an unavailable enabled orca
        runner = self.select("auto", _Stub("herdr"), _Stub("orca", True, False))
        self.assertEqual((runner.orca.host_name, runner.stops), ("herdr", []))

    def test_required_but_unavailable_stops_the_run(self):
        runner = self.select("herdr", _Stub("herdr", enabled=True, ok=False), _Stub("orca"))
        self.assertIsNone(runner.orca)
        self.assertEqual(len(runner.stops), 1)
        self.assertIn("herdr unavailable", runner.stops[0])


if __name__ == "__main__":
    unittest.main()
