import json
import os
import signal
import sys
import tempfile
import textwrap
import time
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec
import agent_exec_watchdog


class WatchdogCaptureTests(unittest.TestCase):
    def run_child(self, body, config, executor="pi", cls="standard"):
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "child.py")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(textwrap.dedent(body))
            result = []
            code, stdout, stderr = agent_exec._spawn_capture(
                [sys.executable, path], cwd=directory, env=None, input_text="",
                watchdog=config, executor=executor, cls=cls, watchdog_result=result,
            )
            return code, stdout, stderr, result[0]

    def test_idle_kills_and_keeps_partial_session(self):
        code, stdout, _, runaway = self.run_child(
            """
            import json, time
            print(json.dumps({'type': 'session', 'id': 'partial'}), flush=True)
            time.sleep(2)
            """,
            {"enabled": True, "idle_seconds": 0.08, "tool_idle_seconds": 1, "wall_seconds": {"standard": 2}},
        )
        parsed = agent_exec.parse_pi_jsonl(stdout, "", code)
        self.assertEqual(runaway["reason"], "idle")
        self.assertEqual(parsed["session_id"], "partial")

    def test_tool_idle_uses_longer_budget(self):
        code, _, _, runaway = self.run_child(
            """
            import json, time
            print(json.dumps({'type': 'tool_execution_start', 'toolCallId': 'c', 'toolName': 'bash', 'args': {'command': 'sleep'}}), flush=True)
            time.sleep(.15)
            """,
            {"enabled": True, "idle_seconds": 0.05, "tool_idle_seconds": 0.8, "wall_seconds": {"standard": 2}},
        )
        self.assertIsNone(runaway)
        self.assertEqual(code, 0)

    def test_consecutive_repeat_only(self):
        code, _, _, runaway = self.run_child(
            """
            import json, time
            def emit(name):
                print(json.dumps({'type': 'tool_execution_start', 'toolCallId': name, 'toolName': name, 'args': {'x': 1}}), flush=True)
                print(json.dumps({'type': 'tool_execution_end', 'toolCallId': name}), flush=True)
            for name in ['a', 'b', 'a', 'b', 'a']:
                emit(name)
            time.sleep(.2)
            """,
            {"enabled": True, "idle_seconds": 1, "tool_idle_seconds": 1, "repeat_limit": 3, "wall_seconds": {"standard": 2}},
        )
        self.assertIsNone(runaway)
        self.assertEqual(code, 0)

    def test_three_consecutive_repeats_kill(self):
        code, _, _, runaway = self.run_child(
            """
            import json, time
            for n in range(3):
                print(json.dumps({'type': 'tool_execution_start', 'toolCallId': str(n), 'toolName': 'bash', 'args': {'x': 1}}), flush=True)
                print(json.dumps({'type': 'tool_execution_end', 'toolCallId': str(n)}), flush=True)
            time.sleep(2)
            """,
            {"enabled": True, "idle_seconds": 1, "tool_idle_seconds": 1, "repeat_limit": 3, "wall_seconds": {"standard": 4}},
        )
        self.assertEqual(runaway["reason"], "repeat")
        self.assertEqual(code, -signal.SIGTERM)

    def test_default_config_is_active_and_uses_contract_values(self):
        config = agent_exec.DEFAULTS["watchdog"]
        self.assertTrue(config["enabled"])
        self.assertEqual(config["idle_seconds"], 600)
        self.assertEqual(config["tool_idle_seconds"], 900)
        self.assertEqual(config["repeat_limit"], 3)
        self.assertEqual(config["wall_seconds"], agent_exec_watchdog.DEFAULT_WALL_SECONDS)
        code, _, _, runaway = self.run_child(
            """
            import json, time
            for n in range(3):
                print(json.dumps({'type': 'tool_execution_start', 'toolCallId': str(n), 'toolName': 'bash', 'args': {'x': 1}}), flush=True)
                print(json.dumps({'type': 'tool_execution_end', 'toolCallId': str(n)}), flush=True)
            time.sleep(2)
            """,
            config,
        )
        self.assertEqual(runaway["reason"], "repeat")
        self.assertEqual(code, -signal.SIGTERM)

    def test_disabled_watchdog_does_not_kill(self):
        code, _, _, runaway = self.run_child(
            "import time; time.sleep(.15)",
            {"enabled": False, "idle_seconds": .01, "wall_seconds": {"standard": .01}},
        )
        self.assertIsNone(runaway)
        self.assertEqual(code, 0)

    def test_unknown_class_uses_standard_wall_budget(self):
        code, _, _, runaway = self.run_child(
            "import time; time.sleep(2)",
            {"enabled": True, "idle_seconds": 10, "tool_idle_seconds": 10,
             "wall_seconds": {"standard": .1}},
            cls="not-a-class",
        )
        self.assertEqual(runaway["reason"], "wall")
        self.assertNotEqual(code, 0)

    def test_runaway_status_and_ledger_telemetry_are_allowlisted_without_args(self):
        result = {"status": "runaway", "reason": "repeat", "runaway": {"last_tool": {"args": "secret"}}}
        record = agent_exec.build_dispatch_record("pi", result, None, "standard")
        self.assertEqual(agent_exec.sanitize_telemetry_record(record)["status"], "runaway")
        self.assertEqual(agent_exec.sanitize_telemetry_record(record)["reason"], "repeat")
        ledger = agent_exec.sanitize_run_ledger_record({"executor": "pi", "cls": "standard", **result})
        self.assertEqual(ledger["status"], "runaway")
        self.assertNotIn("runaway", ledger)
        self.assertFalse(agent_exec.record_unavailable_cooldown({}, "pi", "runaway", time.time()))

    def test_wall_kills_and_grandchild_is_in_group(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = os.path.join(directory, "grandchild.pid")
            body = "import subprocess, sys, time\nsubprocess.Popen([sys.executable, '-c', %r])\ntime.sleep(10)\n" % (
                'import os,time; open(%r, "w").write(str(os.getpid())); time.sleep(10)' % marker
            )
            path = os.path.join(directory, "child.py")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(textwrap.dedent(body))
            result = []
            code, _, _ = agent_exec._spawn_capture(
                [sys.executable, path], cwd=directory, env=None, input_text="",
                watchdog={"enabled": True, "idle_seconds": 10, "tool_idle_seconds": 10, "wall_seconds": {"standard": .5}},
                executor="pi", watchdog_result=result,
            )
            runaway = result[0]
            self.assertEqual(runaway["reason"], "wall")
            with open(marker, encoding="utf-8") as handle:
                pid = int(handle.read())
            for _ in range(20):
                try:
                    os.kill(pid, 0)
                except OSError:
                    break
                time.sleep(.02)
            else:
                self.fail("grandchild survived process-group termination")
            self.assertNotEqual(code, 0)


if __name__ == "__main__":
    unittest.main()
