# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for `agent-exec check` in agent_exec.py.

Run with: uv run test_check_cmd.py
"""

import contextlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402

# Heartbeats are machine-shared (`~/.claude/orchestra/alive`); redirect this
# whole suite into a throwaway directory so a test run never writes into the
# real user's home (same convention as test_isolate_integrate.py).
_ALIVE_TMP = tempfile.mkdtemp(prefix="orch-alive-")
os.environ["ORCHESTRA_ALIVE_DIR"] = _ALIVE_TMP
agent_exec._heartbeat_dir_cache = _ALIVE_TMP


def _git(cwd, *args):
    return subprocess.run(
        ["git"] + list(args),
        cwd=cwd,
        capture_output=True,
        text=True,
        check=True,
        env=dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null"),
    )


class _CheckRepo(unittest.TestCase):
    """A throwaway repo, plus a helper to run `agent-exec check` in-process."""

    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = tempfile.mkdtemp(prefix="orch-check-cmd-")
        self.repo = os.path.join(self.tmp, "repo")
        os.makedirs(self.repo)
        _git(self.repo, "init", "-q", "-b", "main", ".")
        _git(self.repo, "config", "user.email", "t@example.com")
        _git(self.repo, "config", "user.name", "T")
        with open(os.path.join(self.repo, "good.py"), "w") as fh:
            fh.write("print('ok')\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "init")

        # An isolated HOME/project config dir so `resolve_config()` never
        # touches the real user's ~/.claude, and each test starts from a
        # clean 4-layer config.
        self._home = os.path.join(self.tmp, "home")
        os.makedirs(self._home)
        self._orig_home = os.environ.get("HOME")
        os.environ["HOME"] = self._home
        self._orig_cwd = os.getcwd()
        os.chdir(self.repo)

    def tearDown(self):
        os.chdir(self._orig_cwd)
        if self._orig_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = self._orig_home
        shutil.rmtree(self.tmp, ignore_errors=True)
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def _write_config(self, checks_yaml):
        directory = os.path.join(self.repo, ".claude")
        os.makedirs(directory, exist_ok=True)
        with open(os.path.join(directory, "orchestra.yaml"), "w") as fh:
            fh.write(checks_yaml)

    def _edit_file(self, rel, content):
        path = os.path.join(self.repo, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True) if os.path.dirname(path) else None
        with open(path, "w") as fh:
            fh.write(content)

    def _cli(self, *args):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.main(["check"] + list(args))
        return rc, buf.getvalue()


class UsageErrorTests(_CheckRepo):
    def test_neither_task_nor_path_is_a_usage_error(self):
        rc, out = self._cli()
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_both_task_and_path_is_a_usage_error(self):
        rc, out = self._cli("--task", "t1", "--path", self.repo)
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_json_and_text_together(self):
        rc, out = self._cli("--path", self.repo, "--json", "--text")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_duplicate_flag(self):
        rc, out = self._cli("--path", self.repo, "--path", self.repo)
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_missing_value_for_flag(self):
        rc, out = self._cli("--path")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_unknown_flag(self):
        rc, out = self._cli("--path", self.repo, "--frobnicate")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")


class DefaultConfigTests(_CheckRepo):
    def test_checks_default_present_in_resolved_config(self):
        resolved, err = agent_exec.resolve_config()
        self.assertIsNone(err)
        self.assertEqual(resolved["checks"], {"max_parallel": 2, "items": []})

    def test_no_checks_configured_is_no_checks_status_and_exit_zero(self):
        rc, out = self._cli("--path", self.repo)
        self.assertEqual(rc, 0)
        result = json.loads(out)
        self.assertEqual(result["status"], "no-checks")
        self.assertEqual(result["checks"], [])


class PathModeTests(_CheckRepo):
    def test_not_a_git_repository_is_an_error(self):
        outside = os.path.join(self.tmp, "plain")
        os.makedirs(outside)
        os.chdir(outside)
        rc, out = self._cli("--path", outside, "--repo", outside)
        self.assertEqual(rc, 3)
        self.assertEqual(json.loads(out)["status"], "error")

    def test_passing_check_over_changed_files(self):
        self._write_config(
            "checks:\n  items:\n    - name: py\n      run: \"true\"\n"
        )
        self._edit_file("good.py", "print('changed')\n")
        rc, out = self._cli("--path", self.repo)
        self.assertEqual(rc, 0)
        result = json.loads(out)
        self.assertEqual(result["status"], "pass")
        self.assertEqual([c["name"] for c in result["checks"]], ["py"])
        self.assertEqual(result["checks"][0]["status"], "pass")

    def test_failing_check_exits_one_and_reports_fail(self):
        self._write_config(
            "checks:\n  items:\n    - name: py\n      run: \"false\"\n"
        )
        self._edit_file("good.py", "print('changed')\n")
        rc, out = self._cli("--path", self.repo)
        self.assertEqual(rc, 1)
        result = json.loads(out)
        self.assertEqual(result["status"], "fail")
        self.assertEqual(result["checks"][0]["status"], "fail")

    def test_files_override_wins_over_changed_files(self):
        self._write_config(
            "checks:\n"
            "  items:\n"
            "    - name: py\n"
            "      paths: [\"*.explicit\"]\n"
            "      run: \"true\"\n"
        )
        rc, out = self._cli("--path", self.repo, "--files", "a.explicit,b.explicit")
        self.assertEqual(rc, 0)
        result = json.loads(out)
        self.assertEqual(result["files"], 2)
        self.assertEqual(result["checks"][0]["status"], "pass")

    def test_text_mode_lists_name_status_seconds_then_excerpt(self):
        self._write_config(
            "checks:\n  items:\n    - name: py\n      run: \"echo boom-marker; false\"\n"
        )
        self._edit_file("good.py", "print('changed')\n")
        rc, out = self._cli("--path", self.repo, "--text")
        self.assertEqual(rc, 1)
        lines = out.splitlines()
        self.assertTrue(lines[0].startswith("py fail"))
        self.assertIn("boom-marker", out)
        # Text mode never emits the JSON envelope.
        self.assertNotIn("{", out)


class TaskModeTests(_CheckRepo):
    def test_task_mode_runs_over_the_worktree_diff(self):
        created = agent_exec.isolate_create(self.repo, "alpha", backend="git", carry=False)
        self.assertIn(created["status"], ("created", "exists"))
        with open(os.path.join(created["path"], "good.py"), "w") as fh:
            fh.write("print('worker change')\n")
        self._write_config(
            "checks:\n  items:\n    - name: py\n      run: \"true\"\n"
        )
        rc, out = self._cli("--task", "alpha", "--repo", self.repo)
        self.assertEqual(rc, 0)
        result = json.loads(out)
        self.assertEqual(result["status"], "pass")
        self.assertEqual(result["tree"], created["path"])
        self.assertIn("good.py", " ".join(agent_exec.isolate_diff(self.repo, "alpha")["files"]))

    def test_unknown_task_is_an_error(self):
        rc, out = self._cli("--task", "ghost-task", "--repo", self.repo)
        self.assertEqual(rc, 3)
        self.assertEqual(json.loads(out)["status"], "error")


class BaselineTests(_CheckRepo):
    def test_baseline_marks_a_failure_preexisting_when_the_base_also_fails(self):
        # The check fails on HEAD already (before any worker touches anything).
        self._write_config(
            "checks:\n  items:\n    - name: py\n      run: \"false\"\n"
        )
        self._edit_file("good.py", "print('worker edit')\n")
        rc, out = self._cli("--path", self.repo, "--since", "HEAD", "--baseline")
        self.assertEqual(rc, 4)
        result = json.loads(out)
        self.assertEqual(result["status"], "preexisting")
        self.assertEqual(result["checks"][0]["status"], "preexisting")
        # The temporary baseline worktree must not linger afterwards.
        worktrees = _git(self.repo, "worktree", "list", "--porcelain").stdout
        self.assertNotIn("orchestra-check-baseline", worktrees)

    def test_baseline_plain_fail_when_the_base_passes(self):
        # HEAD's good.py contains "ok" (see setUp), so the check passes at
        # the baseline; the worker's uncommitted edit removes it, so the
        # check fails now -- a genuinely NEW failure, not a preexisting one.
        self._write_config(
            "checks:\n  items:\n    - name: py\n      run: \"grep -q ok good.py\"\n"
        )
        self._edit_file("good.py", "print('changed')\n")
        rc, out = self._cli("--path", self.repo, "--since", "HEAD", "--baseline")
        self.assertEqual(rc, 1)
        result = json.loads(out)
        self.assertEqual(result["status"], "fail")
        self.assertEqual(result["checks"][0]["status"], "fail")
        worktrees = _git(self.repo, "worktree", "list", "--porcelain").stdout
        self.assertNotIn("orchestra-check-baseline", worktrees)


if __name__ == "__main__":
    unittest.main(verbosity=2)
