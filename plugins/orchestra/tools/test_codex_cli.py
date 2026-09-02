# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for the codex CLI passthrough executor and per-task session continuity.

Fixtures mirror real `codex exec --json` output captured from codex-cli
0.152.0 (`thread.started` / `turn.started` / `item.completed` /
`turn.completed`), including the resumed form, which re-emits the same
`thread_id`.
"""

import copy
import io
import json
import os
import shutil
import stat
import sys
import tempfile
import time
import unittest

import agent_exec


THREAD = "01a0632d-cd08-7ae0-94d4-56bb74d466c8"


def _line(obj):
    return json.dumps(obj)


def _success_lines(thread_id=THREAD, text="PONG"):
    return [
        _line({"type": "thread.started", "thread_id": thread_id}),
        _line({"type": "turn.started"}),
        _line({
            "type": "item.completed",
            "item": {"id": "item_0", "type": "agent_message", "text": text},
        }),
        _line({
            "type": "turn.completed",
            "usage": {
                "input_tokens": 14578,
                "cached_input_tokens": 9984,
                "cache_write_input_tokens": 0,
                "output_tokens": 6,
                "reasoning_output_tokens": 0,
            },
        }),
    ]


# --- A1: argv shape ---------------------------------------------------------


class BuildCodexArgvTests(unittest.TestCase):
    def test_fresh_argv_shape(self):
        argv = agent_exec._build_codex_argv(
            "codex", "gpt-5.6-sol", "xhigh", "/w", "PROMPT", None, "json",
        )
        self.assertEqual(argv, [
            "codex", "exec", "--json", "--sandbox", "workspace-write",
            "-m", "gpt-5.6-sol",
            "-c", "model_reasoning_effort=xhigh",
            "--",
            "PROMPT",
        ])

    def test_resume_argv_uses_the_resume_subcommand_and_session_id(self):
        argv = agent_exec._build_codex_argv(
            "codex", "gpt-5.6-luna", "medium", "/w", "PROMPT", THREAD, "json",
        )
        self.assertEqual(argv[:4], ["codex", "exec", "resume", THREAD])
        self.assertIn("--json", argv)
        self.assertEqual(argv[-2], "--")
        self.assertEqual(argv[-1], "PROMPT")

    def test_prompt_with_leading_dash_is_unambiguously_positional(self):
        """A prompt whose first line begins with `-` (e.g. a markdown bullet
        like "- fix X" quoted from worker feedback) must not be parsed as a
        flag by clap. Verified empirically against codex-cli 0.152.0:
        without a `--` separator, `codex exec --json --sandbox read-only
        "-say PONG only"` fails with "the argument '--sandbox
        <SANDBOX_MODE>' cannot be used multiple times" because clap
        consumes `-s` out of the prompt string as a short flag."""
        dash_prompt = "- fix X\nmore text"
        for resume in (None, THREAD):
            argv = agent_exec._build_codex_argv(
                "codex", None, None, "/w", dash_prompt, resume, "json",
            )
            sep_index = argv.index("--")
            self.assertEqual(argv[sep_index + 1], dash_prompt)
            self.assertEqual(argv[-1], dash_prompt)
            # Nothing after the separator can be mistaken for another flag.
            self.assertEqual(argv[sep_index + 1:], [dash_prompt])

    def test_resume_sets_the_sandbox_through_a_config_override(self):
        """`codex exec resume` rejects `--sandbox` outright (codex-cli
        0.152.0 exits 2 with "unexpected argument '--sandbox' found"); the
        equivalent generic override is what every subcommand accepts."""
        argv = agent_exec._build_codex_argv(
            "codex", None, None, "/w", "P", THREAD, "json",
        )
        self.assertNotIn("--sandbox", argv)
        self.assertIn("sandbox_mode=workspace-write", argv)
        self.assertEqual(
            argv[argv.index("-c") + 1], "sandbox_mode=workspace-write"
        )

    def test_null_model_and_effort_are_omitted_entirely(self):
        argv = agent_exec._build_codex_argv(
            "codex", None, None, "/w", "P", None, "json",
        )
        self.assertEqual(argv, [
            "codex", "exec", "--json", "--sandbox", "workspace-write",
            "--", "P",
        ])
        self.assertNotIn("None", argv)

    def test_workdir_is_not_a_flag(self):
        """`codex exec resume` has no -C/--cd, so the runner uses cwd= for
        both forms rather than a flag only one of them accepts."""
        for resume in (None, THREAD):
            argv = agent_exec._build_codex_argv(
                "codex", "m", "low", "/some/workdir", "P", resume, "json",
            )
            self.assertNotIn("/some/workdir", argv)
            self.assertNotIn("-C", argv)

    def test_argv_builder_dispatches_by_profile(self):
        argv = agent_exec._build_executor_argv(
            "codex", "codex", "m", "low", "/w", "P", None, "json"
        )
        self.assertEqual(argv[1], "exec")


# --- A2: JSONL parsing ------------------------------------------------------


class ParseCodexJsonlTests(unittest.TestCase):
    def _parse(self, lines, stderr="", exit_code=0, resumed=False):
        return agent_exec.parse_codex_jsonl(
            "\n".join(lines), stderr, exit_code, resumed=resumed
        )

    def test_success_yields_answer_session_and_tokens(self):
        result = self._parse(_success_lines())
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["answer"], "PONG")
        self.assertEqual(result["session_id"], THREAD)
        self.assertIs(result["resumed"], False)
        self.assertIsNone(result["reason"])
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["usage"]["tokens"], {
            "input_tokens": 14578,
            "cached_input_tokens": 9984,
            "cache_write_input_tokens": 0,
            "output_tokens": 6,
            "reasoning_output_tokens": 0,
        })

    def test_no_cost_is_reported_and_none_is_fabricated(self):
        """codex reports no currency figure at all, so the field opencode
        would populate is simply absent -- never a made-up zero."""
        result = self._parse(_success_lines())
        self.assertNotIn("cost_micro_usd", result["usage"])

    def test_resumed_flag_is_carried_through(self):
        result = self._parse(_success_lines(), resumed=True)
        self.assertIs(result["resumed"], True)
        self.assertEqual(result["session_id"], THREAD)

    def test_multiple_agent_messages_are_joined_in_order(self):
        lines = [
            _line({"type": "thread.started", "thread_id": THREAD}),
            _line({"type": "item.completed",
                   "item": {"id": "item_0", "type": "agent_message",
                            "text": "first"}}),
            _line({"type": "item.completed",
                   "item": {"id": "item_1", "type": "reasoning",
                            "text": "IGNORED"}}),
            _line({"type": "item.completed",
                   "item": {"id": "item_2", "type": "agent_message",
                            "text": "second"}}),
        ]
        result = self._parse(lines)
        self.assertEqual(result["answer"], "first\nsecond")

    def test_turn_failed_is_error_bearing(self):
        lines = _success_lines()[:2] + [
            _line({"type": "turn.failed",
                   "error": {"message": "429 rate limit exceeded"}}),
        ]
        result = self._parse(lines, exit_code=1)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "rate-limit")

    def test_agent_message_text_is_never_scanned(self):
        """The worker's own answer can discuss quotas without marking the
        executor exhausted -- default-deny, same as the other parsers."""
        lines = _success_lines(text="Added a guard so quota errors return 429.")
        result = self._parse(lines)
        self.assertEqual(result["status"], "ok")
        self.assertIsNone(result["reason"])

    def test_stderr_is_scanned(self):
        result = self._parse(
            _success_lines(), stderr="not logged in", exit_code=1
        )
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "auth")

    def test_unparseable_stdout_line_is_scanned(self):
        result = self._parse(["thread/resume failed: insufficient credit"],
                             exit_code=1)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "credits")

    def test_nonzero_exit_without_a_pattern_is_nonzero_exit(self):
        result = self._parse(_success_lines(), exit_code=1)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "nonzero-exit")

    def test_empty_output_has_no_answer_session_or_usage(self):
        result = self._parse([])
        self.assertEqual(result["status"], "ok")
        self.assertIsNone(result["answer"])
        self.assertIsNone(result["session_id"])
        self.assertIsNone(result["usage"])

    def test_negative_or_bool_token_counts_are_ignored(self):
        lines = [
            _line({"type": "turn.completed",
                   "usage": {"input_tokens": -5, "output_tokens": True}}),
        ]
        self.assertIsNone(self._parse(lines)["usage"])


# --- A5: registration and defaults -----------------------------------------


class CodexRegistrationTests(unittest.TestCase):
    def test_profile_and_known_executor_registered_as_cli(self):
        self.assertEqual(agent_exec.PROFILES["codex"]["exec"], "codex")
        self.assertEqual(agent_exec.PROFILES["codex"]["mode"], "headless")
        self.assertEqual(
            agent_exec.KNOWN_EXECUTORS["codex"],
            {"binary": "codex", "default_dispatch": "cli"},
        )

    def test_default_dispatch_is_cli_but_agent_type_is_retained(self):
        cfg = agent_exec.DEFAULTS["external_executors"]["codex"]
        self.assertEqual(cfg["dispatch"], "cli")
        # Kept for a user override back to `dispatch: agent`.
        self.assertEqual(cfg["agent_type"], "codex:codex-rescue")

    def test_classes_and_class_policy_are_unchanged(self):
        cfg = agent_exec.DEFAULTS["external_executors"]["codex"]
        self.assertEqual(cfg["classes"], ["standard", "deep", "review"])
        self.assertEqual(cfg["class_policy"], {
            "standard": {"model": "gpt-5.6-luna", "effort": "medium"},
            "deep": {"model": "gpt-5.6-sol", "effort": "xhigh"},
            "review": {"model": "gpt-5.6-sol", "effort": "low"},
        })

    def test_priority_is_unchanged(self):
        priority = agent_exec.DEFAULTS["priority"]
        self.assertEqual(
            priority["standard"]["default"],
            ["opencode", "copilot", "claude", "codex"],
        )
        self.assertEqual(priority["deep"]["default"], ["claude", "codex"])
        self.assertEqual(priority["independent-review"]["default"], ["codex"])

    def test_capture_runner_is_registered_by_name(self):
        self.assertEqual(agent_exec._ARGV_BUILDERS["codex"],
                         "_build_codex_argv")
        self.assertEqual(agent_exec._CAPTURE_RUNNERS["codex"],
                         "_run_codex_capture")


# --- A3: the session store --------------------------------------------------


class SessionStoreTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.cfg = {
            "ledger": {"dir": os.path.join(self.temp.name, "orchestra", "runs"),
                       "enabled": True, "retention_days": 30},
            "telemetry": {"dir": os.path.join(self.temp.name, "orchestra",
                                              "telemetry"), "enabled": False},
        }

    def tearDown(self):
        self.temp.cleanup()

    def test_store_dir_is_a_sibling_of_the_ledger_dir(self):
        self.assertEqual(
            agent_exec._session_store_dir_from_cfg(self.cfg),
            os.path.join(self.temp.name, "orchestra", "sessions"),
        )
        self.assertEqual(
            agent_exec._session_store_path(self.cfg, "codex", "t1"),
            os.path.join(self.temp.name, "orchestra", "sessions",
                         "codex-t1.json"),
        )

    def test_default_config_lands_under_claude_orchestra_sessions(self):
        path = agent_exec._session_store_dir_from_cfg(agent_exec.DEFAULTS)
        self.assertEqual(
            path, os.path.expanduser("~/.claude/orchestra/sessions")
        )

    def test_write_then_read_roundtrip(self):
        workdir = os.path.join(self.temp.name, "tree")
        os.makedirs(workdir)
        written = agent_exec.write_task_session(
            self.cfg, "codex", "t1", THREAD, workdir
        )
        self.assertEqual(
            sorted(written), ["executor", "session_id", "task", "updated",
                              "workdir"])
        self.assertEqual(written["executor"], "codex")
        self.assertEqual(written["task"], "t1")
        self.assertEqual(written["session_id"], THREAD)
        self.assertEqual(written["workdir"], os.path.abspath(workdir))
        self.assertRegex(written["updated"],
                         r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")
        self.assertEqual(
            agent_exec.read_task_session(self.cfg, "codex", "t1"), written
        )

    def test_permissions_are_0600_file_and_0700_dir(self):
        agent_exec.write_task_session(
            self.cfg, "codex", "t1", THREAD, self.temp.name)
        path = agent_exec._session_store_path(self.cfg, "codex", "t1")
        self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE(os.stat(os.path.dirname(path)).st_mode), 0o700)

    def test_update_replaces_and_leaves_no_temp_files(self):
        agent_exec.write_task_session(
            self.cfg, "codex", "t1", THREAD, self.temp.name)
        agent_exec.write_task_session(
            self.cfg, "codex", "t1", "second-uuid", self.temp.name)
        record = agent_exec.read_task_session(self.cfg, "codex", "t1")
        self.assertEqual(record["session_id"], "second-uuid")
        directory = agent_exec._session_store_dir_from_cfg(self.cfg)
        self.assertEqual(sorted(os.listdir(directory)), ["codex-t1.json"])

    def test_missing_file_is_a_miss(self):
        self.assertIsNone(
            agent_exec.read_task_session(self.cfg, "codex", "nope"))

    def test_truncated_json_is_a_miss_not_an_exception(self):
        agent_exec.write_task_session(
            self.cfg, "codex", "t1", THREAD, self.temp.name)
        path = agent_exec._session_store_path(self.cfg, "codex", "t1")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write('{"executor":"codex","task":"t1","sess')
        self.assertIsNone(agent_exec.read_task_session(self.cfg, "codex", "t1"))

    def test_missing_required_key_is_a_miss(self):
        directory = agent_exec._session_store_dir_from_cfg(self.cfg)
        os.makedirs(directory, mode=0o700, exist_ok=True)
        with open(os.path.join(directory, "codex-t1.json"), "w",
                  encoding="utf-8") as handle:
            json.dump({"executor": "codex", "task": "t1",
                       "session_id": THREAD}, handle)
        self.assertIsNone(agent_exec.read_task_session(self.cfg, "codex", "t1"))

    def test_non_object_json_is_a_miss(self):
        directory = agent_exec._session_store_dir_from_cfg(self.cfg)
        os.makedirs(directory, mode=0o700, exist_ok=True)
        with open(os.path.join(directory, "codex-t1.json"), "w",
                  encoding="utf-8") as handle:
            handle.write("[1, 2, 3]")
        self.assertIsNone(agent_exec.read_task_session(self.cfg, "codex", "t1"))

    def test_traversal_in_a_task_id_cannot_escape_the_store(self):
        path = agent_exec._session_store_path(self.cfg, "codex", "../../evil")
        directory = agent_exec._session_store_dir_from_cfg(self.cfg)
        self.assertEqual(os.path.dirname(path), directory)

    def test_retention_sweep_removes_stale_records(self):
        agent_exec.write_task_session(
            self.cfg, "codex", "old", THREAD, self.temp.name)
        agent_exec.write_task_session(
            self.cfg, "codex", "new", THREAD, self.temp.name)
        old_path = agent_exec._session_store_path(self.cfg, "codex", "old")
        stale = time.time() - 31 * 86400
        os.utime(old_path, (stale, stale))
        orig_flag = agent_exec._ledger_retention_ran
        agent_exec._ledger_retention_ran = False
        try:
            agent_exec._sweep_retention(self.cfg)
        finally:
            agent_exec._ledger_retention_ran = orig_flag
        self.assertIsNone(agent_exec.read_task_session(self.cfg, "codex", "old"))
        self.assertIsNotNone(
            agent_exec.read_task_session(self.cfg, "codex", "new"))


# --- A4: dispatch wiring ----------------------------------------------------


class _CodexDispatchHarness(unittest.TestCase):
    """Drives `cmd_dispatch_route` with codex forced as the winning route and
    the codex subprocess replaced by a recorder, so nothing here shells out."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = self.temp.name
        self.workdir = os.path.join(self.root, "tree")
        os.makedirs(self.workdir)
        self.prompt = os.path.join(self.root, "prompt.md")
        with open(self.prompt, "w", encoding="utf-8") as handle:
            handle.write("do the thing")

        self.cfg = copy.deepcopy(agent_exec.DEFAULTS)
        self.cfg["priority"]["standard"]["default"] = ["codex"]
        self.cfg["ledger"] = {"dir": os.path.join(self.root, "orchestra", "runs"),
                              "enabled": True, "retention_days": 30}
        self.cfg["telemetry"] = {"dir": os.path.join(self.root, "orchestra",
                                                     "telemetry"),
                                 "enabled": False}
        self.cfg["cooldown"] = dict(agent_exec.DEFAULTS["cooldown"])
        self.cfg["cooldown"]["enabled"] = False

        self.calls = []
        self.next_result = None

        self._orig_resolve_config = agent_exec.resolve_config
        self._orig_doctor = agent_exec._build_doctor_report
        self._orig_runner = agent_exec._run_codex_capture
        agent_exec.resolve_config = lambda: (self.cfg, None)
        agent_exec._build_doctor_report = lambda: {
            "ready": {"codex": {"ok": True, "missing": []}}
        }

        def fake_runner(profile_name, model, effort, workdir, prompt_text,
                        resume, output_fmt="json"):
            argv = agent_exec._build_codex_argv(
                "codex", model, effort, workdir, prompt_text, resume, output_fmt
            )
            self.calls.append({"resume": resume, "workdir": workdir,
                               "argv": argv})
            result = self.next_result
            if result is None:
                result = agent_exec.parse_codex_jsonl(
                    "\n".join(_success_lines()), "", 0,
                    resumed=resume is not None,
                )
            return 0, copy.deepcopy(result)

        agent_exec._run_codex_capture = fake_runner

    def tearDown(self):
        agent_exec.resolve_config = self._orig_resolve_config
        agent_exec._build_doctor_report = self._orig_doctor
        agent_exec._run_codex_capture = self._orig_runner
        self.temp.cleanup()

    def dispatch(self, *extra):
        args = ["--class", "standard", "--prompt-file", self.prompt,
                "--workdir", self.workdir, "--isolate", "never", "--capture"]
        args.extend(extra)
        buf = io.StringIO()
        orig_stdout = sys.stdout
        sys.stdout = buf
        try:
            rc = agent_exec.cmd_dispatch_route(args)
        finally:
            sys.stdout = orig_stdout
        self.assertEqual(rc, 0, buf.getvalue())
        return json.loads(buf.getvalue())

    def stored(self, task="t1"):
        return agent_exec.read_task_session(self.cfg, "codex", task)


