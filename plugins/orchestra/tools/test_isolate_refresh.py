# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for `agent-exec isolate refresh` in agent_exec.py.

`refresh` moves a task worktree's own changes onto a newer base: patch-ize
the diff, recreate the worktree at the new base, re-apply. The properties
worth pinning are the safety ones -- the patch always lands on disk before
the old tree is touched, a conflict never loses work, and the branch name
and carried dependency dirs come back identical.

Run with: uv run test_isolate_refresh.py
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
# whole suite -- including every CLI subprocess it spawns -- into a throwaway
# directory so a test run never writes into the real user's home.
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


# A file long enough that two edits at opposite ends are unambiguously
# separate hunks for git's 3-way machinery.
_BASE_LINES = ["line %02d\n" % n for n in range(1, 21)]


class _RefreshRepo(unittest.TestCase):
    """A throwaway repo, its main-tree HEAD movable at will, plus a helper to
    build a task worktree in it."""

    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = tempfile.mkdtemp(prefix="orch-refresh-")
        self.repo = os.path.join(self.tmp, "repo")
        os.makedirs(self.repo)
        _git(self.repo, "init", "-q", "-b", "main", ".")
        _git(self.repo, "config", "user.email", "t@example.com")
        _git(self.repo, "config", "user.name", "T")
        with open(os.path.join(self.repo, "shared.txt"), "w") as fh:
            fh.writelines(_BASE_LINES)
        with open(os.path.join(self.repo, ".gitignore"), "w") as fh:
            fh.write("node_modules/\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "init")
        self.old_head = _git(self.repo, "rev-parse", "HEAD").stdout.strip()

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)
        os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def _advance_main(self, relpath="main-only.txt", content="advanced\n"):
        """Move the main tree's HEAD forward, simulating time passing."""
        with open(os.path.join(self.repo, relpath), "w") as fh:
            fh.write(content)
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "advance main")
        return _git(self.repo, "rev-parse", "HEAD").stdout.strip()

    def _make_task(self, task, edits=None, new_files=None, deletes=None, session=None):
        """Create a task worktree and apply edits/new files/deletes to it."""
        created = agent_exec.isolate_create(
            self.repo, task, backend="git", carry=False, session_id=session,
        )
        self.assertIn(created["status"], ("created", "exists"))
        path = created["path"]
        if edits:
            target = os.path.join(path, "shared.txt")
            with open(target) as fh:
                lines = fh.readlines()
            for lineno, text in edits.items():
                lines[lineno - 1] = text
            with open(target, "w") as fh:
                fh.writelines(lines)
        for rel, content in (new_files or {}).items():
            full = os.path.join(path, rel)
            os.makedirs(os.path.dirname(full), exist_ok=True)
            if isinstance(content, bytes):
                with open(full, "wb") as fh:
                    fh.write(content)
            else:
                with open(full, "w") as fh:
                    fh.write(content)
        for rel in deletes or ():
            os.remove(os.path.join(path, rel))
        return path

    def _cli(self, *args):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.cmd_isolate(list(args))
        return rc, buf.getvalue()


class MovesWorkForwardTests(_RefreshRepo):
    """The core case: a modified file, a new file, a deleted file, a binary
    file all survive the move onto a new HEAD."""

    def test_modified_new_deleted_and_binary_survive_refresh(self):
        # A committed file, present in the task's own baseline, that the
        # task deletes.
        with open(os.path.join(self.repo, "doomed.txt"), "w") as fh:
            fh.write("will be deleted\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "add doomed.txt")

        self._make_task(
            "alpha",
            edits={2: "ALPHA\n"},
            new_files={
                "new.txt": "brand new\n",
                "blob.bin": b"\x00\x01binary\xff\xfe",
            },
            deletes=["doomed.txt"],
        )

        new_head = self._advance_main()
        result = agent_exec.isolate_refresh(self.repo, "alpha")
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["baseline"], new_head)

        new_path = result["path"]
        with open(os.path.join(new_path, "shared.txt")) as fh:
            self.assertIn("ALPHA\n", fh.read())
        with open(os.path.join(new_path, "new.txt")) as fh:
            self.assertEqual(fh.read(), "brand new\n")
        with open(os.path.join(new_path, "blob.bin"), "rb") as fh:
            self.assertEqual(fh.read(), b"\x00\x01binary\xff\xfe")
        self.assertFalse(os.path.exists(os.path.join(new_path, "doomed.txt")))
        # The advanced main-tree file must be visible: the worktree really
        # was recreated at the new base, not merely edited in place.
        self.assertTrue(os.path.exists(os.path.join(new_path, "main-only.txt")))

    def test_baseline_equals_onto_and_branch_name_unchanged(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        before = agent_exec.isolate_diff(self.repo, "alpha")
        new_head = self._advance_main()
        result = agent_exec.isolate_refresh(self.repo, "alpha")
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["baseline"], new_head)
        self.assertEqual(result["branch"], before["branch"])
        after_baseline = agent_exec._read_baseline(result["path"])
        self.assertEqual(after_baseline, new_head)

    def test_gitignored_dependency_dir_is_carried_into_refreshed_tree(self):
        os.makedirs(os.path.join(self.repo, "node_modules", "pkg"))
        with open(os.path.join(self.repo, "node_modules", "pkg", "x.js"), "w") as fh:
            fh.write("module\n")
        self._make_task("alpha", {2: "ALPHA\n"})
        self._advance_main()
        result = agent_exec.isolate_refresh(self.repo, "alpha")
        self.assertEqual(result["status"], "ok")
        self.assertTrue(
            os.path.exists(os.path.join(result["path"], "node_modules", "pkg", "x.js"))
        )


