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
PATH = "/definitely-missing/orchestra-contract.md"
PATH_FP = agent_exec._path_fingerprint_from_text(PATH)


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

    def test_stripped_trailing_marker_is_unmeasured(self):
        task = "Print the single word PROBEOK and nothing else."
        lines = [event({"type": "user_message", "message": task}), usage(20)]
        found, total, count = agent_exec.parse_codex_rollout_lines(lines, {CORR})
        self.assertEqual(found, set())
        self.assertEqual(total["input_tokens"], 20)
        self.assertEqual(count, 1)

    def test_path_matches_without_marker(self):
        lines = [
            event({"type": "user_message", "message": "Read " + PATH}),
            usage(21),
        ]
        found, total, count = agent_exec.parse_codex_rollout_lines(
            lines, {CORR}, {CORR: [PATH_FP]})
        self.assertEqual(found, {CORR})
        self.assertEqual(total["input_tokens"], 21)
        self.assertEqual(count, 1)

    def test_unknown_path_and_marker_are_unmeasured(self):
        found, total, count = agent_exec.parse_codex_rollout_lines(
            [event({"type": "user_message", "message": "Read /unknown/file.md"}),
             usage(22)],
            {CORR}, {CORR: [PATH_FP]})
        self.assertEqual(found, set())
        self.assertIsNotNone(total)
        self.assertEqual(count, 1)

    def test_front_marker_matches_and_counts_usage(self):
        task = "Print the single word PROBETWO and nothing else."
        lines = [
            event({"type": "user_message", "message": "[orchestra-run-correlation: " + CORR + "]\n" + task}),
            usage(30),
        ]
        found, total, count = agent_exec.parse_codex_rollout_lines(lines, {CORR})
        self.assertEqual(found, {CORR})
        self.assertEqual(total["input_tokens"], 30)
        self.assertEqual(count, 1)

    def test_marker_matches_without_path(self):
        found, total, count = agent_exec.parse_codex_rollout_lines(
            [event({"type": "user_message", "message": CORR}), usage(23)],
            {CORR}, {CORR: [PATH_FP]})
        self.assertEqual(found, {CORR})
        self.assertEqual(total["input_tokens"], 23)
        self.assertEqual(count, 1)

    def test_path_and_marker_match_once(self):
        found, total, count = agent_exec.parse_codex_rollout_lines(
            [event({"type": "user_message", "message": CORR + " " + PATH}), usage(24)],
            {CORR}, {CORR: [PATH_FP]})
        self.assertEqual(found, {CORR})
        self.assertEqual(total["input_tokens"], 24)
        self.assertEqual(count, 1)

    def test_path_extraction_trims_filename_punctuation(self):
        for suffix in (".", ",", ")", "]", "'", '"', "`", ""):
            with self.subTest(suffix=suffix):
                found, _, _ = agent_exec.parse_codex_rollout_lines(
                    [event({"type": "user_message", "message": "`(" + PATH + suffix + ")`"}),
                     usage(1)],
                    {CORR}, {CORR: [PATH_FP]})
                self.assertEqual(found, {CORR})
        for message in ("relative/path.md", "contract.md"):
            found, _, _ = agent_exec.parse_codex_rollout_lines(
                [event({"type": "user_message", "message": message}), usage(1)],
                {CORR}, {CORR: [PATH_FP]})
            self.assertEqual(found, set())

    def test_matching_does_not_touch_missing_paths(self):
        path = "/no/such/file-" + "a" * 20 + ".md"
        fp = agent_exec._path_fingerprint_from_text(path)
        with tempfile.TemporaryDirectory() as root:
            rollout = os.path.join(root, "rollout.jsonl")
            with open(rollout, "w", encoding="utf-8") as f:
                f.write(event({"type": "user_message", "message": path}) + "\n" + usage(25))
            self.assertEqual(
                agent_exec.match_codex_rollouts(root, {CORR: [fp]})[CORR]["input_tokens"], 25)

    def test_different_paths_do_not_match(self):
        path_a = "/no/such/contract-a.md"
        path_b = "/no/such/contract-b.md"
        with tempfile.TemporaryDirectory() as root:
            rollout = os.path.join(root, "rollout.jsonl")
            with open(rollout, "w", encoding="utf-8") as f:
                f.write(event({"type": "user_message", "message": path_a}) + "\n" + usage(26))
            self.assertEqual(
                agent_exec.match_codex_rollouts(
                    root, {"a": [agent_exec._path_fingerprint_from_text(path_a)],
                           "b": [agent_exec._path_fingerprint_from_text(path_b)]}),
                {"a": {"input_tokens": 26, "output_tokens": 4, "cached_input_tokens": 2,
                       "cache_write_input_tokens": 3, "reasoning_output_tokens": 5}})

    def test_ambiguous_path_rollout_is_attributed_once(self):
        dispatches = {
            "oxc-000000000001": [PATH_FP],
            "oxc-000000000002": [PATH_FP],
        }
        with tempfile.TemporaryDirectory() as root:
            rollout = os.path.join(root, "rollout.jsonl")
            with open(rollout, "w", encoding="utf-8") as f:
                f.write(event({"type": "user_message", "message": PATH}) + "\n" + usage(27))
            measured = agent_exec.match_codex_rollouts(root, dispatches)
            self.assertEqual(set(measured), {"oxc-000000000002"})
            self.assertEqual(measured["oxc-000000000002"]["input_tokens"], 27)

    def test_marker_matches_anywhere_in_body(self):
        for message in (
            CORR + "\nbody",
            "body\n" + CORR + "\nmore",
            "body\n" + CORR,
        ):
            with self.subTest(message=message):
                found, _, _ = agent_exec.parse_codex_rollout_lines(
                    [event({"type": "user_message", "message": message}), usage(1)],
                    {CORR},
                )
                self.assertEqual(found, {CORR})

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
