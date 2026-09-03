# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for `agent-exec isolate sweep` in agent_exec.py.

Sweep is the after-the-fact counterpart to the SessionEnd cleanup hook: it
reclaims leftover orchestra worktrees repo-wide rather than one session's. The
properties worth pinning are the safety ones -- it must never discard work
nobody collected, never touch the running session's own trees by default, and
never let its dry-run preview disagree with what a real sweep does.

Run with: uv run test_isolate_sweep.py
"""

import json
import os
import subprocess
import sys
import tempfile
import time
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


class _SweepRepo(unittest.TestCase):
    """A throwaway repo plus helpers for planting orchestra worktrees."""

    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = tempfile.mkdtemp(prefix="orch-sweep-")
        # Per-test heartbeat dir on top of the suite-wide redirect above, so
        # one test's liveness never leaks into the next.
        self._heartbeat_cache = agent_exec._heartbeat_dir_cache
        agent_exec._heartbeat_dir_cache = os.path.join(self.tmp, "alive")
        self.repo = os.path.join(self.tmp, "repo")
        os.makedirs(self.repo)
        _git(self.repo, "init", "-q", "-b", "main", ".")
        _git(self.repo, "config", "user.email", "t@example.com")
        _git(self.repo, "config", "user.name", "T")
        with open(os.path.join(self.repo, "README.md"), "w") as fh:
            fh.write("hello\n")
        with open(os.path.join(self.repo, ".gitignore"), "w") as fh:
            fh.write("node_modules/\n.venv/\n.env\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "init")

    def tearDown(self):
        import shutil

        shutil.rmtree(self.tmp, ignore_errors=True)
        agent_exec._heartbeat_dir_cache = self._heartbeat_cache
        os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def _create(self, task, session=None):
        """Plant one orchestra worktree, optionally owned by `session`."""
        if session is None:
            os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        else:
            os.environ["CLAUDE_CODE_SESSION_ID"] = session
        created = agent_exec.isolate_create(self.repo, task, backend="git")
        self.assertEqual(created.get("status"), "created", created)
        # Leave no ambient session behind: the tests that care about "the
        # running session" set CLAUDE_CODE_SESSION_ID themselves, and a
        # leftover value here would make every planted worktree look current.
        os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        return created["path"]

    def _dirty(self, path, relpath="worker.txt", content="worker\n"):
        with open(os.path.join(path, relpath), "w") as fh:
            fh.write(content)

    def _tasks(self, result):
        return {e["task"]: e for e in result["worktrees"]}


class SweepBasicsTests(_SweepRepo):
    def test_outside_a_repo_reports_an_error(self):
        result = agent_exec.isolate_sweep(self.tmp)
        self.assertEqual(result["status"], "error")

    def test_clean_leftover_worktrees_are_removed(self):
        self._create("alpha", session="aaaaaaaa")
        self._create("beta", session="bbbbbbbb")
        result = agent_exec.isolate_sweep(self.repo)
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["summary"]["removed"], 2)
        self.assertEqual(agent_exec.isolate_list(self.repo), [])

    def test_a_repo_with_no_orchestra_worktrees_is_a_no_op(self):
        result = agent_exec.isolate_sweep(self.repo)
        self.assertEqual(result["worktrees"], [])
        self.assertEqual(result["branches"], [])
        self.assertEqual(result["summary"]["removed"], 0)

    def test_non_orchestra_worktrees_are_never_touched(self):
        other = os.path.join(self.tmp, "manual")
        _git(self.repo, "worktree", "add", "-q", "-b", "feature/manual", other)
        agent_exec.isolate_sweep(self.repo)
        self.assertTrue(os.path.isdir(other))


class SweepSafetyTests(_SweepRepo):
    def test_uncollected_work_is_kept_and_reported(self):
        path = self._create("alpha", session="aaaaaaaa")
        self._dirty(path)
        result = agent_exec.isolate_sweep(self.repo)
        entry = self._tasks(result)["alpha"]
        self.assertEqual(entry["status"], "dirty")
        self.assertIn("worker.txt", entry["files"])
        self.assertTrue(os.path.isdir(path))
        self.assertEqual(result["summary"]["dirty"], 1)

    def test_force_discards_uncollected_work(self):
        path = self._create("alpha", session="aaaaaaaa")
        self._dirty(path)
        result = agent_exec.isolate_sweep(self.repo, force=True)
        self.assertEqual(result["summary"]["removed"], 1)
        self.assertFalse(os.path.isdir(path))

    def test_collected_work_is_removable_without_force(self):
        path = self._create("alpha", session="aaaaaaaa")
        self._dirty(path)
        agent_exec.isolate_collect(self.repo, "alpha", session_id="aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo)
        self.assertEqual(result["summary"]["removed"], 1)
        self.assertFalse(os.path.isdir(path))

    def test_running_session_worktrees_are_skipped_by_default(self):
        path = self._create("mine", session="cccccccc")
        os.environ["CLAUDE_CODE_SESSION_ID"] = "cccccccc"
        result = agent_exec.isolate_sweep(self.repo)
        entry = self._tasks(result)["mine"]
        self.assertEqual(entry["status"], "skipped")
        self.assertTrue(entry["current"])
        self.assertTrue(os.path.isdir(path))

    def test_include_current_opts_the_running_session_in(self):
        path = self._create("mine", session="cccccccc")
        os.environ["CLAUDE_CODE_SESSION_ID"] = "cccccccc"
        result = agent_exec.isolate_sweep(self.repo, include_current=True)
        self.assertEqual(self._tasks(result)["mine"]["status"], "removed")
        self.assertFalse(os.path.isdir(path))

    def test_legacy_sessionless_worktrees_are_swept(self):
        """`orchestra/<task>` with no session segment: nothing else reclaims it."""
        path = self._create("legacy")
        self.assertIsNone(agent_exec.isolate_list(self.repo)[0]["session"])
        result = agent_exec.isolate_sweep(self.repo)
        self.assertEqual(self._tasks(result)["legacy"]["status"], "removed")
        self.assertFalse(os.path.isdir(path))

    def test_a_sessionless_worktree_is_never_read_as_the_current_session(self):
        self._create("legacy")
        os.environ["CLAUDE_CODE_SESSION_ID"] = "cccccccc"
        result = agent_exec.isolate_sweep(self.repo, dry_run=True)
        self.assertFalse(self._tasks(result)["legacy"]["current"])


class SweepDryRunTests(_SweepRepo):
    def test_dry_run_changes_nothing(self):
        clean = self._create("alpha", session="aaaaaaaa")
        dirty = self._create("beta", session="bbbbbbbb")
        self._dirty(dirty)
        result = agent_exec.isolate_sweep(self.repo, dry_run=True)
        self.assertTrue(result["dry_run"])
        self.assertFalse(result["pruned"])
        self.assertTrue(os.path.isdir(clean))
        self.assertTrue(os.path.isdir(dirty))
        self.assertEqual(len(agent_exec.isolate_list(self.repo)), 2)

    def test_dry_run_preview_matches_the_real_sweep(self):
        """The preview and the sweep must never disagree -- same gate, same verdict."""
        self._create("alpha", session="aaaaaaaa")
        dirty = self._create("beta", session="bbbbbbbb")
        self._dirty(dirty)
        preview = self._tasks(agent_exec.isolate_sweep(self.repo, dry_run=True))
        real = self._tasks(agent_exec.isolate_sweep(self.repo))
        self.assertEqual(preview["alpha"]["status"], "would-remove")
        self.assertEqual(real["alpha"]["status"], "removed")
        self.assertEqual(preview["beta"]["status"], "dirty")
        self.assertEqual(real["beta"]["status"], "dirty")

    def test_dry_run_with_force_previews_dirty_trees_as_removable(self):
        dirty = self._create("beta", session="bbbbbbbb")
        self._dirty(dirty)
        result = agent_exec.isolate_sweep(self.repo, dry_run=True, force=True)
        self.assertEqual(self._tasks(result)["beta"]["status"], "would-remove")
        self.assertTrue(os.path.isdir(dirty))


class SweepAgeFilterTests(_SweepRepo):
    def test_older_than_skips_fresh_worktrees(self):
        path = self._create("alpha", session="aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo, older_than=7)
        entry = self._tasks(result)["alpha"]
        self.assertEqual(entry["status"], "skipped")
        self.assertIn("older-than", entry["reason"])
        self.assertTrue(os.path.isdir(path))

    def test_older_than_zero_sweeps_everything(self):
        self._create("alpha", session="aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo, older_than=0)
        self.assertEqual(self._tasks(result)["alpha"]["status"], "removed")

    def test_age_days_is_reported(self):
        self._create("alpha", session="aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo, dry_run=True)
        age = self._tasks(result)["alpha"]["age_days"]
        self.assertIsNotNone(age)
        self.assertGreaterEqual(age, 0)


class SweepBranchTests(_SweepRepo):
    def _orphan(self, task="ghost", session="dddddddd"):
        path = self._create(task, session=session)
        branch = agent_exec.isolate_branch(task, session)
        # Remove the worktree only; the branch outlives it, which is exactly
        # the leftover shape a killed session leaves behind.
        _git(self.repo, "worktree", "remove", "--force", path)
        return branch

    def test_orphan_branches_are_reported_but_not_deleted_by_default(self):
        branch = self._orphan()
        result = agent_exec.isolate_sweep(self.repo)
        self.assertEqual(result["branches"][0]["branch"], branch)
        self.assertEqual(result["branches"][0]["status"], "orphan")
        self.assertEqual(result["summary"]["branches_orphan"], 1)
        rc = subprocess.run(["git", "rev-parse", "--verify", branch], cwd=self.repo,
                            capture_output=True)
        self.assertEqual(rc.returncode, 0)

    def test_branches_flag_deletes_them(self):
        branch = self._orphan()
        result = agent_exec.isolate_sweep(self.repo, branches=True)
        self.assertEqual(result["summary"]["branches_deleted"], 1)
        rc = subprocess.run(["git", "rev-parse", "--verify", branch], cwd=self.repo,
                            capture_output=True)
        self.assertNotEqual(rc.returncode, 0)

    def test_branches_flag_under_dry_run_deletes_nothing(self):
        branch = self._orphan()
        result = agent_exec.isolate_sweep(self.repo, branches=True, dry_run=True)
        self.assertEqual(result["branches"][0]["status"], "would-delete")
        rc = subprocess.run(["git", "rev-parse", "--verify", branch], cwd=self.repo,
                            capture_output=True)
        self.assertEqual(rc.returncode, 0)

    def test_a_branch_with_a_live_worktree_is_not_an_orphan(self):
        self._create("alpha", session="aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo, dry_run=True)
        self.assertEqual(result["branches"], [])

    def test_non_orchestra_branches_are_never_listed(self):
        _git(self.repo, "branch", "feature/keep-me")
        result = agent_exec.isolate_sweep(self.repo, branches=True)
        self.assertEqual(result["branches"], [])
        rc = subprocess.run(["git", "rev-parse", "--verify", "feature/keep-me"],
                            cwd=self.repo, capture_output=True)
        self.assertEqual(rc.returncode, 0)


class SweepPruneTests(_SweepRepo):
    def test_a_manually_deleted_worktree_directory_is_pruned(self):
        import shutil

        path = self._create("alpha", session="aaaaaaaa")
        shutil.rmtree(path)
        result = agent_exec.isolate_sweep(self.repo)
        self.assertTrue(result["pruned"])
        self.assertEqual(result["worktrees"], [])
        listed = subprocess.run(["git", "worktree", "list"], cwd=self.repo,
                                capture_output=True, text=True)
        self.assertNotIn(path, listed.stdout)


class SweepCliTests(_SweepRepo):
    def _run(self, *args):
        return subprocess.run(
            [sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                          "agent_exec.py"), "isolate", "sweep"] + list(args),
            capture_output=True, text=True,
            env=dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null"),
        )

    def test_json_output_is_the_default(self):
        self._create("alpha", session="aaaaaaaa")
        proc = self._run("--repo", self.repo, "--dry-run")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        data = json.loads(proc.stdout)
        self.assertEqual(data["status"], "ok")
        self.assertEqual(data["worktrees"][0]["status"], "would-remove")

    def test_text_output_is_compact_and_prints_no_patch(self):
        path = self._create("alpha", session="aaaaaaaa")
        self._dirty(path)
        proc = self._run("--repo", self.repo, "--dry-run", "--text")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("dirty", proc.stdout)
        self.assertIn("summary:", proc.stdout)
        self.assertNotIn("+worker", proc.stdout)

    def test_unknown_option_is_a_usage_error(self):
        proc = self._run("--repo", self.repo, "--nope")
        self.assertEqual(proc.returncode, 2)
        self.assertIn("unknown option", proc.stderr)

    def test_json_and_text_are_mutually_exclusive(self):
        proc = self._run("--repo", self.repo, "--json", "--text")
        self.assertEqual(proc.returncode, 2)
        self.assertIn("mutually exclusive", proc.stderr)

    def test_older_than_rejects_a_non_number(self):
        proc = self._run("--repo", self.repo, "--older-than", "soon")
        self.assertEqual(proc.returncode, 2)
        self.assertIn("number of days", proc.stderr)

    def test_outside_a_repo_exits_one(self):
        proc = self._run("--repo", self.tmp)
        self.assertEqual(proc.returncode, 1)
        self.assertEqual(json.loads(proc.stdout)["status"], "error")

    def test_sweep_appears_in_the_isolate_usage_text(self):
        proc = subprocess.run(
            [sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                          "agent_exec.py"), "isolate"],
            capture_output=True, text=True,
        )
        self.assertEqual(proc.returncode, 2)
        self.assertIn("sweep", proc.stderr)


class SweepLivenessTests(_SweepRepo):
    """Cross-session safety: a sweep must not reclaim a running session's trees."""

    def _beat(self, session, age_minutes=0.0):
        path = agent_exec.heartbeat_touch(session)
        self.assertIsNotNone(path)
        if age_minutes:
            when = time.time() - age_minutes * 60.0
            os.utime(path, (when, when))
        return path

    def test_a_live_other_session_is_skipped(self):
        path = self._create("alpha", session="aaaaaaaa")
        self._beat("aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo)
        entry = self._tasks(result)["alpha"]
        self.assertEqual(entry["status"], "skipped")
        self.assertIn("aaaaaaaa", entry["reason"])
        self.assertTrue(os.path.isdir(path))

    def test_a_session_past_the_live_window_is_swept(self):
        self._create("alpha", session="aaaaaaaa")
        self._beat("aaaaaaaa", age_minutes=agent_exec._HEARTBEAT_WINDOW_MINUTES + 30)
        result = agent_exec.isolate_sweep(self.repo)
        self.assertEqual(self._tasks(result)["alpha"]["status"], "removed")

    def test_live_window_is_configurable(self):
        self._create("alpha", session="aaaaaaaa")
        self._beat("aaaaaaaa", age_minutes=30)
        kept = agent_exec.isolate_sweep(self.repo, live_window=60, dry_run=True)
        self.assertEqual(self._tasks(kept)["alpha"]["status"], "skipped")
        swept = agent_exec.isolate_sweep(self.repo, live_window=10)
        self.assertEqual(self._tasks(swept)["alpha"]["status"], "removed")

    def test_zero_live_window_disables_liveness(self):
        self._create("alpha", session="aaaaaaaa")
        self._beat("aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo, live_window=0)
        self.assertEqual(self._tasks(result)["alpha"]["status"], "removed")

    def test_include_live_overrides_a_live_session(self):
        self._create("alpha", session="aaaaaaaa")
        self._beat("aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo, include_live=True)
        self.assertEqual(self._tasks(result)["alpha"]["status"], "removed")

    def test_a_session_with_no_heartbeat_is_not_live(self):
        """Worktrees predating heartbeats must stay reclaimable."""
        self._create("alpha", session="aaaaaaaa")
        result = agent_exec.isolate_sweep(self.repo)
        self.assertEqual(self._tasks(result)["alpha"]["status"], "removed")

    def test_idle_minutes_is_reported(self):
        self._create("alpha", session="aaaaaaaa")
        self._beat("aaaaaaaa", age_minutes=5)
        result = agent_exec.isolate_sweep(self.repo, dry_run=True, live_window=1)
        idle = self._tasks(result)["alpha"]["session_idle_minutes"]
        self.assertAlmostEqual(idle, 5.0, delta=0.5)

    def test_the_current_session_still_wins_over_liveness(self):
        """A live *own* session reports the --include-current reason, not the live one."""
        self._create("mine", session="cccccccc")
        self._beat("cccccccc")
        os.environ["CLAUDE_CODE_SESSION_ID"] = "cccccccc"
        result = agent_exec.isolate_sweep(self.repo, dry_run=True)
        self.assertIn("--include-current", self._tasks(result)["mine"]["reason"])

    def test_a_dirty_tree_of_a_dead_session_is_still_protected(self):
        """Liveness is an extra guard, never a replacement for the review gate."""
        path = self._create("alpha", session="aaaaaaaa")
        self._dirty(path)
        self._beat("aaaaaaaa", age_minutes=agent_exec._HEARTBEAT_WINDOW_MINUTES + 30)
        result = agent_exec.isolate_sweep(self.repo)
        self.assertEqual(self._tasks(result)["alpha"]["status"], "dirty")

    def test_stale_heartbeats_are_pruned(self):
        old = self._beat("eeeeeeee",
                         age_minutes=(agent_exec._HEARTBEAT_RETENTION_DAYS + 1) * 24 * 60)
        fresh = self._beat("ffffffff")
        agent_exec.isolate_sweep(self.repo)
        self.assertFalse(os.path.exists(old))
        self.assertTrue(os.path.exists(fresh))

    def test_a_dry_run_prunes_nothing(self):
        old = self._beat("eeeeeeee",
                         age_minutes=(agent_exec._HEARTBEAT_RETENTION_DAYS + 1) * 24 * 60)
        agent_exec.isolate_sweep(self.repo, dry_run=True)
        self.assertTrue(os.path.exists(old))


class HeartbeatTests(unittest.TestCase):
    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = tempfile.mkdtemp(prefix="orch-beat-")
        self._cache = agent_exec._heartbeat_dir_cache
        agent_exec._heartbeat_dir_cache = os.path.join(self.tmp, "alive")

    def tearDown(self):
        import shutil

        shutil.rmtree(self.tmp, ignore_errors=True)
        agent_exec._heartbeat_dir_cache = self._cache
        os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def test_touch_uses_the_ambient_session_id(self):
        os.environ["CLAUDE_CODE_SESSION_ID"] = "abcdef01-2345-6789"
        path = agent_exec.heartbeat_touch()
        self.assertEqual(os.path.basename(path), "abcdef01")

    def test_touch_without_a_session_is_a_no_op(self):
        self.assertIsNone(agent_exec.heartbeat_touch())
        self.assertFalse(os.path.isdir(agent_exec._heartbeat_dir_cache))

    def test_touch_rejects_an_unusable_session_id(self):
        self.assertIsNone(agent_exec.heartbeat_touch("../../etc/passwd"))

    def test_touch_refreshes_an_existing_heartbeat(self):
        path = agent_exec.heartbeat_touch("aaaaaaaa")
        stale = time.time() - 3600
        os.utime(path, (stale, stale))
        agent_exec.heartbeat_touch("aaaaaaaa")
        self.assertGreater(os.path.getmtime(path), stale + 1)

    def test_an_unwritable_directory_never_raises(self):
        agent_exec._heartbeat_dir_cache = os.path.join(self.tmp, "file", "alive")
        with open(os.path.join(self.tmp, "file"), "w") as fh:
            fh.write("x")
        self.assertIsNone(agent_exec.heartbeat_touch("aaaaaaaa"))
        self.assertFalse(agent_exec._session_is_live("aaaaaaaa", 120))


class DiffPatchLaziness(_SweepRepo):
    """The removal gate must not pay for `git diff --binary` it cannot use."""

    def test_with_patch_false_returns_files_but_no_patch(self):
        path = self._create("alpha", session="aaaaaaaa")
        self._dirty(path)
        full = agent_exec.isolate_diff(self.repo, "alpha", session_id="aaaaaaaa")
        lean = agent_exec.isolate_diff(self.repo, "alpha", session_id="aaaaaaaa",
                                       with_patch=False)
        self.assertEqual(lean["files"], full["files"])
        self.assertEqual(lean["patch"], "")
        self.assertNotEqual(full["patch"], "")

    def test_the_gate_skips_the_binary_diff_when_nothing_was_collected(self):
        path = self._create("alpha", session="aaaaaaaa")
        self._dirty(path)
        calls = []
        original = agent_exec._git

        def spy(cwd, *args, **kwargs):
            calls.append(args)
            return original(cwd, *args, **kwargs)

        agent_exec._git = spy
        try:
            verdict = agent_exec._uncollected(self.repo, path, "alpha", session_id="aaaaaaaa")
        finally:
            agent_exec._git = original
        self.assertIsNotNone(verdict)
        self.assertFalse([a for a in calls if a[:2] == ("diff", "--binary")])

    def test_the_gate_still_computes_the_digest_once_collected(self):
        path = self._create("alpha", session="aaaaaaaa")
        self._dirty(path)
        agent_exec.isolate_collect(self.repo, "alpha", session_id="aaaaaaaa")
        calls = []
        original = agent_exec._git

        def spy(cwd, *args, **kwargs):
            calls.append(args)
            return original(cwd, *args, **kwargs)

        agent_exec._git = spy
        try:
            verdict = agent_exec._uncollected(self.repo, path, "alpha", session_id="aaaaaaaa")
        finally:
            agent_exec._git = original
        self.assertIsNone(verdict)  # collected -> reclaimable
        self.assertTrue([a for a in calls if a[:2] == ("diff", "--binary")])


class HeartbeatDirOverrideTests(unittest.TestCase):
    def test_env_override_wins(self):
        cache = agent_exec._heartbeat_dir_cache
        previous = os.environ.get("ORCHESTRA_ALIVE_DIR")
        tmp = tempfile.mkdtemp(prefix="orch-alive-override-")
        try:
            agent_exec._heartbeat_dir_cache = None
            os.environ["ORCHESTRA_ALIVE_DIR"] = tmp
            self.assertEqual(agent_exec._heartbeat_dir(), tmp)
        finally:
            import shutil

            shutil.rmtree(tmp, ignore_errors=True)
            agent_exec._heartbeat_dir_cache = cache
            if previous is None:
                os.environ.pop("ORCHESTRA_ALIVE_DIR", None)
            else:
                os.environ["ORCHESTRA_ALIVE_DIR"] = previous


if __name__ == "__main__":
    unittest.main(verbosity=2)
