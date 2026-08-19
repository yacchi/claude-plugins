# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for the numbers-only external-dispatch run ledger."""

import contextlib
import io
import json
import os
import stat
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402


class RunLedgerSanitizingTests(unittest.TestCase):
    def test_allowlist_and_numeric_validation(self):
        raw = {
            "ts": "2026-08-19T07:00:00+00:00",
            "executor": "copilot",
            "cls": "standard",
            "model": "gpt-5.6-luna",
            "status": "error",
            "input_tokens": 1,
            "output_tokens": 2,
            "cached_input_tokens": 3,
            "aiu_nano": 4,
            "premium_requests": 5,
            "api_duration_ms": 6,
            "session_duration_ms": 7,
            "extra": "drop",
        }
        result = agent_exec.sanitize_run_ledger_record(raw)
        self.assertNotIn("extra", result)
        for key in raw:
            if key != "extra":
                self.assertIn(key, result)

        for key in (
            "input_tokens", "output_tokens", "cached_input_tokens", "aiu_nano",
            "premium_requests", "api_duration_ms", "session_duration_ms",
        ):
            for value in (True, False, -1):
                invalid = dict(raw, **{key: value})
                self.assertNotIn(key, agent_exec.sanitize_run_ledger_record(invalid))

    def test_bad_model_is_dropped(self):
        base = {"executor": "copilot", "status": "ok", "input_tokens": 1}
        for model in ("has space", "has\nnewline", "x" * 65):
            result = agent_exec.sanitize_run_ledger_record(dict(base, model=model))
            self.assertNotIn("model", result)
            self.assertEqual(result["input_tokens"], 1)

    def test_adversarial_fields_never_reach_disk(self):
        raw = {
            "executor": "copilot",
            "cls": "light",
            "status": "ok",
            "model": "model",
            "prompt": "secret prompt text",
            "path": "/absolute/private/file",
            "session_id": "session-secret",
            "nested": {"prompt": "secret prompt text"},
        }
        with tempfile.TemporaryDirectory() as tmp:
            cfg = {"telemetry": {"dir": os.path.join(tmp, "telemetry"), "enabled": False}}
            agent_exec.run_ledger_append(raw, "run_a", cfg)
            path = os.path.join(tmp, "runs", "run_a.jsonl")
            with open(path, encoding="utf-8") as f:
                line = f.read()
            for secret in ("secret prompt text", "/absolute/private/file", "session-secret"):
                self.assertNotIn(secret, line)


class RunLedgerWritingTests(unittest.TestCase):
    def test_append_mode_compact_json_and_telemetry_independent(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = {"telemetry": {"dir": os.path.join(tmp, "telemetry"), "enabled": False}}
            record = {"executor": "copilot", "model": "gpt-5.6-luna", "status": "ok"}
            agent_exec.run_ledger_append(record, "wf_a26027ae-bdb", cfg)
            agent_exec.run_ledger_append(record, "wf_a26027ae-bdb", cfg)
            directory = os.path.join(tmp, "runs")
            self.assertEqual(stat.S_IMODE(os.stat(directory).st_mode), 0o700)
            with open(os.path.join(directory, "wf_a26027ae-bdb.jsonl"), encoding="utf-8") as f:
                lines = f.readlines()
            self.assertEqual(len(lines), 2)
            for line in lines:
                self.assertEqual(json.loads(line)["executor"], "copilot")
                self.assertNotIn(": ", line)

    def test_append_never_raises(self):
        cfg = {"telemetry": {"dir": "/dev/null/telemetry", "enabled": False}}
        agent_exec.run_ledger_append({"executor": "copilot", "status": "ok"}, "run", cfg)


class RunIdValidationTests(unittest.TestCase):
    def _run(self, command, run_id):
        prompt = tempfile.NamedTemporaryFile(mode="w", delete=False)
        prompt.write("hello")
        prompt.close()
        try:
            args = (
                ["run", "copilot", "--run-id", run_id, "--model", "m",
                 "--effort", "e", "--workdir", ".", "--prompt-file", prompt.name]
                if command == "run" else
                ["dispatch", "--run-id", run_id, "--class", "light",
                 "--prompt-file", prompt.name, "--workdir", "."]
            )
            out, err = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                code = agent_exec.main(args)
            return code, out.getvalue(), err.getvalue()
        finally:
            os.unlink(prompt.name)

    def test_invalid_ids_exit_two_without_stdout(self):
        for value in ("", "a b", "../x", "x" * 65):
            for command in ("run", "dispatch"):
                code, out, err = self._run(command, value)
                self.assertEqual(code, 2)
                self.assertEqual(out, "")
                self.assertIn(value, err)

    def test_no_run_id_does_not_create_ledger(self):
        with tempfile.TemporaryDirectory() as tmp:
            old = agent_exec.resolve_config
            old_dryrun = os.environ.get("AGENT_EXEC_DRYRUN")
            os.environ["AGENT_EXEC_DRYRUN"] = "1"
            agent_exec.resolve_config = lambda: (
                {"telemetry": {"dir": os.path.join(tmp, "telemetry"), "enabled": False}},
                None,
            )
            try:
                prompt = tempfile.NamedTemporaryFile(mode="w", delete=False)
                prompt.write("hello")
                prompt.close()
                try:
                    self.assertEqual(
                        agent_exec.main([
                            "run", "copilot", "--model", "m", "--effort", "e",
                            "--workdir", ".", "--prompt-file", prompt.name,
                        ]),
                        0,
                    )
                finally:
                    os.unlink(prompt.name)
            finally:
                agent_exec.resolve_config = old
                if old_dryrun is None:
                    os.environ.pop("AGENT_EXEC_DRYRUN", None)
                else:
                    os.environ["AGENT_EXEC_DRYRUN"] = old_dryrun
            self.assertFalse(os.path.exists(os.path.join(tmp, "runs")))


if __name__ == "__main__":
    unittest.main()
