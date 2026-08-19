# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for worktree isolation in agent_exec.py.

Run with: uv run test_isolate.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402


def _git(cwd, *args):
    return subprocess.run(
        ["git"] + list(args),
        cwd=cwd,
        capture_output=True,
        text=True,
        check=True,
        env=dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null"),
    )


class _RepoMixin:
    """A throwaway git repo with one commit."""

    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = tempfile.mkdtemp(prefix="orch-iso-")
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
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def _write(self, relpath, content="x\n"):
        full = os.path.join(self.repo, relpath)
        os.makedirs(os.path.dirname(full), exist_ok=True)
        with open(full, "w") as fh:
            fh.write(content)
        return full


class GitConfigEnvTests(unittest.TestCase):
    """`GIT_CONFIG_*` injection: gtr settings without touching any config file."""

    def test_pairs_become_numbered_env_vars(self):
        env = agent_exec.git_config_env(
            [("gtr.copy.include", ".env*"), ("gtr.copy.include", "*.local")], base_env={}
        )
        self.assertEqual(env["GIT_CONFIG_COUNT"], "2")
        self.assertEqual(env["GIT_CONFIG_KEY_0"], "gtr.copy.include")
        self.assertEqual(env["GIT_CONFIG_VALUE_0"], ".env*")
        self.assertEqual(env["GIT_CONFIG_KEY_1"], "gtr.copy.include")
        self.assertEqual(env["GIT_CONFIG_VALUE_1"], "*.local")

    def test_empty_pairs_inject_nothing(self):
        env = agent_exec.git_config_env([], base_env={"PATH": "/bin"})
        self.assertNotIn("GIT_CONFIG_COUNT", env)
        self.assertEqual(env["PATH"], "/bin")

    def test_base_env_is_not_mutated(self):
        base = {"PATH": "/bin"}
        agent_exec.git_config_env([("a.b", "c")], base_env=base)
        self.assertNotIn("GIT_CONFIG_COUNT", base)

    def test_existing_git_config_count_is_extended_not_clobbered(self):
        base = {
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "user.name",
            "GIT_CONFIG_VALUE_0": "outer",
        }
        env = agent_exec.git_config_env([("gtr.copy.include", ".env*")], base_env=base)
        self.assertEqual(env["GIT_CONFIG_COUNT"], "2")
        self.assertEqual(env["GIT_CONFIG_KEY_0"], "user.name")
        self.assertEqual(env["GIT_CONFIG_KEY_1"], "gtr.copy.include")

    def test_injection_is_visible_to_git_but_leaves_no_file(self):
        """The whole premise of the gtr integration, exercised against real git."""
        tmp = tempfile.mkdtemp(prefix="orch-cfg-")
        try:
            _git(tmp, "init", "-q", "-b", "main", ".")
            env = agent_exec.git_config_env(
                [("gtr.hook.postCreate", "echo hi")],
                base_env=dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null"),
            )
            got = subprocess.run(
                ["git", "config", "--get-all", "gtr.hook.postCreate"],
                cwd=tmp, capture_output=True, text=True, env=env,
            )
            self.assertEqual(got.stdout.strip(), "echo hi")
            with open(os.path.join(tmp, ".git", "config")) as fh:
                self.assertNotIn("gtr", fh.read())
            self.assertFalse(os.path.exists(os.path.join(tmp, ".gtrconfig")))
        finally:
            import shutil

            shutil.rmtree(tmp, ignore_errors=True)


class TaskIdTests(unittest.TestCase):
    def test_sanitize_keeps_readable_ids(self):
        self.assertEqual(agent_exec.sanitize_task_id("fix-parser"), "fix-parser")
        self.assertEqual(agent_exec.sanitize_task_id("TASK_12"), "TASK_12")

    def test_sanitize_strips_path_and_ref_hostile_characters(self):
        self.assertEqual(agent_exec.sanitize_task_id("../../etc/passwd"), "etc-passwd")
        self.assertEqual(agent_exec.sanitize_task_id("a b~c^d:e?f*g[h"), "a-b-c-d-e-f-g-h")
        self.assertEqual(agent_exec.sanitize_task_id("a//b"), "a-b")

    def test_sanitize_rejects_empty_result(self):
        for bad in ("", "   ", "///", "..", "@{"):
            with self.assertRaises(ValueError):
                agent_exec.sanitize_task_id(bad)

    def test_branch_name_is_namespaced(self):
        self.assertEqual(agent_exec.isolate_branch("fix-parser"), "orchestra/fix-parser")


