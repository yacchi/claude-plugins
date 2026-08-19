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

    def _ordinal_home(self):
        """A session directory with ordinals spanning past the 3-digit
        width, deliberately created out of numeric order on disk."""
        home = tempfile.TemporaryDirectory()
        ledger = os.path.join(home.name, "ledger")
        os.makedirs(os.path.join(home.name, ".claude"), exist_ok=True)
        with open(os.path.join(home.name, ".claude", "orchestra.yaml"), "w",
                  encoding="utf-8") as handle:
            handle.write("ledger:\n  dir: %s\n" % ledger)
        session = os.path.join(ledger, "ord-session")
        os.makedirs(session)
        for name, count in (
            ("1000-run_c.jsonl", 1),
            ("001-run_a.jsonl", 1),
            ("999-run_b.jsonl", 1),
        ):
            with open(os.path.join(session, name), "w",
                      encoding="utf-8") as handle:
                for _ in range(count):
                    handle.write(json.dumps({
                        "executor": "copilot", "input_tokens": 1,
                    }) + "\n")
        return home, ledger

    def test_show_displays_ordinals_ordered_numerically_past_999(self):
        home, ledger = self._ordinal_home()
        try:
            proc = run_cli(home.name, ["ledger", "show", "--json"])
            self.assertEqual(proc.returncode, 0, proc.stderr)
            parsed = json.loads(proc.stdout)
            session_summary = parsed["sessions"]["ord-session"]
            self.assertEqual(session_summary["ordinals"], {
                "run_a": 1, "run_b": 999, "run_c": 1000,
            })
            # Numeric order, not lexical: "1000-" would sort before
            # "999-" as strings but must come after as integers.
            self.assertEqual(
                list(session_summary["runs"].keys()),
                ["run_a", "run_b", "run_c"])

            proc = run_cli(home.name, ["ledger", "show", "--text"])
            self.assertEqual(proc.returncode, 0, proc.stderr)
            lines = [ln for ln in proc.stdout.splitlines() if "run_" in ln]
            self.assertEqual(len(lines), 3)
            self.assertIn("001 run_a", lines[0])
            self.assertIn("999 run_b", lines[1])
            self.assertIn("1000 run_c", lines[2])
        finally:
            home.cleanup()

    def test_show_run_ordinal_matches_same_run_as_full_id(self):
        home, ledger = self._ordinal_home()
        try:
            env = dict(os.environ)
            env["HOME"] = home.name
            env["CLAUDE_CODE_SESSION_ID"] = "ord-session"
            env.pop("CLAUDE_CONFIG_DIR", None)
            proc_ordinal = subprocess.run(
                [sys.executable, AGENT_EXEC, "ledger", "show",
                 "--run", "999", "--json"],
                cwd=home.name, env=env, capture_output=True, text=True,
                timeout=60,
            )
            proc_full = subprocess.run(
                [sys.executable, AGENT_EXEC, "ledger", "show",
                 "--run", "run_b", "--json"],
                cwd=home.name, env=env, capture_output=True, text=True,
                timeout=60,
            )
            self.assertEqual(proc_ordinal.returncode, 0, proc_ordinal.stderr)
            self.assertEqual(proc_full.returncode, 0, proc_full.stderr)
            parsed_ordinal = json.loads(proc_ordinal.stdout)
            parsed_full = json.loads(proc_full.stdout)
            self.assertEqual(parsed_ordinal, parsed_full)
            self.assertEqual(parsed_ordinal["ordinal"], 999)
            self.assertEqual(parsed_ordinal["records"], 1)
        finally:
            home.cleanup()

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