class DispatchSessionContinuityTests(_CodexDispatchHarness):
    def test_1_first_round_is_fresh_and_writes_the_store(self):
        output = self.dispatch("--task", "t1")
        self.assertEqual(output["executor"], "codex")
        self.assertEqual(output["session_id"], THREAD)
        self.assertIs(output["resumed"], False)
        self.assertIsNone(self.calls[0]["resume"])
        record = self.stored()
        self.assertEqual(record["session_id"], THREAD)
        self.assertEqual(record["workdir"], os.path.abspath(self.workdir))

    def test_2_second_round_resumes_the_same_session(self):
        self.dispatch("--task", "t1")
        output = self.dispatch("--task", "t1")
        self.assertIs(output["resumed"], True)
        self.assertEqual(self.calls[1]["resume"], THREAD)
        argv = self.calls[1]["argv"]
        self.assertEqual(argv[1:4], ["exec", "resume", THREAD])

    def test_3_a_record_from_another_workdir_is_a_miss_and_is_overwritten(self):
        other = os.path.join(self.root, "other-tree")
        os.makedirs(other)
        agent_exec.write_task_session(self.cfg, "codex", "t1", "foreign", other)
        output = self.dispatch("--task", "t1")
        self.assertIs(output["resumed"], False)
        self.assertIsNone(self.calls[0]["resume"])
        record = self.stored()
        self.assertEqual(record["session_id"], THREAD)
        self.assertEqual(record["workdir"], os.path.abspath(self.workdir))

    def test_4_explicit_resume_beats_a_store_hit(self):
        self.dispatch("--task", "t1")
        output = self.dispatch("--task", "t1", "--resume", "explicit-uuid")
        self.assertEqual(self.calls[1]["resume"], "explicit-uuid")
        self.assertIs(output["resumed"], True)

    def test_5_no_resume_skips_the_lookup_but_still_updates_the_store(self):
        self.dispatch("--task", "t1")
        self.next_result = agent_exec.parse_codex_jsonl(
            "\n".join(_success_lines(thread_id="second-uuid")), "", 0)
        output = self.dispatch("--task", "t1", "--no-resume")
        self.assertIsNone(self.calls[1]["resume"])
        self.assertIs(output["resumed"], False)
        self.assertEqual(self.stored()["session_id"], "second-uuid")

    def test_6_without_a_task_nothing_is_read_or_written(self):
        output = self.dispatch()
        self.assertIs(output["resumed"], False)
        self.assertIsNone(self.calls[0]["resume"])
        self.assertFalse(
            os.path.exists(agent_exec._session_store_dir_from_cfg(self.cfg)))

    def test_7_a_malformed_store_is_a_miss_and_does_not_raise(self):
        directory = agent_exec._session_store_dir_from_cfg(self.cfg)
        os.makedirs(directory, mode=0o700, exist_ok=True)
        with open(os.path.join(directory, "codex-t1.json"), "w",
                  encoding="utf-8") as handle:
            handle.write("{not json")
        output = self.dispatch("--task", "t1")
        self.assertIs(output["resumed"], False)
        self.assertEqual(self.stored()["session_id"], THREAD)

    def test_8_a_failed_round_leaves_the_previous_record_intact(self):
        self.dispatch("--task", "t1")
        self.next_result = agent_exec.parse_codex_jsonl(
            "\n".join(_success_lines(thread_id="doomed")), "429 rate limit", 1)
        output = self.dispatch("--task", "t1")
        self.assertEqual(output["status"], "unavailable")
        self.assertEqual(self.stored()["session_id"], THREAD)

    def test_8b_a_run_without_a_session_id_writes_nothing(self):
        self.next_result = agent_exec.parse_codex_jsonl("", "", 0)
        self.dispatch("--task", "t1")
        self.assertIsNone(self.stored())

    def test_no_store_read_warns_on_stderr(self):
        err = io.StringIO()
        orig_stderr = sys.stderr
        sys.stderr = err
        try:
            self.dispatch("--task", "brand-new")
        finally:
            sys.stderr = orig_stderr
        self.assertEqual(err.getvalue(), "")

    def test_dryrun_previews_the_resumed_argv(self):
        self.dispatch("--task", "t1")
        os.environ["AGENT_EXEC_DRYRUN"] = "1"
        try:
            buf = io.StringIO()
            orig_stdout = sys.stdout
            sys.stdout = buf
            try:
                rc = agent_exec.cmd_dispatch_route([
                    "--class", "standard", "--prompt-file", self.prompt,
                    "--workdir", self.workdir, "--isolate", "never",
                    "--task", "t1",
                ])
            finally:
                sys.stdout = orig_stdout
        finally:
            os.environ.pop("AGENT_EXEC_DRYRUN", None)
        self.assertEqual(rc, 0)
        self.assertIn("exec resume %s" % THREAD, buf.getvalue())