class BranchNameFunctionTests(unittest.TestCase):
    """`isolate_branch` is the one pure module-level function that decides the
    session-scoped branch shape; it takes the session id explicitly so these
    tests never depend on the ambient environment."""

    def test_with_session_id_namespaces_under_it(self):
        self.assertEqual(
            agent_exec.isolate_branch("t1", "abcdefgh"), "orchestra/abcdefgh/t1"
        )

    def test_without_session_id_is_unchanged_legacy_shape(self):
        self.assertEqual(agent_exec.isolate_branch("t1", None), "orchestra/t1")

    def test_uppercase_session_id_is_lowercased(self):
        self.assertEqual(
            agent_exec.isolate_branch("t1", "ABCDEFGH"), "orchestra/abcdefgh/t1"
        )

    def test_session_id_shorter_than_8_chars_is_unavailable(self):
        self.assertEqual(agent_exec.isolate_branch("t1", "abcd12"), "orchestra/t1")

    def test_session_id_whose_first_8_chars_are_not_alnum_is_unavailable(self):
        for bad in ("abcd-123", "abcd 123", "abcd!23x"):
            self.assertEqual(agent_exec.isolate_branch("t1", bad), "orchestra/t1")

    def test_ref_hostile_session_ids_never_appear_in_the_branch(self):
        for bad in ("..", "a/b", "-", "ab\ncdef12", "-abcdefg"):
            branch = agent_exec.isolate_branch("t1", bad)
            self.assertEqual(branch, "orchestra/t1")
            self.assertNotIn(bad, branch)

    def test_ref_hostile_task_ids_never_appear_in_the_branch_either(self):
        for bad in ("../../etc/passwd", "a/b", "x\ny"):
            branch = agent_exec.isolate_branch(bad, "abcdefgh")
            self.assertNotIn("..", branch)
            self.assertNotIn("\n", branch)
            self.assertTrue(branch.startswith("orchestra/abcdefgh/"))

    def test_hostile_task_id_with_no_usable_characters_creates_nothing(self):
        """`sanitize_task_id` fails closed here; no branch is ever produced."""
        for bad in ("-", "..", "///"):
            with self.assertRaises(ValueError):
                agent_exec.isolate_branch(bad, "abcdefgh")


class DetectCarryDirsTests(_RepoMixin, unittest.TestCase):
    def test_detects_ignored_env_dirs_that_exist(self):
        os.makedirs(os.path.join(self.repo, "node_modules", "pkg"))
        self._write("node_modules/pkg/index.js")
        found = agent_exec.detect_carry_dirs(self.repo)
        self.assertIn("node_modules", found)

    def test_skips_dirs_that_do_not_exist(self):
        self.assertEqual(agent_exec.detect_carry_dirs(self.repo), [])

    def test_skips_tracked_dirs(self):
        """A directory that git tracks comes along with the worktree already."""
        self._write("vendor/lib.go")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "vendor")
        self.assertNotIn("vendor", agent_exec.detect_carry_dirs(self.repo))

    def test_finds_nested_workspace_dirs(self):
        os.makedirs(os.path.join(self.repo, "packages", "a", "node_modules"))
        self._write("packages/a/node_modules/dep.js")
        found = agent_exec.detect_carry_dirs(self.repo)
        self.assertIn(os.path.join("packages", "a", "node_modules"), found)

    def test_does_not_descend_into_a_carried_dir(self):
        """node_modules/.pnpm/**/node_modules must not each be listed."""
        nested = os.path.join(self.repo, "node_modules", ".pnpm", "x", "node_modules")
        os.makedirs(nested)
        self._write("node_modules/.pnpm/x/node_modules/dep.js")
        found = agent_exec.detect_carry_dirs(self.repo)
        self.assertEqual(found, ["node_modules"])


class CopyTreeTests(unittest.TestCase):
    def test_copies_contents_and_reports_success(self):
        tmp = tempfile.mkdtemp(prefix="orch-cp-")
        try:
            src = os.path.join(tmp, "src")
            os.makedirs(os.path.join(src, "deep"))
            with open(os.path.join(src, "deep", "f.txt"), "w") as fh:
                fh.write("data")
            dst = os.path.join(tmp, "dst")
            self.assertTrue(agent_exec.copy_tree_fast(src, dst))
            with open(os.path.join(dst, "deep", "f.txt")) as fh:
                self.assertEqual(fh.read(), "data")
        finally:
            import shutil

            shutil.rmtree(tmp, ignore_errors=True)

    def test_missing_source_is_reported_not_raised(self):
        tmp = tempfile.mkdtemp(prefix="orch-cp-")
        try:
            self.assertFalse(
                agent_exec.copy_tree_fast(os.path.join(tmp, "nope"), os.path.join(tmp, "dst"))
            )
        finally:
            import shutil

            shutil.rmtree(tmp, ignore_errors=True)