class UnchangedAndAbsentTests(_RefreshRepo):
    def test_unchanged_when_already_on_onto(self):
        path = self._make_task("alpha", {2: "ALPHA\n"})
        # The task's own worktree commits a synthetic baseline on top of
        # whatever HEAD was at create time, so "already on onto" means
        # naming THAT commit, not the plain HEAD it started from.
        baseline = agent_exec._read_baseline(path)
        result = agent_exec.isolate_refresh(self.repo, "alpha", onto=baseline)
        self.assertEqual(result["status"], "unchanged")
        self.assertEqual(result["old_baseline"], baseline)
        self.assertEqual(result["baseline"], baseline)
        # Nothing touched: still the same worktree path.
        self.assertEqual(result["path"], path)

    def test_absent_task_exits_three(self):
        rc, out = self._cli("refresh", "--task", "ghost", "--repo", self.repo)
        self.assertEqual(rc, 3)
        self.assertEqual(json.loads(out)["status"], "absent")

    def test_absent_leaves_nothing_behind(self):
        result = agent_exec.isolate_refresh(self.repo, "ghost")
        self.assertEqual(result["status"], "absent")
        self.assertIsNone(result["patch_file"])


class ExplicitOntoTests(_RefreshRepo):
    def test_explicit_onto_sha_is_honoured(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        new_head = self._advance_main()
        result = agent_exec.isolate_refresh(self.repo, "alpha", onto=new_head)
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["baseline"], new_head)

    def test_unresolvable_onto_is_an_error_and_untouched(self):
        path = self._make_task("alpha", {2: "ALPHA\n"})
        result = agent_exec.isolate_refresh(self.repo, "alpha", onto="no-such-ref")
        self.assertEqual(result["status"], "error")
        # Untouched: the original worktree is still exactly where it was.
        self.assertTrue(os.path.isdir(path))
        with open(os.path.join(path, "shared.txt")) as fh:
            self.assertIn("ALPHA\n", fh.read())


class ConflictTests(_RefreshRepo):
    def test_new_head_changing_same_lines_conflicts_and_patch_file_exists(self):
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        # Advance main by editing the very same line the task edited.
        with open(os.path.join(self.repo, "shared.txt")) as fh:
            lines = fh.readlines()
        lines[9] = "MAIN WINS\n"
        with open(os.path.join(self.repo, "shared.txt"), "w") as fh:
            fh.writelines(lines)
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "main also edits line 10")
        new_head = _git(self.repo, "rev-parse", "HEAD").stdout.strip()

        result = agent_exec.isolate_refresh(self.repo, "alpha")
        self.assertEqual(result["status"], "conflicted")
        self.assertEqual(result["baseline"], new_head)
        conflicts = result["conflicts"]
        self.assertEqual([c["file"] for c in conflicts], ["shared.txt"])
        self.assertGreaterEqual(conflicts[0]["hunks"], 1)
        self.assertIsNotNone(result["patch_file"])
        self.assertTrue(os.path.isfile(result["patch_file"]))
        with open(os.path.join(result["path"], "shared.txt")) as fh:
            self.assertIn("<<<<<<<", fh.read())

    def test_cli_exit_code_one_on_conflict(self):
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        with open(os.path.join(self.repo, "shared.txt")) as fh:
            lines = fh.readlines()
        lines[9] = "MAIN WINS\n"
        with open(os.path.join(self.repo, "shared.txt"), "w") as fh:
            fh.writelines(lines)
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "main also edits line 10")
        rc, out = self._cli("refresh", "--task", "alpha", "--repo", self.repo)
        self.assertEqual(rc, 1)
        self.assertEqual(json.loads(out)["status"], "conflicted")