class DispatchTokenCarriesTaskTests(_CodexDispatchHarness):
    def test_token_replay_auto_resumes_through_the_stored_task(self):
        buf = io.StringIO()
        orig_stdout = sys.stdout
        sys.stdout = buf
        try:
            rc = agent_exec.cmd_dispatch_prepare([
                "--class", "standard", "--prompt-file", self.prompt,
                "--workdir", self.workdir, "--isolate", "never",
                "--task", "t1", "--json",
            ])
        finally:
            sys.stdout = orig_stdout
        self.assertEqual(rc, 0)
        token = json.loads(buf.getvalue())["token"]

        def run_token(*extra):
            out = io.StringIO()
            saved = sys.stdout
            sys.stdout = out
            try:
                code = agent_exec.cmd_dispatch_route(
                    ["--token", token, "--capture"] + list(extra))
            finally:
                sys.stdout = saved
            self.assertEqual(code, 0, out.getvalue())
            return json.loads(out.getvalue())

        first = run_token()
        self.assertIs(first["resumed"], False)
        second = run_token()
        self.assertIs(second["resumed"], True)
        self.assertEqual(self.calls[1]["resume"], THREAD)
        third = run_token("--no-resume")
        self.assertIs(third["resumed"], False)


# --- A6: the `dispatch session` subcommand ---------------------------------