class TreeStateTests(_RepoMixin, unittest.TestCase):
    def test_clean_tree_is_not_dirty(self):
        self.assertFalse(agent_exec.tree_is_dirty(self.repo))

    def test_modified_tracked_file_is_dirty(self):
        self._write("README.md", "changed\n")
        self.assertTrue(agent_exec.tree_is_dirty(self.repo))

    def test_untracked_file_is_dirty(self):
        self._write("new.txt")
        self.assertTrue(agent_exec.tree_is_dirty(self.repo))

    def test_ignored_file_alone_is_not_dirty(self):
        """node_modules must not by itself force isolation on every run."""
        os.makedirs(os.path.join(self.repo, "node_modules"))
        self._write("node_modules/x.js")
        self.assertFalse(agent_exec.tree_is_dirty(self.repo))

    def test_non_repo_is_not_dirty(self):
        tmp = tempfile.mkdtemp(prefix="orch-nr-")
        try:
            self.assertFalse(agent_exec.tree_is_dirty(tmp))
        finally:
            import shutil

            shutil.rmtree(tmp, ignore_errors=True)


class ShouldIsolateTests(_RepoMixin, unittest.TestCase):
    def test_never_wins_over_a_dirty_tree(self):
        self._write("README.md", "changed\n")
        d = agent_exec.should_isolate(self.repo, "never")
        self.assertFalse(d["isolate"])

    def test_always_wins_over_a_clean_tree(self):
        d = agent_exec.should_isolate(self.repo, "always")
        self.assertTrue(d["isolate"])

    def test_auto_isolates_a_dirty_tree(self):
        self._write("README.md", "changed\n")
        d = agent_exec.should_isolate(self.repo, "auto")
        self.assertTrue(d["isolate"])
        self.assertIn("dirty", d["reason"])

    def test_auto_skips_a_clean_tree(self):
        d = agent_exec.should_isolate(self.repo, "auto")
        self.assertFalse(d["isolate"])

    def test_auto_cannot_isolate_outside_a_repo(self):
        tmp = tempfile.mkdtemp(prefix="orch-nr-")
        try:
            d = agent_exec.should_isolate(tmp, "auto")
            self.assertFalse(d["isolate"])
            self.assertIn("not a git repository", d["reason"])
        finally:
            import shutil

            shutil.rmtree(tmp, ignore_errors=True)

    def test_always_outside_a_repo_fails_closed_with_a_reason(self):
        """No repo means no worktree; say so rather than silently running shared."""
        tmp = tempfile.mkdtemp(prefix="orch-nr-")
        try:
            d = agent_exec.should_isolate(tmp, "always")
            self.assertFalse(d["isolate"])
            self.assertIn("not a git repository", d["reason"])
        finally:
            import shutil

            shutil.rmtree(tmp, ignore_errors=True)

    def test_unknown_mode_is_rejected(self):
        with self.assertRaises(ValueError):
            agent_exec.should_isolate(self.repo, "sometimes")


