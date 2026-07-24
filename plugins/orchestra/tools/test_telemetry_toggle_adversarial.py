# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Adversarial tests for the telemetry toggle line-oriented editor.

Run with: uv run plugins/orchestra/tools/test_telemetry_toggle_adversarial.py
"""
import importlib.util
import os
import unittest

import yaml

_HERE = os.path.dirname(os.path.abspath(__file__))
_AGENT_EXEC_PATH = os.path.join(_HERE, "agent_exec.py")
_EXAMPLE_YAML_PATH = os.path.join(_HERE, "..", "examples", "orchestra.yaml")

_spec = importlib.util.spec_from_file_location("agent_exec", _AGENT_EXEC_PATH)
agent_exec = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(agent_exec)

set_telemetry_enabled_in_text = agent_exec.set_telemetry_enabled_in_text


class RealisticConfigPreserved(unittest.TestCase):
    def test_realistic_config_preserved_on_enable(self):
        with open(_EXAMPLE_YAML_PATH, "r", encoding="utf-8") as f:
            original = f.read()

        before = yaml.safe_load(original) or {}

        text = original
        if "telemetry:" not in text or not any(
            line.rstrip("\r\n") == "telemetry:" for line in text.splitlines()
        ):
            text = text + "\ntelemetry:\n  enabled: false\n"

        out = set_telemetry_enabled_in_text(text, True)

        # Every original line OUTSIDE the telemetry block must still be present
        # byte-for-byte (the enabled: line itself is expected to flip false->true).
        out_line_set = out.splitlines()
        in_telemetry_block = False
        for line in original.splitlines():
            stripped = line.rstrip("\r\n")
            if stripped == "telemetry:":
                in_telemetry_block = True
                continue
            if in_telemetry_block:
                if stripped.strip() == "" or stripped[:1] in (" ", "\t"):
                    continue
                in_telemetry_block = False
            self.assertIn(
                line,
                out_line_set,
                "original line lost/modified: %r" % (line,),
            )

        parsed = yaml.safe_load(out)
        self.assertIsInstance(parsed, dict)
        self.assertEqual(parsed.get("tiers"), before.get("tiers"))
        self.assertEqual(
            parsed.get("external_executors"), before.get("external_executors")
        )
        self.assertEqual(parsed.get("priority"), before.get("priority"))
        self.assertEqual(parsed["telemetry"]["enabled"], True)


class NoDuplicationOverCycles(unittest.TestCase):
    def test_enable_disable_enable_no_duplicate_keys(self):
        text = "tiers:\n  light: []\ntelemetry:\n  enabled: false\nother: 1\n"
        t1 = set_telemetry_enabled_in_text(text, True)
        t2 = set_telemetry_enabled_in_text(t1, False)
        t3 = set_telemetry_enabled_in_text(t2, True)

        top_level_telemetry_count = sum(
            1 for l in t3.splitlines() if l.rstrip("\r\n") == "telemetry:"
        )
        self.assertEqual(top_level_telemetry_count, 1, "duplicate telemetry: block")

        # count "enabled:" children specifically inside the telemetry block
        lines = t3.splitlines()
        idx = lines.index("telemetry:")
        enabled_count = 0
        for l in lines[idx + 1 :]:
            if l.strip() == "" :
                continue
            if l[0] not in (" ", "\t"):
                break
            if l.strip().startswith("enabled:") or l.strip().startswith("enabled :"):
                enabled_count += 1
        self.assertEqual(enabled_count, 1, "duplicate enabled: child")

        parsed = yaml.safe_load(t3)
        self.assertEqual(parsed["telemetry"]["enabled"], True)


class BlockBoundary(unittest.TestCase):
    def test_telemetry_followed_by_another_top_level_key_not_absorbed(self):
        text = "telemetry:\n  enabled: false\ntiers:\n  light: []\n"
        out = set_telemetry_enabled_in_text(text, True)
        parsed = yaml.safe_load(out)
        self.assertEqual(parsed["tiers"], {"light": []})
        self.assertEqual(parsed["telemetry"]["enabled"], True)
        # tiers: line itself must be untouched
        self.assertIn("tiers:\n  light: []\n", out)

    def test_telemetry_in_comment_not_treated_as_block(self):
        text = "# telemetry: false\nother: 1\n"
        out = set_telemetry_enabled_in_text(text, True)
        parsed = yaml.safe_load(out)
        self.assertEqual(parsed["other"], 1)
        self.assertIn("telemetry", parsed)
        self.assertEqual(parsed["telemetry"]["enabled"], True)
        # original comment line preserved verbatim
        self.assertIn("# telemetry: false", out)

    def test_telemetry_nested_under_another_key_not_treated_as_block(self):
        text = "foo:\n  telemetry:\n    enabled: false\nbar: 1\n"
        out = set_telemetry_enabled_in_text(text, True)
        parsed = yaml.safe_load(out)
        # nested one must remain untouched (still false)
        self.assertEqual(parsed["foo"]["telemetry"]["enabled"], False)
        self.assertEqual(parsed["bar"], 1)
        # a NEW top-level telemetry block must have been appended
        self.assertIn("telemetry", parsed)
        self.assertEqual(parsed["telemetry"]["enabled"], True)


class ValueSpellings(unittest.TestCase):
    def _check(self, child_line, keep_comment=False):
        text = "telemetry:\n%s\n" % child_line
        out = set_telemetry_enabled_in_text(text, True)
        parsed = yaml.safe_load(out)
        self.assertTrue(
            parsed["telemetry"]["enabled"] is True
            or parsed["telemetry"]["enabled"] == "true",
            "expected truthy enabled for input %r, got %r"
            % (child_line, out),
        )
        if keep_comment:
            self.assertIn("# keep", out, "trailing comment lost for %r -> %r" % (child_line, out))

    def test_enabled_no(self):
        self._check("  enabled: no")

    def test_enabled_False_capitalized(self):
        self._check("  enabled: False")

    def test_enabled_quoted_false_string(self):
        self._check("  enabled: 'false'")

    def test_enabled_space_before_colon(self):
        self._check("  enabled : false")

    def test_enabled_false_with_trailing_comment(self):
        self._check("  enabled: false   # keep", keep_comment=True)


class StubCreation(unittest.TestCase):
    def test_stub_from_none(self):
        out = set_telemetry_enabled_in_text(None, True)
        parsed = yaml.safe_load(out)
        self.assertEqual(parsed["telemetry"]["enabled"], True)

    def test_stub_from_empty_string(self):
        out = set_telemetry_enabled_in_text("", True)
        parsed = yaml.safe_load(out)
        self.assertEqual(parsed["telemetry"]["enabled"], True)


class Idempotence(unittest.TestCase):
    def test_enable_twice_stable(self):
        text = "telemetry:\n  enabled: false\n"
        once = set_telemetry_enabled_in_text(text, True)
        twice = set_telemetry_enabled_in_text(once, True)
        self.assertEqual(once, twice)
        parsed = yaml.safe_load(twice)
        self.assertEqual(parsed["telemetry"]["enabled"], True)
        count = sum(1 for l in twice.splitlines() if l.rstrip() == "telemetry:")
        self.assertEqual(count, 1)


class NeverRaises(unittest.TestCase):
    def test_no_trailing_newline(self):
        text = "telemetry:\n  enabled: false"
        out = set_telemetry_enabled_in_text(text, True)
        yaml.safe_load(out)

    def test_odd_indentation(self):
        text = "telemetry:\n   enabled: false\n"
        out = set_telemetry_enabled_in_text(text, True)
        yaml.safe_load(out)

    def test_empty_string(self):
        out = set_telemetry_enabled_in_text("", False)
        yaml.safe_load(out)

    def test_whitespace_only(self):
        # The helper itself must not raise; the surviving tab/whitespace-only
        # line is caller-supplied garbage and is preserved verbatim (not the
        # toggle's job to sanitize), so we do not require the result to be
        # valid YAML here.
        out = set_telemetry_enabled_in_text("   \n\t\n", False)
        self.assertIsInstance(out, str)


if __name__ == "__main__":
    unittest.main()
