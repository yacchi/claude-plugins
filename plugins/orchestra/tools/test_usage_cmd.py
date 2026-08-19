# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for the `agent-exec usage` cross-executor aggregation.

Every test drives the pure aggregators directly, or the I/O shell against a
synthetic HOME, so the real ~/.claude and ~/.codex are never touched.

Run with: uv run test_usage_cmd.py
"""

import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402

AGENT_EXEC_PY = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                             "agent_exec.py")


def run_cli(args, home, config_dir=None, extra_env=None, unset_env=None):
    """Invoke tools/agent_exec.py as a real subprocess, isolated to a
    synthetic HOME (and optionally CLAUDE_CONFIG_DIR), so a signature or
    wiring break in the argv -> cmd_usage -> collector path is caught even
    when every helper unit-tests green in isolation."""
    env = dict(os.environ)
    env["HOME"] = home
    if config_dir is None:
        env.pop("CLAUDE_CONFIG_DIR", None)
    else:
        env["CLAUDE_CONFIG_DIR"] = config_dir
    if unset_env:
        for key in unset_env:
            env.pop(key, None)
    if extra_env:
        env.update(extra_env)
    proc = subprocess.run(
        [sys.executable, AGENT_EXEC_PY, "usage"] + args,
        capture_output=True, text=True, env=env, cwd=home, timeout=60,
    )
    return proc.returncode, proc.stdout, proc.stderr


NOW = datetime(2026, 8, 19, 12, 0, 0, tzinfo=timezone.utc)
CUTOFF = NOW - timedelta(hours=24)


def iso(dt):
    return dt.isoformat().replace("+00:00", "Z")


def claude_line(ts, input_tokens=0, output_tokens=0, cache_creation=0,
                cache_read=0, model="claude-opus-5", sidechain=False):
    return json.dumps(
        {
            "timestamp": iso(ts),
            "isSidechain": sidechain,
            "sessionId": "s-1",
            "message": {
                "model": model,
                "usage": {
                    "input_tokens": input_tokens,
                    "cache_creation_input_tokens": cache_creation,
                    "cache_read_input_tokens": cache_read,
                    "output_tokens": output_tokens,
                },
            },
        }
    )


def codex_token_count(ts, input_tokens, cached, output_tokens):
    return json.dumps(
        {
            "timestamp": iso(ts),
            "type": "event_msg",
            "payload": {
                "type": "token_count",
                "info": {
                    "total_token_usage": {
                        "input_tokens": input_tokens,
                        "cached_input_tokens": cached,
                        "output_tokens": output_tokens,
                        "reasoning_output_tokens": 1,
                        "total_tokens": input_tokens + output_tokens,
                    },
                    "last_token_usage": {"input_tokens": 1, "output_tokens": 1},
                    "model_context_window": 258400,
                },
            },
        }
    )


def codex_rollout_lines(corr, samples):
    """Lines for one synthetic codex rollout: a user_message carrying `corr`
    verbatim, then one token_count event per value in `samples` (all five
    numeric fields set to that value, plus a total_tokens that would only
    match a sum if the aggregator wrongly used it)."""
    lines = [json.dumps({"type": "event_msg", "payload": {
        "type": "user_message", "message": "do the task " + corr}})]
    for value in samples:
        lines.append(json.dumps({"type": "event_msg", "payload": {
            "type": "token_count", "info": {"total_token_usage": {
                "input_tokens": value, "output_tokens": value,
                "cached_input_tokens": value, "cache_write_input_tokens": value,
                "reasoning_output_tokens": value, "total_tokens": 999999,
            }}}}))
    return lines


def delegated_ledger_line(corr):
    return json.dumps({"executor": "codex", "status": "delegated", "corr": corr})


def write_lines(path, lines):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


class SinceParsingTests(unittest.TestCase):
    def test_relative_windows(self):
        self.assertEqual(
            agent_exec.parse_usage_since("90m", NOW), NOW - timedelta(minutes=90)
        )
        self.assertEqual(
            agent_exec.parse_usage_since("6h", NOW), NOW - timedelta(hours=6)
        )
        self.assertEqual(
            agent_exec.parse_usage_since("2d", NOW), NOW - timedelta(days=2)
        )

    def test_iso8601_absolute(self):
        self.assertEqual(
            agent_exec.parse_usage_since("2026-08-18T00:00:00Z", NOW),
            datetime(2026, 8, 18, 0, 0, 0, tzinfo=timezone.utc),
        )
        # naive stamps are read as UTC rather than rejected
        self.assertEqual(
            agent_exec.parse_usage_since("2026-08-18T00:00:00", NOW),
            datetime(2026, 8, 18, 0, 0, 0, tzinfo=timezone.utc),
        )

    def test_garbage_is_none(self):
        for bad in ("", "   ", "later", "6x", "-3h", "6 h", None, 7, "2026-13-45"):
            self.assertIsNone(agent_exec.parse_usage_since(bad, NOW), repr(bad))

    def test_garbage_since_exits_2_with_empty_stdout(self):
        out, err = io.StringIO(), io.StringIO()
        real_out, real_err = sys.stdout, sys.stderr
        sys.stdout, sys.stderr = out, err
        try:
            rc = agent_exec.cmd_usage(["--since", "yesterday"])
        finally:
            sys.stdout, sys.stderr = real_out, real_err
        self.assertEqual(rc, 2)
        self.assertEqual(out.getvalue(), "")
        self.assertIn("--since", err.getvalue())

    def test_both_formats_is_an_error(self):
        err = io.StringIO()
        real_err = sys.stderr
        sys.stderr = err
        try:
            rc = agent_exec.cmd_usage(["--json", "--text"])
        finally:
            sys.stderr = real_err
        self.assertEqual(rc, 2)

    def test_unknown_source_is_an_error(self):
        err = io.StringIO()
        real_err = sys.stderr
        sys.stderr = err
        try:
            rc = agent_exec.cmd_usage(["--source", "claude,gemini"])
        finally:
            sys.stderr = real_err
        self.assertEqual(rc, 2)
        self.assertIn("gemini", err.getvalue())


class ClaudeAggregationTests(unittest.TestCase):
    def test_window_filtering_and_totals(self):
        lines = [
            claude_line(NOW - timedelta(hours=1), input_tokens=10, output_tokens=5,
                        cache_creation=2, cache_read=100),
            claude_line(NOW - timedelta(days=3), input_tokens=999, output_tokens=999),
        ]
        acc = agent_exec.aggregate_claude_lines(lines, CUTOFF)
        self.assertEqual(acc["records"], 1)
        self.assertEqual(
            acc["tokens"],
            {"input_tokens": 12, "output_tokens": 5, "cached_input_tokens": 100},
        )

    def test_sidechain_split_and_model_breakdown(self):
        lines = [
            claude_line(NOW - timedelta(hours=1), input_tokens=10, output_tokens=1),
            claude_line(NOW - timedelta(hours=2), input_tokens=3, output_tokens=2,
                        model="claude-haiku-5", sidechain=True),
        ]
        acc = agent_exec.aggregate_claude_lines(lines, CUTOFF)
        self.assertEqual(acc["main"]["input_tokens"], 10)
        self.assertEqual(acc["sidechain"]["input_tokens"], 3)
        self.assertEqual(sorted(acc["by_model"]), ["claude-haiku-5", "claude-opus-5"])
        self.assertEqual(acc["by_model"]["claude-haiku-5"]["output_tokens"], 2)

    def test_garbage_and_shape_drift_lines_are_skipped(self):
        lines = [
            "",
            "not json at all",
            '{"truncated": ',
            json.dumps([1, 2, 3]),
            json.dumps({"message": {"usage": {"input_tokens": 5}}}),  # no timestamp
            json.dumps({"timestamp": iso(NOW), "message": "a string"}),
            json.dumps({"timestamp": iso(NOW), "message": {"usage": []}}),
            json.dumps({"timestamp": "nope", "message": {"usage": {"input_tokens": 5}}}),
            json.dumps(
                {
                    "timestamp": iso(NOW - timedelta(hours=1)),
                    "message": {"model": 42, "usage": {"input_tokens": "7",
                                                       "output_tokens": 4}},
                }
            ),
        ]
        acc = agent_exec.aggregate_claude_lines(lines, CUTOFF)
        self.assertEqual(acc["records"], 1)
        # "7" is not an int, so it coerces to 0 rather than raising.
        self.assertEqual(acc["tokens"]["input_tokens"], 0)
        self.assertEqual(acc["tokens"]["output_tokens"], 4)
        self.assertEqual(acc["by_model"], {})

    def test_accumulator_folds_multiple_transcripts(self):
        acc = agent_exec.new_claude_usage_acc()
        agent_exec.aggregate_claude_lines(
            [claude_line(NOW, input_tokens=1)], CUTOFF, acc)
        agent_exec.aggregate_claude_lines(
            [claude_line(NOW, input_tokens=2)], CUTOFF, acc)
        self.assertEqual(acc["tokens"]["input_tokens"], 3)
        self.assertEqual(acc["records"], 2)

    def test_cwd_scoping_uses_the_project_slug(self):
        with tempfile.TemporaryDirectory() as home:
            cwd = "/Users/x/p"
            slug = agent_exec.claude_project_slug(cwd)
            self.assertEqual(slug, "-Users-x-p")
            write_lines(
                os.path.join(home, ".claude", "projects", slug, "a.jsonl"),
                [claude_line(NOW, input_tokens=11)],
            )
            write_lines(
                os.path.join(home, ".claude", "projects", "-other", "b.jsonl"),
                [claude_line(NOW, input_tokens=500)],
            )

            scoped = agent_exec.collect_claude_usage(
                CUTOFF, home=home, cwd=cwd, config_dir="")
            self.assertEqual(scoped["status"], "ok")
            self.assertEqual(scoped["scope"], "cwd")
            self.assertEqual(scoped["tokens"]["input_tokens"], 11)
            self.assertEqual(scoped["sessions"], 1)

            wide = agent_exec.collect_claude_usage(
                CUTOFF, all_projects=True, home=home, cwd=cwd, config_dir="")
            self.assertEqual(wide["scope"], "all-projects")
            self.assertEqual(wide["tokens"]["input_tokens"], 511)
            self.assertEqual(wide["sessions"], 2)

    def test_missing_tree_is_unavailable_not_zero_ok(self):
        with tempfile.TemporaryDirectory() as home:
            entry = agent_exec.collect_claude_usage(
                CUTOFF, home=home, cwd="/x", config_dir="")
            self.assertEqual(entry["status"], "unavailable")
            self.assertEqual(entry["tokens"], agent_exec._zero_tokens())

    def test_subagents_and_both_roots_are_counted_once(self):
        with tempfile.TemporaryDirectory() as home, tempfile.TemporaryDirectory() as cfg:
            cwd = "/x"
            slug = agent_exec.claude_project_slug(cwd)
            home_project = os.path.join(home, ".claude", "projects", slug)
            cfg_project = os.path.join(cfg, "projects", slug)
            shared = os.path.join(home_project, "shared.jsonl")
            write_lines(shared, [claude_line(NOW, input_tokens=7)])
            write_lines(
                os.path.join(cfg_project, "config.jsonl"),
                [claude_line(NOW, input_tokens=11)],
            )
            subagents = os.path.join(cfg_project, "session-id", "subagents",
                                     "workflows", "wf_x")
            write_lines(
                os.path.join(subagents, "agent-y.jsonl"),
                [
                    claude_line(NOW - timedelta(days=2), input_tokens=100),
                    json.dumps({
                        "timestamp": iso(NOW - timedelta(hours=1)),
                        "isSidechain": True,
                        "message": {
                            "model": "claude-haiku-4-5-20251001",
                            "usage": {
                                "input_tokens": 13,
                                "output_tokens": 5,
                                "cache_creation_input_tokens": 2,
                                "cache_read_input_tokens": 3,
                                "cache_creation": {"ignored": 99},
                                "server_tool_use": {"ignored": 99},
                                "output_tokens_details": {"ignored": 99},
                            },
                        },
                    }),
                ],
            )
            write_lines(os.path.join(subagents, "journal.jsonl"),
                        [json.dumps({"journal": "not usage"})])
            os.symlink(shared, os.path.join(cfg_project, "shared.jsonl"))

            env_before = os.environ.get("CLAUDE_CONFIG_DIR")
            os.environ["CLAUDE_CONFIG_DIR"] = cfg
            try:
                entry = agent_exec.collect_claude_usage(
                    CUTOFF, home=home, cwd=cwd)
            finally:
                if env_before is None:
                    os.environ.pop("CLAUDE_CONFIG_DIR", None)
                else:
                    os.environ["CLAUDE_CONFIG_DIR"] = env_before

            self.assertEqual(entry["status"], "ok")
            self.assertEqual(entry["tokens"]["input_tokens"], 33)
            self.assertEqual(entry["sidechain"]["input_tokens"], 15)
            self.assertEqual(entry["sidechain"]["output_tokens"], 5)
            self.assertEqual(entry["sidechain"]["cached_input_tokens"], 3)
            self.assertEqual(
                entry["by_model"]["claude-haiku-4-5-20251001"]["input_tokens"], 15)
            self.assertEqual(entry["sessions"], 3)
            self.assertEqual(entry["records"], 3)


class CodexAggregationTests(unittest.TestCase):
    def test_last_token_count_wins_and_is_not_summed(self):
        lines = [
            json.dumps({"type": "session_meta", "payload": {"id": "abc",
                                                            "cwd": "/x"}}),
            codex_token_count(NOW - timedelta(hours=3), 100, 40, 10),
            codex_token_count(NOW - timedelta(hours=2), 300, 200, 25),
        ]
        out = agent_exec.aggregate_codex_lines(lines, CUTOFF)
        self.assertTrue(out["contributed"])
        # input is reported inclusive of the cached share, so it is netted out
        self.assertEqual(
            out["tokens"],
            {"input_tokens": 100, "output_tokens": 25, "cached_input_tokens": 200},
        )

    def test_no_token_count_contributes_nothing(self):
        lines = [
            json.dumps({"type": "session_meta", "payload": {"id": "abc"}}),
            json.dumps({"type": "event_msg", "payload": {"type": "agent_message"}}),
            "garbage",
        ]
        out = agent_exec.aggregate_codex_lines(lines, CUTOFF)
        self.assertFalse(out["contributed"])
        self.assertEqual(out["tokens"], agent_exec._zero_tokens())

    def test_out_of_window_last_event_is_dropped(self):
        lines = [codex_token_count(NOW - timedelta(days=5), 100, 0, 10)]
        out = agent_exec.aggregate_codex_lines(lines, CUTOFF)
        self.assertFalse(out["contributed"])

    def test_mtime_fallback_when_event_has_no_timestamp(self):
        raw = json.loads(codex_token_count(NOW, 100, 0, 10))
        del raw["timestamp"]
        lines = [json.dumps(raw)]
        self.assertFalse(agent_exec.aggregate_codex_lines(lines, CUTOFF)["contributed"])
        out = agent_exec.aggregate_codex_lines(
            lines, CUTOFF, mtime=NOW.timestamp())
        self.assertTrue(out["contributed"])
        self.assertEqual(out["tokens"]["input_tokens"], 100)

    def test_multiple_session_files_add_up(self):
        with tempfile.TemporaryDirectory() as home:
            day = os.path.join(home, ".codex", "sessions", "2026", "08", "19")
            write_lines(
                os.path.join(day, "rollout-1.jsonl"),
                [codex_token_count(NOW - timedelta(hours=1), 100, 40, 10)],
            )
            write_lines(
                os.path.join(day, "rollout-2.jsonl"),
                [codex_token_count(NOW - timedelta(hours=2), 50, 0, 5)],
            )
            write_lines(
                os.path.join(day, "rollout-3.jsonl"),
                [json.dumps({"type": "session_meta", "payload": {"id": "z"}})],
            )
            entry = agent_exec.collect_codex_usage(CUTOFF, home=home)
            self.assertEqual(entry["status"], "ok")
            self.assertEqual(entry["scope"], "global")
            self.assertEqual(entry["sessions"], 2)
            self.assertEqual(
                entry["tokens"],
                {"input_tokens": 110, "output_tokens": 15,
                 "cached_input_tokens": 40},
            )

    def test_missing_tree_is_unavailable(self):
        with tempfile.TemporaryDirectory() as home:
            entry = agent_exec.collect_codex_usage(CUTOFF, home=home)
            self.assertEqual(entry["status"], "unavailable")
            self.assertIn("note", entry)


class DelegatedCodexRunScopeTests(unittest.TestCase):
    """Section B: `usage --run` resolving delegated codex ledger lines
    against a synthetic ~/.codex sessions root."""

    def _corr(self, n):
        return "oxc-%012x" % n

    def _setup(self, home, matched_n, total_n):
        telemetry_dir = os.path.join(home, "telemetry")
        ledger_dir = os.path.join(home, "runs")
        corrs = [self._corr(i + 1) for i in range(total_n)]
        write_lines(os.path.join(ledger_dir, "session-1", "run1.jsonl"),
                   [delegated_ledger_line(c) for c in corrs])
        sessions_root = os.path.join(home, ".codex", "sessions")
        for c in corrs[:matched_n]:
            write_lines(os.path.join(sessions_root, "rollout-%s.jsonl" % c),
                       codex_rollout_lines(c, [10, 20]))
        cfg = {"telemetry": {"dir": telemetry_dir, "enabled": False}}
        return cfg

    def test_partial_match_reports_ok_with_summed_measured_tokens(self):
        with tempfile.TemporaryDirectory() as home:
            cfg = self._setup(home, matched_n=2, total_n=3)
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["codex"], cfg=cfg, home=home, cwd="/x",
                run_ids=["run1"])
            entry = report["sources"]["codex"]
            self.assertEqual(entry["status"], "ok")
            self.assertEqual(entry["delegated"], 3)
            self.assertEqual(entry["measured"], 2)
            for key in ("input_tokens", "output_tokens", "cached_input_tokens",
                       "cache_write_input_tokens", "reasoning_output_tokens"):
                self.assertEqual(entry["tokens"][key], 40)  # 2 matches * last=20

    def test_no_match_reports_not_attributable_with_counters(self):
        with tempfile.TemporaryDirectory() as home:
            cfg = self._setup(home, matched_n=0, total_n=3)
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["codex"], cfg=cfg, home=home, cwd="/x",
                run_ids=["run1"])
            entry = report["sources"]["codex"]
            self.assertEqual(entry, {
                "attributable": False,
                "reason": "codex delegated but no rollout matched",
                "delegated": 3, "measured": 0,
            })

    def test_text_counter_renders_for_ok_and_not_attributable(self):
        with tempfile.TemporaryDirectory() as home:
            ok_cfg = self._setup(home, matched_n=2, total_n=3)
            ok_report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["codex"], cfg=ok_cfg, home=home, cwd="/x",
                run_ids=["run1"])
            out = io.StringIO()
            real_out = sys.stdout
            sys.stdout = out
            try:
                agent_exec._print_usage_text(ok_report)
            finally:
                sys.stdout = real_out
            self.assertIn("(measured 2/3 delegated)", out.getvalue())

        with tempfile.TemporaryDirectory() as home:
            none_cfg = self._setup(home, matched_n=0, total_n=3)
            none_report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["codex"], cfg=none_cfg, home=home, cwd="/x",
                run_ids=["run1"])
            out = io.StringIO()
            real_out = sys.stdout
            sys.stdout = out
            try:
                agent_exec._print_usage_text(none_report)
            finally:
                sys.stdout = real_out
            self.assertIn("(measured 0/3 delegated)", out.getvalue())

    def test_window_scope_codex_report_shape_is_unchanged(self):
        with tempfile.TemporaryDirectory() as home:
            day = os.path.join(home, ".codex", "sessions", "2026", "08", "19")
            write_lines(
                os.path.join(day, "rollout-1.jsonl"),
                [codex_token_count(NOW - timedelta(hours=1), 100, 40, 10)],
            )
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["codex"], cfg={"telemetry": {"enabled": False}},
                home=home, cwd="/x")
            entry = report["sources"]["codex"]
            self.assertEqual(sorted(entry), [
                "files_scanned", "scope", "sessions", "status", "tokens"])
            self.assertEqual(entry["status"], "ok")
            self.assertEqual(entry["sessions"], 1)
            self.assertEqual(
                entry["tokens"],
                {"input_tokens": 60, "output_tokens": 10, "cached_input_tokens": 40},
            )
            self.assertEqual(
                entry,
                agent_exec.collect_codex_usage(CUTOFF, home=home))


class CopilotAggregationTests(unittest.TestCase):
    def _record(self, ts, **usage):
        return {"event": "dispatch", "executor": "copilot", "ts": iso(ts),
                "usage": usage}

    def test_sums_in_window_dispatch_records(self):
        records = [
            self._record(NOW - timedelta(hours=1), input_tokens=10,
                         output_tokens=2, cached_input_tokens=5,
                         aiu_nano=1000, premium_requests=1),
            self._record(NOW - timedelta(hours=2), input_tokens=1,
                         output_tokens=1, aiu_nano=500, premium_requests=2),
            self._record(NOW - timedelta(days=4), input_tokens=999),
            {"event": "run_summary", "executor": "copilot", "ts": iso(NOW),
             "usage": {"input_tokens": 777}},
            {"event": "dispatch", "executor": "codex", "ts": iso(NOW),
             "usage": {"input_tokens": 888}},
            {"event": "dispatch", "executor": "copilot", "ts": iso(NOW)},
            {"event": "dispatch", "executor": "copilot", "usage": {"input_tokens": 3}},
            "not a dict",
        ]
        acc = agent_exec.aggregate_copilot_records(records, CUTOFF)
        self.assertEqual(acc["records"], 2)
        self.assertEqual(
            acc["tokens"],
            {"input_tokens": 11, "output_tokens": 3, "cached_input_tokens": 5},
        )
        self.assertEqual(acc["aiu_nano"], 1500)
        self.assertEqual(acc["premium_requests"], 3)

    def test_telemetry_disabled_is_unavailable_not_zeros(self):
        cfg = {"telemetry": {"enabled": False, "dir": "/nonexistent-dir"}}
        entry = agent_exec.collect_copilot_usage(CUTOFF, cfg)
        self.assertEqual(entry["status"], "unavailable")
        self.assertIn("note", entry)
        self.assertEqual(entry["tokens"], agent_exec._zero_tokens())

    def test_enabled_but_missing_file_is_unavailable(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = {"telemetry": {"enabled": True, "dir": tmp}}
            entry = agent_exec.collect_copilot_usage(CUTOFF, cfg)
            self.assertEqual(entry["status"], "unavailable")

    def test_enabled_with_records_reads_them(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "records.jsonl")
            with open(path, "w", encoding="utf-8") as f:
                f.write(json.dumps(self._record(NOW, input_tokens=7,
                                                premium_requests=1)) + "\n")
                f.write("truncated{\n")
            cfg = {"telemetry": {"enabled": True, "dir": tmp}}
            entry = agent_exec.collect_copilot_usage(CUTOFF, cfg)
            self.assertEqual(entry["status"], "ok")
            self.assertEqual(entry["tokens"]["input_tokens"], 7)
            self.assertEqual(entry["premium_requests"], 1)


class TotalsAndReportTests(unittest.TestCase):
    def test_totals_only_sum_ok_sources(self):
        sources = {
            "claude": {"status": "ok",
                       "tokens": {"input_tokens": 10, "output_tokens": 1,
                                  "cached_input_tokens": 2}},
            "codex": {"status": "empty", "tokens": agent_exec._zero_tokens()},
            "copilot": {"status": "unavailable",
                        "tokens": {"input_tokens": 999, "output_tokens": 999,
                                   "cached_input_tokens": 999}},
        }
        self.assertEqual(
            agent_exec.sum_usage_totals(sources),
            {"input_tokens": 10, "output_tokens": 1, "cached_input_tokens": 2},
        )

    def test_report_shape_and_requested_sources_only(self):
        with tempfile.TemporaryDirectory() as home:
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["codex", "claude"], cfg={"telemetry": {"enabled": False}},
                home=home, cwd="/x")
            self.assertEqual(sorted(report), ["now", "scope", "since", "sources", "totals"])
            self.assertEqual(report["scope"], {
                "kind": "window", "since": CUTOFF.isoformat(), "until": NOW.isoformat()
            })
            self.assertEqual(sorted(report["sources"]), ["claude", "codex"])
            self.assertEqual(report["since"], CUTOFF.isoformat())
            self.assertEqual(report["now"], NOW.isoformat())
            for entry in report["sources"].values():
                self.assertIn(entry["status"],
                              ("ok", "empty", "unavailable"))
                self.assertIn("scope", entry)
                self.assertEqual(sorted(entry["tokens"]),
                                 ["cached_input_tokens", "input_tokens",
                                  "output_tokens"])

    def test_empty_environment_is_exit_0_with_zeroed_structures(self):
        with tempfile.TemporaryDirectory() as home:
            env_before = os.environ.get("CLAUDE_CONFIG_DIR")
            os.environ.pop("CLAUDE_CONFIG_DIR", None)
            try:
                report = agent_exec.build_usage_report(
                    CUTOFF, NOW, agent_exec._USAGE_SOURCES,
                    cfg={"telemetry": {"enabled": False}}, home=home, cwd="/x")
            finally:
                if env_before is not None:
                    os.environ["CLAUDE_CONFIG_DIR"] = env_before
            self.assertEqual(report["totals"], agent_exec._zero_tokens())
            for entry in report["sources"].values():
                self.assertEqual(entry["status"], "unavailable")

            out = io.StringIO()
            real_out = sys.stdout
            sys.stdout = out
            try:
                rc = agent_exec.cmd_usage([])
            finally:
                sys.stdout = real_out
            self.assertEqual(rc, 0)
            parsed = json.loads(out.getvalue())
            self.assertEqual(sorted(parsed),
                             ["now", "scope", "since", "sources", "totals"])

    def test_every_emitted_number_is_a_nonnegative_int(self):
        with tempfile.TemporaryDirectory() as home:
            slug = agent_exec.claude_project_slug("/x")
            write_lines(
                os.path.join(home, ".claude", "projects", slug, "a.jsonl"),
                [claude_line(NOW, input_tokens=3, output_tokens=1)],
            )
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, agent_exec._USAGE_SOURCES,
                cfg={"telemetry": {"enabled": False}}, home=home, cwd="/x")

            def walk(node):
                if isinstance(node, dict):
                    for v in node.values():
                        walk(v)
                elif isinstance(node, list):
                    for v in node:
                        walk(v)
                elif isinstance(node, bool) or isinstance(node, float):
                    self.fail("non-int number in report: %r" % (node,))
                elif isinstance(node, int):
                    self.assertGreaterEqual(node, 0)

            walk(report)

    def test_text_output_leaks_no_paths_or_ids(self):
        with tempfile.TemporaryDirectory() as home:
            slug = agent_exec.claude_project_slug("/Users/secret/proj")
            write_lines(
                os.path.join(home, ".claude", "projects", slug, "sess-uuid.jsonl"),
                [claude_line(NOW, input_tokens=3, output_tokens=1)],
            )
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, agent_exec._USAGE_SOURCES,
                cfg={"telemetry": {"enabled": False}}, home=home,
                cwd="/Users/secret/proj")
            out = io.StringIO()
            real_out = sys.stdout
            sys.stdout = out
            try:
                agent_exec._print_usage_text(report)
            finally:
                sys.stdout = real_out
            text = out.getvalue()
            self.assertIn("claude-opus-5", text)
            for leak in ("/Users", "secret", slug, "sess-uuid", home):
                self.assertNotIn(leak, text)


class ReadOnlyTests(unittest.TestCase):
    def _snapshot(self, root):
        entries = {}
        for dirpath, dirnames, filenames in os.walk(root):
            dirnames.sort()
            for name in sorted(filenames):
                full = os.path.join(dirpath, name)
                st = os.stat(full)
                with open(full, "rb") as f:
                    entries[full] = (st.st_size, st.st_mtime, f.read())
        return entries

    def test_report_writes_nothing_and_leaves_environ_alone(self):
        with tempfile.TemporaryDirectory() as home:
            slug = agent_exec.claude_project_slug("/x")
            write_lines(
                os.path.join(home, ".claude", "projects", slug, "a.jsonl"),
                [claude_line(NOW, input_tokens=3, output_tokens=1)],
            )
            day = os.path.join(home, ".codex", "sessions", "2026", "08", "19")
            write_lines(os.path.join(day, "rollout-1.jsonl"),
                        [codex_token_count(NOW, 10, 2, 1)])
            telemetry_dir = os.path.join(home, "telemetry")
            write_lines(
                os.path.join(telemetry_dir, "records.jsonl"),
                [json.dumps({"event": "dispatch", "executor": "copilot",
                             "ts": iso(NOW), "usage": {"input_tokens": 4}})],
            )

            before = self._snapshot(home)
            env_before = dict(os.environ)

            report = agent_exec.build_usage_report(
                CUTOFF, NOW, agent_exec._USAGE_SOURCES,
                cfg={"telemetry": {"enabled": True, "dir": telemetry_dir}},
                home=home, cwd="/x")
            self.assertEqual(report["totals"]["input_tokens"], 3 + 8 + 4)

            self.assertEqual(self._snapshot(home), before)
            self.assertEqual(dict(os.environ), env_before)


class DeterministicScopeTests(unittest.TestCase):
    def _write_claude(self, home, session, relative, lines):
        slug = agent_exec.claude_project_slug("/x")
        path = os.path.join(home, ".claude", "projects", slug, session, relative)
        write_lines(path, lines)
        return path

    def test_run_scope_is_recursive_and_ignores_time(self):
        with tempfile.TemporaryDirectory() as home:
            self._write_claude(
                home, "s-1", "subagents/workflows/wf_one/deep/a.jsonl",
                [claude_line(NOW - timedelta(days=2000), input_tokens=7)],
            )
            self._write_claude(
                home, "s-1", "subagents/workflows/wf_two/a.jsonl",
                [claude_line(NOW, input_tokens=100)],
            )
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude"], cfg={"telemetry": {"enabled": False}},
                home=home, cwd="/x", run_ids=["wf_one"],)
            self.assertEqual(report["scope"], {"kind": "run", "run_ids": ["wf_one"]})
            self.assertEqual(report["sources"]["claude"]["tokens"]["input_tokens"], 7)
            self.assertNotIn("since", report)
            self.assertNotIn("now", report)

    def test_session_and_union_deduplicate(self):
        with tempfile.TemporaryDirectory() as home:
            slug = agent_exec.claude_project_slug("/x")
            write_lines(
                os.path.join(home, ".claude", "projects", slug, "s-1.jsonl"),
                [claude_line(NOW, input_tokens=2)])
            self._write_claude(
                home, "s-1", "subagents/workflows/wf_one/a.jsonl",
                [claude_line(NOW, input_tokens=3)])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude", "codex"], cfg={"telemetry": {}},
                home=home, cwd="/x", run_ids=["wf_one"], session_ids=["s-1"])
            self.assertEqual(report["scope"], {
                "kind": "run+session", "run_ids": ["wf_one"], "session_ids": ["s-1"]})
            self.assertEqual(report["sources"]["claude"]["tokens"]["input_tokens"], 5)
            self.assertFalse(report["sources"]["codex"]["attributable"])
            self.assertNotIn("since", report)
            self.assertNotIn("now", report)

    def test_run_and_session_union_still_reports_ledger_totals(self):
        # Regression: a non-empty --session must not suppress the --run
        # scope's real ledger contribution for copilot/codex.
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            write_lines(os.path.join(ledger_dir, "ledger-session", "one.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 9}),
                json.dumps({"executor": "codex", "input_tokens": 6}),
            ])
            slug = agent_exec.claude_project_slug("/x")
            write_lines(
                os.path.join(home, ".claude", "projects", slug, "s-1.jsonl"),
                [claude_line(NOW, input_tokens=2)])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude", "copilot", "codex"],
                cfg={"telemetry": {"dir": telemetry_dir}},
                home=home, cwd="/x", run_ids=["one"], session_ids=["s-1"])
            self.assertEqual(report["scope"], {
                "kind": "run+session", "run_ids": ["one"], "session_ids": ["s-1"]})
            self.assertEqual(
                report["sources"]["copilot"]["tokens"]["input_tokens"], 9)
            self.assertEqual(
                report["sources"]["codex"]["tokens"]["input_tokens"], 6)
            self.assertNotIn("attributable", report["sources"]["copilot"])
            self.assertNotIn("attributable", report["sources"]["codex"])
            self.assertNotIn("since", report)
            self.assertNotIn("now", report)

    def test_session_scope_reports_no_matching_run_ledger_data_per_source(self):
        # Section C: under --session, a source with no lines in the
        # session's ledger file keeps the exact "no matching run ledger
        # data" reason, even while a sibling source in the SAME file is
        # attributable.
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            write_lines(os.path.join(ledger_dir, "sess-x", "no.run.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 5}),
            ])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot", "codex"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", session_ids=["sess-x"])
            self.assertEqual(report["sources"]["copilot"]["status"], "ok")
            self.assertEqual(
                report["sources"]["copilot"]["tokens"]["input_tokens"], 5)
            self.assertEqual(report["sources"]["codex"], {
                "attributable": False,
                "reason": "no matching run ledger data",
            })

    def test_session_only_scope_omits_window_keys(self):
        with tempfile.TemporaryDirectory() as home:
            slug = agent_exec.claude_project_slug("/x")
            write_lines(
                os.path.join(home, ".claude", "projects", slug, "s-1.jsonl"),
                [claude_line(NOW, input_tokens=2)])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude"], cfg={"telemetry": {"enabled": False}},
                home=home, cwd="/x", session_ids=["s-1"])
            self.assertEqual(report["scope"], {
                "kind": "session", "session_ids": ["s-1"]})
            self.assertEqual(sorted(report), ["scope", "sources", "totals"])
            self.assertNotIn("since", report)
            self.assertNotIn("now", report)

    def test_window_scope_keeps_since_and_now(self):
        with tempfile.TemporaryDirectory() as home:
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude"], cfg={"telemetry": {"enabled": False}},
                home=home, cwd="/x")
            self.assertIn("since", report)
            self.assertIn("now", report)
            self.assertEqual(report["since"], CUTOFF.isoformat())
            self.assertEqual(report["now"], NOW.isoformat())
            self.assertEqual(report["scope"]["kind"], "window")

    def test_run_scope_text_output_has_no_window_wording(self):
        with tempfile.TemporaryDirectory() as home:
            self._write_claude(
                home, "s-1", "subagents/workflows/wf_one/a.jsonl",
                [claude_line(NOW, input_tokens=7)])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude"], cfg={"telemetry": {"enabled": False}},
                home=home, cwd="/x", run_ids=["wf_one"])
            out = io.StringIO()
            real_out = sys.stdout
            sys.stdout = out
            try:
                agent_exec._print_usage_text(report)
            finally:
                sys.stdout = real_out
            text = out.getvalue()
            self.assertIn("scope: run wf_one", text)
            self.assertNotIn("since=", text)
            self.assertNotIn("until=", text)
            self.assertNotIn("window", text)

    def test_ledger_reader_and_unknown_run(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(agent_exec.read_run_ledger(directory, "missing"), [])
            write_lines(os.path.join(directory, "session", "r.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 4}),
                "garbage",
            ])
            self.assertEqual(agent_exec.read_run_ledger(directory, "r"),
                             [{"executor": "copilot", "input_tokens": 4}])
            write_lines(os.path.join(directory, "r.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 999}),
            ])
            self.assertEqual(agent_exec.read_run_ledger(directory, "r"),
                             [{"executor": "copilot", "input_tokens": 4}])
            self.assertEqual(agent_exec.read_run_ledger(
                os.path.join(directory, "absent"), "r"), [])

    def test_scope_option_conflicts_are_exit_two_without_stdout(self):
        for args in (
            ["--since", "1h", "--run", "r"],
            ["--since", "1h", "--session", "s"],
            ["--list-runs", "--run", "r"],
            ["--list-runs", "--since", "1h"],
            ["--run", "a,,b"],
        ):
            out, err = io.StringIO(), io.StringIO()
            old_out, old_err = sys.stdout, sys.stderr
            sys.stdout, sys.stderr = out, err
            try:
                rc = agent_exec.cmd_usage(args)
            finally:
                sys.stdout, sys.stderr = old_out, old_err
            self.assertEqual(rc, 2)
            self.assertEqual(out.getvalue(), "")

    def test_list_runs_is_sorted_and_reports_metadata(self):
        with tempfile.TemporaryDirectory() as home:
            self._write_claude(
                home, "s", "subagents/workflows/wf_a/x.jsonl",
                [claude_line(NOW - timedelta(hours=2), input_tokens=1),
                 claude_line(NOW - timedelta(hours=1), input_tokens=1)])
            self._write_claude(
                home, "s", "subagents/workflows/wf_b/x.jsonl",
                [claude_line(NOW - timedelta(hours=3), input_tokens=1)])
            runs = agent_exec._list_usage_runs(
                home=home, config_dir="", sources=["claude"],
                cfg={"telemetry": {"dir": os.path.join(home, "telemetry")}},
                all_projects=True)
            self.assertEqual([r["run_id"] for r in runs], ["wf_b", "wf_a"])
            self.assertEqual(runs[0]["files"], 1)

    def test_run_scope_never_matches_a_session_id_regression(self):
        # THE REGRESSION this task exists to fix: a session id passed to
        # --run must never resolve the session's directory as if it were a
        # run file. With a populated <ledger-dir>/<sid>/ directory holding
        # several runs, --run <sid> must report ZERO records for every
        # ledger source -- never the session's aggregate numbers.
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            sid = "30ad8eb2-33a4-4ed8-bc5a-f21d111223b2"
            write_lines(os.path.join(ledger_dir, sid, "wf_a.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 100}),
            ])
            write_lines(os.path.join(ledger_dir, sid, "wf_b.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 200}),
                json.dumps({"executor": "codex", "input_tokens": 50}),
            ])
            write_lines(os.path.join(ledger_dir, sid, "no.run.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 300}),
            ])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot", "codex"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", run_ids=[sid])
            self.assertEqual(report["scope"], {"kind": "run", "run_ids": [sid]})
            for name in ("copilot", "codex"):
                self.assertEqual(report["sources"][name], {
                    "attributable": False,
                    "reason": "no matching run ledger data",
                })
                self.assertNotIn("tokens", report["sources"][name])

    def test_run_scope_never_matches_session_id_with_shadowing_flat_file_regression(self):
        # THE LITERAL REGRESSION 1 SCENARIO: a session directory containing a
        # differently-named run file, PLUS a flat legacy file that happens to
        # share the session id's own name. --run <sid> must report
        # attributable:false for every ledger source, and the flat file's
        # numbers must appear nowhere in the report.
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            sid = "30ad8eb2-33a4-4ed8-bc5a-f21d111223b2"
            write_lines(os.path.join(ledger_dir, sid, "wf_a.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 100}),
                json.dumps({"executor": "codex", "input_tokens": 25}),
            ])
            write_lines(os.path.join(ledger_dir, sid + ".jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 6370990}),
                json.dumps({"executor": "codex", "input_tokens": 561018}),
            ])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot", "codex"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", run_ids=[sid])
            self.assertEqual(report["scope"], {"kind": "run", "run_ids": [sid]})
            for name in ("copilot", "codex"):
                self.assertEqual(report["sources"][name], {
                    "attributable": False,
                    "reason": "no matching run ledger data",
                })
                self.assertNotIn("tokens", report["sources"][name])
            rendered = json.dumps(report)
            self.assertNotIn("6370990", rendered)
            self.assertNotIn("561018", rendered)

    def test_run_scope_sums_same_run_id_across_two_sessions_once(self):
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            write_lines(os.path.join(ledger_dir, "sess-a", "shared.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 3}),
            ])
            write_lines(os.path.join(ledger_dir, "sess-b", "shared.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 5}),
            ])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", run_ids=["shared"])
            self.assertEqual(
                report["sources"]["copilot"]["tokens"]["input_tokens"], 8)
            self.assertEqual(report["sources"]["copilot"]["records"], 2)

    def test_run_scope_ignores_legacy_flat_file(self):
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            write_lines(os.path.join(ledger_dir, "sess-a", "combo.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 4}),
            ])
            write_lines(os.path.join(ledger_dir, "combo.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 9}),
            ])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", run_ids=["combo"])
            self.assertEqual(
                report["sources"]["copilot"]["tokens"]["input_tokens"], 4)
            self.assertEqual(report["sources"]["copilot"]["records"], 1)

    def test_run_scope_requires_workflows_parent(self):
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            self._write_claude(
                home, "s-1", "subagents/session-shaped/a.jsonl",
                [claude_line(NOW, input_tokens=99)])
            self._write_claude(
                home, "s-1", "subagents/workflows/wf_a/a.jsonl",
                [claude_line(NOW, input_tokens=7)])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", run_ids=["s-1"])
            self.assertEqual(report["sources"]["claude"], {
                "attributable": False,
                "reason": "no matching transcript data",
            })
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", run_ids=["wf_a"])
            self.assertEqual(
                report["sources"]["claude"]["tokens"]["input_tokens"], 7)

    def test_session_scope_never_reads_legacy_flat_file_of_same_name(self):
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            sid = "sess-shadow"
            write_lines(os.path.join(ledger_dir, sid, "no.run.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 2}),
            ])
            # A legacy flat file that happens to share the session's name.
            write_lines(os.path.join(ledger_dir, sid + ".jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 999}),
            ])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", session_ids=[sid])
            self.assertEqual(
                report["sources"]["copilot"]["tokens"]["input_tokens"], 2)
            self.assertEqual(report["sources"]["copilot"]["records"], 1)

    def test_session_scope_runs_breakdown_exact_shape(self):
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            sid = "sess-breakdown"
            write_lines(os.path.join(ledger_dir, sid, "wf_a.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 1})
                for _ in range(3)
            ])
            write_lines(os.path.join(ledger_dir, sid, "wf_b.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 1})
                for _ in range(5)
            ])
            write_lines(os.path.join(ledger_dir, sid, "no.run.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 1})
                for _ in range(2)
            ])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot", "codex"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", session_ids=[sid])
            self.assertEqual(
                report["sources"]["copilot"]["runs"],
                {"wf_a": 3, "wf_b": 5, "no-run": 2})
            # codex has no ledger lines at all in this session: the key is
            # omitted, not present-and-empty.
            self.assertNotIn("runs", report["sources"]["codex"])

            out = io.StringIO()
            real_out = sys.stdout
            sys.stdout = out
            try:
                agent_exec._print_usage_text(report)
            finally:
                sys.stdout = real_out
            text = out.getvalue()
            self.assertIn("wf_a=3", text)
            self.assertIn("wf_b=5", text)
            self.assertIn("no-run=2", text)

    def test_run_scope_and_window_scope_omit_runs_key(self):
        with tempfile.TemporaryDirectory() as home:
            telemetry_dir = os.path.join(home, "telemetry")
            ledger_dir = os.path.join(home, "runs")
            write_lines(os.path.join(ledger_dir, "sess-only", "wf_c.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 1}),
            ])
            run_report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x", run_ids=["wf_c"])
            self.assertNotIn("runs", run_report["sources"]["copilot"])

            window_report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot"],
                cfg={"telemetry": {"dir": telemetry_dir, "enabled": False}},
                home=home, cwd="/x")
            self.assertNotIn("runs", window_report["sources"].get("copilot", {}))

    def test_list_runs_respects_cwd_scope_unless_all_projects(self):
        with tempfile.TemporaryDirectory() as home:
            self._write_claude(
                home, "s", "subagents/workflows/wf_a/x.jsonl",
                [claude_line(NOW, input_tokens=1)])
            slug_dir = os.path.join(
                home, ".claude", "projects", agent_exec.claude_project_slug("/other"))
            write_lines(
                os.path.join(slug_dir, "s2", "subagents", "workflows",
                             "wf_b", "x.jsonl"),
                [claude_line(NOW, input_tokens=1)])

            cwd_scoped = agent_exec._list_usage_runs(
                home=home, config_dir="", sources=["claude"], cfg={},
                cwd="/x")
            self.assertEqual([r["run_id"] for r in cwd_scoped], ["wf_a"])

            all_scoped = agent_exec._list_usage_runs(
                home=home, config_dir="", sources=["claude"], cfg={},
                all_projects=True)
            self.assertEqual(
                sorted(r["run_id"] for r in all_scoped), ["wf_a", "wf_b"])


class OrdinalAddressingTests(unittest.TestCase):
    """`usage --run <ordinal>` prefix addressing (ORD1)."""

    def setUp(self):
        self._session_before = os.environ.get("CLAUDE_CODE_SESSION_ID")

    def tearDown(self):
        if self._session_before is None:
            os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        else:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_before

    def test_bare_ordinal_and_leading_zeros_resolve_same_run(self):
        with tempfile.TemporaryDirectory() as ledger_dir:
            session = "ord-sess"
            write_lines(os.path.join(ledger_dir, session, "001-run_a.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 1})])
            write_lines(os.path.join(ledger_dir, session, "002-run_b.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 2})])
            write_lines(os.path.join(ledger_dir, session, "003-wf_ord.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 3})])
            os.environ["CLAUDE_CODE_SESSION_ID"] = session
            for selector in ("3", "03", "003", "wf_ord"):
                self.assertEqual(
                    agent_exec._resolve_run_selector(ledger_dir, selector),
                    "wf_ord", "selector %r" % selector)

    def test_ordinal_without_session_id_matches_nothing(self):
        with tempfile.TemporaryDirectory() as ledger_dir:
            session = "ord-sess"
            write_lines(os.path.join(ledger_dir, session, "001-run_a.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 1})])
            os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
            self.assertIsNone(agent_exec._resolve_run_selector(ledger_dir, "1"))

    def test_ordinal_in_different_session_matches_nothing(self):
        with tempfile.TemporaryDirectory() as ledger_dir:
            write_lines(
                os.path.join(ledger_dir, "session-other", "001-wf_other.jsonl"),
                [json.dumps({"executor": "copilot", "input_tokens": 1})])
            os.environ["CLAUDE_CODE_SESSION_ID"] = "session-mine"
            self.assertIsNone(agent_exec._resolve_run_selector(ledger_dir, "1"))

    def test_full_run_id_matches_both_prefixed_and_unprefixed_forms(self):
        with tempfile.TemporaryDirectory() as ledger_dir:
            write_lines(os.path.join(ledger_dir, "s1", "legacyrun.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 1})])
            write_lines(os.path.join(ledger_dir, "s2", "005-otherrun.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 2})])
            os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
            self.assertEqual(
                agent_exec.read_run_ledger(ledger_dir, "legacyrun"),
                [{"executor": "copilot", "input_tokens": 1}])
            self.assertEqual(
                agent_exec.read_run_ledger(ledger_dir, "otherrun"),
                [{"executor": "copilot", "input_tokens": 2}])

    def test_runs_breakdown_ordinal_order_with_no_run_last(self):
        with tempfile.TemporaryDirectory() as home:
            ledger_dir = os.path.join(home, "runs")
            session = "ord-runs-sess"
            # Written out of ordinal order on purpose: b (002) before a
            # (001), plus no.run.jsonl, so an alphabetical or filesystem
            # listing order would fail this assertion.
            write_lines(os.path.join(ledger_dir, session, "002-run_b.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 1})])
            write_lines(os.path.join(ledger_dir, session, "001-run_a.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 1})])
            write_lines(os.path.join(ledger_dir, session, "no.run.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 1})])
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["copilot"],
                cfg={"ledger": {"dir": ledger_dir}}, home=home, cwd="/x",
                session_ids=[session])
            runs = report["sources"]["copilot"]["runs"]
            self.assertEqual(list(runs.keys()), ["run_a", "run_b", "no-run"])

    def test_list_runs_ordinal_field_present_and_null(self):
        with tempfile.TemporaryDirectory() as home:
            ledger_dir = os.path.join(home, "runs")
            session = "ord-list-sess"
            write_lines(os.path.join(ledger_dir, session, "001-wf_ledgered.jsonl"),
                       [json.dumps({"executor": "copilot", "input_tokens": 1,
                                    "ts": iso(NOW)})])
            slug = agent_exec.claude_project_slug("/x")
            write_lines(
                os.path.join(home, ".claude", "projects", slug, session,
                             "subagents/workflows/wf_claude_only/x.jsonl"),
                [claude_line(NOW, input_tokens=1)])
            os.environ["CLAUDE_CODE_SESSION_ID"] = session
            runs = agent_exec._list_usage_runs(
                home=home, config_dir="", sources=["claude", "copilot"],
                cfg={"ledger": {"dir": ledger_dir},
                     "telemetry": {"dir": os.path.join(home, "telemetry")}},
                all_projects=True)
            by_id = {r["run_id"]: r for r in runs}
            self.assertEqual(by_id["wf_ledgered"]["ordinal"], 1)
            self.assertIsNone(by_id["wf_claude_only"]["ordinal"])

    def test_since_window_scope_unchanged(self):
        with tempfile.TemporaryDirectory() as home:
            report = agent_exec.build_usage_report(
                CUTOFF, NOW, ["claude"], cfg={"telemetry": {"enabled": False}},
                home=home, cwd="/x")
            self.assertEqual(report["scope"], {
                "kind": "window", "since": CUTOFF.isoformat(),
                "until": NOW.isoformat()})
            self.assertEqual(report["since"], CUTOFF.isoformat())
            self.assertEqual(report["now"], NOW.isoformat())


class EndToEndSubprocessTests(unittest.TestCase):
    """Drives tools/agent_exec.py as a real subprocess. A caller/callee
    signature mismatch (argv parsing calling a helper with a keyword it does
    not accept) or a real-layout wiring bug (e.g. matching the wrong
    directory name) raises/returns wrong data here even when every helper
    passes in isolation -- that is the gap this class exists to close."""

    def _write_claude(self, home, session, relative, lines):
        slug = agent_exec.claude_project_slug(home)
        path = os.path.join(home, ".claude", "projects", slug, session, relative)
        write_lines(path, lines)
        return path

    def test_list_runs_json_over_subprocess(self):
        with tempfile.TemporaryDirectory() as home:
            # realpath: on macOS the raw tempdir path traverses a /var ->
            # /private/var symlink, which would otherwise make the child
            # process's os.getcwd() resolve to a different project slug
            # than the one used to write the fixture below.
            home = os.path.realpath(home)
            self._write_claude(
                home, "s", "subagents/workflows/wf_e2e-a/x.jsonl",
                [claude_line(NOW - timedelta(days=2000), input_tokens=1)])
            rc, out, err = run_cli(["--list-runs", "--json"], home=home)
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            self.assertEqual(sorted(parsed), ["runs"])
            self.assertEqual([r["run_id"] for r in parsed["runs"]], ["wf_e2e-a"])
            self.assertEqual(parsed["runs"][0]["files"], 1)

    def test_run_scope_real_hit_over_subprocess(self):
        with tempfile.TemporaryDirectory() as home:
            self._write_claude(
                home, "s", "subagents/workflows/wf_e2e-b/deep/x.jsonl",
                [claude_line(NOW - timedelta(days=2000), input_tokens=42,
                             output_tokens=7)])
            rc, out, err = run_cli(
                ["--run", "wf_e2e-b", "--json"], home=home)
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            self.assertEqual(parsed["scope"], {"kind": "run", "run_ids": ["wf_e2e-b"]})
            self.assertEqual(
                parsed["sources"]["claude"]["tokens"]["input_tokens"], 42)
            self.assertEqual(
                parsed["sources"]["claude"]["tokens"]["output_tokens"], 7)
            self.assertNotIn("since", parsed)
            self.assertNotIn("now", parsed)

    def test_run_scope_text_over_subprocess_has_no_window_wording(self):
        with tempfile.TemporaryDirectory() as home:
            self._write_claude(
                home, "s", "subagents/workflows/wf_e2e-text/x.jsonl",
                [claude_line(NOW - timedelta(days=2000), input_tokens=42)])
            rc, out, err = run_cli(["--run", "wf_e2e-text", "--text"], home=home)
            self.assertEqual(rc, 0, err)
            self.assertIn("scope: run wf_e2e-text", out)
            self.assertNotIn("since=", out)
            self.assertNotIn("until=", out)
            self.assertNotIn("window", out)

    def test_run_and_session_combined_over_subprocess(self):
        with tempfile.TemporaryDirectory() as home:
            slug = agent_exec.claude_project_slug(home)
            write_lines(
                os.path.join(home, ".claude", "projects", slug, "sess-1.jsonl"),
                [claude_line(NOW, input_tokens=2)])
            self._write_claude(
                home, "s", "subagents/workflows/wf_e2e-c/x.jsonl",
                [claude_line(NOW, input_tokens=3)])
            rc, out, err = run_cli(
                ["--run", "wf_e2e-c", "--session", "sess-1", "--json"], home=home)
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            self.assertEqual(parsed["scope"], {
                "kind": "run+session", "run_ids": ["wf_e2e-c"],
                "session_ids": ["sess-1"]})
            self.assertEqual(
                parsed["sources"]["claude"]["tokens"]["input_tokens"], 5)
            self.assertNotIn("since", parsed)
            self.assertNotIn("now", parsed)

    def test_delegated_codex_run_scope_over_subprocess(self):
        with tempfile.TemporaryDirectory() as home, \
             tempfile.TemporaryDirectory() as codex_home:
            corrs = ["oxc-%012x" % n for n in (1, 2, 3)]
            write_lines(
                os.path.join(home, ".claude", "orchestra", "runs",
                             "ledger-session", "run_e2e.jsonl"),
                [delegated_ledger_line(c) for c in corrs])
            for c in corrs[:2]:
                write_lines(
                    os.path.join(codex_home, "sessions", "rollout-%s.jsonl" % c),
                    codex_rollout_lines(c, [5]))
            rc, out, err = run_cli(
                ["--run", "run_e2e", "--source", "codex", "--json"],
                home=home, extra_env={"CODEX_HOME": codex_home})
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            entry = parsed["sources"]["codex"]
            self.assertEqual(entry["status"], "ok")
            self.assertEqual(entry["delegated"], 3)
            self.assertEqual(entry["measured"], 2)
            self.assertEqual(entry["tokens"]["input_tokens"], 10)

    def test_run_scope_session_id_reports_zero_over_subprocess(self):
        # THE REGRESSION, driven through the real CLI subprocess: a session
        # id passed to --run over a populated <ledger-dir>/<sid>/ directory
        # must report zero ledger records, never the session's total.
        with tempfile.TemporaryDirectory() as home:
            os.makedirs(os.path.join(home, ".claude"), exist_ok=True)
            ledger_dir = os.path.join(home, ".claude", "orchestra", "runs")
            sid = "e2e-session-30ad8eb2"
            write_lines(os.path.join(ledger_dir, sid, "wf_a.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 100}),
            ])
            write_lines(os.path.join(ledger_dir, sid, "wf_b.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 200}),
            ])
            write_lines(os.path.join(ledger_dir, sid, "no.run.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 300}),
            ])
            rc, out, err = run_cli(
                ["--run", sid, "--source", "copilot", "--json"], home=home)
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            self.assertEqual(parsed["sources"]["copilot"], {
                "attributable": False,
                "reason": "no matching run ledger data",
            })
            self.assertNotIn("tokens", parsed["sources"]["copilot"])

    def test_run_scope_requires_workflows_parent_over_subprocess(self):
        # THE LITERAL REGRESSION 2 SCENARIO, driven through the real CLI
        # subprocess: a session directory whose subagents/ holds a
        # non-workflows transcript (session-shaped, named after the session
        # id) AND whose subagents/workflows/wf_a/ holds a genuine run
        # transcript. --run <sid> must count nothing; --run wf_a must count
        # only wf_a's data.
        with tempfile.TemporaryDirectory() as home:
            sid = "e2e-session-workflows-parent"
            self._write_claude(
                home, sid, "subagents/session-shaped/a.jsonl",
                [claude_line(NOW - timedelta(days=2000), input_tokens=99)])
            self._write_claude(
                home, sid, "subagents/workflows/wf_a/a.jsonl",
                [claude_line(NOW - timedelta(days=2000), input_tokens=7)])

            rc, out, err = run_cli(["--run", sid, "--json"], home=home)
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            self.assertEqual(parsed["sources"]["claude"], {
                "attributable": False,
                "reason": "no matching transcript data",
            })
            self.assertNotIn("tokens", parsed["sources"]["claude"])

            rc, out, err = run_cli(["--run", "wf_a", "--json"], home=home)
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            self.assertEqual(
                parsed["sources"]["claude"]["tokens"]["input_tokens"], 7)

    def test_since_with_run_exits_two_with_empty_stdout(self):
        with tempfile.TemporaryDirectory() as home:
            rc, out, err = run_cli(
                ["--since", "1h", "--run", "wf_x"], home=home)
            self.assertEqual(rc, 2)
            self.assertEqual(out, "")
            self.assertIn("--since", err)

    def test_list_runs_with_session_exits_two_with_empty_stdout(self):
        with tempfile.TemporaryDirectory() as home:
            rc, out, err = run_cli(
                ["--list-runs", "--session", "s"], home=home)
            self.assertEqual(rc, 2)
            self.assertEqual(out, "")
            self.assertIn("--list-runs", err)

    def test_empty_run_element_exits_two_over_subprocess(self):
        with tempfile.TemporaryDirectory() as home:
            rc, out, err = run_cli(["--run", "a,,b"], home=home)
            self.assertEqual(rc, 2)
            self.assertEqual(out, "")

    def test_ordinal_addressing_matches_full_run_id_over_subprocess(self):
        # ORD1: --run 7 / --run 07 / --run 007 / --run <full id> must all
        # resolve the same run and report identical numbers, including the
        # claude source (whose transcripts are addressed by the harness run
        # id recovered from the ordinal-prefixed ledger filename).
        with tempfile.TemporaryDirectory() as home:
            home = os.path.realpath(home)
            session_id = "e2e-ord-session"
            run_id = "wf_e2e_ord"
            ledger_dir = os.path.join(home, ".claude", "orchestra", "runs")
            write_lines(
                os.path.join(ledger_dir, session_id, "007-%s.jsonl" % run_id),
                [json.dumps({"executor": "copilot", "input_tokens": 11,
                            "output_tokens": 3})])
            self._write_claude(
                home, "s", "subagents/workflows/%s/x.jsonl" % run_id,
                [claude_line(NOW - timedelta(days=2000), input_tokens=42,
                            output_tokens=7)])
            reports = {}
            for selector in ("7", "07", "007", run_id):
                rc, out, err = run_cli(
                    ["--run", selector, "--json"], home=home,
                    extra_env={"CLAUDE_CODE_SESSION_ID": session_id})
                self.assertEqual(rc, 0, err)
                reports[selector] = json.loads(out)
            base = reports[run_id]
            self.assertEqual(base["scope"], {"kind": "run", "run_ids": [run_id]})
            self.assertEqual(
                base["sources"]["claude"]["tokens"]["input_tokens"], 42)
            self.assertEqual(
                base["sources"]["claude"]["tokens"]["output_tokens"], 7)
            self.assertEqual(
                base["sources"]["copilot"]["tokens"]["input_tokens"], 11)
            for selector in ("7", "07", "007"):
                self.assertEqual(reports[selector], base, "selector %r" % selector)

    def test_ordinal_without_session_id_matches_nothing_over_subprocess(self):
        with tempfile.TemporaryDirectory() as home:
            session_id = "e2e-ord-session2"
            run_id = "wf_e2e_ord2"
            ledger_dir = os.path.join(home, ".claude", "orchestra", "runs")
            write_lines(
                os.path.join(ledger_dir, session_id, "001-%s.jsonl" % run_id),
                [json.dumps({"executor": "copilot", "input_tokens": 5})])
            rc, out, err = run_cli(
                ["--run", "1", "--source", "copilot", "--json"], home=home,
                unset_env=["CLAUDE_CODE_SESSION_ID"])
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            self.assertEqual(parsed["sources"]["copilot"], {
                "attributable": False, "reason": "no matching run ledger data"})

    def test_ordinal_in_different_session_matches_nothing_over_subprocess(self):
        with tempfile.TemporaryDirectory() as home:
            ledger_dir = os.path.join(home, ".claude", "orchestra", "runs")
            write_lines(
                os.path.join(ledger_dir, "session-other", "001-wf_other.jsonl"),
                [json.dumps({"executor": "copilot", "input_tokens": 9})])
            rc, out, err = run_cli(
                ["--run", "1", "--source", "copilot", "--json"], home=home,
                extra_env={"CLAUDE_CODE_SESSION_ID": "session-mine"})
            self.assertEqual(rc, 0, err)
            parsed = json.loads(out)
            self.assertEqual(parsed["sources"]["copilot"], {
                "attributable": False, "reason": "no matching run ledger data"})


if __name__ == "__main__":
    unittest.main()