class IsolateLifecycleTests(_RepoMixin, unittest.TestCase):
    """create -> diff -> remove, on the plain-git backend (no gtr required)."""

    def _create(self, task="t1", **kw):
        return agent_exec.isolate_create(self.repo, task, backend="git", **kw)

    def test_create_returns_an_existing_worktree_path(self):
        r = self._create()
        self.assertEqual(r["status"], "created")
        self.assertTrue(os.path.isdir(r["path"]))
        self.assertTrue(os.path.exists(os.path.join(r["path"], "README.md")))
        self.assertEqual(r["branch"], "orchestra/t1")

    def test_worktree_is_outside_the_repo_working_tree(self):
        """Otherwise the worktree shows up as untracked junk in the user's tree."""
        r = self._create()
        self.assertFalse(
            os.path.realpath(r["path"]).startswith(os.path.realpath(self.repo) + os.sep)
        )

    def test_uncommitted_work_is_carried_into_the_worktree(self):
        """The whole point: the worker sees the user's in-progress state."""
        self._write("README.md", "work in progress\n")
        self._write("brand-new.txt", "new\n")
        r = self._create()
        with open(os.path.join(r["path"], "README.md")) as fh:
            self.assertEqual(fh.read(), "work in progress\n")
        self.assertTrue(os.path.exists(os.path.join(r["path"], "brand-new.txt")))

    def test_worker_edits_do_not_touch_the_users_tree(self):
        """The accident this whole feature exists to prevent."""
        self._write("README.md", "precious\n")
        r = self._create()
        with open(os.path.join(r["path"], "README.md"), "w") as fh:
            fh.write("worker stomped this\n")
        _git(r["path"], "checkout", "--", "README.md")  # the reset accident
        with open(os.path.join(self.repo, "README.md")) as fh:
            self.assertEqual(fh.read(), "precious\n")

    def test_env_dirs_are_carried_when_requested(self):
        os.makedirs(os.path.join(self.repo, "node_modules", "dep"))
        self._write("node_modules/dep/index.js", "module\n")
        r = self._create(carry=True)
        self.assertIn("node_modules", r["carried"])
        self.assertTrue(
            os.path.exists(os.path.join(r["path"], "node_modules", "dep", "index.js"))
        )

    def test_carry_can_be_disabled(self):
        os.makedirs(os.path.join(self.repo, "node_modules"))
        self._write("node_modules/x.js")
        r = self._create(carry=False)
        self.assertEqual(r["carried"], [])
        self.assertFalse(os.path.exists(os.path.join(r["path"], "node_modules")))

    def test_create_is_idempotent_for_the_same_task(self):
        """Retry rounds reuse one worktree instead of piling up new ones."""
        first = self._create()
        second = self._create()
        self.assertEqual(second["status"], "exists")
        self.assertEqual(first["path"], second["path"])

    def test_list_reports_created_worktrees(self):
        self._create(task="t1")
        self._create(task="t2")
        names = sorted(x["task"] for x in agent_exec.isolate_list(self.repo))
        self.assertEqual(names, ["t1", "t2"])

    def test_list_ignores_worktrees_orchestra_did_not_create(self):
        _git(self.repo, "worktree", "add", "-q", "-b", "mine", os.path.join(self.tmp, "mine"))
        self._create(task="t1")
        self.assertEqual([x["task"] for x in agent_exec.isolate_list(self.repo)], ["t1"])

    def test_diff_reports_only_worker_changes(self):
        r = self._create()
        with open(os.path.join(r["path"], "README.md"), "a") as fh:
            fh.write("worker line\n")
        d = agent_exec.isolate_diff(self.repo, "t1")
        self.assertIn("worker line", d["patch"])
        self.assertEqual(d["files"], ["README.md"])

    def test_diff_of_an_untouched_worktree_is_empty(self):
        self._create()
        d = agent_exec.isolate_diff(self.repo, "t1")
        self.assertEqual(d["files"], [])
        self.assertEqual(d["patch"], "")

    def test_diff_includes_new_files(self):
        r = self._create()
        with open(os.path.join(r["path"], "added.py"), "w") as fh:
            fh.write("print(1)\n")
        d = agent_exec.isolate_diff(self.repo, "t1")
        self.assertEqual(d["files"], ["added.py"])

    def test_diff_excludes_carried_env_dirs(self):
        """node_modules must never land in the patch handed back to the user."""
        os.makedirs(os.path.join(self.repo, "node_modules"))
        self._write("node_modules/x.js")
        r = self._create(carry=True)
        with open(os.path.join(r["path"], "node_modules", "x.js"), "w") as fh:
            fh.write("worker rebuilt this\n")
        d = agent_exec.isolate_diff(self.repo, "t1")
        self.assertEqual(d["files"], [])

    def test_remove_deletes_the_worktree_and_branch(self):
        r = self._create()
        out = agent_exec.isolate_remove(self.repo, "t1")
        self.assertEqual(out["status"], "removed")
        self.assertFalse(os.path.exists(r["path"]))
        self.assertEqual(agent_exec.isolate_list(self.repo), [])

    def test_remove_refuses_to_discard_unreviewed_work(self):
        r = self._create()
        with open(os.path.join(r["path"], "README.md"), "a") as fh:
            fh.write("unreviewed\n")
        out = agent_exec.isolate_remove(self.repo, "t1")
        self.assertEqual(out["status"], "dirty")
        self.assertTrue(os.path.exists(r["path"]))

    def test_remove_force_discards_unreviewed_work(self):
        r = self._create()
        with open(os.path.join(r["path"], "README.md"), "a") as fh:
            fh.write("unreviewed\n")
        out = agent_exec.isolate_remove(self.repo, "t1", force=True)
        self.assertEqual(out["status"], "removed")
        self.assertFalse(os.path.exists(r["path"]))

    def test_remove_of_an_unknown_task_is_reported_not_raised(self):
        out = agent_exec.isolate_remove(self.repo, "never-existed")
        self.assertEqual(out["status"], "absent")


def _set_session(test, value):
    """Set CLAUDE_CODE_SESSION_ID for one test and guarantee cleanup."""
    os.environ["CLAUDE_CODE_SESSION_ID"] = value
    test.addCleanup(lambda: os.environ.pop("CLAUDE_CODE_SESSION_ID", None))


