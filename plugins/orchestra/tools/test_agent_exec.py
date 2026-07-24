# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for the anonymized telemetry facility in agent_exec.py.

Run with: uv run test_agent_exec.py
"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402


class SanitizeTelemetryRecordTests(unittest.TestCase):
    def test_missing_event_returns_none(self):
        self.assertIsNone(agent_exec.sanitize_telemetry_record({}))
        self.assertIsNone(agent_exec.sanitize_telemetry_record({"event": "bogus"}))
        self.assertIsNone(agent_exec.sanitize_telemetry_record({"foo": "bar"}))

    def test_non_dict_input_returns_none(self):
        self.assertIsNone(agent_exec.sanitize_telemetry_record(None))
        self.assertIsNone(agent_exec.sanitize_telemetry_record("run_summary"))
        self.assertIsNone(agent_exec.sanitize_telemetry_record([1, 2, 3]))

    def test_forbidden_keys_are_stripped(self):
        raw = {
            "event": "dispatch",
            "prompt": "do the secret thing",
            "file": "/Users/me/secret.py",
            "path": "/Users/me/project",
            "task_id": "TASK-123",
            "summary": "did a thing",
            "error": "Traceback (most recent call last): ...",
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertIsNotNone(out)
        for forbidden in ("prompt", "file", "path", "task_id", "summary", "error"):
            self.assertNotIn(forbidden, out)

    def test_free_text_disguised_as_enum_value_is_dropped(self):
        raw = {
            "event": "dispatch",
            "reason": "quota: /Users/x/s.py",
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertIsNotNone(out)
        self.assertNotIn("reason", out)

    def test_valid_reason_is_kept(self):
        out = agent_exec.sanitize_telemetry_record({"event": "dispatch", "reason": "quota"})
        self.assertEqual(out["reason"], "quota")

    def test_stamped_fields(self):
        out = agent_exec.sanitize_telemetry_record({"event": "run_summary"})
        self.assertEqual(out["schema_version"], 1)
        self.assertIsInstance(out["ts"], str)
        self.assertTrue(len(out["ts"]) > 0)
        self.assertEqual(out["os"], out["os"].lower())
        import platform
        self.assertEqual(out["os"], platform.system().lower())

    def test_stamped_fields_ignore_caller_supplied_values(self):
        raw = {
            "event": "run_summary",
            "schema_version": 999,
            "ts": "1999-01-01T00:00:00Z",
            "os": "not-a-real-os",
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["schema_version"], 1)
        self.assertNotEqual(out["ts"], "1999-01-01T00:00:00Z")
        self.assertNotEqual(out["os"], "not-a-real-os")

    def test_orchestra_version_pattern(self):
        out = agent_exec.sanitize_telemetry_record(
            {"event": "run_summary", "orchestra_version": "1.2.3"}
        )
        self.assertEqual(out["orchestra_version"], "1.2.3")

        out2 = agent_exec.sanitize_telemetry_record(
            {"event": "run_summary", "orchestra_version": "1.2.3-dirty/path"}
        )
        self.assertNotIn("orchestra_version", out2)

    def test_lane_enum(self):
        out = agent_exec.sanitize_telemetry_record(
            {"event": "run_summary", "lane": "express"}
        )
        self.assertEqual(out["lane"], "express")
        out2 = agent_exec.sanitize_telemetry_record(
            {"event": "run_summary", "lane": "sideways"}
        )
        self.assertNotIn("lane", out2)

    def test_executor_and_cls_enum(self):
        out = agent_exec.sanitize_telemetry_record(
            {"event": "dispatch", "executor": "copilot", "cls": "light"}
        )
        self.assertEqual(out["executor"], "copilot")
        self.assertEqual(out["cls"], "light")

        out2 = agent_exec.sanitize_telemetry_record(
            {"event": "dispatch", "executor": "some-random-cli", "cls": "ultra"}
        )
        self.assertNotIn("executor", out2)
        self.assertNotIn("cls", out2)

    def test_status_enum(self):
        out = agent_exec.sanitize_telemetry_record({"event": "dispatch", "status": "ok"})
        self.assertEqual(out["status"], "ok")
        out2 = agent_exec.sanitize_telemetry_record(
            {"event": "dispatch", "status": "crashed horribly"}
        )
        self.assertNotIn("status", out2)

    def test_resumed_rejects_int_requires_real_bool(self):
        out = agent_exec.sanitize_telemetry_record({"event": "dispatch", "resumed": 1})
        self.assertNotIn("resumed", out)
        out2 = agent_exec.sanitize_telemetry_record({"event": "dispatch", "resumed": 0})
        self.assertNotIn("resumed", out2)
        out3 = agent_exec.sanitize_telemetry_record({"event": "dispatch", "resumed": True})
        self.assertIs(out3["resumed"], True)
        out4 = agent_exec.sanitize_telemetry_record({"event": "dispatch", "resumed": False})
        self.assertIs(out4["resumed"], False)

    def test_nonnegative_int_counters(self):
        raw = {
            "event": "run_summary",
            "task_count": 5,
            "pass": 4,
            "fail": -1,
            "exhausted": 0,
            "fallbacks": "2",
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["task_count"], 5)
        self.assertEqual(out["pass"], 4)
        self.assertNotIn("fail", out)
        self.assertEqual(out["exhausted"], 0)
        self.assertNotIn("fallbacks", out)

    def test_classes_subkey_filtering(self):
        raw = {
            "event": "run_summary",
            "classes": {"light": 3, "standard": 2, "bogus": 1, "deep": -1, "review": "x"},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["classes"], {"light": 3, "standard": 2})

    def test_classes_rejects_non_dict(self):
        out = agent_exec.sanitize_telemetry_record({"event": "run_summary", "classes": "oops"})
        self.assertNotIn("classes", out)

    def test_executors_used_subkey_filtering(self):
        raw = {
            "event": "run_summary",
            "executors_used": {"claude": 2, "copilot": 1, "gemini": 5, "codex": -3},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["executors_used"], {"claude": 2, "copilot": 1})

    def test_rounds_subkey_filtering(self):
        raw = {
            "event": "run_summary",
            "rounds": {"1": 3, "2": 1, "0": 5, "abc": 2, "3": -1, "07": 2},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["rounds"], {"1": 3, "2": 1})

    def test_external_enabled_filtering(self):
        raw = {
            "event": "run_summary",
            "external_enabled": {"copilot": True, "codex": False, "claude": True, "x": 1},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["external_enabled"], {"copilot": True, "codex": False})

    def test_unknown_keys_dropped(self):
        raw = {"event": "run_summary", "some_new_field": "value"}
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertNotIn("some_new_field", out)


class BuildDispatchRecordTests(unittest.TestCase):
    def test_shape_without_resume_or_cls(self):
        result = {
            "status": "ok",
            "answer": "the actual answer text",
            "session_id": "sess-1",
            "reason": None,
            "exit_code": 0,
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, None)
        self.assertEqual(record["event"], "dispatch")
        self.assertEqual(record["executor"], "copilot")
        self.assertEqual(record["status"], "ok")
        self.assertIsNone(record["reason"])
        self.assertIs(record["resumed"], False)
        self.assertNotIn("cls", record)
        self.assertNotIn("answer", record)

    def test_shape_with_resume_and_cls(self):
        result = {
            "status": "unavailable",
            "answer": None,
            "session_id": None,
            "reason": "quota",
            "exit_code": 1,
        }
        record = agent_exec.build_dispatch_record("copilot", result, "sess-abc", "standard")
        self.assertIs(record["resumed"], True)
        self.assertEqual(record["cls"], "standard")
        self.assertEqual(record["reason"], "quota")

    def test_sanitizes_cleanly_end_to_end(self):
        result = {
            "status": "ok",
            "answer": "leaked prompt text should never appear",
            "session_id": "sess-1",
            "reason": None,
            "exit_code": 0,
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        sanitized = agent_exec.sanitize_telemetry_record(record)
        self.assertIsNotNone(sanitized)
        self.assertNotIn("answer", sanitized)
        self.assertNotIn("session_id", sanitized)
        self.assertEqual(sanitized["executor"], "copilot")
        self.assertEqual(sanitized["cls"], "light")


class TelemetryAppendTests(unittest.TestCase):
    def _cfg(self, tmpdir, enabled):
        return {"telemetry": {"enabled": enabled, "dir": tmpdir}}

    def test_noop_when_disabled(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = self._cfg(tmpdir, False)
            agent_exec.telemetry_append({"event": "dispatch", "status": "ok"}, cfg)
            path = os.path.join(tmpdir, "records.jsonl")
            self.assertFalse(os.path.exists(path))

    def test_writes_when_enabled(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = self._cfg(tmpdir, True)
            agent_exec.telemetry_append({"event": "dispatch", "status": "ok"}, cfg)
            path = os.path.join(tmpdir, "records.jsonl")
            self.assertTrue(os.path.exists(path))
            with open(path, "r", encoding="utf-8") as f:
                lines = [line for line in f if line.strip()]
            self.assertEqual(len(lines), 1)
            record = json.loads(lines[0])
            self.assertEqual(record["event"], "dispatch")
            self.assertEqual(record["status"], "ok")

    def test_invalid_record_is_skipped(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = self._cfg(tmpdir, True)
            agent_exec.telemetry_append({"event": "not-a-real-event"}, cfg)
            path = os.path.join(tmpdir, "records.jsonl")
            self.assertFalse(os.path.exists(path))

    def test_never_raises_on_bad_cfg(self):
        # cfg is not a dict at all -> must not raise.
        try:
            agent_exec.telemetry_append({"event": "dispatch"}, None)
            agent_exec.telemetry_append({"event": "dispatch"}, "not-a-dict")
        except Exception as e:  # pragma: no cover - failure path
            self.fail("telemetry_append raised: %r" % e)

    def test_size_cap_enforced(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = self._cfg(tmpdir, True)
            path = os.path.join(tmpdir, "records.jsonl")
            os.makedirs(tmpdir, exist_ok=True)
            with open(path, "w", encoding="utf-8") as f:
                for i in range(10000):
                    f.write(json.dumps({"event": "dispatch", "task_count": i}) + "\n")
            agent_exec.telemetry_append({"event": "dispatch", "status": "ok"}, cfg)
            with open(path, "r", encoding="utf-8") as f:
                lines = [line for line in f if line.strip()]
            self.assertEqual(len(lines), 10000)
            last = json.loads(lines[-1])
            self.assertEqual(last["status"], "ok")


class SetTelemetryEnabledInTextTests(unittest.TestCase):
    def test_create_from_scratch(self):
        out = agent_exec.set_telemetry_enabled_in_text(None, True)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)

    def test_existing_content_preserves_comment_and_sibling_block(self):
        text = (
            "# my personal orchestra config\n"
            "tiers:\n"
            "  light: haiku\n"
            "  standard: sonnet\n"
            "\n"
            "telemetry:\n"
            "  enabled: false\n"
        )
        out = agent_exec.set_telemetry_enabled_in_text(text, True)
        self.assertIn("# my personal orchestra config", out)
        self.assertIn("tiers:\n  light: haiku\n  standard: sonnet\n", out)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)

    def test_inserts_enabled_when_block_has_no_child(self):
        text = "telemetry:\n  dir: ~/.claude/orchestra/telemetry\n"
        out = agent_exec.set_telemetry_enabled_in_text(text, True)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)
        self.assertEqual(data["telemetry"]["dir"], "~/.claude/orchestra/telemetry")

    def test_trailing_comment_on_enabled_line_preserved(self):
        text = "telemetry:\n  enabled: false   # keep me\n"
        out = agent_exec.set_telemetry_enabled_in_text(text, True)
        self.assertIn("# keep me", out)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)

    def test_disable_flips_true_to_false(self):
        text = "telemetry:\n  enabled: true\n"
        out = agent_exec.set_telemetry_enabled_in_text(text, False)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], False)

    def test_enable_twice_is_idempotent(self):
        text = "telemetry:\n  enabled: false\n"
        once = agent_exec.set_telemetry_enabled_in_text(text, True)
        twice = agent_exec.set_telemetry_enabled_in_text(once, True)
        self.assertEqual(once, twice)
        import yaml
        data = yaml.safe_load(twice)
        self.assertIs(data["telemetry"]["enabled"], True)
        self.assertEqual(twice.count("enabled:"), 1)

    def test_telemetry_only_in_comment_or_indented_is_not_treated_as_block(self):
        text = (
            "# telemetry: this is just a comment mentioning telemetry:\n"
            "other:\n"
            "  telemetry: not-a-real-block\n"
        )
        out = agent_exec.set_telemetry_enabled_in_text(text, True)
        self.assertIn("# telemetry: this is just a comment mentioning telemetry:", out)
        self.assertIn("  telemetry: not-a-real-block", out)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)
        # the fake indented "telemetry" under other: must remain untouched
        self.assertEqual(data["other"]["telemetry"], "not-a-real-block")


class TelemetryEnableDisableCommandTests(unittest.TestCase):
    def test_command_creates_user_scope_file(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            fake_home_claude = os.path.join(tmpdir, "home", ".claude")
            orig_expanduser = os.path.expanduser

            def fake_expanduser(p):
                if p == "~/.claude":
                    return fake_home_claude
                return orig_expanduser(p)

            agent_exec.os.path.expanduser = fake_expanduser
            try:
                rc = agent_exec._cmd_telemetry_toggle(True, [])
            finally:
                agent_exec.os.path.expanduser = orig_expanduser

            self.assertEqual(rc, 0)
            target = os.path.join(fake_home_claude, "orchestra.yaml")
            self.assertTrue(os.path.isfile(target))
            with open(target, "r", encoding="utf-8") as f:
                import yaml
                data = yaml.safe_load(f.read())
            self.assertIs(data["telemetry"]["enabled"], True)

    def test_command_rejects_bad_scope(self):
        rc = agent_exec._cmd_telemetry_toggle(True, ["--scope", "bogus"])
        self.assertEqual(rc, 2)


if __name__ == "__main__":
    unittest.main()
