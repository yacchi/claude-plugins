# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for `agent-exec write-file` in agent_exec.py.

Run with: python3 -m unittest test_write_file
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))


class TestWriteFile(unittest.TestCase):
    """Tests for agent-exec write-file command."""

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="test-write-file-")

    def tearDown(self):
        if os.path.exists(self.tmpdir):
            shutil.rmtree(self.tmpdir)

    def _run_write_file(self, args, stdin_content):
        """Run agent-exec write-file with given args and stdin."""
        cmd = [sys.executable, "tools/agent_exec.py", "write-file"] + args
        result = subprocess.run(
            cmd,
            input=stdin_content,
            capture_output=True,
            text=True,
            cwd=os.path.dirname(os.path.abspath(__file__)) + "/..",
        )
        return result

    def test_basic_write(self):
        """Test basic file write without trailing newline handling."""
        path = os.path.join(self.tmpdir, "test.txt")
        content = "hello world"
        result = self._run_write_file(["--path", path], content)

        self.assertEqual(result.returncode, 0)
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), content)

        output = json.loads(result.stdout.strip())
        self.assertEqual(output["status"], "ok")
        self.assertEqual(output["path"], os.path.abspath(path))
        self.assertEqual(output["chars"], 11)

    def test_empty_stdin(self):
        """Test writing empty file."""
        path = os.path.join(self.tmpdir, "empty.txt")
        result = self._run_write_file(["--path", path], "")

        self.assertEqual(result.returncode, 0)
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), "")

        output = json.loads(result.stdout.strip())
        self.assertEqual(output["status"], "ok")
        self.assertEqual(output["chars"], 0)

    def test_trailing_newline_removed_for_count(self):
        """Test that trailing newline is removed for char count."""
        path = os.path.join(self.tmpdir, "test.txt")
        content = "abc\n"
        result = self._run_write_file(["--path", path, "--expect-chars", "3"], content)

        self.assertEqual(result.returncode, 0)
        # File should contain the newline
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), "abc\n")

        output = json.loads(result.stdout.strip())
        self.assertEqual(output["status"], "ok")
        self.assertEqual(output["chars"], 3)

    def test_no_trailing_newline(self):
        """Test file without trailing newline."""
        path = os.path.join(self.tmpdir, "test.txt")
        content = "abc"
        result = self._run_write_file(["--path", path, "--expect-chars", "3"], content)

        self.assertEqual(result.returncode, 0)
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), "abc")

        output = json.loads(result.stdout.strip())
        self.assertEqual(output["status"], "ok")
        self.assertEqual(output["chars"], 3)

    def test_mismatch_multiple_newlines(self):
        """Test mismatch with multiple trailing newlines."""
        path = os.path.join(self.tmpdir, "test.txt")
        content = "abc\n\n"
        result = self._run_write_file(["--path", path, "--expect-chars", "3"], content)

        self.assertEqual(result.returncode, 1)
        # File should not be created
        self.assertFalse(os.path.exists(path))

        output = json.loads(result.stdout.strip())
        self.assertEqual(output["status"], "mismatch")
        self.assertEqual(output["chars"], 4)
        self.assertEqual(output["expected"], 3)

    def test_mismatch_leaves_file_untouched(self):
        """Test that mismatch leaves existing file untouched."""
        path = os.path.join(self.tmpdir, "test.txt")
        # Write initial file
        with open(path, "w", encoding="utf-8") as f:
            f.write("original")

        content = "abc\n"
        result = self._run_write_file(["--path", path, "--expect-chars", "5"], content)

        self.assertEqual(result.returncode, 1)
        # File should still have original content
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), "original")

    def test_parent_dirs_created(self):
        """Test that parent directories are created."""
        path = os.path.join(self.tmpdir, "a", "b", "c", "test.txt")
        content = "test"
        result = self._run_write_file(["--path", path], content)

        self.assertEqual(result.returncode, 0)
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), "test")

    def test_special_characters(self):
        """Test content with special characters."""
        path = os.path.join(self.tmpdir, "special.txt")
        # Content with $HOME, backticks, EOF, Japanese characters
        content = "HOME=$HOME `cmd` <<EOF\n日本語\n"
        result = self._run_write_file(["--path", path], content)

        self.assertEqual(result.returncode, 0)
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), content)

    def test_crlf_preserved(self):
        """Test that CRLF is preserved exactly."""
        path = os.path.join(self.tmpdir, "crlf.txt")
        content = "line1\r\nline2\r\n"
        result = self._run_write_file(["--path", path], content)

        self.assertEqual(result.returncode, 0)
        with open(path, "rb") as f:
            self.assertEqual(f.read(), content.encode("utf-8"))

    def test_japanese_characters(self):
        """Test Japanese characters are counted correctly."""
        path = os.path.join(self.tmpdir, "japanese.txt")
        content = "日本語\n"  # 3 Japanese characters + newline
        result = self._run_write_file(["--path", path, "--expect-chars", "3"], content)

        self.assertEqual(result.returncode, 0)
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), content)

    def test_missing_path_argument(self):
        """Test missing --path argument."""
        result = self._run_write_file([], "content")
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing required option: --path", result.stderr)

    def test_missing_path_value(self):
        """Test --path without value."""
        result = self._run_write_file(["--path"], "content")
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing value for --path", result.stderr)

    def test_invalid_expect_chars(self):
        """Test invalid --expect-chars value."""
        path = os.path.join(self.tmpdir, "test.txt")
        result = self._run_write_file(["--path", path, "--expect-chars", "x"], "content")
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be an integer", result.stderr)

    def test_negative_expect_chars(self):
        """Test negative --expect-chars value."""
        path = os.path.join(self.tmpdir, "test.txt")
        result = self._run_write_file(["--path", path, "--expect-chars", "-1"], "content")
        self.assertEqual(result.returncode, 1)

    def test_unknown_flag(self):
        """Test unknown flag."""
        path = os.path.join(self.tmpdir, "test.txt")
        result = self._run_write_file(["--path", path, "--unknown"], "content")
        self.assertEqual(result.returncode, 2)
        self.assertIn("unknown option", result.stderr)

    def test_missing_expect_chars_value(self):
        """Test --expect-chars without value."""
        path = os.path.join(self.tmpdir, "test.txt")
        result = self._run_write_file(["--path", path, "--expect-chars"], "content")
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing value for --expect-chars", result.stderr)

    def test_absolute_path(self):
        """Test that output path is absolute."""
        path = "test.txt"
        full_path = os.path.join(self.tmpdir, path)
        result = self._run_write_file(["--path", full_path], "content")

        self.assertEqual(result.returncode, 0)
        output = json.loads(result.stdout.strip())
        self.assertEqual(output["path"], os.path.abspath(full_path))

    def test_large_file(self):
        """Test writing a large file."""
        path = os.path.join(self.tmpdir, "large.txt")
        # Create content with 10000 lines
        content = "line\n" * 10000
        result = self._run_write_file(["--path", path], content)

        self.assertEqual(result.returncode, 0)
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), content)

    def test_unicode_normalization(self):
        """Test that Unicode content is written exactly as received."""
        path = os.path.join(self.tmpdir, "unicode.txt")
        # Test with combining characters
        content = "é\n"  # é as e + combining acute accent
        result = self._run_write_file(["--path", path, "--expect-chars", "2"], content)

        self.assertEqual(result.returncode, 0)
        with open(path, "r", encoding="utf-8") as f:
            self.assertEqual(f.read(), content)


if __name__ == "__main__":
    unittest.main()
