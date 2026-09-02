"""Tests for the opencode executor: argv shape, JSONL parsing, ledger flow.

Fixtures mirror real `opencode run --format json` output captured from
opencode 1.18.26 against `github-copilot/gpt-5.6-luna`.
"""

import json
import shutil
import tempfile
import unittest

import agent_exec


def _line(obj):
    return json.dumps(obj)


SESSION = "ses_f9d466279ffeI5zKC2hLql5MAK"


def _success_lines(text="OCOK", cost=0.00269905):
    return [
        _line({
            "type": "step_start",
            "sessionID": SESSION,
            "part": {"id": "prt_a", "type": "step-start"},
        }),
        _line({
            "type": "text",
            "sessionID": SESSION,
            "part": {"id": "prt_b", "type": "text", "text": text},
        }),
        _line({
            "type": "step_finish",
            "sessionID": SESSION,
            "part": {
                "id": "prt_c",
                "type": "step-finish",
                "reason": "stop",
                "tokens": {
                    "total": 10774,
                    "input": 10768,
                    "output": 6,
                    "reasoning": 0,
                    "cache": {"write": 0, "read": 0},
                },
                "cost": cost,
            },
        }),
    ]


class ParseOpencodeJsonlTests(unittest.TestCase):
    def _parse(self, lines, stderr="", exit_code=0):
        return agent_exec.parse_opencode_jsonl(
            "\n".join(lines), stderr, exit_code
        )

    def test_success_yields_answer_session_tokens_and_cost(self):
        result = self._parse(_success_lines())
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["answer"], "OCOK")
        self.assertEqual(result["session_id"], SESSION)
        self.assertIsNone(result["reason"])
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(
            result["usage"]["tokens"],
            {
                "input_tokens": 10768,
                "output_tokens": 6,
                "reasoning_output_tokens": 0,
                "cached_input_tokens": 0,
                "cache_write_input_tokens": 0,
            },
        )
        # 0.00269905 USD -> micro-USD, rounded to an int the ledger accepts.
        self.assertEqual(result["usage"]["cost_micro_usd"], 2699)

    def test_resumed_turn_reports_cache_read_and_reasoning(self):
        lines = [
            _line({
                "type": "text",
                "sessionID": SESSION,
                "part": {"id": "prt_x", "type": "text", "text": "OCOK"},
            }),
            _line({
                "type": "step_finish",
                "sessionID": SESSION,
                "part": {
                    "type": "step-finish",
                    "tokens": {
                        "total": 10818,
                        "input": 35,
                        "output": 8,
                        "reasoning": 10,
                        "cache": {"write": 0, "read": 10765},
                    },
                    "cost": 0.0002455,
                },
            }),
        ]
        result = self._parse(lines)
        tokens = result["usage"]["tokens"]
        self.assertEqual(tokens["cached_input_tokens"], 10765)
        self.assertEqual(tokens["reasoning_output_tokens"], 10)
        self.assertEqual(result["usage"]["cost_micro_usd"], 246)

    def test_multiple_steps_accumulate_tokens_and_cost(self):
        lines = _success_lines(text="A", cost=0.001) + [
            _line({
                "type": "text",
                "sessionID": SESSION,
                "part": {"id": "prt_d", "type": "text", "text": "B"},
            }),
            _line({
                "type": "step_finish",
                "sessionID": SESSION,
                "part": {
                    "type": "step-finish",
                    "tokens": {
                        "input": 32,
                        "output": 4,
                        "reasoning": 1,
                        "cache": {"write": 7, "read": 11},
                    },
                    "cost": 0.002,
                },
            }),
        ]
        result = self._parse(lines)
        self.assertEqual(result["answer"], "AB")
        tokens = result["usage"]["tokens"]
        self.assertEqual(tokens["input_tokens"], 10768 + 32)
        self.assertEqual(tokens["output_tokens"], 6 + 4)
        self.assertEqual(tokens["cache_write_input_tokens"], 7)
        self.assertEqual(tokens["cached_input_tokens"], 11)
        self.assertEqual(result["usage"]["cost_micro_usd"], 3000)

    def test_repeated_part_id_keeps_latest_not_concatenation(self):
        lines = [
            _line({
                "type": "text",
                "sessionID": SESSION,
                "part": {"id": "prt_b", "type": "text", "text": "part"},
            }),
            _line({
                "type": "text",
                "sessionID": SESSION,
                "part": {"id": "prt_b", "type": "text", "text": "partial"},
            }),
        ]
        self.assertEqual(self._parse(lines)["answer"], "partial")

    def test_error_event_with_nonzero_exit_is_unavailable(self):
        lines = [
            _line({
                "type": "error",
                "sessionID": "ses_bad",
                "error": {
                    "name": "UnknownError",
                    "data": {
                        "message":
                            "Model not found: github-copilot/nope-xyz."
                    },
                },
            }),
        ]
        result = self._parse(lines, exit_code=1)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "nonzero-exit")
        self.assertIsNone(result["answer"])
        self.assertEqual(result["session_id"], "ses_bad")

    def test_auth_error_classifies_reason_before_exit_code(self):
        lines = [
            _line({
                "type": "error",
                "sessionID": SESSION,
                "error": {"data": {"message": "401 unauthorized"}},
            }),
        ]
        result = self._parse(lines, exit_code=1)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "auth")

    def test_quota_wording_in_worker_answer_does_not_mark_unavailable(self):
        """Default-deny: `text` is the worker's own answer, never scanned.

        A worker asked to implement a rate limiter says "rate limit" in its
        reply; treating that as executor health would exhaust the executor
        for the rest of the run."""
        lines = [
            _line({
                "type": "text",
                "sessionID": SESSION,
                "part": {
                    "id": "prt_b",
                    "type": "text",
                    "text": "Added a rate limit guard; quota errors now 429.",
                },
            }),
            _line({
                "type": "step_finish",
                "sessionID": SESSION,
                "part": {"type": "step-finish", "cost": 0.0},
            }),
        ]
        result = self._parse(lines)
        self.assertEqual(result["status"], "ok")
        self.assertIsNone(result["reason"])

    def test_quota_wording_on_stderr_is_scanned(self):
        result = self._parse(
            _success_lines(), stderr="rate limit exceeded", exit_code=1
        )
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "rate-limit")

    def test_unparseable_stdout_line_is_scanned(self):
        result = self._parse(
            ["not json at all: insufficient credits"], exit_code=1
        )
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "credits")

    def test_empty_output_has_no_usage(self):
        result = self._parse([])
        self.assertEqual(result["status"], "ok")
        self.assertIsNone(result["answer"])
        self.assertIsNone(result["usage"])
        self.assertIsNone(result["session_id"])

    def test_negative_or_bool_values_are_ignored(self):
        lines = [
            _line({
                "type": "step_finish",
                "sessionID": SESSION,
                "part": {
                    "type": "step-finish",
                    "tokens": {"input": -5, "output": True},
                    "cost": -1.0,
                },
            }),
        ]
        result = self._parse(lines)
        self.assertIsNone(result["usage"])