class SessionScopedCreateTests(_RepoMixin, unittest.TestCase):
    """Idempotency is now per (session, task), not per task alone."""

    def test_create_twice_in_one_session_reuses_the_worktree(self):
        _set_session(self, "aaaaaaaa-one")
        first = agent_exec.isolate_create(self.repo, "t1", backend="git")
        second = agent_exec.isolate_create(self.repo, "t1", backend="git")
        self.assertEqual(first["status"], "created")
        self.assertEqual(second["status"], "exists")
        self.assertEqual(first["path"], second["path"])
        self.assertEqual(first["branch"], "orchestra/aaaaaaaa/t1")

    def test_create_same_task_two_different_sessions_makes_two_worktrees(self):
        _set_session(self, "aaaaaaaa-one")
        first = agent_exec.isolate_create(self.repo, "t1", backend="git")
        _set_session(self, "bbbbbbbb-two")
        second = agent_exec.isolate_create(self.repo, "t1", backend="git")
        self.assertEqual(first["status"], "created")
        self.assertEqual(second["status"], "created")
        self.assertNotEqual(first["path"], second["path"])
        self.assertNotEqual(first["branch"], second["branch"])
        self.assertEqual(first["branch"], "orchestra/aaaaaaaa/t1")
        self.assertEqual(second["branch"], "orchestra/bbbbbbbb/t1")

    def test_hostile_session_id_from_the_environment_is_neutralized(self):
        _set_session(self, "..\n-abcdefg")
        r = agent_exec.isolate_create(self.repo, "t1", backend="git")
        self.assertEqual(r["status"], "created")
        self.assertEqual(r["branch"], "orchestra/t1")


class ResolutionTests(_RepoMixin, unittest.TestCase):
    """`isolate diff|remove|integrate` resolution order (§2 of the contract)."""

    def _extra_worktree(self, branch, suffix):
        path = os.path.join(self.tmp, suffix)
        _git(self.repo, "worktree", "add", "-q", "-b", branch, path)
        return path

    def test_current_session_hit_wins_over_a_cross_session_one(self):
        _set_session(self, "aaaaaaaa-cur")
        mine = agent_exec.isolate_create(self.repo, "t1", backend="git")
        self._extra_worktree("orchestra/bbbbbbbb/t1", "foreign")
        branch, entry = agent_exec._resolve_worktree(
            self.repo, "t1", agent_exec._current_session()
        )
        self.assertEqual(branch, mine["branch"])
        self.assertEqual(entry["path"], mine["path"])

    def test_unique_cross_session_match_is_used_and_reports_its_owner(self):
        self._extra_worktree("orchestra/bbbbbbbb/t1", "foreign")
        branch, entry = agent_exec._resolve_worktree(self.repo, "t1", None)
        self.assertEqual(branch, "orchestra/bbbbbbbb/t1")
        self.assertEqual(agent_exec._branch_session(branch), "bbbbbbbb")
        self.assertIsNotNone(entry)

    def test_two_cross_session_matches_is_an_error_not_a_guess(self):
        self._extra_worktree("orchestra/bbbbbbbb/t1", "foreign1")
        self._extra_worktree("orchestra/cccccccc/t1", "foreign2")
        with self.assertRaises(ValueError):
            agent_exec._resolve_worktree(self.repo, "t1", None)

    def test_legacy_branch_is_still_resolvable(self):
        self._extra_worktree("orchestra/t1", "legacy")
        branch, entry = agent_exec._resolve_worktree(self.repo, "t1", None)
        self.assertEqual(branch, "orchestra/t1")
        self.assertIsNotNone(entry)


class AmbiguityCLITests(_RepoMixin, unittest.TestCase):
    """A cross-session ambiguity must exit 2 with nothing on stdout."""

    def _run(self, *args):
        import io
        import contextlib

        buf_out, buf_err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(buf_out), contextlib.redirect_stderr(buf_err):
            rc = agent_exec.cmd_isolate(list(args))
        return rc, buf_out.getvalue(), buf_err.getvalue()

    def test_diff_with_two_cross_session_matches_exits_2_with_empty_stdout(self):
        _git(self.repo, "worktree", "add", "-q", "-b", "orchestra/bbbbbbbb/t1",
             os.path.join(self.tmp, "foreign1"))
        _git(self.repo, "worktree", "add", "-q", "-b", "orchestra/cccccccc/t1",
             os.path.join(self.tmp, "foreign2"))
        rc, out, err = self._run("diff", "--task", "t1", "--repo", self.repo)
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")
        self.assertIn("orchestra/bbbbbbbb/t1", err)
        self.assertIn("orchestra/cccccccc/t1", err)


