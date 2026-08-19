# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for `agent-exec isolate integrate` in agent_exec.py.

Run with: uv run test_isolate_integrate.py
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


def _git(cwd, *args):
    return subprocess.run(
        ["git"] + list(args),
        cwd=cwd,
        capture_output=True,
        text=True,
        check=True,
        env=dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null"),
    )


# A file long enough that two edits at opposite ends are unambiguously separate
# hunks for git's 3-way machinery.
_BASE_LINES = ["line %02d\n" % n for n in range(1, 21)]


class _IntegrateRepo(unittest.TestCase):
    """A throwaway repo plus helpers to build task worktrees in it."""

    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = tempfile.mkdtemp(prefix="orch-integ-")
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

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def _make_task(self, task, edits=None, new_files=None):
        """Create a task worktree and apply `edits` ({lineno: text}) to shared.txt."""
        created = agent_exec.isolate_create(self.repo, task, backend="git", carry=False)
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
            with open(full, "w") as fh:
                fh.write(content)
        return path

    def _integrated(self, result, relpath="shared.txt"):
        with open(os.path.join(result["integration"]["path"], relpath)) as fh:
            return fh.read()

    def _by_task(self, result):
        return {entry["task"]: entry for entry in result["tasks"]}

    def _cli(self, *args):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.cmd_isolate(list(args))
        return rc, buf.getvalue()


class DisjointApplyTests(_IntegrateRepo):
    """The case that used to force serialization: two workers, one file."""

    def test_disjoint_hunks_in_one_file_both_apply(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        self._make_task("beta", {19: "BETA\n"})
        result = agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        self.assertEqual(result["status"], "ok")
        entries = self._by_task(result)
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertEqual(entries["beta"]["status"], "applied")
        self.assertEqual(entries["alpha"]["files_changed"], 1)
        self.assertEqual(entries["alpha"]["conflicts"], [])
        merged = self._integrated(result)
        self.assertIn("ALPHA\n", merged)
        self.assertIn("BETA\n", merged)

    def test_exit_code_zero_when_everything_applies(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        self._make_task("beta", {19: "BETA\n"})
        rc, out = self._cli("integrate", "--tasks", "alpha,beta", "--repo", self.repo)
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out)["status"], "ok")

    def test_new_files_from_separate_tasks_both_land(self):
        self._make_task("alpha", new_files={"a.txt": "from alpha\n"})
        self._make_task("beta", new_files={"pkg/b.txt": "from beta\n"})
        result = agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        self.assertEqual(result["status"], "ok")
        self.assertEqual(self._integrated(result, "a.txt"), "from alpha\n")
        self.assertEqual(self._integrated(result, "pkg/b.txt"), "from beta\n")


class ConflictTests(_IntegrateRepo):
    """A conflict is a reported outcome, not an error and not a rollback."""

    def setUp(self):
        super().setUp()
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        self._make_task("beta", {10: "BETA WINS\n"})
        self._make_task("gamma", {2: "GAMMA\n"})

    def test_same_lines_conflict_and_exit_one(self):
        rc, out = self._cli("integrate", "--tasks", "alpha,beta", "--repo", self.repo)
        self.assertEqual(rc, 1)
        result = json.loads(out)
        self.assertEqual(result["status"], "conflicted")
        entries = self._by_task(result)
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertEqual(entries["beta"]["status"], "conflicted")
        conflicts = entries["beta"]["conflicts"]
        self.assertEqual([c["file"] for c in conflicts], ["shared.txt"])
        self.assertGreaterEqual(conflicts[0]["hunks"], 1)

    def test_clean_earlier_task_is_not_rolled_back(self):
        result = agent_exec.isolate_integrate(self.repo, ["gamma", "alpha", "beta"])
        self.assertEqual(result["status"], "conflicted")
        entries = self._by_task(result)
        self.assertEqual(entries["gamma"]["status"], "applied")
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertEqual(entries["beta"]["status"], "conflicted")
        merged = self._integrated(result)
        self.assertIn("GAMMA\n", merged)
        self.assertIn("ALPHA WINS\n", merged)

    def test_conflicted_worktree_is_left_in_place(self):
        result = agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        self.assertTrue(os.path.isdir(result["integration"]["path"]))
        listed = [w["task"] for w in agent_exec.isolate_list(self.repo)]
        self.assertIn("integrate", listed)

    def test_order_decides_which_task_conflicts(self):
        forward = agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        reverse = agent_exec.isolate_integrate(
            self.repo, ["beta", "alpha"], into="integrate-rev"
        )
        self.assertEqual(self._by_task(forward)["beta"]["status"], "conflicted")
        self.assertEqual(self._by_task(forward)["alpha"]["status"], "applied")
        self.assertEqual(self._by_task(reverse)["alpha"]["status"], "conflicted")
        self.assertEqual(self._by_task(reverse)["beta"]["status"], "applied")


