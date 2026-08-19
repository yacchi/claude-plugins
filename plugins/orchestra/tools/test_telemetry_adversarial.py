# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Adversarial tests for the anonymized telemetry gate in agent_exec.py.

These tests specifically try to break the "structurally impossible for free
text to reach stored telemetry records" guarantee: disguised free text in
enum fields, bool/int confusion, regex anchoring bypasses, numeric-type
confusion, nested-dict key/value injection, stamp-field spoofing, arbitrary
top-level sensitive keys, and non-raising behavior on malformed input.

Run with: uv run test_telemetry_adversarial.py
"""

import importlib
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
agent_exec = importlib.import_module("agent_exec")

sanitize_telemetry_record = agent_exec.sanitize_telemetry_record
telemetry_append = agent_exec.telemetry_append
build_dispatch_record = agent_exec.build_dispatch_record


SENSITIVE_TOP_LEVEL_KEYS = (
    "prompt",
    "file",
    "path",
    "cwd",
    "argv",
    "error",
    "summary",
    "task_id",
    "stack",
)


class DisguisedFreeTextInEnumFields(unittest.TestCase):
    def test_reason_with_embedded_path_is_dropped(self):
        raw = {"event": "dispatch", "reason": "quota: /Users/x/secret.py"}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("reason", out)

    def test_status_with_embedded_path_is_dropped(self):
        raw = {"event": "dispatch", "status": "ok because /etc/passwd"}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("status", out)

    def test_executor_with_embedded_path_is_dropped(self):
        raw = {"event": "dispatch", "executor": "claude /home/u/proj"}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("executor", out)


class BoolIntConfusion(unittest.TestCase):
    def test_resumed_int_one_is_dropped(self):
        raw = {"event": "dispatch", "resumed": 1}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("resumed", out)

    def test_resumed_int_zero_is_dropped(self):
        raw = {"event": "dispatch", "resumed": 0}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("resumed", out)

    def test_resumed_bool_true_survives(self):
        raw = {"event": "dispatch", "resumed": True}
        out = sanitize_telemetry_record(raw)
        self.assertIs(out.get("resumed"), True)

    def test_resumed_bool_false_survives(self):
        raw = {"event": "dispatch", "resumed": False}
        out = sanitize_telemetry_record(raw)
        self.assertIn("resumed", out)
        self.assertIs(out.get("resumed"), False)


class VersionRegexAnchoring(unittest.TestCase):
    def test_version_with_trailing_shell_injection_dropped(self):
        raw = {"event": "dispatch", "orchestra_version": "1.2.3; cat /etc/passwd"}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("orchestra_version", out)

    def test_version_with_embedded_newline_path_dropped(self):
        raw = {"event": "dispatch", "orchestra_version": "1.2.3\n/evil/path"}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("orchestra_version", out)

    def test_version_with_leading_garbage_dropped(self):
        raw = {"event": "dispatch", "orchestra_version": "x1.2.3"}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("orchestra_version", out)

    def test_version_with_trailing_garbage_dropped(self):
        raw = {"event": "dispatch", "orchestra_version": "1.2.3extra"}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("orchestra_version", out)

    def test_clean_version_survives(self):
        raw = {"event": "dispatch", "orchestra_version": "1.2.3"}
        out = sanitize_telemetry_record(raw)
        self.assertEqual(out.get("orchestra_version"), "1.2.3")


class NumericFieldTypeConfusion(unittest.TestCase):
    def test_task_count_string_injection_dropped(self):
        raw = {"event": "dispatch", "task_count": "5; rm -rf"}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("task_count", out)

    def test_task_count_float_dropped(self):
        raw = {"event": "dispatch", "task_count": 3.5}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("task_count", out)

    def test_pass_negative_dropped(self):
        raw = {"event": "dispatch", "pass": -1}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("pass", out)


class NestedDictFiltering(unittest.TestCase):
    def test_classes_bad_subkey_and_bad_value_dropped(self):
        raw = {
            "event": "dispatch",
            "classes": {"light": 5, "../../etc/passwd": 9, "deep": "/path"},
        }
        out = sanitize_telemetry_record(raw)
        self.assertEqual(out.get("classes"), {"light": 5})

    def test_rounds_bad_keys_dropped(self):
        raw = {"event": "dispatch", "rounds": {"1": 2, "abc": 3, "-1": 4}}
        out = sanitize_telemetry_record(raw)
        self.assertEqual(out.get("rounds"), {"1": 2})

    def test_external_enabled_bad_subkey_and_int_value_dropped(self):
        raw = {
            "event": "dispatch",
            "external_enabled": {"copilot": True, "codex": 1, "evil": True},
        }
        out = sanitize_telemetry_record(raw)
        self.assertEqual(out.get("external_enabled"), {"copilot": True})


class StampFieldsCannotBeSpoofed(unittest.TestCase):
    def test_ts_os_schema_version_are_real_not_caller_supplied(self):
        raw = {
            "event": "dispatch",
            "ts": "EVIL",
            "os": "/etc/x",
            "schema_version": 999,
        }
        out = sanitize_telemetry_record(raw)
        self.assertNotEqual(out.get("ts"), "EVIL")
        self.assertNotEqual(out.get("os"), "/etc/x")
        self.assertEqual(out.get("schema_version"), 2)
        # ts should parse as a real ISO-8601 timestamp
        from datetime import datetime

        datetime.fromisoformat(out["ts"])
        # os should be the real platform string
        import platform

        self.assertEqual(out.get("os"), platform.system().lower())


class ArbitrarySensitiveTopLevelKeys(unittest.TestCase):
    def test_sensitive_keys_never_appear(self):
        raw = {"event": "dispatch"}
        for k in SENSITIVE_TOP_LEVEL_KEYS:
            raw[k] = "/Users/secret/leak-%s" % k
        out = sanitize_telemetry_record(raw)
        for k in SENSITIVE_TOP_LEVEL_KEYS:
            self.assertNotIn(k, out, "sensitive key %r leaked into record" % k)


class EventValidation(unittest.TestCase):
    def test_missing_event_rejects_whole_record(self):
        raw = {"status": "ok"}
        self.assertIsNone(sanitize_telemetry_record(raw))

    def test_bogus_event_rejects_whole_record(self):
        raw = {"event": "dispatch; rm"}
        self.assertIsNone(sanitize_telemetry_record(raw))


class BuildDispatchRecordNeverLeaksAnswer(unittest.TestCase):
    def test_answer_field_never_copied(self):
        result = {
            "status": "ok",
            "answer": "the secret file is at /Users/x/.ssh/id_rsa",
            "session_id": "abc123",
            "reason": None,
            "exit_code": 0,
        }
        record = build_dispatch_record("copilot", result, resume=None, cls=None)
        self.assertNotIn("answer", record)
        allowed = {"event", "executor", "status", "reason", "resumed", "cls"}
        self.assertTrue(set(record.keys()).issubset(allowed))

    def test_answer_field_never_copied_with_cls_and_resume(self):
        result = {
            "status": "unavailable",
            "answer": "/etc/passwd contents here",
            "session_id": "s2",
            "reason": "quota",
            "exit_code": 1,
        }
        record = build_dispatch_record(
            "copilot", result, resume="sess-123", cls="standard"
        )
        self.assertNotIn("answer", record)
        self.assertEqual(
            set(record.keys()),
            {"event", "executor", "status", "reason", "resumed", "cls"},
        )


class TelemetryAppendGating(unittest.TestCase):
    def test_disabled_writes_nothing(self):
        with tempfile.TemporaryDirectory() as tmp:
            telemetry_dir = os.path.join(tmp, "telemetry")
            cfg = {"telemetry": {"enabled": False, "dir": telemetry_dir}}
            telemetry_append({"event": "dispatch", "status": "ok"}, cfg)
            path = os.path.join(telemetry_dir, "records.jsonl")
            self.assertFalse(os.path.exists(path))
            self.assertFalse(os.path.isdir(telemetry_dir))

    def test_enabled_writes_exactly_one_sanitized_line(self):
        with tempfile.TemporaryDirectory() as tmp:
            telemetry_dir = os.path.join(tmp, "telemetry")
            cfg = {"telemetry": {"enabled": True, "dir": telemetry_dir}}
            raw = {
                "event": "dispatch",
                "status": "ok",
                "prompt": "/Users/secret/leak",
            }
            telemetry_append(raw, cfg)
            path = os.path.join(telemetry_dir, "records.jsonl")
            self.assertTrue(os.path.isfile(path))
            with open(path, "r", encoding="utf-8") as f:
                lines = [l for l in f.read().splitlines() if l.strip()]
            self.assertEqual(len(lines), 1)
            record = json.loads(lines[0])
            self.assertNotIn("prompt", record)
            self.assertEqual(record.get("status"), "ok")

    def test_never_touches_real_home(self):
        # Regression guard: ensure the default telemetry dir constant is not
        # accidentally used when a cfg explicitly disables telemetry.
        home = os.path.expanduser("~")
        default_dir = os.path.expanduser(agent_exec.DEFAULTS["telemetry"]["dir"])
        self.assertTrue(default_dir.startswith(home))
        cfg = {"telemetry": {"enabled": False}}
        # Should be a no-op; if it wrote anywhere under home this would be a
        # privacy incident, so we only assert it does not raise and that we
        # never call telemetry_append with enabled True against the real
        # default dir in this suite.
        telemetry_append({"event": "dispatch"}, cfg)


class MalformedInputNeverRaises(unittest.TestCase):
    def test_classes_as_list_does_not_raise(self):
        raw = {"event": "dispatch", "classes": []}
        out = sanitize_telemetry_record(raw)
        self.assertNotIn("classes", out)

    def test_raw_none_does_not_raise(self):
        self.assertIsNone(sanitize_telemetry_record(None))

    def test_raw_list_does_not_raise(self):
        self.assertIsNone(sanitize_telemetry_record([]))

    def test_deeply_nested_junk_does_not_raise(self):
        raw = {
            "event": "dispatch",
            "classes": {"light": {"nested": "junk"}},
            "rounds": {"1": [1, 2, 3]},
            "external_enabled": {"copilot": {"nested": True}},
            "reason": {"nested": "dict"},
            "resumed": {"nested": "dict"},
            "orchestra_version": 12345,
            "task_count": {"nested": "dict"},
        }
        try:
            out = sanitize_telemetry_record(raw)
        except Exception as e:  # pragma: no cover - must never happen
            self.fail("sanitize_telemetry_record raised: %r" % e)
        self.assertIsInstance(out, dict)
        for k in ("classes", "rounds", "external_enabled"):
            # nested junk values must never survive as-is
            self.assertNotEqual(out.get(k), raw.get(k))

    def test_telemetry_append_never_raises_on_garbage(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = {"telemetry": {"enabled": True, "dir": tmp}}
            try:
                telemetry_append({"event": object()}, cfg)
                telemetry_append(None, cfg)
                telemetry_append([], cfg)
                telemetry_append({"event": "dispatch", "classes": "not-a-dict"}, cfg)
            except Exception as e:  # pragma: no cover
                self.fail("telemetry_append raised: %r" % e)


if __name__ == "__main__":
    unittest.main(verbosity=2)