class DispatchSessionSubcommandTests(_CodexDispatchHarness):
    def _session(self, *extra):
        buf = io.StringIO()
        orig_stdout = sys.stdout
        sys.stdout = buf
        try:
            rc = agent_exec.cmd_dispatch_route(["session"] + list(extra))
        finally:
            sys.stdout = orig_stdout
        return rc, buf.getvalue()

    def test_unknown_task_reports_status_none_and_exits_zero(self):
        rc, out = self._session("--task", "never-ran", "--json")
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out), {"status": "none"})
        rc, out = self._session("--task", "never-ran")
        self.assertEqual(rc, 0)
        self.assertEqual(out.strip(), "status=none")

    def test_it_never_creates_the_store(self):
        self._session("--task", "never-ran", "--json")
        self.assertFalse(
            os.path.exists(agent_exec._session_store_dir_from_cfg(self.cfg)))

    def test_a_stored_record_is_printed_both_ways(self):
        self.dispatch("--task", "t1")
        rc, out = self._session("--task", "t1", "--json")
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out), self.stored())
        rc, text = self._session("--task", "t1")
        self.assertEqual(rc, 0)
        pairs = dict(line.split("=", 1) for line in text.strip().splitlines())
        self.assertEqual(pairs["executor"], "codex")
        self.assertEqual(pairs["task"], "t1")
        self.assertEqual(pairs["session_id"], THREAD)

    def test_executor_defaults_to_codex_and_is_overridable(self):
        self.dispatch("--task", "t1")
        rc, out = self._session("--task", "t1", "--executor", "codex", "--json")
        self.assertEqual(json.loads(out)["session_id"], THREAD)
        rc, out = self._session("--task", "t1", "--executor", "opencode",
                                "--json")
        self.assertEqual(json.loads(out), {"status": "none"})

    def test_usage_errors(self):
        self.assertEqual(agent_exec.cmd_dispatch_route(["session"]), 2)
        self.assertEqual(
            agent_exec.cmd_dispatch_route(["session", "--bogus"]), 2)


