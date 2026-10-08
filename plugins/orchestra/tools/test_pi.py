# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for the pi CLI executor and the shared streaming spawn helper.

Fixtures are small excerpts of real `pi -p --mode json` output (pi 1.1.0).
"""

import json
import os
import sys
import tempfile
import textwrap
import unittest
from unittest import mock

import agent_exec


SID = "01a11b14-6032-733f-aa30-17504543f162"


def _line(obj):
    return json.dumps(obj, ensure_ascii=False)


def _usage(inp, out, cache_read=0, cost=0.0, reasoning=0):
    return {
        "input": inp, "output": out, "cacheRead": cache_read, "cacheWrite": 0,
        "reasoning": reasoning, "totalTokens": inp + out,
        "cost": {"total": cost},
    }


def _assistant_end(text=None, usage=None, stop="stop", error=None, tool=False):
    content = []
    if text is not None:
        content.append({"type": "text", "text": text})
    if tool:
        content.append({"type": "toolCall", "name": "bash"})
    message = {
        "role": "assistant", "content": content,
        "usage": usage or _usage(0, 0), "stopReason": stop,
    }
    if error is not None:
        message["errorMessage"] = error
    return _line({"type": "message_end", "message": message})


def _session():
    return _line({"type": "session", "version": 3, "id": SID, "cwd": "/w"})


def _stdout(*lines):
    return "\n".join(lines) + "\n"


class ParsePiTests(unittest.TestCase):
    def test_rc0_with_final_error_stop_is_not_ok(self):
        out = _stdout(
            _session(),
            _assistant_end(
                stop="error",
                error="Codex error: The 'no-such-model' model is not supported "
                      "when using Codex with a ChatGPT account.",
            ),
        )
        r = agent_exec.parse_pi_jsonl(out, "Warning: Model not found.\n", 0)
        self.assertNotEqual(r["status"], "ok")
        self.assertEqual(r["status"], "unavailable")
        self.assertEqual(r["session_id"], SID)

    def test_error_message_is_classified_with_unavailable_patterns(self):
        out = _stdout(
            _session(),
            _assistant_end(stop="error", error="429 rate limit exceeded"),
        )
        r = agent_exec.parse_pi_jsonl(out, "", 0)
        self.assertEqual(r["status"], "unavailable")
        self.assertEqual(r["reason"], "rate-limit")

    def test_no_api_key_stderr_is_auth(self):
        stderr = (
            "No API key found for openai.\n\nUse /login to log into a "
            "provider via OAuth or API key.\n"
        )
        r = agent_exec.parse_pi_jsonl(_stdout(_session()), stderr, 1)
        self.assertEqual(r["status"], "unavailable")
        self.assertEqual(r["reason"], "auth")

    def test_answer_comes_from_last_assistant_message(self):
        out = _stdout(
            _session(),
            _assistant_end(text="let me look", tool=True, stop="toolUse"),
            _assistant_end(text="DONE", stop="stop"),
        )
        r = agent_exec.parse_pi_jsonl(out, "", 0)
        self.assertEqual(r["status"], "ok")
        self.assertEqual(r["answer"], "DONE")

    def test_usage_sums_assistant_message_end_only(self):
        out = _stdout(
            _session(),
            _line({"type": "message_update", "usage": _usage(999, 999, cost=9.0)}),
            _line({"type": "message_end", "message": {
                "role": "user", "content": [], "usage": _usage(500, 500, cost=5.0)}}),
            _assistant_end(text="a", usage=_usage(100, 10, cache_read=40,
                                                  cost=0.001, reasoning=3),
                           stop="toolUse"),
            _line({"type": "message_update", "usage": _usage(888, 888, cost=8.0)}),
            _assistant_end(text="b", usage=_usage(200, 20, cache_read=60,
                                                  cost=0.002, reasoning=4)),
        )
        r = agent_exec.parse_pi_jsonl(out, "", 0)
        self.assertEqual(r["usage"]["tokens"], {
            "input_tokens": 300, "output_tokens": 30, "cached_input_tokens": 100,
            "cache_write_input_tokens": 0, "reasoning_output_tokens": 7,
        })
        self.assertEqual(r["usage"]["cost_micro_usd"], 3000)

    def test_tool_output_mentioning_rate_limit_is_not_unavailable(self):
        out = _stdout(
            _session(),
            _line({"type": "tool_execution_end", "isError": True,
                   "result": {"content": [{"type": "text",
                                           "text": "rate limit quota 429 exceeded"}]}}),
            _line({"type": "message_end", "message": {
                "role": "toolResult",
                "content": [{"type": "text", "text": "quota usage limit 401 login"}]}}),
            _assistant_end(text="the file talks about a rate limit", stop="stop"),
        )
        r = agent_exec.parse_pi_jsonl(out, "", 0)
        self.assertEqual(r["status"], "ok")
        self.assertIsNone(r["reason"])

    def test_record_with_u2028_is_not_split(self):
        text = "before after"
        out = _stdout(_session(), _assistant_end(text=text))
        r = agent_exec.parse_pi_jsonl(out, "", 0)
        self.assertEqual(r["status"], "ok")
        self.assertEqual(r["answer"], text)


class BuildPiArgvTests(unittest.TestCase):
    def test_prompt_not_in_argv_and_session_only_on_resume(self):
        secret = "SECRET-PROMPT-BODY"
        fresh = agent_exec._build_pi_argv(
            "pi", "openai-codex/gpt-5.6-luna", "medium", "/w", secret, None, "json")
        self.assertNotIn(secret, " ".join(fresh))
        self.assertNotIn("--session", fresh)
        resumed = agent_exec._build_pi_argv(
            "pi", "openai-codex/gpt-5.6-luna", "medium", "/w", secret, SID, "json")
        self.assertNotIn(secret, " ".join(resumed))
        self.assertEqual(resumed[resumed.index("--session") + 1], SID)


class UnregisteredProfileTests(unittest.TestCase):
    def test_unregistered_profile_does_not_fall_back_to_another_executor(self):
        with self.assertRaises(ValueError):
            agent_exec._build_executor_argv(
                "nonesuch", "nonesuch", "m", "low", "/w", "p", None, "json")
        with self.assertRaises(ValueError):
            agent_exec._run_executor_capture(
                "nonesuch", "m", "low", "/w", "p", None)


class RunPiCaptureTests(unittest.TestCase):
    def test_child_env_has_skip_version_check_and_prompt_on_stdin(self):
        seen = {}

        def fake_spawn(argv, *, cwd, env, input_text, on_line=None, sandbox=None):
            seen.update(argv=argv, env=env, input_text=input_text)
            return 0, _stdout(_session(), _assistant_end(text="PONG")), ""

        env = {k: v for k, v in os.environ.items() if k != "PI_SKIP_VERSION_CHECK"}
        with mock.patch.object(agent_exec, "_spawn_capture", fake_spawn), \
                mock.patch.object(agent_exec.shutil, "which", return_value="/bin/pi"), \
                mock.patch.dict(os.environ, env, clear=True):
            code, result = agent_exec._run_pi_capture(
                "pi", "m", "low", tempfile.gettempdir(), "the prompt", None)
        self.assertEqual(code, 0)
        self.assertEqual(result["answer"], "PONG")
        self.assertEqual(seen["env"]["PI_SKIP_VERSION_CHECK"], "1")
        self.assertEqual(seen["input_text"], "the prompt")


class RunPiCaptureWorkdirTests(unittest.TestCase):
    """The CLI must run *inside* the workdir, else a worker told to create a
    file "in the working directory" writes it wherever agent-exec was invoked
    from -- outside the isolated worktree."""

    def _run(self, workdir):
        seen = {}

        def fake_spawn(argv, *, cwd, env, input_text, on_line=None, sandbox=None):
            seen["cwd"] = cwd
            return 0, _stdout(_session(), _assistant_end(text="ok")), ""

        with mock.patch.object(agent_exec, "_spawn_capture", fake_spawn), \
                mock.patch.object(agent_exec.shutil, "which", return_value="/bin/pi"):
            agent_exec._run_pi_capture("pi", "m", "low", workdir, "p", None)
        return seen["cwd"]

    def test_capture_runs_in_workdir(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(self._run(tmp), tmp)

    def test_missing_workdir_inherits_cwd_instead_of_raising(self):
        self.assertIsNone(self._run("/no/such/dir"))


class PiLedgerRecordTests(unittest.TestCase):
    def test_cost_and_extra_token_keys_reach_the_ledger_record(self):
        result = agent_exec.parse_pi_jsonl(
            _stdout(_session(), _assistant_end(
                text="x", usage=_usage(10, 6, cache_read=4, cost=0.002699))),
            "", 0)
        record = agent_exec.build_run_ledger_record(
            "pi", "openai-codex/gpt-5.6-luna", "light", result)
        self.assertEqual(record["executor"], "pi")
        self.assertEqual(record["status"], "ok")
        self.assertEqual(record["cost_micro_usd"], 2699)

    def test_ledger_sanitizer_keeps_executor_cost_and_extra_tokens(self):
        """`executor` must survive sanitization or `usage --run` finds
        nothing: `_ledger_usage` filters records by that exact field."""
        sanitized = agent_exec.sanitize_run_ledger_record({
            "executor": "pi", "cls": "light", "status": "ok",
            "cost_micro_usd": 2699, "reasoning_output_tokens": 10,
            "cache_write_input_tokens": 4,
        })
        self.assertEqual(sanitized["executor"], "pi")
        self.assertEqual(sanitized["cost_micro_usd"], 2699)
        self.assertEqual(sanitized["reasoning_output_tokens"], 10)
        self.assertEqual(sanitized["cache_write_input_tokens"], 4)

    def test_ledger_usage_attributes_cost_to_pi(self):
        records = [
            {"executor": "pi", "input_tokens": 3, "output_tokens": 8,
             "cached_input_tokens": 10766, "cost_micro_usd": 226},
            {"executor": "codex", "input_tokens": 99, "cost_micro_usd": 5},
        ]
        acc = agent_exec._ledger_usage(records, "pi")
        self.assertEqual(acc["records"], 1)
        self.assertEqual(acc["cost_micro_usd"], 226)
        self.assertEqual(acc["tokens"]["cached_input_tokens"], 10766)

    def test_pi_is_a_usage_source(self):
        self.assertIn("pi", agent_exec._USAGE_SOURCES)


class SpawnCaptureTests(unittest.TestCase):
    def test_large_stdin_stdout_stderr_does_not_deadlock(self):
        child = textwrap.dedent("""
            import sys
            data = sys.stdin.buffer.read()
            sys.stdout.buffer.write(data)
            sys.stderr.buffer.write(data)
        """)
        payload = ("x" * 99 + "\n") * 10000  # ~1 MB
        code, out, err = agent_exec._spawn_capture(
            [sys.executable, "-c", child], cwd=None, env=None,
            input_text=payload)
        self.assertEqual(code, 0)
        self.assertEqual(out, payload)
        self.assertEqual(err, payload)

    def test_on_line_splits_on_newline_only(self):
        child = (
            "import sys\n"
            "sys.stdout.buffer.write('a\\u2028b\\r\\nc\\n'.encode())\n"
        )
        lines = []
        code, out, _ = agent_exec._spawn_capture(
            [sys.executable, "-c", child], cwd=None, env=None,
            input_text="", on_line=lines.append)
        self.assertEqual(code, 0)
        self.assertEqual(lines, ["a b", "c"])


if __name__ == "__main__":
    unittest.main()
