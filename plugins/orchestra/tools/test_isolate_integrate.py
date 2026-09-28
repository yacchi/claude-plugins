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
import unittest.mock as mock

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


class CollectedMarkerTests(_IntegrateRepo):
    """A successful `integrate` marks its applied source tasks collected."""

    def setUp(self):
        super().setUp()
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        self._make_task("beta", {10: "BETA WINS\n"})

    def test_applied_task_is_removable_without_force_conflicted_is_not(self):
        result = agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        entries = self._by_task(result)
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertEqual(entries["beta"]["status"], "conflicted")

        out_alpha = agent_exec.isolate_remove(self.repo, "alpha")
        self.assertEqual(out_alpha["status"], "removed")

        out_beta = agent_exec.isolate_remove(self.repo, "beta")
        self.assertEqual(out_beta["status"], "dirty")


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

    def test_integration_worktree_ignored_drift_is_not_reported_as_dirty(self):
        # A rolling integration base regenerates gitignored build output
        # (e.g. `pnpm run build:wasm`) across rounds; that drift is routine
        # for THIS role and must not make `isolate sweep` treat it the same
        # as an ordinary task worktree's unreviewed ignored surface.
        self._make_task("alpha", {2: "ALPHA\n"})
        result = agent_exec.isolate_integrate(self.repo, ["alpha"], into="round-2")
        path = result["integration"]["path"]
        self.assertEqual(agent_exec._read_role(path), "integration")

        # Collect the tracked diff (the integrated task's change) so only the
        # ignored surface is left to differ -- isolating exactly the case
        # this fix addresses.
        collected = agent_exec.isolate_collect(self.repo, "round-2")
        self.assertEqual(collected["status"], "collected")

        os.makedirs(os.path.join(path, "node_modules"))
        with open(os.path.join(path, "node_modules", "dep.js"), "w") as fh:
            fh.write("built artifact\n")

        self.assertIsNone(agent_exec._uncollected(self.repo, path, "round-2"))


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


class OnConflictKeepTests(_IntegrateRepo):
    """`--on-conflict keep` is the default and must not change the JSON shape."""

    def test_default_keep_json_has_no_new_fields(self):
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        self._make_task("beta", {10: "BETA WINS\n"})
        result = agent_exec.isolate_integrate(self.repo, ["alpha", "beta"])
        self.assertEqual(result["status"], "conflicted")
        self.assertEqual(
            sorted(result), ["integration", "note", "onto", "status", "tasks"]
        )
        for entry in result["tasks"]:
            self.assertNotIn("rolled_back", entry)
            self.assertNotIn("revert", entry)


class OnConflictSkipStopTests(_IntegrateRepo):
    """`skip` rolls a conflicting task back and continues; `stop` also halts."""

    def setUp(self):
        super().setUp()
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        self._make_task("beta", {10: "BETA WINS\n"})
        self._make_task("gamma", {2: "GAMMA\n"})

    def test_skip_rolls_back_conflict_and_continues(self):
        result = agent_exec.isolate_integrate(
            self.repo, ["alpha", "beta", "gamma"], on_conflict="skip",
        )
        entries = self._by_task(result)
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertEqual(entries["beta"]["status"], "conflicted")
        self.assertTrue(entries["beta"]["rolled_back"])
        self.assertEqual(entries["gamma"]["status"], "applied")
        merged = self._integrated(result)
        self.assertIn("ALPHA WINS\n", merged)
        self.assertIn("GAMMA\n", merged)
        self.assertNotIn("BETA WINS\n", merged)
        self.assertNotIn("<<<<<<<", merged)
        # The rolled-back task's source is NOT marked collected.
        out_beta = agent_exec.isolate_remove(self.repo, "beta")
        self.assertEqual(out_beta["status"], "dirty")

    def test_stop_marks_remaining_tasks_skipped(self):
        result = agent_exec.isolate_integrate(
            self.repo, ["alpha", "beta", "gamma"], on_conflict="stop",
        )
        entries = self._by_task(result)
        self.assertEqual(entries["alpha"]["status"], "applied")
        self.assertEqual(entries["beta"]["status"], "conflicted")
        self.assertTrue(entries["beta"]["rolled_back"])
        self.assertEqual(entries["gamma"]["status"], "skipped")
        self.assertEqual(entries["gamma"]["files_changed"], 0)
        self.assertEqual(entries["gamma"]["conflicts"], [])