# --- A5/A8: the `dispatch: agent` fallback stays byte-identical -------------


class CodexAgentDispatchFallbackTests(unittest.TestCase):
    def test_override_to_agent_still_delegates_with_a_correlation_id(self):
        cfg = copy.deepcopy(agent_exec.DEFAULTS)
        cfg["external_executors"]["codex"]["dispatch"] = "agent"
        cfg["priority"]["standard"]["default"] = ["codex"]
        with tempfile.TemporaryDirectory() as tmp:
            cfg["ledger"] = {"dir": os.path.join(tmp, "runs"),
                             "enabled": True, "retention_days": 30}
            cfg["telemetry"] = {"dir": os.path.join(tmp, "telemetry"),
                                "enabled": False}
            prompt = os.path.join(tmp, "p.md")
            with open(prompt, "w", encoding="utf-8") as handle:
                handle.write("x")
            orig_resolve_config = agent_exec.resolve_config
            orig_doctor = agent_exec._build_doctor_report
            agent_exec.resolve_config = lambda: (cfg, None)
            agent_exec._build_doctor_report = lambda: {"ready": {}}
            try:
                buf = io.StringIO()
                orig_stdout = sys.stdout
                sys.stdout = buf
                try:
                    rc = agent_exec.cmd_dispatch_route([
                        "--class", "standard", "--prompt-file", prompt,
                        "--workdir", tmp, "--isolate", "never",
                    ])
                finally:
                    sys.stdout = orig_stdout
            finally:
                agent_exec.resolve_config = orig_resolve_config
                agent_exec._build_doctor_report = orig_doctor
        self.assertEqual(rc, 0)
        output = json.loads(buf.getvalue())
        self.assertEqual(output["status"], "delegate")
        self.assertEqual(output["executor"], "codex")
        self.assertEqual(output["agent_type"], "codex:codex-rescue")
        self.assertEqual(output["model"], "gpt-5.6-luna")
        self.assertEqual(output["effort"], "medium")
        self.assertRegex(output["correlation_id"], r"^oxc-[0-9a-f]{12}$")
        self.assertEqual(output["route"]["dispatch"], "agent")

    def test_the_rollout_correlation_helpers_are_still_present(self):
        for name in ("parse_codex_rollout_lines", "match_codex_rollouts",
                     "_ledger_codex_usage"):
            self.assertTrue(callable(getattr(agent_exec, name)))


