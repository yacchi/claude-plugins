"""Regression tests for task session persistence after terminal dispatch outcomes."""
import io
import json
import os
import shutil
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_exec


class DispatchSessionOutcomeTests(unittest.TestCase):
    def setUp(self):
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home, True)
        self.work = os.path.join(self.home, "work")
        os.makedirs(self.work)
        self.prompt = os.path.join(self.home, "prompt.md")
        with open(self.prompt, "w") as fh:
            fh.write("hello")
        self.cfg = agent_exec.copy.deepcopy(agent_exec.DEFAULTS)
        self.cfg["ledger"]["enabled"] = False
        self.cfg["telemetry"] = {"enabled": False, "dir": os.path.join(self.home, "tel")}

    def test_needs_permission_and_runaway_keep_the_session_for_cli_session(self):
        route = {"dispatch": "cli", "executor": "pi", "model": "m",
                 "effort": "low", "agent_type": None}
        for status in ("needs-permission", "runaway"):
            with self.subTest(status=status), mock.patch.dict(os.environ, {"HOME": self.home}), \
                    mock.patch.object(agent_exec, "resolve_config", return_value=(self.cfg, None)), \
                    mock.patch.object(agent_exec, "resolve_route", return_value=route), \
                    mock.patch.object(agent_exec, "_build_doctor_report", return_value={}), \
                    mock.patch.object(agent_exec, "_capture_with_grants", return_value=(
                        0, {"status": status, "reason": "sandbox" if status == "needs-permission" else "idle",
                            "answer": "x", "session_id": "sid-" + status,
                            "exit_code": 0, "usage": None, "resumed": False,
                            "sandbox": {"backend": "none", "denials": [], "granted": [], "cycles": 0}},
                        {"backend": "none"})), \
                    mock.patch.object(sys, "stdout", io.StringIO()):
                agent_exec.cmd_dispatch_route([
                    "--class", "standard", "--prompt-file", self.prompt,
                    "--workdir", self.work, "--isolate", "never", "--task", "task-" + status])
            out = io.StringIO()
            with mock.patch.dict(os.environ, {"HOME": self.home}), \
                    mock.patch.object(agent_exec, "resolve_config", return_value=(self.cfg, None)), \
                    mock.patch.object(sys, "stdout", out):
                agent_exec.cmd_dispatch_route(["session", "--task", "task-" + status, "--json"])
            session = json.loads(out.getvalue())
            self.assertEqual(session["session_id"], "sid-" + status)


if __name__ == "__main__":
    unittest.main()
