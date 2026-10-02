# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for agent_exec_wave.py (contract W2).

Run with: uv run test_agent_exec_wave.py
"""

import json
import contextlib
import io
import multiprocessing
import os
import shutil
import sys
import tempfile
import threading
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec_wave as wave  # noqa: E402


class _StateDirCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="orch-wave-")
        self.state_path = os.path.join(self.tmp, "state.json")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _clock(self, value):
        return lambda: value


class LintCommandTests(_StateDirCase):
    def test_lint_text_and_json_exit_codes(self):
        import agent_exec_wave_plan as plan_mod
        spec = os.path.join(self.tmp, "spec.md")
        with open(spec, "w") as f:
            f.write("remove old code\n")
        plan = os.path.join(self.tmp, "plan.json")
        with open(plan, "w") as f:
            json.dump({"packages": [{"id": "X", "spec": spec, "cls": "light",
                                     "files_owned": ["src/a.py"]}]}, f)
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = wave.cmd_wave(["lint", "--plan", plan, "--text"])
        self.assertEqual(rc, 1)
        self.assertIn("plan-warning: X:", out.getvalue())
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = wave.cmd_wave(["lint", "--plan", plan, "--json"])
        self.assertEqual(rc, 1)
        self.assertEqual(json.loads(out.getvalue())[0]["pkg"], "X")
        with open(spec, "w") as f:
            f.write("nothing\n")
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            rc = wave.cmd_wave(["lint", "--plan", plan])
        self.assertEqual(rc, 0)


class InitTests(_StateDirCase):
    def test_init_creates_schema_exact_file(self):
        store = wave.StateStore(self.state_path, clock=self._clock(1000.0))
        state = store.init("/abs/plan.json", ["CORE-1", "CORE-2"], "wave-int")

        self.assertEqual(state["version"], 1)
        self.assertEqual(state["plan"], "/abs/plan.json")
        self.assertEqual(state["integration"], {"task": "wave-int", "path": None, "base": None})
        self.assertEqual(state["wave"], 0)
        self.assertEqual(state["stopped"], None)
        self.assertEqual(state["updated"], 1000.0)
        self.assertEqual(set(state["packages"].keys()), {"CORE-1", "CORE-2"})
        for pkg in state["packages"].values():
            self.assertEqual(pkg, {
                "status": "pending", "since": 1000.0, "attempts": 0,
                "tree": None, "executor": None, "session": False,
                "files_changed": None, "commit": None, "detail": "",
            })
        self.assertEqual(state["needs"], [])

        with open(self.state_path) as fh:
            on_disk = json.load(fh)
        self.assertEqual(on_disk, state)

    def test_reinit_adds_only_missing_ids(self):
        store = wave.StateStore(self.state_path, clock=self._clock(1000.0))
        store.init("/abs/plan.json", ["CORE-1"], "wave-int")
        store.set_status("CORE-1", "implementing")

        store2 = wave.StateStore(self.state_path, clock=self._clock(2000.0))
        state = store2.init("/abs/plan.json", ["CORE-1", "CORE-2"], "wave-int")

        self.assertEqual(state["packages"]["CORE-1"]["status"], "implementing")
        self.assertEqual(state["packages"]["CORE-2"]["status"], "pending")
        self.assertEqual(state["packages"]["CORE-2"]["since"], 2000.0)
        # existing top-level fields untouched by re-init
        self.assertEqual(state["plan"], "/abs/plan.json")


class SetStatusTests(_StateDirCase):
    def setUp(self):
        super().setUp()
        self.store = wave.StateStore(self.state_path, clock=lambda: self._now)
        self._now = 1000.0
        self.store.init("/abs/plan.json", ["CORE-1"], "wave-int")

    def test_rejects_unknown_status(self):
        with self.assertRaises(ValueError):
            self.store.set_status("CORE-1", "bogus")

    def test_rejects_unknown_field(self):
        with self.assertRaises(ValueError):
            self.store.set_status("CORE-1", "implementing", nonsense="x")

    def test_since_unchanged_when_status_unchanged(self):
        self._now = 1000.0
        self.store.set_status("CORE-1", "implementing")
        self._now = 1500.0
        state = self.store.set_status("CORE-1", "implementing")
        self.assertEqual(state["packages"]["CORE-1"]["since"], 1000.0)

    def test_since_updates_on_status_change(self):
        self._now = 1000.0
        self.store.set_status("CORE-1", "implementing")
        self._now = 1500.0
        state = self.store.set_status("CORE-1", "verifying")
        self.assertEqual(state["packages"]["CORE-1"]["since"], 1500.0)

    def test_accepts_extra_package_fields(self):
        state = self.store.set_status(
            "CORE-1", "verifying", detail="running checks",
            executor="claude", attempts=1, files_changed=3,
        )
        entry = state["packages"]["CORE-1"]
        self.assertEqual(entry["detail"], "running checks")
        self.assertEqual(entry["executor"], "claude")
        self.assertEqual(entry["attempts"], 1)
        self.assertEqual(entry["files_changed"], 3)

    def test_events_appended_with_from_to(self):
        self._now = 1000.0
        self.store.set_status("CORE-1", "implementing", detail="starting")
        self._now = 1200.0
        self.store.set_status("CORE-1", "verifying", detail="checks running")

        events = wave.read_events(self.state_path, 10)
        self.assertEqual(len(events), 2)
        self.assertEqual(events[0]["pkg"], "CORE-1")
        self.assertEqual(events[0]["from"], "pending")
        self.assertEqual(events[0]["to"], "implementing")
        self.assertEqual(events[0]["event"], "status")
        self.assertEqual(events[1]["from"], "implementing")
        self.assertEqual(events[1]["to"], "verifying")
        self.assertEqual(events[1]["detail"], "checks running")


class UpdateTests(_StateDirCase):
    def setUp(self):
        super().setUp()
        self.store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        self.store.init("/abs/plan.json", ["CORE-1"], "wave-int")

    def test_update_changes_fields_without_status_event(self):
        state = self.store.update("CORE-1", tree="/tmp/tree", executor="codex", attempts=2)
        entry = state["packages"]["CORE-1"]
        self.assertEqual(entry["tree"], "/tmp/tree")
        self.assertEqual(entry["executor"], "codex")
        self.assertEqual(entry["attempts"], 2)
        self.assertEqual(entry["status"], "pending")  # untouched
        self.assertEqual(wave.read_events(self.state_path, 10), [])

    def test_update_rejects_status_field(self):
        with self.assertRaises(ValueError):
            self.store.update("CORE-1", status="ready")


class NeedsTests(_StateDirCase):
    def setUp(self):
        super().setUp()
        self.store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        self.store.init("/abs/plan.json", ["CORE-1", "CORE-2"], "wave-int")

    def test_add_need_sets_status_and_appends(self):
        state = self.store.add_need("CORE-1", "escalate", "needs a human call")
        self.assertEqual(state["packages"]["CORE-1"]["status"], "needs")
        self.assertEqual(len(state["needs"]), 1)
        self.assertEqual(state["needs"][0]["id"], "CORE-1")
        self.assertEqual(state["needs"][0]["kind"], "escalate")
        self.assertEqual(state["needs"][0]["detail"], "needs a human call")

    def test_add_need_rejects_unknown_kind(self):
        with self.assertRaises(ValueError):
            self.store.add_need("CORE-1", "bogus-kind", "x")

    def test_clear_need_removes_entries_leaves_status(self):
        self.store.add_need("CORE-1", "conflict", "merge conflict")
        state = self.store.clear_need("CORE-1")
        self.assertEqual(state["needs"], [])
        self.assertEqual(state["packages"]["CORE-1"]["status"], "needs")

    def test_clear_need_only_targets_its_own_package(self):
        self.store.add_need("CORE-1", "conflict", "a")
        self.store.add_need("CORE-2", "escalate", "b")
        state = self.store.clear_need("CORE-1")
        ids = [n["id"] for n in state["needs"]]
        self.assertEqual(ids, ["CORE-2"])


class IntegrationWaveStopTests(_StateDirCase):
    def setUp(self):
        super().setUp()
        self.store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        self.store.init("/abs/plan.json", ["CORE-1"], "wave-int")

    def test_set_integration(self):
        state = self.store.set_integration(path="/abs/int-tree", base="deadbeef")
        self.assertEqual(state["integration"]["path"], "/abs/int-tree")
        self.assertEqual(state["integration"]["base"], "deadbeef")
        self.assertEqual(state["integration"]["task"], "wave-int")

    def test_set_wave(self):
        state = self.store.set_wave(3)
        self.assertEqual(state["wave"], 3)

    def test_mark_stopped(self):
        state = self.store.mark_stopped("user requested stop")
        self.assertEqual(state["stopped"]["reason"], "user requested stop")
        self.assertIn("at", state["stopped"])

    def test_request_stop_and_stop_requested(self):
        self.assertFalse(wave.stop_requested(self.state_path))
        wave.request_stop(self.state_path, "budget exceeded")
        self.assertTrue(wave.stop_requested(self.state_path))
        stop_path = os.path.join(self.tmp, "STOP")
        with open(stop_path) as fh:
            self.assertEqual(fh.read(), "budget exceeded")


class EventTests(_StateDirCase):
    def setUp(self):
        super().setUp()
        self.store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        self.store.init("/abs/plan.json", ["CORE-1"], "wave-int")

    def test_free_form_event(self):
        self.store.event("wave-start", detail="starting wave 1")
        events = wave.read_events(self.state_path, 10)
        self.assertEqual(events[-1]["event"], "wave-start")
        self.assertEqual(events[-1]["detail"], "starting wave 1")
        self.assertIsNone(events[-1]["pkg"])

    def test_read_events_tolerates_truncated_last_line(self):
        events_path = self.store._events_path()
        with open(events_path, "a") as fh:
            fh.write('{"at": 1, "pkg": null, "from": null, "to": null, '
                     '"event": "wave-start", "detail": ""}\n')
            fh.write('{"at": 2, "pkg": "CORE-1", "truncated')  # no trailing newline / brace
        events = wave.read_events(self.state_path, 10)
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]["event"], "wave-start")

    def test_read_events_limit(self):
        for i in range(5):
            self.store.event("e%d" % i)
        events = wave.read_events(self.state_path, 2)
        self.assertEqual([e["event"] for e in events], ["e3", "e4"])


def _thread_worker(store, pkg, n):
    for i in range(n):
        if i % 2 == 0:
            store.set_status(pkg, "implementing", detail="t%d" % i)
        else:
            store.update(pkg, attempts=i)


def _process_worker(state_path, pkg, n):
    store = wave.StateStore(state_path)
    for i in range(n):
        store.set_status(pkg, "verifying", detail="p%d" % i)


class ConcurrencyTests(_StateDirCase):
    def test_concurrent_threads_and_processes_no_lost_updates(self):
        store = wave.StateStore(self.state_path)
        store.init("/abs/plan.json", ["CORE-1"], "wave-int")

        threads = [
            threading.Thread(target=_thread_worker, args=(store, "CORE-1", 50))
            for _ in range(8)
        ]
        procs = [
            multiprocessing.Process(target=_process_worker, args=(self.state_path, "CORE-1", 50))
            for _ in range(2)
        ]
        for t in threads:
            t.start()
        for p in procs:
            p.start()
        for t in threads:
            t.join()
        for p in procs:
            p.join()
            self.assertEqual(p.exitcode, 0)

        with open(self.state_path) as fh:
            state = json.load(fh)  # must parse: no torn writes
        self.assertIn(state["packages"]["CORE-1"]["status"], wave.STATUSES)

        events = wave.read_events(self.state_path, 100000)
        # 8 threads * 25 set_status calls each (every other iteration), plus
        # 2 processes * 50 set_status calls each.
        self.assertEqual(len(events), 8 * 25 + 2 * 50)


class RenderStatusTests(_StateDirCase):
    def test_render_status_environment_need_shows_wave(self):
        store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        store.init("/abs/plan.json", ["CORE-1"], "wave-int")
        state = store.add_need(None, "environment", "ensure failed\nmore")
        self.assertEqual(state["needs"][0]["id"], None)
        self.assertEqual(state["packages"]["CORE-1"]["status"], "pending")
        text = wave.render_status(state, now=1100.0)
        self.assertIn("(wave)  environment  ensure failed", text)
        self.assertNotIn("None", text)

    def test_render_status_contains_elapsed_and_needs(self):
        store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        state = store.init("/abs/plan.json", ["CORE-1", "CORE-2"], "wave-int")
        state = store.set_status("CORE-1", "implementing", executor="claude",
                                  files_changed=2, attempts=1)
        state = store.add_need("CORE-2", "escalate", "human call needed\nmore detail")

        text = wave.render_status(state, now=1192.0)
        self.assertIn("CORE-1", text)
        self.assertIn("implementing", text)
        self.assertIn("3m12s", text)
        self.assertIn("claude", text)
        self.assertIn("needs:", text)
        self.assertIn("CORE-2", text)
        self.assertIn("escalate", text)
        self.assertIn("human call needed", text)
        # pending packages are not listed as detail rows
        self.assertNotIn("CORE-2  pending", text)

    def test_render_status_recent_events(self):
        store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        store.init("/abs/plan.json", ["CORE-1"], "wave-int")
        store.set_status("CORE-1", "implementing")
        events = wave.read_events(self.state_path, 5)
        state = store.load()
        text = wave.render_status(state, now=1000.0, events=events)
        self.assertIn("recent:", text)


class StatusLineTests(_StateDirCase):
    def test_status_line_is_one_line_valid_json_nonzero_counts(self):
        store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        state = store.init("/abs/plan.json", ["CORE-1", "CORE-2", "CORE-3"], "wave-int")
        state = store.set_status("CORE-1", "implementing")

        line = wave.status_line(state, now=1100.0)
        self.assertNotIn("\n", line)
        payload = json.loads(line)
        self.assertEqual(payload["counts"], {"pending": 2, "implementing": 1})
        self.assertEqual(payload["needs"], 0)
        self.assertFalse(payload["stopped"])
        self.assertEqual(payload["updated_ago"], 100)


class CmdWaveTests(_StateDirCase):
    def setUp(self):
        super().setUp()
        self.store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        self.store.init("/abs/plan.json", ["CORE-1"], "wave-int")

    def _run(self, args):
        import contextlib
        import io
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = wave.cmd_wave(args)
        return code, out.getvalue(), err.getvalue()

    def test_status_text_default(self):
        code, out, err = self._run(["status", "--state", self.state_path])
        self.assertEqual(code, 0)
        self.assertIn("wave 0", out)

    def test_status_json(self):
        code, out, err = self._run(["status", "--state", self.state_path, "--json"])
        self.assertEqual(code, 0)
        payload = json.loads(out)
        self.assertEqual(payload["version"], 1)

    def test_status_line(self):
        code, out, err = self._run(["status", "--state", self.state_path, "--line"])
        self.assertEqual(code, 0)
        self.assertEqual(out.count("\n"), 1)
        json.loads(out.strip())

    def test_status_text_and_line_show_wave_level_need(self):
        self.store.add_need(None, "environment", "ensure failed")
        code, out, err = self._run(["status", "--state", self.state_path])
        self.assertEqual(code, 0)
        self.assertIn("(wave)  environment  ensure failed", out)
        code, out, err = self._run(["status", "--state", self.state_path, "--line"])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out.strip())["needs"], 1)

    def test_status_missing_state_exit_3(self):
        missing = os.path.join(self.tmp, "nope.json")
        code, out, err = self._run(["status", "--state", missing])
        self.assertEqual(code, 3)
        self.assertIn("nope.json", err)

    def test_stop(self):
        code, out, err = self._run(["stop", "--state", self.state_path, "--reason", "manual"])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out), {"stop_requested": True})
        self.assertTrue(wave.stop_requested(self.state_path))

    def test_unknown_subcommand_exit_2(self):
        code, out, err = self._run(["bogus"])
        self.assertEqual(code, 2)

    def test_missing_flag_value_exit_2(self):
        code, out, err = self._run(["status", "--state"])
        self.assertEqual(code, 2)

    def test_status_requires_state_flag(self):
        code, out, err = self._run(["status"])
        self.assertEqual(code, 2)


class LiveChangesTests(_StateDirCase):
    def _git_tree(self, name):
        import subprocess
        tree = os.path.join(self.tmp, name)
        os.makedirs(tree)
        env = dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")
        for args in (["init", "-q", "-b", "main", "."],
                     ["config", "user.email", "t@example.com"],
                     ["config", "user.name", "T"]):
            subprocess.run(["git"] + args, cwd=tree, check=True, env=env, capture_output=True)
        with open(os.path.join(tree, "tracked.txt"), "w") as fh:
            fh.write("one\n")
        subprocess.run(["git", "add", "-A"], cwd=tree, check=True, env=env, capture_output=True)
        subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=tree, check=True,
                       env=env, capture_output=True)
        return tree

    def _state(self, trees):
        store = wave.StateStore(self.state_path, clock=lambda: 1000.0)
        store.init("/abs/plan.json", sorted(trees), "wave-int")
        for pid, (status, tree) in trees.items():
            store.set_status(pid, status, tree=tree)
        return store.load()

    def test_counts_untracked_and_modified(self):
        tree = self._git_tree("t1")
        with open(os.path.join(tree, "tracked.txt"), "w") as fh:
            fh.write("changed\n")
        os.makedirs(os.path.join(tree, "sub"))
        with open(os.path.join(tree, "sub", "new.txt"), "w") as fh:
            fh.write("x\n")
        with open(os.path.join(tree, "sub", "new2.txt"), "w") as fh:
            fh.write("y\n")
        state = self._state({"A-1": ("implementing", tree)})
        self.assertEqual(wave.live_changes(state), {"A-1": 3})

    def test_skips_missing_tree_and_not_in_flight(self):
        tree = self._git_tree("t2")
        state = self._state({
            "A-1": ("implementing", os.path.join(self.tmp, "gone")),
            "A-2": ("integrated", tree),
            "A-3": ("pending", None),
            "A-4": ("verifying", tree),
        })
        self.assertEqual(wave.live_changes(state), {"A-4": 0})

    def test_timeout_is_none(self):
        import subprocess
        tree = self._git_tree("t3")
        state = self._state({"A-1": ("fixing", tree)})
        original = wave.subprocess.run

        def boom(*args, **kwargs):
            raise subprocess.TimeoutExpired(args[0], 5)

        wave.subprocess.run = boom
        try:
            self.assertEqual(wave.live_changes(state), {"A-1": None})
        finally:
            wave.subprocess.run = original

    def test_status_outputs_without_and_with_live(self):
        tree = self._git_tree("t4")
        with open(os.path.join(tree, "a.txt"), "w") as fh:
            fh.write("a\n")
        state = self._state({"A-1": ("implementing", tree), "A-2": ("pending", None)})
        plain = wave.render_status(state, now=1000.0)
        self.assertIn("running: 1 in flight", plain.splitlines()[0])
        self.assertNotIn("*", plain)
        live = wave.render_status(state, now=1000.0, live=True)
        self.assertIn("1*", live)
        self.assertEqual(plain.splitlines()[0], live.splitlines()[0])
        line = json.loads(wave.status_line(state, now=1000.0))
        self.assertEqual(line["in_flight"], 1)
        self.assertNotIn("live_changes", line)
        live_line = json.loads(wave.status_line(state, now=1000.0, live=True))
        self.assertEqual(live_line["live_changes"], {"A-1": 1})

    def test_cmd_status_live_flags(self):
        import contextlib
        import io
        tree = self._git_tree("t5")
        with open(os.path.join(tree, "a.txt"), "w") as fh:
            fh.write("a\n")
        self._state({"A-1": ("implementing", tree)})

        def run(args):
            out = io.StringIO()
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
                code = wave.cmd_wave(args)
            return code, out.getvalue()

        code, out = run(["status", "--state", self.state_path, "--json", "--live"])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out)["live_changes"], {"A-1": 1})
        code, out = run(["status", "--state", self.state_path, "--json"])
        self.assertNotIn("live_changes", json.loads(out))
        code, out = run(["status", "--state", self.state_path, "--line", "--live"])
        self.assertEqual(json.loads(out)["live_changes"], {"A-1": 1})
        code, out = run(["status", "--state", self.state_path, "--live"])
        self.assertIn("1*", out)


class AgentExecReachabilityTests(unittest.TestCase):
    def test_wave_status_reachable_through_main(self):
        sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
        import agent_exec  # noqa: E402

        tmp = tempfile.mkdtemp(prefix="orch-wave-main-")
        try:
            state_path = os.path.join(tmp, "state.json")
            store = wave.StateStore(state_path, clock=lambda: 1000.0)
            store.init("/abs/plan.json", ["CORE-1"], "wave-int")

            import contextlib
            import io
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                code = agent_exec.main(["wave", "status", "--state", state_path, "--json"])
            self.assertEqual(code, 0)
            payload = json.loads(out.getvalue())
            self.assertEqual(payload["version"], 1)
        finally:
            shutil.rmtree(tmp, ignore_errors=True)


class RegistryTests(_StateDirCase):
    def setUp(self):
        _StateDirCase.setUp(self)
        self.registry = os.path.join(self.tmp, "reg", "waves.jsonl")
        self._orig = os.environ.get("ORCHESTRA_WAVE_REGISTRY")
        os.environ["ORCHESTRA_WAVE_REGISTRY"] = self.registry

    def tearDown(self):
        if self._orig is None:
            os.environ.pop("ORCHESTRA_WAVE_REGISTRY", None)
        else:
            os.environ["ORCHESTRA_WAVE_REGISTRY"] = self._orig
        _StateDirCase.tearDown(self)

    def _state(self, name):
        path = os.path.join(self.tmp, name, "state.json")
        os.makedirs(os.path.dirname(path))
        with open(path, "w") as fh:
            fh.write("{}")
        return path

    def test_registry_path_env_override_and_default(self):
        self.assertEqual(wave.registry_path(), self.registry)
        del os.environ["ORCHESTRA_WAVE_REGISTRY"]
        self.assertEqual(wave.registry_path(), os.path.join(
            os.path.expanduser("~"), ".claude", "orchestra", "waves.jsonl"))

    def test_register_writes_absolute_record(self):
        state = self._state("a")
        record = wave.register_wave(state, "plan.json", "repo", "wave-int",
                                    clock=self._clock(42.0))
        self.assertEqual(record, {
            "state": state, "plan": os.path.abspath("plan.json"),
            "repo": os.path.abspath("repo"), "into": "wave-int", "registered_at": 42.0})
        with open(self.registry) as fh:
            self.assertEqual(json.loads(fh.read()), record)

    def test_list_dedupes_last_wins_most_recent_first(self):
        a, b = self._state("a"), self._state("b")
        wave.register_wave(a, "/p1", "/r", "i1", clock=self._clock(1.0))
        wave.register_wave(b, "/p2", "/r", "i2", clock=self._clock(2.0))
        wave.register_wave(a, "/p3", "/r", "i3", clock=self._clock(3.0))
        listed = wave.list_waves()
        self.assertEqual([r["state"] for r in listed], [a, b])
        self.assertEqual(listed[0]["plan"], "/p3")

    def test_missing_state_file_skipped(self):
        a = self._state("a")
        wave.register_wave(a, "/p", "/r", "i")
        wave.register_wave(os.path.join(self.tmp, "gone", "state.json"), "/p", "/r", "i")
        self.assertEqual([r["state"] for r in wave.list_waves()], [a])

    def test_truncated_last_line_tolerated(self):
        a = self._state("a")
        wave.register_wave(a, "/p", "/r", "i")
        with open(self.registry, "a") as fh:
            fh.write('{"state": "/tru')
        self.assertEqual([r["state"] for r in wave.list_waves()], [a])

    def test_no_registry_file_lists_nothing(self):
        self.assertEqual(wave.list_waves(), [])


if __name__ == "__main__":
    unittest.main()