class ListSessionFieldTests(_RepoMixin, unittest.TestCase):
    def _run(self, *args):
        import io
        import contextlib

        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.cmd_isolate(list(args))
        return rc, buf.getvalue()

    def test_list_reports_session_and_current_for_each_kind_of_worktree(self):
        _set_session(self, "aaaaaaaa-cur")
        mine = agent_exec.isolate_create(self.repo, "mine", backend="git")
        _git(self.repo, "worktree", "add", "-q", "-b", "orchestra/bbbbbbbb/theirs",
             os.path.join(self.tmp, "foreign"))
        _git(self.repo, "worktree", "add", "-q", "-b", "orchestra/legacy",
             os.path.join(self.tmp, "legacy"))
        entries = {e["task"]: e for e in agent_exec.isolate_list(self.repo)}
        self.assertEqual(entries["mine"]["session"], "aaaaaaaa")
        self.assertTrue(entries["mine"]["current"])
        self.assertEqual(entries["theirs"]["session"], "bbbbbbbb")
        self.assertFalse(entries["theirs"]["current"])
        self.assertIsNone(entries["legacy"]["session"])
        self.assertFalse(entries["legacy"]["current"])

    def test_session_flag_filters_by_full_id_and_by_8_char_prefix(self):
        _git(self.repo, "worktree", "add", "-q", "-b", "orchestra/bbbbbbbb/one",
             os.path.join(self.tmp, "b1"))
        _git(self.repo, "worktree", "add", "-q", "-b", "orchestra/cccccccc/two",
             os.path.join(self.tmp, "c1"))
        rc, out = self._run("list", "--repo", self.repo, "--session", "bbbbbbbb")
        self.assertEqual(rc, 0)
        self.assertEqual([w["task"] for w in json.loads(out)["worktrees"]], ["one"])
        rc, out = self._run("list", "--repo", self.repo, "--session", "bbbbbbbb-long-suffix")
        self.assertEqual(rc, 0)
        self.assertEqual([w["task"] for w in json.loads(out)["worktrees"]], ["one"])


class RemoveSessionTests(_RepoMixin, unittest.TestCase):
    def _run(self, *args):
        import io
        import contextlib

        buf_out, buf_err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(buf_out), contextlib.redirect_stderr(buf_err):
            rc = agent_exec.cmd_isolate(list(args))
        return rc, buf_out.getvalue(), buf_err.getvalue()

    def _mk(self, branch, suffix):
        path = os.path.join(self.tmp, suffix)
        _git(self.repo, "worktree", "add", "-q", "-b", branch, path)
        return path

    def test_remove_session_removes_exactly_that_sessions_worktrees(self):
        self._mk("orchestra/bbbbbbbb/one", "b1")
        self._mk("orchestra/bbbbbbbb/two", "b2")
        self._mk("orchestra/cccccccc/three", "c1")
        rc, out, err = self._run(
            "remove", "--session", "bbbbbbbb", "--repo", self.repo, "--force"
        )
        self.assertEqual(rc, 0)
        remaining = {e["task"] for e in agent_exec.isolate_list(self.repo)}
        self.assertEqual(remaining, {"three"})

    def test_remove_session_works_for_a_session_that_is_not_the_current_one(self):
        """This is how an orphaned session's worktrees get cleaned up after a crash."""
        _set_session(self, "aaaaaaaa-cur")
        self._mk("orchestra/bbbbbbbb/one", "b1")
        rc, out, err = self._run(
            "remove", "--session", "bbbbbbbb", "--repo", self.repo, "--force"
        )
        self.assertEqual(rc, 0)
        self.assertEqual(agent_exec.isolate_list(self.repo), [])

    def test_task_and_session_together_is_a_usage_error(self):
        rc, out, err = self._run(
            "remove", "--task", "t1", "--session", "bbbbbbbb", "--repo", self.repo
        )
        self.assertEqual(rc, 2)

    def test_neither_task_nor_session_is_a_usage_error(self):
        rc, out, err = self._run("remove", "--repo", self.repo)
        self.assertEqual(rc, 2)


class CarryMethodTests(_RepoMixin, unittest.TestCase):
    """`carry_method`/`carry_note`, driven by monkeypatching the copy attempt
    rather than depending on the host filesystem's CoW support."""

    def _create(self):
        os.makedirs(os.path.join(self.repo, "node_modules"))
        self._write("node_modules/x.js")
        return agent_exec.isolate_create(self.repo, "t1", backend="git", carry=True)

    def test_none_when_there_is_nothing_to_carry(self):
        r = agent_exec.isolate_create(self.repo, "t1", backend="git", carry=True)
        self.assertEqual(r["carry_method"], "none")
        self.assertNotIn("carry_note", r)

    def test_clone_when_the_cow_path_succeeds(self):
        orig = agent_exec.copy_tree_fast_method
        agent_exec.copy_tree_fast_method = lambda src, dst: "clone"
        try:
            r = self._create()
        finally:
            agent_exec.copy_tree_fast_method = orig
        self.assertEqual(r["carry_method"], "clone")
        self.assertEqual(r["carried"], ["node_modules"])
        self.assertNotIn("carry_note", r)

    def test_copy_when_only_the_plain_copy_succeeds(self):
        orig = agent_exec.copy_tree_fast_method
        agent_exec.copy_tree_fast_method = lambda src, dst: "copy"
        try:
            r = self._create()
        finally:
            agent_exec.copy_tree_fast_method = orig
        self.assertEqual(r["carry_method"], "copy")
        self.assertIn("carry_note", r)
        self.assertNotIn(self.repo, r["carry_note"])
        self.assertNotIn(r["path"], r["carry_note"])

    def test_failed_when_every_attempt_fails(self):
        orig = agent_exec.copy_tree_fast_method
        agent_exec.copy_tree_fast_method = lambda src, dst: "failed"
        try:
            r = self._create()
        finally:
            agent_exec.copy_tree_fast_method = orig
        self.assertEqual(r["carry_method"], "failed")
        self.assertEqual(r["carried"], [])
        self.assertIn("carry_note", r)
        self.assertNotIn(self.repo, r["carry_note"])
        self.assertNotIn(r["path"], r["carry_note"])


