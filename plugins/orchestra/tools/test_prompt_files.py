# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for repeated disk-resident prompt files."""

import contextlib
import io
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402


class PromptFileTests(unittest.TestCase):
    def _files(self, *contents):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        paths = []
        for index, content in enumerate(contents):
            path = os.path.join(directory.name, str(index))
            with open(path, "w", encoding="utf-8", newline="") as fh:
                fh.write(content)
            paths.append(path)
        return paths

    def test_single_file_is_byte_identical(self):
        path = self._files("one\n\n")[0]
        self.assertEqual(agent_exec.read_prompt_files([path]), "one\n\n")

    def test_two_files_are_joined_in_order(self):
        paths = self._files("one", "two")
        self.assertEqual(agent_exec.read_prompt_files(paths), "one\n\ntwo")

    def test_three_files_normalize_boundaries(self):
        paths = self._files("one", "two\n\n\n", "three")
        self.assertEqual(agent_exec.read_prompt_files(paths), "one\n\ntwo\n\nthree")

    def test_utf8_payload_survives(self):
        payload = '日本語 "quotes" and \\\\slashes'
        self.assertEqual(agent_exec.read_prompt_files(self._files(payload)), payload)

    def test_empty_middle_file_does_not_add_extra_blank_lines(self):
        self.assertEqual(
            agent_exec.read_prompt_files(self._files("one", "", "three")),
            "one\n\nthree",
        )

    def test_missing_file_names_path_and_returns_usage_error(self):
        path = os.path.join(tempfile.gettempdir(), "agent-exec-missing-prompt")
        with self.assertRaisesRegex(ValueError, path):
            agent_exec.read_prompt_files([path])
        err = io.StringIO()
        out = io.StringIO()
        with contextlib.redirect_stderr(err), contextlib.redirect_stdout(out):
            code = agent_exec.cmd_dispatch_route(
                ["--class", "light", "--prompt-file", path, "--workdir", "."]
            )
        self.assertEqual(code, 2)
        self.assertIn(path, err.getvalue())
        self.assertEqual(out.getvalue(), "")

    def test_unreadable_file_returns_usage_error(self):
        path = self._files("valid")[0]
        os.unlink(path)
        os.mkdir(path)
        self.addCleanup(os.rmdir, path)
        err = io.StringIO()
        out = io.StringIO()
        with contextlib.redirect_stderr(err), contextlib.redirect_stdout(out):
            code = agent_exec.cmd_dispatch_route(
                ["--class", "light", "--prompt-file", path, "--workdir", "."]
            )
        self.assertEqual(code, 2)
        self.assertIn(path, err.getvalue())
        self.assertEqual(out.getvalue(), "")

    def test_dispatch_without_prompt_file_is_usage_error(self):
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            code = agent_exec.cmd_dispatch_route(["--class", "light", "--workdir", "."])
        self.assertEqual(code, 2)
        self.assertIn("missing required option: --prompt-file", err.getvalue())

    def test_removed_inline_symbol_is_absent_from_source(self):
        with open(agent_exec.__file__, encoding="utf-8") as fh:
            self.assertNotIn("prompt_" + "b64", fh.read())


if __name__ == "__main__":
    unittest.main()