class DegenerateTaskTests(_IntegrateRepo):
    """Tasks that produced nothing, or never ran at all."""

    def test_missing_worktree_is_reported_and_the_run_continues(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        result = agent_exec.isolate_integrate(self.repo, ["ghost", "alpha"])
        self.assertEqual(result["status"], "ok")
        entries = self._by_task(result)
        self.assertEqual(entries["ghost"]["status"], "missing")
        self.assertEqual(entries["ghost"]["files_changed"], 0)
        self.assertEqual(entries["ghost"]["conflicts"], [])
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertIn("ALPHA\n", self._integrated(result))

    def test_worktree_with_no_changes_is_empty(self):
        self._make_task("idle")
        result = agent_exec.isolate_integrate(self.repo, ["idle"])
        self.assertEqual(result["status"], "ok")
        entry = self._by_task(result)["idle"]
        self.assertEqual(entry["status"], "empty")
        self.assertEqual(entry["files_changed"], 0)

    def test_every_task_missing_still_exits_zero(self):
        rc, out = self._cli("integrate", "--tasks", "ghost", "--repo", self.repo)
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out)["status"], "ok")

    def test_not_a_git_repository_is_an_environment_error(self):
        outside = os.path.join(self.tmp, "plain")
        os.makedirs(outside)
        rc, out = self._cli("integrate", "--tasks", "alpha", "--repo", outside)
        self.assertEqual(rc, 3)
        self.assertEqual(json.loads(out)["status"], "error")

    def test_unresolvable_onto_is_an_environment_error(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        rc, out = self._cli(
            "integrate", "--tasks", "alpha", "--repo", self.repo, "--onto", "no-such-ref"
        )
        self.assertEqual(rc, 3)
        self.assertEqual(json.loads(out)["status"], "error")


class UserTreeUntouchedTests(_IntegrateRepo):
    """The whole safety premise: integration happens elsewhere."""

    def _snapshot(self):
        return {
            "head": _git(self.repo, "rev-parse", "HEAD").stdout,
            "branch": _git(self.repo, "rev-parse", "--abbrev-ref", "HEAD").stdout,
            "status": _git(self.repo, "status", "--porcelain").stdout,
            "index": _git(self.repo, "ls-files", "-s").stdout,
        }

    def test_repository_is_byte_identical_before_and_after(self):
        with open(os.path.join(self.repo, "wip.txt"), "w") as fh:
            fh.write("user work in progress\n")
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        self._make_task("beta", {10: "BETA WINS\n"})
        before = self._snapshot()
        agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        self.assertEqual(self._snapshot(), before)

    def test_task_worktrees_keep_their_own_content(self):
        alpha = self._make_task("alpha", {2: "ALPHA\n"})
        self._make_task("beta", {19: "BETA\n"})
        agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        with open(os.path.join(alpha, "shared.txt")) as fh:
            body = fh.read()
        self.assertIn("ALPHA\n", body)
        self.assertNotIn("BETA\n", body)


class NoPatchTextTests(_IntegrateRepo):
    """The point of the subcommand: the orchestrator never ingests a diff."""

    MARKER = "zqxjv-distinctive-payload"

    def _leaf_values(self, node):
        if isinstance(node, dict):
            for value in node.values():
                for leaf in self._leaf_values(value):
                    yield leaf
        elif isinstance(node, list):
            for value in node:
                for leaf in self._leaf_values(value):
                    yield leaf
        else:
            yield node

    def test_stdout_never_carries_diff_bodies(self):
        self._make_task("alpha", {2: self.MARKER + "\n"})
        self._make_task("beta", {2: self.MARKER + "-other\n"})
        rc, out = self._cli("integrate", "--tasks", "alpha,beta", "--repo", self.repo)
        self.assertEqual(rc, 1)
        self.assertNotIn(self.MARKER, out)
        self.assertNotIn("@@", out)
        self.assertNotIn("<<<<<<<", out)

    def test_text_mode_never_carries_diff_bodies(self):
        self._make_task("alpha", {2: self.MARKER + "\n"})
        self._make_task("beta", {2: self.MARKER + "-other\n"})
        rc, out = self._cli(
            "integrate", "--tasks", "alpha,beta", "--repo", self.repo, "--text"
        )
        self.assertEqual(rc, 1)
        self.assertNotIn(self.MARKER, out)
        self.assertIn("conflicted", out)

    def test_every_json_value_is_an_enum_path_int_or_the_note(self):
        self._make_task("alpha", {2: self.MARKER + "\n"})
        rc, out = self._cli("integrate", "--tasks", "alpha,ghost", "--repo", self.repo)
        self.assertEqual(rc, 0)
        result = json.loads(out)
        self.assertEqual(
            sorted(result), ["integration", "note", "onto", "status", "tasks"]
        )
        note = result["note"]
        allowed_enums = {
            "ok", "conflicted", "error", "applied", "missing", "empty",
            "alpha", "ghost", "integrate", "orchestra/integrate",
        }
        for leaf in self._leaf_values(result):
            if leaf is None or isinstance(leaf, int):
                continue
            self.assertIsInstance(leaf, str)
            if leaf in allowed_enums or leaf == note:
                continue
            # Everything else must be a path or a sha: no whitespace, and it
            # exists on disk or reads as a hex object name.
            self.assertNotIn(" ", leaf)
            self.assertTrue(
                os.path.exists(leaf) or all(c in "0123456789abcdef" for c in leaf),
                "unexpected free text in JSON: %r" % (leaf,),
            )


class OntoAndIntoTests(_IntegrateRepo):
    """The integration worktree is an ordinary orchestra worktree."""

    def test_default_onto_is_the_first_tasks_baseline(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        baseline = agent_exec.isolate_diff(self.repo, "alpha")["baseline"]
        result = agent_exec.isolate_integrate(self.repo, ["alpha"])
        self.assertEqual(result["onto"], baseline)

    def test_explicit_onto_is_honoured(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        head = _git(self.repo, "rev-parse", "HEAD").stdout.strip()
        result = agent_exec.isolate_integrate(self.repo, ["alpha"], onto="HEAD")
        self.assertEqual(result["onto"], head)

    def test_into_names_the_integration_worktree(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        result = agent_exec.isolate_integrate(self.repo, ["alpha"], into="round-2")
        self.assertEqual(result["integration"]["task"], "round-2")
        self.assertEqual(result["integration"]["branch"], "orchestra/round-2")

    def test_list_shows_it_and_remove_cleans_it_up(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        result = agent_exec.isolate_integrate(self.repo, ["alpha"], into="round-2")
        listed = {w["task"]: w for w in agent_exec.isolate_list(self.repo)}
        self.assertIn("round-2", listed)
        self.assertEqual(listed["round-2"]["path"], result["integration"]["path"])
        removed = agent_exec.isolate_remove(self.repo, "round-2", force=True)
        self.assertEqual(removed["status"], "removed")
        self.assertNotIn(
            "round-2", [w["task"] for w in agent_exec.isolate_list(self.repo)]
        )

    def test_integration_diff_reads_as_changes_on_top_of_onto(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        agent_exec.isolate_integrate(self.repo, ["alpha"])
        diff = agent_exec.isolate_diff(self.repo, "integrate")
        self.assertEqual(diff["status"], "ok")
        self.assertEqual(diff["files"], ["shared.txt"])


class IntegrateUsageTests(_IntegrateRepo):
    """Usage errors exit 2 and keep stdout empty."""

    def test_missing_tasks_flag(self):
        rc, out = self._cli("integrate", "--repo", self.repo)
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_unknown_flag(self):
        rc, out = self._cli("integrate", "--tasks", "a", "--frobnicate")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_json_and_text_together(self):
        rc, out = self._cli(
            "integrate", "--tasks", "a", "--repo", self.repo, "--json", "--text"
        )
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_duplicate_flag(self):
        rc, out = self._cli("integrate", "--tasks", "a", "--tasks", "b")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_missing_value_for_flag(self):
        rc, out = self._cli("integrate", "--tasks")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_empty_tasks_list(self):
        rc, out = self._cli("integrate", "--tasks", " , ", "--repo", self.repo)
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_task_id_with_no_usable_characters(self):
        rc, out = self._cli("integrate", "--tasks", "///", "--repo", self.repo)
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_usage_text_lists_integrate(self):
        buf = io.StringIO()
        agent_exec._isolate_usage(stream=buf)
        self.assertIn("integrate", buf.getvalue())

    def test_json_is_the_default_output(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        rc, out = self._cli("integrate", "--tasks", "alpha", "--repo", self.repo)
        self.assertEqual(rc, 0)
        json.loads(out)

    def test_main_routes_isolate_integrate(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.main(
                ["isolate", "integrate", "--tasks", "alpha", "--repo", self.repo]
            )
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(buf.getvalue())["status"], "ok")


class SessionScopedIntegrateTests(_IntegrateRepo):
    """`isolate integrate` resolves each task by the same session rules as
    `diff`/`remove`: current session first, unique cross-session match otherwise."""

    def test_integrate_resolves_tasks_created_under_the_current_session(self):
        os.environ["CLAUDE_CODE_SESSION_ID"] = "aaaaaaaa-sess"
        self.addCleanup(lambda: os.environ.pop("CLAUDE_CODE_SESSION_ID", None))
        self._make_task("alpha", {2: "ALPHA\n"})
        self._make_task("beta", {19: "BETA\n"})
        result = agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        self.assertEqual(result["status"], "ok")
        entries = self._by_task(result)
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertEqual(entries["beta"]["status"], "applied")

    def test_integrate_resolves_a_cross_session_task(self):
        os.environ["CLAUDE_CODE_SESSION_ID"] = "aaaaaaaa-sess"
        self.addCleanup(lambda: os.environ.pop("CLAUDE_CODE_SESSION_ID", None))
        self._make_task("alpha", {2: "ALPHA\n"})
        # beta was created under a different session than the one integrating.
        os.environ["CLAUDE_CODE_SESSION_ID"] = "bbbbbbbb-sess"
        self._make_task("beta", {19: "BETA\n"})
        os.environ["CLAUDE_CODE_SESSION_ID"] = "aaaaaaaa-sess"
        result = agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        self.assertEqual(result["status"], "ok")
        entries = self._by_task(result)
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertEqual(entries["beta"]["status"], "applied")


if __name__ == "__main__":
    unittest.main(verbosity=2)