class VerifyTests(_IntegrateRepo):
    """`--verify` runs a command in the integration worktree after all tasks."""

    def test_verify_pass_exits_zero(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["alpha"], verify="test -f shared.txt",
        )
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["verify"]["status"], "pass")
        self.assertEqual(result["verify"]["exit"], 0)
        self.assertEqual(result["verify"]["excerpt"], "")

    def test_verify_restores_tracked_and_untracked_but_keeps_ignored(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["alpha"],
            verify=("echo changed > shared.txt; echo extra > verify.txt; "
                    "mkdir -p node_modules; echo ignored > node_modules/keep.txt"),
        )
        path = result["integration"]["path"]
        self.assertEqual(result["verify"]["status"], "pass")
        self.assertEqual(result["verify"]["dirtied"], 2)
        self.assertEqual(_git(path, "status", "--porcelain").stdout, "")
        with open(os.path.join(path, "node_modules", "keep.txt")) as fh:
            self.assertEqual(fh.read(), "ignored\n")

    def test_verify_fail_without_bisect_exits_four_with_excerpt(self):
        self._make_task("alpha", new_files={"bad.txt": "oops\n"})
        rc, out = self._cli(
            "integrate", "--tasks", "alpha", "--repo", self.repo,
            "--verify", "! test -f bad.txt || (echo FAIL missing bad.txt && exit 1)",
        )
        self.assertEqual(rc, 4)
        result = json.loads(out)
        self.assertEqual(result["status"], "verify-failed")
        self.assertEqual(result["verify"]["status"], "fail")
        self.assertNotEqual(result["verify"]["exit"], 0)
        self.assertIn("FAIL", result["verify"]["excerpt"])

    def test_verify_timeout_sets_timed_out(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["alpha"], verify="sleep 5", verify_timeout=1,
        )
        self.assertEqual(result["status"], "verify-failed")
        self.assertEqual(result["verify"]["status"], "fail")
        self.assertTrue(result["verify"]["timed_out"])

    def test_verify_skipped_under_keep_plus_conflict(self):
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        self._make_task("beta", {10: "BETA WINS\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["alpha", "beta"], verify="true",
        )
        self.assertEqual(result["status"], "conflicted")
        self.assertEqual(result["verify"]["status"], "skipped")
        self.assertEqual(result["verify"]["reason"], "conflict markers committed")

    def test_verify_skipped_when_nothing_applied(self):
        result = agent_exec.isolate_integrate(self.repo, ["ghost"], verify="true")
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["verify"]["status"], "skipped")
        self.assertEqual(result["verify"]["reason"], "nothing applied")

    def test_skip_plus_verify_runs_on_the_surviving_tasks(self):
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        self._make_task("beta", {10: "BETA WINS\n"})
        self._make_task("gamma", {2: "GAMMA\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["alpha", "beta", "gamma"],
            on_conflict="skip", verify="true",
        )
        entries = self._by_task(result)
        self.assertEqual(entries["beta"]["status"], "conflicted")
        self.assertTrue(entries["beta"]["rolled_back"])
        self.assertEqual(result["verify"]["status"], "pass")
        self.assertNotEqual(result["verify"]["reason"], "conflict markers committed")
        # A rolled-back conflict still makes the overall run "conflicted".
        self.assertEqual(result["status"], "conflicted")


class BisectTests(_IntegrateRepo):
    """`--bisect` binary-searches a verify failure to the culprit commit."""

    def _branch_of(self, result):
        return result["integration"]["branch"]

    def _head_ref(self, result):
        proc = subprocess.run(
            ["git", "symbolic-ref", "-q", "--short", "HEAD"],
            cwd=result["integration"]["path"], capture_output=True, text=True,
        )
        return proc.returncode, proc.stdout.strip()

    def test_bisect_without_verify_is_a_usage_error(self):
        rc, out = self._cli("integrate", "--tasks", "a", "--repo", self.repo, "--bisect")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_single_culprit_is_found_reverted_and_tip_passes(self):
        self._make_task("good", {2: "GOOD\n"})
        bad_path = self._make_task("bad", new_files={"bad.txt": "oops\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["good", "bad"],
            verify="test ! -f bad.txt", bisect=True,
        )
        self.assertEqual(result["status"], "reverted")
        entries = self._by_task(result)
        self.assertEqual(entries["good"]["status"], "applied")
        self.assertEqual(entries["bad"]["status"], "reverted")
        self.assertIn("verify_excerpt", entries["bad"])
        self.assertEqual(result["verify"]["status"], "pass")
        self.assertFalse(os.path.exists(os.path.join(result["integration"]["path"], "bad.txt")))
        # The reverted task's source worktree is no longer marked collected.
        self.assertIsNone(agent_exec._read_collected(bad_path))
        rc, ref = self._head_ref(result)
        self.assertEqual(rc, 0)
        self.assertEqual(ref, self._branch_of(result))

    def test_dirty_bisect_probes_leave_attached_clean_tree(self):
        self._make_task("good", {2: "GOOD\n"})
        self._make_task("bad", new_files={"bad.txt": "oops\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["good", "bad"],
            verify="touch probe.txt; test ! -f bad.txt", bisect=True,
        )
        path = result["integration"]["path"]
        self.assertEqual(result["verify"]["status"], "pass")
        self.assertGreaterEqual(result["verify"]["dirtied"], 3)
        self.assertEqual(_git(path, "status", "--porcelain").stdout, "")
        rc, ref = self._head_ref(result)
        self.assertEqual(rc, 0)
        self.assertEqual(ref, self._branch_of(result))

    def test_baseline_already_red_means_no_revert(self):
        self._make_task("preexisting_bad", new_files={"bad.txt": "oops\n"})
        first = agent_exec.isolate_integrate(self.repo, ["preexisting_bad"])
        self.assertEqual(first["status"], "ok")
        self._make_task("good", {2: "GOOD\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["good"], into="integrate",
            verify="test ! -f bad.txt", bisect=True,
        )
        self.assertEqual(result["status"], "verify-failed")
        self.assertEqual(result["verify"]["baseline"], "fail")
        entries = self._by_task(result)
        self.assertEqual(entries["good"]["status"], "applied")
        rc, ref = self._head_ref(result)
        self.assertEqual(rc, 0)
        self.assertEqual(ref, self._branch_of(result))

    def test_two_culprits_found_within_bisect_max(self):
        self._make_task("bad1", new_files={"bad1.txt": "oops1\n"})
        self._make_task("bad2", new_files={"bad2.txt": "oops2\n"})
        result = agent_exec.isolate_integrate(
            self.repo, ["bad1", "bad2"],
            verify="test ! -f bad1.txt && test ! -f bad2.txt",
            bisect=True, bisect_max=3,
        )
        self.assertEqual(result["status"], "reverted")
        entries = self._by_task(result)
        self.assertEqual(entries["bad1"]["status"], "reverted")
        self.assertEqual(entries["bad2"]["status"], "reverted")
        self.assertEqual(result["verify"]["status"], "pass")

    def test_revert_conflict_is_aborted_and_tree_stays_clean_on_branch(self):
        self._make_task("good", {2: "GOOD\n"})
        self._make_task("bad", new_files={"bad.txt": "oops\n"})

        real_git = agent_exec._git

        def _fake_git(cwd, *args, **kwargs):
            if len(args) >= 2 and args[0] == "revert" and args[1] == "--no-commit":
                return 1, ""
            return real_git(cwd, *args, **kwargs)

        with mock.patch.object(agent_exec, "_git", side_effect=_fake_git):
            result = agent_exec.isolate_integrate(
                self.repo, ["good", "bad"],
                verify="test ! -f bad.txt", bisect=True,
            )
        self.assertEqual(result["status"], "verify-failed")
        entries = self._by_task(result)
        self.assertEqual(entries["bad"]["revert"], "conflicted")
        # Never left mid-revert or detached.
        status_out = _git(result["integration"]["path"], "status", "--porcelain").stdout
        self.assertEqual(status_out.strip(), "")
        rc, ref = self._head_ref(result)
        self.assertEqual(rc, 0)
        self.assertEqual(ref, self._branch_of(result))


class ProgressTests(_IntegrateRepo):
    """`progress` receives one dict per stage and can never break integrate."""

    def _collect(self, *args, **kwargs):
        events = []
        result = agent_exec.isolate_integrate(*args, progress=events.append, **kwargs)
        return result, events

    def _names(self, events):
        return [e["event"] for e in events]

    def test_clean_integrate_events(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        self._make_task("beta", {19: "BETA\n"})
        result, events = self._collect(self.repo, ["alpha", "beta"])
        self.assertEqual(result["status"], "ok")
        self.assertEqual(self._names(events), [
            "integrate-start", "integrate-task", "integrate-task", "integrate-end"])
        self.assertEqual(events[0], {
            "event": "integrate-start", "pkg": None, "detail": {"tasks": ["alpha", "beta"]}})
        self.assertEqual([(e["pkg"], e["detail"]["status"]) for e in events[1:3]],
                         [("alpha", "applied"), ("beta", "applied")])
        self.assertEqual(events[3], {
            "event": "integrate-end", "pkg": None, "detail": {"status": "ok"}})

    def test_progress_does_not_change_the_result(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        with_progress, _ = self._collect(self.repo, ["alpha"], into="i1")
        plain = agent_exec.isolate_integrate(self.repo, ["alpha"], into="i2")
        for res in (with_progress, plain):
            self.assertEqual(sorted(res), ["integration", "note", "onto", "status", "tasks"])
        self.assertEqual(with_progress["tasks"], plain["tasks"])

    def test_skip_conflict_events(self):
        self._make_task("alpha", {10: "ALPHA WINS\n"})
        self._make_task("beta", {10: "BETA WINS\n"})
        result, events = self._collect(
            self.repo, ["alpha", "beta", "nothere"], on_conflict="stop")
        statuses = [(e["pkg"], e["detail"]["status"])
                    for e in events if e["event"] == "integrate-task"]
        self.assertEqual(statuses, [
            ("alpha", "applied"), ("beta", "conflicted"), ("nothere", "skipped")])
        self.assertEqual(events[-1]["detail"], {"status": "conflicted"})

    def test_missing_and_empty_events(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        result, events = self._collect(self.repo, ["ghost", "alpha"])
        statuses = [(e["pkg"], e["detail"]["status"])
                    for e in events if e["event"] == "integrate-task"]
        self.assertEqual(statuses, [("ghost", "missing"), ("alpha", "applied")])

    def test_verify_pass_events(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        result, events = self._collect(self.repo, ["alpha"], verify="true")
        self.assertEqual(self._names(events), [
            "integrate-start", "integrate-task", "verify-start", "verify-end",
            "integrate-end"])
        start = events[2]["detail"]["at_commit"]
        tip = _git(result["integration"]["path"], "rev-parse", "HEAD").stdout.strip()
        self.assertEqual(start, tip)
        self.assertEqual(events[3]["detail"]["status"], "pass")
        self.assertIsInstance(events[3]["detail"]["seconds"], float)

    def test_verify_skipped_emits_no_verify_events(self):
        result, events = self._collect(self.repo, ["ghost"], verify="true")
        self.assertNotIn("verify-start", self._names(events))
        self.assertNotIn("verify-end", self._names(events))

    def test_bisect_events_name_the_culprit(self):
        self._make_task("good", {2: "GOOD\n"})
        self._make_task("bad", new_files={"bad.txt": "oops\n"})
        result, events = self._collect(
            self.repo, ["good", "bad"], verify="test ! -f bad.txt", bisect=True)
        self.assertEqual(result["status"], "reverted")
        verify_end = [e for e in events if e["event"] == "verify-end"]
        self.assertEqual(verify_end[0]["detail"]["status"], "fail")
        probes = [e for e in events if e["event"] == "bisect-probe"]
        self.assertTrue(probes)
        for probe in probes:
            self.assertIn(probe["pkg"], ("good", "bad"))
            self.assertRegex(probe["detail"]["commit"], r"^[0-9a-f]{40}$")
            self.assertIn(probe["detail"]["result"], ("pass", "fail"))
        self.assertIn(("bad", "fail"), [(p["pkg"], p["detail"]["result"]) for p in probes])
        reverts = [e for e in events if e["event"] == "revert"]
        self.assertEqual(reverts, [{
            "event": "revert", "pkg": "bad", "detail": {"result": "reverted"}}])
        self.assertEqual(events[-1], {
            "event": "integrate-end", "pkg": None, "detail": {"status": "reverted"}})

    def test_revert_conflict_event(self):
        self._make_task("bad", new_files={"bad.txt": "oops\n"})
        real_git = agent_exec._git

        def _fake_git(cwd, *args, **kwargs):
            if len(args) >= 2 and args[0] == "revert" and args[1] == "--no-commit":
                return 1, ""
            return real_git(cwd, *args, **kwargs)

        events = []
        with mock.patch.object(agent_exec, "_git", side_effect=_fake_git):
            agent_exec.isolate_integrate(
                self.repo, ["bad"], verify="test ! -f bad.txt", bisect=True,
                progress=events.append)
        reverts = [e for e in events if e["event"] == "revert"]
        self.assertEqual(reverts[0]["detail"], {"result": "conflicted"})
        self.assertEqual(reverts[0]["pkg"], "bad")

    def test_raising_progress_never_breaks_integrate(self):
        self._make_task("good", {2: "GOOD\n"})
        self._make_task("bad", new_files={"bad.txt": "oops\n"})

        def boom(record):
            raise RuntimeError("observer down")

        result = agent_exec.isolate_integrate(
            self.repo, ["good", "bad"], verify="test ! -f bad.txt", bisect=True,
            progress=boom)
        self.assertEqual(result["status"], "reverted")
        self.assertEqual(self._by_task(result)["bad"]["status"], "reverted")


class ProgressJsonlCliTests(_IntegrateRepo):
    def test_progress_jsonl_writes_valid_lines(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        target = os.path.join(self.tmp, "deep", "er", "progress.jsonl")
        rc, out = self._cli("integrate", "--tasks", "alpha", "--repo", self.repo,
                            "--verify", "true", "--progress-jsonl", target)
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out)["status"], "ok")
        with open(target) as fh:
            lines = [json.loads(line) for line in fh.read().splitlines()]
        self.assertEqual([r["event"] for r in lines], [
            "integrate-start", "integrate-task", "verify-start", "verify-end",
            "integrate-end"])
        for record in lines:
            self.assertIsInstance(record["at"], float)
            self.assertIn("pkg", record)
            self.assertIn("detail", record)

    def test_progress_jsonl_appends(self):
        self._make_task("alpha", {2: "ALPHA\n"})
        target = os.path.join(self.tmp, "p.jsonl")
        with open(target, "w") as fh:
            fh.write('{"event": "old"}\n')
        self._cli("integrate", "--tasks", "alpha", "--repo", self.repo,
                  "--progress-jsonl", target)
        with open(target) as fh:
            lines = fh.read().splitlines()
        self.assertEqual(json.loads(lines[0]), {"event": "old"})
        self.assertGreater(len(lines), 1)

    def test_duplicate_progress_jsonl_is_usage_error(self):
        rc, out = self._cli("integrate", "--tasks", "a", "--progress-jsonl", "x",
                            "--progress-jsonl", "y")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")

    def test_missing_progress_jsonl_value_is_usage_error(self):
        rc, out = self._cli("integrate", "--tasks", "a", "--progress-jsonl")
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")


if __name__ == "__main__":
    unittest.main(verbosity=2)
