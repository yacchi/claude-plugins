# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""End-to-end tests for the agent-exec ledger command."""

import json
import os
import subprocess
import sys
import tempfile
import unittest


ROOT = os.path.dirname(os.path.abspath(__file__))
AGENT_EXEC = os.path.join(ROOT, "agent_exec.py")


def run_cli(home, args):
    env = dict(os.environ)
    env["HOME"] = home
    env.pop("CLAUDE_CONFIG_DIR", None)
    return subprocess.run(
        [sys.executable, AGENT_EXEC] + args,
        cwd=home, env=env, capture_output=True, text=True, timeout=60,
    )


class LedgerCommandTests(unittest.TestCase):
    def _home(self):
        home = tempfile.TemporaryDirectory()
        ledger = os.path.join(home.name, "ledger")
        os.makedirs(os.path.join(home.name, ".claude"), exist_ok=True)
        with open(os.path.join(home.name, ".claude", "orchestra.yaml"), "w",
                  encoding="utf-8") as handle:
            handle.write("ledger:\n  dir: %s\n" % ledger)
        os.makedirs(ledger)
        with open(os.path.join(ledger, "session-1.jsonl"), "w",
                  encoding="utf-8") as handle:
            handle.write(json.dumps({
                "executor": "copilot", "run": "run-1",
                "input_tokens": 4, "output_tokens": 2,
            }) + "\n")
            handle.write(json.dumps({
                "executor": "codex", "input_tokens": 3,
            }) + "\n")
        return home, ledger

    def test_show_json_and_selectors(self):
        home, ledger = self._home()
        try:
            proc = run_cli(home.name, ["ledger", "show", "--json"])
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertEqual(json.loads(proc.stdout)["records"], 2)
            proc = run_cli(home.name, ["ledger", "show", "--session", "session-1",
                                       "--json"])
            self.assertEqual(json.loads(proc.stdout)["executors"]["copilot"]["input_tokens"], 4)
            proc = run_cli(home.name, ["ledger", "show", "--run", "run-1", "--json"])
            self.assertEqual(json.loads(proc.stdout)["records"], 1)
        finally:
            home.cleanup()

    def test_empty_and_invalid_commands(self):
        with tempfile.TemporaryDirectory() as home:
            for args in (["ledger", "show"], ["ledger", "archive"]):
                proc = run_cli(home, args)
                self.assertEqual(proc.returncode, 0)
            for args in (["ledger", "show", "--bad"],
                         ["ledger", "show", "--run", "a/b"]):
                proc = run_cli(home, args)
                self.assertEqual(proc.returncode, 2)
                self.assertEqual(proc.stdout, "")

    def test_clear_yes_removes_files(self):
        home, ledger = self._home()
        try:
            proc = run_cli(home.name, ["ledger", "clear", "--yes"])
            self.assertEqual(proc.returncode, 0)
            self.assertEqual(os.listdir(ledger), [])
        finally:
            home.cleanup()


if __name__ == "__main__":
    unittest.main()