class BuildOpencodeArgvTests(unittest.TestCase):
    def test_argv_shape_without_resume(self):
        argv = agent_exec._build_opencode_argv(
            "opencode", "github-copilot/gpt-5.6-luna", "medium", "/w",
            "PROMPT", None, "json",
        )
        self.assertEqual(argv, [
            "opencode", "run", "--format", "json", "--auto",
            "--model", "github-copilot/gpt-5.6-luna",
            "--variant", "medium",
            "--dir", "/w",
        ])

    def test_prompt_never_appears_in_argv(self):
        """The prompt goes on stdin: it can be large and can begin with `-`."""
        argv = agent_exec._build_opencode_argv(
            "opencode", "m", "high", "/w", "--not-a-flag", None, "json",
        )
        self.assertNotIn("--not-a-flag", argv)

    def test_resume_uses_session_flag(self):
        argv = agent_exec._build_opencode_argv(
            "opencode", "m", "high", "/w", "P", "ses_123", "json",
        )
        self.assertIn("--session", argv)
        self.assertEqual(argv[argv.index("--session") + 1], "ses_123")


class OpencodeRegistrationTests(unittest.TestCase):
    def test_profile_and_known_executor_registered(self):
        self.assertEqual(agent_exec.PROFILES["opencode"]["exec"], "opencode")
        self.assertEqual(agent_exec.PROFILES["opencode"]["mode"], "headless")
        self.assertEqual(
            agent_exec.KNOWN_EXECUTORS["opencode"],
            {"binary": "opencode", "default_dispatch": "cli"},
        )

    def test_default_config_prefers_opencode_ahead_of_copilot(self):
        """opencode leads the implementation bands: measured on an identical
        task it tied copilot on quality and wall-clock while processing about
        a third of the fresh input tokens, and it is the only executor that
        reports a dollar cost."""
        priority = agent_exec.DEFAULTS["priority"]
        self.assertEqual(
            priority["light"]["default"], ["opencode", "copilot", "claude"]
        )
        self.assertEqual(
            priority["light"]["investigation"],
            ["opencode", "copilot", "claude"],
        )
        self.assertEqual(
            priority["standard"]["default"],
            ["opencode", "copilot", "claude", "codex"],
        )

    def test_class_policy_model_carries_provider_prefix(self):
        policy = (agent_exec.DEFAULTS["external_executors"]["opencode"]
                  ["class_policy"]["light"])
        self.assertEqual(policy["model"], "github-copilot/gpt-5.6-luna")
        self.assertEqual(policy["effort"], "medium")

    def test_argv_builder_dispatches_by_profile(self):
        argv = agent_exec._build_executor_argv(
            "opencode", "opencode", "m", "high", "/w", "P", None, "json"
        )
        self.assertEqual(argv[1], "run")
        argv = agent_exec._build_executor_argv(
            "copilot", "copilot", "m", "high", "/w", "P", None, "json"
        )
        self.assertIn("-p", argv)