# --- A5: availability gating ------------------------------------------------


class CodexAvailabilityGatingTests(unittest.TestCase):
    def test_missing_binary_makes_doctor_not_ok_and_route_skips_codex(self):
        fake_home = tempfile.mkdtemp()
        tmp_project = tempfile.mkdtemp()
        empty_path_dir = tempfile.mkdtemp()
        orig_expanduser = os.path.expanduser
        orig_cwd = os.getcwd()
        orig_path = os.environ.get("PATH")

        def fake_expanduser(p):
            if p == "~/.claude":
                return os.path.join(fake_home, ".claude")
            return orig_expanduser(p)

        try:
            agent_exec.os.path.expanduser = fake_expanduser
            os.chdir(tmp_project)
            os.environ["PATH"] = empty_path_dir  # no codex binary anywhere

            resolved, err = agent_exec.resolve_config()
            self.assertIsNone(err)
            self.assertEqual(
                resolved["external_executors"]["codex"]["dispatch"], "cli")

            report = agent_exec._build_doctor_report()
            self.assertFalse(report["executors"]["codex"]["available"])
            self.assertIn("codex", report["ready"])
            self.assertFalse(report["ready"]["codex"]["ok"])
            self.assertIn("executor-binary-unavailable",
                          report["ready"]["codex"]["missing"])

            # `standard` lists codex behind claude, which always survives,
            # so exhaust everything ahead of it to actually reach the gate.
            route = agent_exec.resolve_route(
                resolved, report, "standard",
                exhausted=["opencode", "copilot", "claude"],
            )
            self.assertNotEqual(route["executor"], "codex")
            self.assertIsNone(route["executor"])
            skipped = dict((s["executor"], s["reason"])
                           for s in route["skipped"])
            self.assertIn("codex", skipped)
            self.assertTrue(skipped["codex"].startswith("not-ready:"))
        finally:
            os.chdir(orig_cwd)
            agent_exec.os.path.expanduser = orig_expanduser
            if orig_path is None:
                os.environ.pop("PATH", None)
            else:
                os.environ["PATH"] = orig_path
            shutil.rmtree(fake_home, ignore_errors=True)
            shutil.rmtree(tmp_project, ignore_errors=True)
            shutil.rmtree(empty_path_dir, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
