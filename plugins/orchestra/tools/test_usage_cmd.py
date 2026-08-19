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


def run_cli(args, home, config_dir=None, extra_env=None):
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
            write_lines(os.path.join(ledger_dir, "one.jsonl"), [
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
            write_lines(os.path.join(directory, "r.jsonl"), [
                json.dumps({"executor": "copilot", "input_tokens": 4}),
                "garbage",
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


if __name__ == "__main__":
    unittest.main()