class ExecutorWorkdirTests(unittest.TestCase):
    """Both capture paths must run the CLI *inside* the workdir.

    Regression: copilot's `--add-dir` only grants access, so without an
    explicit `cwd` a worker told to create a file "in the working directory"
    wrote it wherever agent-exec was invoked from — outside the isolated
    worktree the dispatch had just created. Measured on a real run before
    the fix."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.calls = []
        self._real_run = agent_exec.subprocess.run
        self._real_which = agent_exec.shutil.which

        class _Proc(object):
            stdout = ""
            stderr = ""
            returncode = 0

        def fake_run(argv, **kwargs):
            self.calls.append(kwargs)
            return _Proc()

        agent_exec.subprocess.run = fake_run
        agent_exec.shutil.which = lambda name: "/usr/bin/" + name

    def tearDown(self):
        agent_exec.subprocess.run = self._real_run
        agent_exec.shutil.which = self._real_which
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_opencode_capture_runs_in_workdir(self):
        agent_exec._run_opencode_capture(
            "opencode", "m", "medium", self.tmp, "PROMPT", None
        )
        self.assertEqual(self.calls[0]["cwd"], self.tmp)
        self.assertEqual(self.calls[0]["input"], "PROMPT")

    def test_copilot_capture_runs_in_workdir(self):
        agent_exec._run_copilot_capture(
            "copilot", "m", "medium", self.tmp, "PROMPT", None
        )
        self.assertEqual(self.calls[0]["cwd"], self.tmp)

    def test_missing_workdir_inherits_cwd_instead_of_raising(self):
        agent_exec._run_opencode_capture(
            "opencode", "m", "medium", "/no/such/dir", "PROMPT", None
        )
        self.assertIsNone(self.calls[0]["cwd"])


class OpencodeLedgerRecordTests(unittest.TestCase):
    def test_cost_and_extra_token_keys_reach_the_ledger_record(self):
        result = agent_exec.parse_opencode_jsonl(
            "\n".join(_success_lines()), "", 0
        )
        record = agent_exec.build_run_ledger_record(
            "opencode", "github-copilot/gpt-5.6-luna", "light", result
        )
        self.assertEqual(record["executor"], "opencode")
        self.assertEqual(record["status"], "ok")
        self.assertEqual(record["input_tokens"], 10768)
        self.assertEqual(record["output_tokens"], 6)
        self.assertEqual(record["cost_micro_usd"], 2699)

    def test_ledger_sanitizer_keeps_executor_cost_and_extra_tokens(self):
        """`executor` must survive sanitization or `usage --run` finds
        nothing: `_ledger_usage` filters records by that exact field, so an
        executor missing from the allowlist silently produces a ledger line
        with real numbers that no report can ever attribute."""
        sanitized = agent_exec.sanitize_run_ledger_record({
            "executor": "opencode",
            "cls": "light",
            "status": "ok",
            "cost_micro_usd": 2699,
            "reasoning_output_tokens": 10,
            "cache_write_input_tokens": 4,
        })
        self.assertEqual(sanitized["executor"], "opencode")
        self.assertEqual(sanitized["cost_micro_usd"], 2699)
        self.assertEqual(sanitized["reasoning_output_tokens"], 10)
        self.assertEqual(sanitized["cache_write_input_tokens"], 4)

    def test_ledger_usage_attributes_cost_to_opencode(self):
        records = [
            {"executor": "opencode", "input_tokens": 3, "output_tokens": 8,
             "cached_input_tokens": 10766, "cost_micro_usd": 226},
            {"executor": "copilot", "input_tokens": 99, "cost_micro_usd": 5},
        ]
        acc = agent_exec._ledger_usage(records, "opencode")
        self.assertEqual(acc["records"], 1)
        self.assertEqual(acc["cost_micro_usd"], 226)
        self.assertEqual(acc["tokens"]["input_tokens"], 3)
        self.assertEqual(acc["tokens"]["cached_input_tokens"], 10766)

    def test_opencode_is_a_usage_source(self):
        self.assertIn("opencode", agent_exec._USAGE_SOURCES)


if __name__ == "__main__":
    unittest.main()
