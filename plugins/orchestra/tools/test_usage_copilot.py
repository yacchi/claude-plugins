# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for Copilot usage and OpenTelemetry parsing."""

import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402


def event(**kwargs):
    return json.dumps(kwargs)


class CopilotJsonlUsageTests(unittest.TestCase):
    def parse(self, lines):
        return agent_exec.parse_copilot_jsonl("\n".join(lines), "", 0)

    def test_result_usage_and_last_checkpoint(self):
        result = self.parse([
            event(type="session.usage_checkpoint", data={
                "totalNanoAiu": 1, "totalPremiumRequests": 2,
            }),
            event(type="session.usage_checkpoint", data={
                "totalNanoAiu": 358285000, "totalPremiumRequests": 4,
            }),
            event(type="result", usage={
                "premiumRequests": 0, "totalApiDurationMs": 2067,
                "sessionDurationMs": 4555, "codeChanges": {},
            }),
        ])
        self.assertEqual(result["usage"], {
            "premium_requests": 0,
            "api_duration_ms": 2067,
            "session_duration_ms": 4555,
            "aiu_nano": 358285000,
        })

    def test_result_premium_requests_wins(self):
        result = self.parse([
            event(type="session.usage_checkpoint", data={"totalPremiumRequests": 9}),
            event(type="result", usage={"premiumRequests": 1}),
        ])
        self.assertEqual(result["usage"]["premium_requests"], 1)

    def test_no_usage_is_none(self):
        self.assertIsNone(self.parse([event(type="result")])["usage"])

    def test_invalid_values_are_dropped(self):
        result = self.parse([
            event(type="result", usage={
                "premiumRequests": True, "totalApiDurationMs": -1,
                "sessionDurationMs": "5",
            }),
            event(type="session.usage_checkpoint", data={
                "totalNanoAiu": False, "totalPremiumRequests": -2,
            }),
        ])
        self.assertIsNone(result["usage"])

    def test_usage_events_do_not_change_result_or_scan(self):
        result = self.parse([
            event(type="assistant.message", data={"content": "answer"}),
            event(type="session.usage_checkpoint", data={
                "totalNanoAiu": 1, "totalPremiumRequests": 0,
            }),
        ])
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["answer"], "answer")
        self.assertIsNone(result["reason"])


class CopilotOtelTests(unittest.TestCase):
    def test_span_sums_and_metrics_are_not_double_counted(self):
        text = "\n".join([
            event(type="span", attributes={
                "gen_ai.usage.input_tokens": 2,
                "gen_ai.usage.output_tokens": 3,
                "gen_ai.usage.cache_creation.input_tokens": 5,
                "gen_ai.usage.cache_read.input_tokens": 7,
            }),
            event(type="metric", name="gen_ai.client.token.usage",
                  attributes={"gen_ai.usage.input_tokens": 100}),
            event(type="span", attributes={
                "gen_ai.usage.input_tokens": 4,
                "gen_ai.usage.output_tokens": 0,
            }),
            "garbage",
            "[]",
        ])
        self.assertEqual(agent_exec.parse_copilot_otel(text), {
            "input_tokens": 6,
            "output_tokens": 3,
            "cached_input_tokens": 12,
        })

    def test_invalid_and_empty_input(self):
        self.assertIsNone(agent_exec.parse_copilot_otel(""))
        self.assertIsNone(agent_exec.parse_copilot_otel(
            event(type="span", attributes={
                "gen_ai.usage.input_tokens": True,
                "gen_ai.usage.output_tokens": -1,
                "gen_ai.usage.cache_read.input_tokens": "4",
            })
        ))


if __name__ == "__main__":
    unittest.main()
