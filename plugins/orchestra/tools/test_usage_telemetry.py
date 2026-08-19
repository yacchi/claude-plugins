# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for usage telemetry schema v2 in agent_exec."""

import json
import unittest

import agent_exec


class TestUsageTelemetrySanitization(unittest.TestCase):
    """Test sanitize_telemetry_record usage key validation."""

    def test_allowlisted_usage_subkeys_survive(self):
        """Each allowlisted usage key passes through _sanitize_count_dict."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": 100,
                "output_tokens": 50,
                "cached_input_tokens": 25,
                "aiu_nano": 1000000,
                "premium_requests": 5,
                "api_duration_ms": 2500,
                "session_duration_ms": 3000,
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertIsNotNone(out)
        self.assertIn("usage", out)
        usage = out["usage"]
        self.assertEqual(usage["input_tokens"], 100)
        self.assertEqual(usage["output_tokens"], 50)
        self.assertEqual(usage["cached_input_tokens"], 25)
        self.assertEqual(usage["aiu_nano"], 1000000)
        self.assertEqual(usage["premium_requests"], 5)
        self.assertEqual(usage["api_duration_ms"], 2500)
        self.assertEqual(usage["session_duration_ms"], 3000)

    def test_unknown_usage_subkeys_dropped(self):
        """Unknown subkeys in usage dict are silently dropped."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": 100,
                "unknown_key": 999,
                "another_unknown": "value",
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        usage = out["usage"]
        self.assertEqual(usage["input_tokens"], 100)
        self.assertNotIn("unknown_key", usage)
        self.assertNotIn("another_unknown", usage)

    def test_bool_usage_values_rejected(self):
        """Bool True/False are rejected (bool is subclass of int)."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": True,
                "output_tokens": False,
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        usage = out.get("usage")
        if usage:
            self.assertNotIn("input_tokens", usage)
            self.assertNotIn("output_tokens", usage)

    def test_negative_usage_values_rejected(self):
        """Negative integers are rejected."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": -100,
                "output_tokens": 50,
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        usage = out.get("usage")
        self.assertIn("output_tokens", usage)
        self.assertNotIn("input_tokens", usage)

    def test_float_usage_values_rejected(self):
        """Float values are rejected."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": 100.5,
                "output_tokens": 50,
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        usage = out.get("usage")
        self.assertIn("output_tokens", usage)
        self.assertNotIn("input_tokens", usage)

    def test_numeric_string_usage_values_rejected(self):
        """Numeric strings are rejected (must be int type)."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": "100",
                "output_tokens": 50,
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        usage = out.get("usage")
        self.assertIn("output_tokens", usage)
        self.assertNotIn("input_tokens", usage)

    def test_none_usage_values_rejected(self):
        """None values are rejected."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": None,
                "output_tokens": 50,
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        usage = out.get("usage")
        self.assertIn("output_tokens", usage)
        self.assertNotIn("input_tokens", usage)

    def test_nested_dict_in_usage_rejected(self):
        """Nested dicts in usage (except tokens) are rejected."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": 100,
                "nested": {"key": "value"},
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        usage = out.get("usage")
        self.assertIn("input_tokens", usage)
        self.assertNotIn("nested", usage)

    def test_list_in_usage_rejected(self):
        """Lists in usage are rejected."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": 100,
                "list_key": [1, 2, 3],
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        usage = out.get("usage")
        self.assertIn("input_tokens", usage)
        self.assertNotIn("list_key", usage)

    def test_usage_non_dict_rejected(self):
        """When usage is not a dict, it is rejected."""
        for bad_usage in ["string", 123, True, [1, 2, 3], None]:
            raw = {
                "event": "dispatch",
                "usage": bad_usage,
            }
            out = agent_exec.sanitize_telemetry_record(raw)
            self.assertNotIn("usage", out, f"usage={bad_usage} should not be in output")

    def test_all_dropped_usage_omits_key(self):
        """When all usage values are dropped, usage key is omitted entirely."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": True,  # rejected
                "output_tokens": -50,  # rejected
                "unknown_key": 999,  # unknown, dropped
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertNotIn("usage", out)

    def test_empty_usage_dict_omits_key(self):
        """Empty usage dict omits the usage key entirely."""
        raw = {
            "event": "dispatch",
            "usage": {},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertNotIn("usage", out)

    def test_free_text_smuggling_blocked_in_usage_dict(self):
        """Free text cannot be smuggled into usage as a value."""
        raw = {
            "event": "dispatch",
            "usage": {
                "input_tokens": 100,
                "suspicious_path": "/etc/passwd",
                "model_name": "gpt-5",
                "task_description": "do something dangerous",
            },
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        serialized = json.dumps(out, ensure_ascii=False)
        # Verify dangerous strings never appear in serialized record.
        self.assertNotIn("/etc/passwd", serialized)
        self.assertNotIn("do something dangerous", serialized)

    def test_schema_version_stamped_2(self):
        """schema_version is stamped 2 regardless of caller input."""
        raw = {
            "event": "dispatch",
            "schema_version": 999,
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["schema_version"], 2)


class TestBuildDispatchRecordUsageFlattening(unittest.TestCase):
    """Test build_dispatch_record usage flattening."""

    def test_flatten_top_level_usage_keys(self):
        """Top-level usage keys are copied to record."""
        result = {
            "status": "ok",
            "reason": None,
            "usage": {
                "premium_requests": 5,
                "api_duration_ms": 2500,
                "session_duration_ms": 3000,
                "aiu_nano": 1000000,
            },
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        self.assertIn("usage", record)
        usage = record["usage"]
        self.assertEqual(usage["premium_requests"], 5)
        self.assertEqual(usage["api_duration_ms"], 2500)
        self.assertEqual(usage["session_duration_ms"], 3000)
        self.assertEqual(usage["aiu_nano"], 1000000)

    def test_flatten_tokens_from_nested_dict(self):
        """Tokens are flattened from nested usage['tokens'] dict."""
        result = {
            "status": "ok",
            "reason": None,
            "usage": {
                "premium_requests": 5,
                "tokens": {
                    "input_tokens": 100,
                    "output_tokens": 50,
                    "cached_input_tokens": 25,
                },
            },
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        usage = record["usage"]
        self.assertEqual(usage["premium_requests"], 5)
        self.assertEqual(usage["input_tokens"], 100)
        self.assertEqual(usage["output_tokens"], 50)
        self.assertEqual(usage["cached_input_tokens"], 25)

    def test_omit_usage_when_absent(self):
        """usage key is omitted when result has no usage."""
        result = {
            "status": "ok",
            "reason": None,
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        self.assertNotIn("usage", record)

    def test_omit_usage_when_empty_dict(self):
        """usage key is omitted when all values are filtered out."""
        result = {
            "status": "ok",
            "reason": None,
            "usage": {},
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        self.assertNotIn("usage", record)

    def test_omit_usage_when_only_invalid_values(self):
        """usage key is omitted when all values fail validation."""
        result = {
            "status": "ok",
            "reason": None,
            "usage": {
                "premium_requests": -5,  # negative
                "api_duration_ms": True,  # bool
                "unknown_key": 999,  # unknown
            },
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        self.assertNotIn("usage", record)

    def test_never_copy_answer(self):
        """answer field is never copied to record."""
        result = {
            "status": "ok",
            "reason": None,
            "answer": "This is a dangerous payload",
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        self.assertNotIn("answer", record)
        serialized = json.dumps(record, ensure_ascii=False)
        self.assertNotIn("dangerous payload", serialized)

    def test_record_contains_core_fields(self):
        """Core record fields (event, executor, status, reason, resumed) are always present."""
        result = {
            "status": "ok",
            "reason": None,
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        self.assertEqual(record["event"], "dispatch")
        self.assertEqual(record["executor"], "copilot")
        self.assertEqual(record["status"], "ok")
        self.assertIsNone(record["reason"])
        self.assertFalse(record["resumed"])
        self.assertEqual(record["cls"], "light")

    def test_usage_with_valid_and_invalid_mixed(self):
        """Mixed valid/invalid usage values: valid ones pass through."""
        result = {
            "status": "ok",
            "reason": None,
            "usage": {
                "premium_requests": 5,
                "api_duration_ms": -100,  # invalid
                "session_duration_ms": 3000,
                "tokens": {
                    "input_tokens": 100,
                    "output_tokens": True,  # invalid
                    "cached_input_tokens": 25,
                },
            },
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        usage = record["usage"]
        self.assertEqual(usage["premium_requests"], 5)
        self.assertNotIn("api_duration_ms", usage)
        self.assertEqual(usage["session_duration_ms"], 3000)
        self.assertEqual(usage["input_tokens"], 100)
        self.assertNotIn("output_tokens", usage)
        self.assertEqual(usage["cached_input_tokens"], 25)

    def test_zero_values_allowed(self):
        """Zero is a valid nonneg int value."""
        result = {
            "status": "ok",
            "reason": None,
            "usage": {
                "premium_requests": 0,
                "api_duration_ms": 0,
                "tokens": {
                    "input_tokens": 0,
                },
            },
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        usage = record["usage"]
        self.assertEqual(usage["premium_requests"], 0)
        self.assertEqual(usage["api_duration_ms"], 0)
        self.assertEqual(usage["input_tokens"], 0)


class TestSchemaVersionV2(unittest.TestCase):
    """Test schema_version is consistently v2."""

    def test_sanitize_stamps_v2_overriding_caller(self):
        """Caller-supplied schema_version is ignored, v2 is stamped."""
        for bad_version in [1, 3, 999, "invalid"]:
            raw = {
                "event": "run_summary",
                "schema_version": bad_version,
            }
            out = agent_exec.sanitize_telemetry_record(raw)
            self.assertEqual(out["schema_version"], 2)

    def test_v2_in_dispatch_record_after_sanitize(self):
        """Dispatch records sanitize to v2."""
        result = {"status": "ok", "reason": None}
        record = agent_exec.build_dispatch_record("copilot", result, None, None)
        sanitized = agent_exec.sanitize_telemetry_record(record)
        self.assertEqual(sanitized["schema_version"], 2)


if __name__ == "__main__":
    unittest.main()