class SubprocessEndToEndTests(_RepoMixin, unittest.TestCase):
    """A real `agent-exec isolate` subprocess, not the in-process CLI wrapper --
    covers (1) session-scoped branches and (3) listing/session fields."""

    def _agent_exec(self, *args, session_id=None):
        script = os.path.join(os.path.dirname(os.path.abspath(__file__)), "agent_exec.py")
        env = dict(os.environ)
        if session_id is None:
            env.pop("CLAUDE_CODE_SESSION_ID", None)
        else:
            env["CLAUDE_CODE_SESSION_ID"] = session_id
        return subprocess.run(
            [sys.executable, script, "isolate"] + list(args),
            capture_output=True, text=True, env=env,
        )

    def test_two_sessions_create_two_worktrees_and_list_reports_them(self):
        p1 = self._agent_exec(
            "create", "--task", "t1", "--repo", self.repo, "--backend", "git",
            session_id="aaaaaaaa-one",
        )
        self.assertEqual(p1.returncode, 0, p1.stderr)
        out1 = json.loads(p1.stdout)
        self.assertEqual(out1["status"], "created")
        self.assertEqual(out1["branch"], "orchestra/aaaaaaaa/t1")

        p2 = self._agent_exec(
            "create", "--task", "t1", "--repo", self.repo, "--backend", "git",
            session_id="bbbbbbbb-two",
        )
        self.assertEqual(p2.returncode, 0, p2.stderr)
        out2 = json.loads(p2.stdout)
        self.assertEqual(out2["status"], "created")
        self.assertEqual(out2["branch"], "orchestra/bbbbbbbb/t1")
        self.assertNotEqual(out1["path"], out2["path"])

        p3 = self._agent_exec("list", "--repo", self.repo, session_id="aaaaaaaa-one")
        self.assertEqual(p3.returncode, 0, p3.stderr)
        by_session = {w["session"]: w for w in json.loads(p3.stdout)["worktrees"]}
        self.assertTrue(by_session["aaaaaaaa"]["current"])
        self.assertFalse(by_session["bbbbbbbb"]["current"])


class IsolateCommandTests(_RepoMixin, unittest.TestCase):
    """The CLI surface the instructor actually calls."""

    def _run(self, *args):
        import io
        import contextlib

        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.cmd_isolate(list(args))
        return rc, buf.getvalue()

    def test_create_emits_json(self):
        rc, out = self._run("create", "--task", "t1", "--repo", self.repo, "--backend", "git")
        self.assertEqual(rc, 0)
        payload = json.loads(out)
        self.assertEqual(payload["status"], "created")
        self.assertTrue(os.path.isdir(payload["path"]))

    def test_should_emits_the_auto_verdict(self):
        self._write("README.md", "changed\n")
        rc, out = self._run("should", "--repo", self.repo)
        self.assertEqual(rc, 0)
        self.assertTrue(json.loads(out)["isolate"])

    def test_missing_task_is_a_usage_error(self):
        rc, _ = self._run("create", "--repo", self.repo)
        self.assertEqual(rc, 2)

    def test_unknown_subcommand_is_a_usage_error(self):
        rc, _ = self._run("frobnicate")
        self.assertEqual(rc, 2)

    def test_main_routes_isolate(self):
        rc = agent_exec.main(["isolate", "should", "--repo", self.repo])
        self.assertEqual(rc, 0)