class IntegrationRoleRefusalTests(_RefreshRepo):
    def test_integration_worktree_is_refused_untouched(self):
        with open(os.path.join(self.repo, "b.txt"), "w") as fh:
            fh.write("b\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "b")
        self._make_task("alpha", {2: "ALPHA\n"})
        integ = agent_exec.isolate_integrate(self.repo, ["alpha"])
        into_path = integ["integration"]["path"]

        result = agent_exec.isolate_refresh(self.repo, "integrate")
        self.assertEqual(result["status"], "error")
        self.assertIn("integration", result["note"])
        self.assertTrue(os.path.isdir(into_path))
        # untouched: still whatever `integrate` left there.
        self.assertEqual(agent_exec._read_role(into_path), "integration")


class EmptyWorkTests(_RefreshRepo):
    def test_no_diff_refreshes_cleanly_with_no_patch_file(self):
        self._make_task("idle")
        new_head = self._advance_main()
        result = agent_exec.isolate_refresh(self.repo, "idle")
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["baseline"], new_head)
        self.assertIsNone(result["patch_file"])
        self.assertEqual(result["files"], [])


class RaisesAfterOldTreeRemovedTests(_RefreshRepo):
    """isolate_create/_apply_patch_3way can raise, not just return an error
    status. The old worktree/branch are already gone by the time either is
    called, so the exception must be caught and turned into a status "error"
    result with patch_file set (contract item 8) instead of propagating."""

    def test_isolate_create_raising_returns_error_with_patch_file(self):
        self._make_task("flaky", edits={1: "changed by flaky\n"})
        new_head = self._advance_main()
        from unittest import mock
        with mock.patch.object(
            agent_exec, "isolate_create", side_effect=RuntimeError("boom"),
        ):
            result = agent_exec.isolate_refresh(self.repo, "flaky")
        self.assertEqual(result["status"], "error")
        self.assertIsNotNone(result["patch_file"])
        self.assertTrue(os.path.isfile(result["patch_file"]))
        with open(result["patch_file"]) as fh:
            self.assertIn("changed by flaky", fh.read())

    def test_apply_patch_3way_raising_returns_error_with_patch_file(self):
        self._make_task("flaky2", edits={1: "changed by flaky2\n"})
        self._advance_main()
        from unittest import mock
        with mock.patch.object(
            agent_exec, "_apply_patch_3way", side_effect=RuntimeError("boom"),
        ):
            result = agent_exec.isolate_refresh(self.repo, "flaky2")
        self.assertEqual(result["status"], "error")
        self.assertIsNotNone(result["patch_file"])
        self.assertTrue(os.path.isfile(result["patch_file"]))


class CliUsageTests(_RefreshRepo):
    def test_missing_task_flag(self):
        rc, out = self._cli("refresh", "--repo", self.repo)
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_unknown_flag(self):
        rc, out = self._cli("refresh", "--task", "a", "--frobnicate")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_json_and_text_together(self):
        rc, out = self._cli(
            "refresh", "--task", "a", "--repo", self.repo, "--json", "--text"
        )
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_duplicate_flag(self):
        rc, out = self._cli("refresh", "--task", "a", "--task", "b", "--repo", self.repo)
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_missing_value_for_flag(self):
        rc, out = self._cli("refresh", "--task")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_usage_text_lists_refresh(self):
        buf = io.StringIO()
        agent_exec._isolate_usage(stream=buf)
        self.assertIn("refresh", buf.getvalue())

    def test_json_is_the_default_output(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        self._advance_main()
        rc, out = self._cli("refresh", "--task", "alpha", "--repo", self.repo)
        self.assertEqual(rc, 0)
        json.loads(out)

    def test_text_mode_reports_status(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        self._advance_main()
        rc, out = self._cli("refresh", "--task", "alpha", "--repo", self.repo, "--text")
        self.assertEqual(rc, 0)
        self.assertIn("refresh ok", out)

    def test_main_routes_isolate_refresh(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        self._advance_main()
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.main(
                ["isolate", "refresh", "--task", "alpha", "--repo", self.repo]
            )
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(buf.getvalue())["status"], "ok")


if __name__ == "__main__":
    unittest.main(verbosity=2)
