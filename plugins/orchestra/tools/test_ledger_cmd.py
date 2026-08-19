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
        session = os.path.join(ledger, "session-1")
        os.makedirs(session)
        with open(os.path.join(session, "run-1.jsonl"), "w",
                  encoding="utf-8") as handle:
            handle.write(json.dumps({
                "executor": "copilot",
                "input_tokens": 4, "output_tokens": 2,
            }) + "\n")
        with open(os.path.join(session, "no.run.jsonl"), "w",
                  encoding="utf-8") as handle:
            handle.write(json.dumps({
                "executor": "codex", "input_tokens": 3,
            }) + "\n")
        with open(os.path.join(ledger, "legacy-run.jsonl"), "w",
                  encoding="utf-8") as handle:
            handle.write(json.dumps({
                "executor": "copilot", "input_tokens": 1,
            }) + "\n")
        return home, ledger

    def test_show_json_and_selectors(self):
        home, ledger = self._home()
        try:
            proc = run_cli(home.name, ["ledger", "show", "--json"])
            self.assertEqual(proc.returncode, 0, proc.stderr)
            parsed = json.loads(proc.stdout)
            self.assertEqual(parsed["records"], 3)
            # Distinguishes session directories (with their per-run
            # breakdown) from legacy flat run files.
            self.assertIn("session-1", parsed["sessions"])
            self.assertEqual(parsed["sessions"]["session-1"]["records"], 2)
            self.assertEqual(
                parsed["sessions"]["session-1"]["runs"]["run-1"], 1)
            self.assertIn("legacy-run", parsed["legacy_runs"])
            self.assertEqual(parsed["legacy_runs"]["legacy-run"]["records"], 1)
            proc = run_cli(home.name, ["ledger", "show", "--session", "session-1",
                                       "--json"])
            self.assertEqual(json.loads(proc.stdout)["executors"]["copilot"]["input_tokens"], 4)
            proc = run_cli(home.name, ["ledger", "show", "--run", "run-1", "--json"])
            self.assertEqual(json.loads(proc.stdout)["records"], 1)
            proc = run_cli(home.name, ["ledger", "show", "--run", "legacy-run",
                                       "--json"])
            self.assertEqual(json.loads(proc.stdout)["records"], 0)
        finally:
            home.cleanup()

    def test_show_run_selector_ignores_session_id(self):
        # A session id must never satisfy --run: the session is a
        # directory, not a file named <sid>.jsonl.
        home, ledger = self._home()
        try:
            proc = run_cli(home.name, ["ledger", "show", "--run", "session-1",
                                       "--json"])
            self.assertEqual(proc.returncode, 0, proc.stderr)
            parsed = json.loads(proc.stdout)
            self.assertEqual(parsed["records"], 0)
            self.assertEqual(parsed["executors"], {})
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
            # Legacy flat files are removed...
            self.assertFalse(
                os.path.exists(os.path.join(ledger, "legacy-run.jsonl")))
            # ...and the session directory's contents are removed, but the
            # session directory itself survives.
            self.assertEqual(os.listdir(ledger), ["session-1"])
            self.assertEqual(os.listdir(os.path.join(ledger, "session-1")), [])
        finally:
            home.cleanup()


if __name__ == "__main__":
    unittest.main()