class DispatchIsolationTests(_RepoMixin, unittest.TestCase):
    """`dispatch --isolate`: the worker is handed a worktree, not the user's tree."""

    def setUp(self):
        super().setUp()
        self._orig_cfg = agent_exec.resolve_config
        self._orig_doctor = agent_exec._build_doctor_report
        self._orig_capture = agent_exec._run_copilot_capture
        self.captured = {}

        def fake_capture(profile, model, effort, workdir, prompt_file, resume, fmt):
            self.captured["workdir"] = workdir
            return 0, {"status": "ok", "answer": "done", "session_id": None,
                       "reason": None, "exit_code": 0}

        agent_exec._run_copilot_capture = fake_capture

    def tearDown(self):
        agent_exec.resolve_config = self._orig_cfg
        agent_exec._build_doctor_report = self._orig_doctor
        agent_exec._run_copilot_capture = self._orig_capture
        super().tearDown()

    def _dispatch(self, *extra, cls="review"):
        """cls=review resolves to claude (no subprocess); light hits copilot."""
        import contextlib
        import copy as _copy
        import io

        cfg = _copy.deepcopy(agent_exec.DEFAULTS)
        agent_exec.resolve_config = lambda: (cfg, None)
        agent_exec._build_doctor_report = lambda: {
            "ready": {"copilot": {"ok": True, "models": ["gpt-5.6-luna"]}}
        }
        prompt_file = os.path.join(self.tmp, "prompt.txt")
        with open(prompt_file, "w") as fh:
            fh.write("do the thing")
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.cmd_dispatch_route(
                ["--class", cls, "--prompt-file", prompt_file, "--workdir", self.repo]
                + list(extra)
            )
        return rc, json.loads(buf.getvalue())

    def test_dirty_tree_is_isolated_by_default(self):
        self._write("README.md", "user work in progress\n")
        rc, out = self._dispatch("--task", "t1")
        self.assertEqual(rc, 0)
        self.assertTrue(out["isolation"]["isolate"])
        self.assertNotEqual(out["isolation"]["path"], self.repo)
        self.assertTrue(os.path.isdir(out["isolation"]["path"]))

    def test_clean_tree_runs_in_place(self):
        rc, out = self._dispatch("--task", "t1")
        self.assertFalse(out["isolation"]["isolate"])
        self.assertEqual(agent_exec.isolate_list(self.repo), [])

    def test_never_leaves_a_dirty_tree_shared(self):
        self._write("README.md", "changed\n")
        rc, out = self._dispatch("--task", "t1", "--isolate", "never")
        self.assertFalse(out["isolation"]["isolate"])

    def test_always_isolates_a_clean_tree(self):
        rc, out = self._dispatch("--task", "t1", "--isolate", "always")
        self.assertTrue(out["isolation"]["isolate"])

    def test_executor_actually_runs_in_the_worktree(self):
        """The point of the whole feature: the CLI executor's cwd is the worktree."""
        self._write("README.md", "changed\n")
        rc, out = self._dispatch("--task", "t1", cls="light")
        self.assertEqual(out["status"], "ok")
        self.assertEqual(self.captured["workdir"], out["isolation"]["path"])
        self.assertNotEqual(self.captured["workdir"], self.repo)

    def test_delegate_route_still_reports_the_worktree(self):
        """A Claude subagent must be told where to work, same as a CLI executor."""
        self._write("README.md", "changed\n")
        rc, out = self._dispatch("--task", "t1", cls="review")
        self.assertEqual(out["status"], "delegate")
        self.assertTrue(out["isolation"]["isolate"])

    def test_retry_rounds_reuse_one_worktree(self):
        self._write("README.md", "changed\n")
        _, first = self._dispatch("--task", "t1")
        _, second = self._dispatch("--task", "t1")
        self.assertEqual(first["isolation"]["path"], second["isolation"]["path"])
        self.assertEqual(len(agent_exec.isolate_list(self.repo)), 1)

    def test_missing_task_falls_back_to_shared_with_a_stated_reason(self):
        """Without an id there is no stable worktree to reuse across retries."""
        self._write("README.md", "changed\n")
        rc, out = self._dispatch()
        self.assertEqual(rc, 0)
        self.assertFalse(out["isolation"]["isolate"])
        self.assertIn("--task", out["isolation"]["reason"])

    def test_unknown_isolate_mode_is_a_usage_error(self):
        with self.assertRaises(json.JSONDecodeError):
            self._dispatch("--task", "t1", "--isolate", "sometimes")

    def test_isolation_failure_does_not_abort_the_dispatch(self):
        """A worktree we could not create must degrade, not kill the run."""
        orig = agent_exec.isolate_create
        agent_exec.isolate_create = lambda *a, **k: {"status": "error", "reason": "disk full"}
        try:
            self._write("README.md", "changed\n")
            rc, out = self._dispatch("--task", "t1")
        finally:
            agent_exec.isolate_create = orig
        self.assertEqual(rc, 0)
        self.assertFalse(out["isolation"]["isolate"])
        self.assertIn("disk full", out["isolation"]["reason"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
