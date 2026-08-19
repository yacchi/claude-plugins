# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for deterministic codex rollout correlation."""

import json
import os
import stat
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_exec  # noqa: E402


CORR = "oxc-0123456789ab"


def event(payload):
    return json.dumps({"type": "event_msg", "payload": payload})


def usage(value):
    return event({
        "type": "token_count",
        "info": {"total_token_usage": {
            "input_tokens": value, "cached_input_tokens": 2,
            "cache_write_input_tokens": 3, "output_tokens": 4,
            "reasoning_output_tokens": 5, "total_tokens": 999999,
        }},
    })


class CodexCorrelationTests(unittest.TestCase):
    def test_last_usage_and_user_message_match(self):
        lines = [
            "{garbage",
            event({"type": "user_message", "message": "do " + CORR}),
            usage(10), usage(20), usage(30),
        ]
        found, total, count = agent_exec.parse_codex_rollout_lines(lines, {CORR})
        self.assertEqual(found, {CORR})
        self.assertEqual(count, 3)
        self.assertEqual(total, {
            "input_tokens": 30, "cached_input_tokens": 2,
            "cache_write_input_tokens": 3, "output_tokens": 4,
            "reasoning_output_tokens": 5,
        })

    def test_no_tokens_and_nonmatching_message_are_unmeasured(self):
        found, total, count = agent_exec.parse_codex_rollout_lines(
            [event({"type": "user_message", "message": "other"})], {CORR})
        self.assertEqual(found, set())
        self.assertIsNone(total)
        self.assertEqual(count, 0)

    def test_best_duplicate_and_lexical_tie(self):
        with tempfile.TemporaryDirectory() as root:
            paths = [
                os.path.join(root, "a", "rollout-a.jsonl"),
                os.path.join(root, "b", "rollout-b.jsonl"),
                os.path.join(root, "c", "rollout-c.jsonl"),
            ]
            for path, lines in (
                (paths[0], [event({"type": "user_message", "message": CORR}), usage(1)]),
                (paths[1], [event({"type": "user_message", "message": CORR}), usage(2), usage(3)]),
                (paths[2], [event({"type": "user_message", "message": CORR}), usage(4), usage(5)]),
            ):
                os.makedirs(os.path.dirname(path))
                with open(path, "w", encoding="utf-8") as f:
                    f.write("\n".join(lines) + "\n")
            self.assertEqual(
                agent_exec.match_codex_rollouts(root, {CORR})[CORR]["input_tokens"], 5)

    def test_unreadable_and_old_matching_rollout_are_safe(self):
        with tempfile.TemporaryDirectory() as root:
            path = os.path.join(root, "rollout.jsonl")
            with open(path, "w", encoding="utf-8") as f:
                f.write(event({"type": "user_message", "message": CORR}) + "\n" + usage(7))
            os.chmod(path, stat.S_IRUSR)
            self.assertEqual(
                agent_exec.match_codex_rollouts(root, {CORR})[CORR]["input_tokens"], 7)


if __name__ == "__main__":
    unittest.main()
