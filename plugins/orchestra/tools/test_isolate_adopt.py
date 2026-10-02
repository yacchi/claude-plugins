# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for `agent-exec isolate adopt/unadopt` in agent_exec.py.

An externally created worktree (Orca's, on its own branch name) is registered
as a task so diff/collect/integrate/check/refresh reach it by task id, while
orchestra never deletes it: refresh works in place, remove only unadopts and
sweep reports it as `external`.

Run with: uv run test_isolate_adopt.py
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
# whole suite into a throwaway directory.
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


_BASE_LINES = ["line %02d\n" % n for n in range(1, 21)]


class _AdoptRepo(unittest.TestCase):
    def setUp(self):
        self._session_id = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.tmp = tempfile.mkdtemp(prefix="orch-adopt-")
        self.repo = os.path.join(self.tmp, "repo")
        os.makedirs(self.repo)
        _git(self.repo, "init", "-q", "-b", "main", ".")
        _git(self.repo, "config", "user.email", "t@example.com")
        _git(self.repo, "config", "user.name", "T")
        with open(os.path.join(self.repo, "shared.txt"), "w") as fh:
            fh.writelines(_BASE_LINES)
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "init")
        self.old_head = _git(self.repo, "rev-parse", "HEAD").stdout.strip()

        # Isolated HOME so config resolution never touches the real user's.
        self._home = os.path.join(self.tmp, "home")
        os.makedirs(self._home)
        self._orig_home = os.environ.get("HOME")
        os.environ["HOME"] = self._home
        self._orig_cwd = os.getcwd()
        os.chdir(self.repo)

        # An externally created worktree on a custom branch name.
        self.ext = os.path.join(self.tmp, "orca", "repo", "feat-x")
        os.makedirs(os.path.dirname(self.ext))
        _git(self.repo, "worktree", "add", "-q", "-b", "feat-x", self.ext, "HEAD")

    def tearDown(self):
        os.chdir(self._orig_cwd)
        if self._orig_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = self._orig_home
        shutil.rmtree(self.tmp, ignore_errors=True)
        os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        if self._session_id is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self._session_id

    def _advance_main(self, relpath="main-only.txt", content="advanced\n"):
        with open(os.path.join(self.repo, relpath), "w") as fh:
            fh.write(content)
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "advance main")
        return _git(self.repo, "rev-parse", "HEAD").stdout.strip()

    def _edit_ext(self, lineno, text, new_file=None):
        target = os.path.join(self.ext, "shared.txt")
        with open(target) as fh:
            lines = fh.readlines()
        lines[lineno - 1] = text
        with open(target, "w") as fh:
            fh.writelines(lines)
        if new_file:
            with open(os.path.join(self.ext, new_file), "w") as fh:
                fh.write("new\n")

    def _adopt(self, task="ext-1", **kw):
        return agent_exec.isolate_adopt(self.repo, task, self.ext, **kw)

    def _cli(self, *args):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.cmd_isolate(list(args))
        return rc, buf.getvalue()


class AdoptTests(_AdoptRepo):
    def test_adopt_plain_worktree_then_diff_collect_integrate_by_task_id(self):
        result = self._adopt()
        self.assertEqual(result["status"], "adopted")
        self.assertEqual(result["branch"], "feat-x")
        self.assertEqual(result["baseline"], self.old_head)
        self.assertEqual(os.path.realpath(result["path"]), os.path.realpath(self.ext))

        self._edit_ext(3, "EXT\n", new_file="ext-new.txt")

        diff = agent_exec.isolate_diff(self.repo, "ext-1")
        self.assertEqual(diff["status"], "ok")
        self.assertEqual(sorted(diff["files"]), ["ext-new.txt", "shared.txt"])
        self.assertIsNone(diff["session"])

        collected = agent_exec.isolate_collect(self.repo, "ext-1")
        self.assertEqual(collected["status"], "collected")

        integrated = agent_exec.isolate_integrate(self.repo, ["ext-1"])
        self.assertEqual(integrated["status"], "ok")
        self.assertEqual(integrated["tasks"][0]["status"], "applied")
        self.assertEqual(integrated["tasks"][0]["files_changed"], 2)

    def test_baseline_ref_is_honoured(self):
        head = self._advance_main()
        result = self._adopt(baseline=head)
        self.assertEqual(result["baseline"], head)
        self.assertEqual(agent_exec._read_baseline(self.ext), head)

    def test_bad_baseline_ref_is_an_error(self):
        result = self._adopt(baseline="no-such-ref")
        self.assertEqual(result["status"], "error")
        self.assertIsNone(agent_exec._adopted_task(self.ext))

    def test_marker_records_task_session_and_time(self):
        self._adopt(session_id="abcd1234")
        record = agent_exec._adopted_task(self.ext)
        self.assertEqual(record["task"], "ext-1")
        self.assertEqual(record["session"], "abcd1234")
        self.assertIsInstance(record["adopted_at"], float)

    def test_path_that_is_not_a_worktree_errors(self):
        stray = os.path.join(self.tmp, "stray")
        os.makedirs(stray)
        result = agent_exec.isolate_adopt(self.repo, "t", stray)
        self.assertEqual(result["status"], "error")

    def test_main_worktree_errors(self):
        result = agent_exec.isolate_adopt(self.repo, "t", self.repo)
        self.assertEqual(result["status"], "error")

    def test_orchestra_created_tree_errors(self):
        created = agent_exec.isolate_create(self.repo, "native", backend="git", carry=False)
        result = agent_exec.isolate_adopt(self.repo, "other", created["path"])
        self.assertEqual(result["status"], "error")

    def test_task_that_resolves_to_a_normal_task_errors(self):
        agent_exec.isolate_create(self.repo, "native", backend="git", carry=False)
        result = self._adopt(task="native")
        self.assertEqual(result["status"], "error")

    def test_task_already_adopted_elsewhere_errors(self):
        self._adopt()
        other = os.path.join(self.tmp, "orca", "repo", "feat-y")
        _git(self.repo, "worktree", "add", "-q", "-b", "feat-y", other, "HEAD")
        result = agent_exec.isolate_adopt(self.repo, "ext-1", other)
        self.assertEqual(result["status"], "error")
        self.assertIsNone(agent_exec._adopted_task(other))

    def test_path_already_adopted_under_another_task_errors(self):
        self._adopt()
        self.assertEqual(self._adopt(task="different")["status"], "error")

    def test_readopt_same_path_is_exists_and_keeps_baseline(self):
        self._adopt()
        self._advance_main()
        result = self._adopt()
        self.assertEqual(result["status"], "exists")
        self.assertEqual(agent_exec._read_baseline(self.ext), self.old_head)

    def test_readopt_with_baseline_updates_it(self):
        self._adopt()
        head = self._advance_main()
        result = self._adopt(baseline=head)
        self.assertEqual(result["status"], "exists")
        self.assertEqual(agent_exec._read_baseline(self.ext), head)

    def test_dependency_dir_already_in_the_tree_is_left_alone(self):
        # gtr copy patterns / an Orca setup script may have put node_modules
        # there first; copying onto it would nest node_modules/node_modules.
        with open(os.path.join(self.repo, ".gitignore"), "a") as fh:
            fh.write("node_modules/\n")
        os.makedirs(os.path.join(self.repo, "node_modules"))
        with open(os.path.join(self.repo, "node_modules", "x"), "w") as fh:
            fh.write("main\n")
        _git(self.repo, "add", ".gitignore")
        _git(self.repo, "commit", "-q", "-m", "ignore deps")
        os.makedirs(os.path.join(self.ext, "node_modules"))
        with open(os.path.join(self.ext, "node_modules", "x"), "w") as fh:
            fh.write("tool\n")
        result = self._adopt()
        self.assertEqual(result["status"], "adopted")
        self.assertNotIn("node_modules", result["carried"])
        self.assertEqual(result["already_present"], ["node_modules"])
        self.assertFalse(os.path.exists(os.path.join(self.ext, "node_modules", "node_modules")))
        with open(os.path.join(self.ext, "node_modules", "x")) as fh:
            self.assertEqual(fh.read(), "tool\n")

    def test_adopt_carries_dependencies_and_local_files(self):
        with open(os.path.join(self.repo, ".gitignore"), "a") as fh:
            fh.write("node_modules/\n")
        os.makedirs(os.path.join(self.repo, "node_modules"))
        with open(os.path.join(self.repo, "node_modules", "x"), "w") as fh:
            fh.write("dep\n")
        os.makedirs(os.path.join(self.repo, ".claude"))
        with open(os.path.join(self.repo, ".claude", "settings.local.json"), "w") as fh:
            fh.write("{}\n")
        with open(os.path.join(self.repo, "CLAUDE.local.md"), "w") as fh:
            fh.write("local\n")
        _git(self.repo, "add", ".gitignore")
        _git(self.repo, "commit", "-q", "-m", "ignore deps")
        result = self._adopt()
        self.assertEqual(result["status"], "adopted")
        self.assertIn("node_modules", result["carried"])
        self.assertEqual(
            result["carried_files"],
            [".claude/settings.local.json", "CLAUDE.local.md"],
        )
        self.assertTrue(os.path.isfile(os.path.join(self.ext, "node_modules", "x")))
        self.assertEqual(agent_exec.isolate_remove(self.repo, "ext-1")["status"], "unadopted")

    def test_adopt_no_carry_and_readopt_do_not_copy(self):
        os.makedirs(os.path.join(self.repo, "node_modules"))
        with open(os.path.join(self.repo, "node_modules", "x"), "w") as fh:
            fh.write("dep\n")
        self.assertEqual(self._adopt(carry=False)["carried"], [])
        self.assertFalse(os.path.exists(os.path.join(self.ext, "node_modules")))
        again = self._adopt(carry=True)
        self.assertEqual(again["status"], "exists")
        self.assertEqual(again["carried"], [])


class UnadoptTests(_AdoptRepo):
    def test_unadopt_removes_markers_and_leaves_files(self):
        self._adopt()
        self._edit_ext(3, "EXT\n", new_file="ext-new.txt")
        agent_exec.isolate_collect(self.repo, "ext-1")
        gitdir = agent_exec._worktree_gitdir(self.ext)

        result = agent_exec.isolate_unadopt(self.repo, "ext-1")
        self.assertEqual(result["status"], "unadopted")
        self.assertEqual(os.path.realpath(result["path"]), os.path.realpath(self.ext))
        for name in ("orchestra-adopted", "orchestra-baseline", "orchestra-collected"):
            self.assertFalse(os.path.exists(os.path.join(gitdir, name)), name)
        self.assertTrue(os.path.isfile(os.path.join(self.ext, "ext-new.txt")))
        self.assertEqual(agent_exec.isolate_diff(self.repo, "ext-1")["status"], "absent")

    def test_unadopt_absent(self):
        result = agent_exec.isolate_unadopt(self.repo, "nope")
        self.assertEqual(result["status"], "absent")

    def test_unadopt_never_touches_a_normal_task(self):
        created = agent_exec.isolate_create(self.repo, "native", backend="git", carry=False)
        result = agent_exec.isolate_unadopt(self.repo, "native")
        self.assertEqual(result["status"], "absent")
        self.assertTrue(os.path.isdir(created["path"]))


class ResolveTests(_AdoptRepo):
    def test_session_match_is_preferred_and_ambiguity_raises(self):
        self._adopt(session_id="aaaaaaaa")
        other = os.path.join(self.tmp, "orca", "repo", "feat-y")
        _git(self.repo, "worktree", "add", "-q", "-b", "feat-y", other, "HEAD")
        # Force a second marker with the same task id (adopt itself refuses).
        gitdir = agent_exec._worktree_gitdir(other)
        with open(os.path.join(gitdir, "orchestra-adopted"), "w") as fh:
            json.dump({"task": "ext-1", "session": "bbbbbbbb", "adopted_at": 1.0}, fh)
        _, entry = agent_exec._resolve_worktree(self.repo, "ext-1", "bbbbbbbb")
        self.assertEqual(os.path.realpath(entry["path"]), os.path.realpath(other))
        _, entry = agent_exec._resolve_worktree(self.repo, "ext-1", "aaaaaaaa")
        self.assertEqual(os.path.realpath(entry["path"]), os.path.realpath(self.ext))
        with self.assertRaises(ValueError):
            agent_exec._resolve_worktree(self.repo, "ext-1", "cccccccc")


class RefreshInPlaceTests(_AdoptRepo):
    def test_refresh_in_place_keeps_path_and_branch_and_reapplies_work(self):
        self._adopt()
        self._edit_ext(3, "EXT\n", new_file="ext-new.txt")
        onto = self._advance_main()

        result = agent_exec.isolate_refresh(self.repo, "ext-1", onto=onto)
        self.assertEqual(result["status"], "ok")
        self.assertTrue(result["in_place"])
        self.assertEqual(os.path.realpath(result["path"]), os.path.realpath(self.ext))
        self.assertEqual(result["branch"], "feat-x")
        self.assertEqual(result["baseline"], onto)
        self.assertEqual(agent_exec._read_baseline(self.ext), onto)
        self.assertTrue(result["patch_file"] and os.path.isfile(result["patch_file"]))

        self.assertEqual(_git(self.ext, "rev-parse", "--abbrev-ref", "HEAD").stdout.strip(), "feat-x")
        self.assertEqual(_git(self.ext, "rev-parse", "HEAD").stdout.strip(), onto)
        with open(os.path.join(self.ext, "shared.txt")) as fh:
            self.assertEqual(fh.readlines()[2], "EXT\n")
        self.assertTrue(os.path.isfile(os.path.join(self.ext, "ext-new.txt")))
        self.assertTrue(os.path.isfile(os.path.join(self.ext, "main-only.txt")))
        self.assertIsNotNone(agent_exec._adopted_task(self.ext))

    def test_refresh_in_place_conflict_leaves_markers(self):
        self._adopt()
        self._edit_ext(3, "EXT\n")
        with open(os.path.join(self.repo, "shared.txt")) as fh:
            lines = fh.readlines()
        lines[2] = "MAIN\n"
        with open(os.path.join(self.repo, "shared.txt"), "w") as fh:
            fh.writelines(lines)
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "conflicting main")
        onto = _git(self.repo, "rev-parse", "HEAD").stdout.strip()

        result = agent_exec.isolate_refresh(self.repo, "ext-1", onto=onto)
        self.assertEqual(result["status"], "conflicted")
        self.assertTrue(result["in_place"])
        self.assertEqual([c["file"] for c in result["conflicts"]], ["shared.txt"])
        with open(os.path.join(self.ext, "shared.txt")) as fh:
            self.assertIn("<<<<<<<", fh.read())
        self.assertTrue(os.path.isfile(result["patch_file"]))
        self.assertTrue(os.path.isdir(self.ext))

    def test_refresh_does_not_remove_ignored_files(self):
        with open(os.path.join(self.repo, ".gitignore"), "w") as fh:
            fh.write("node_modules/\n")
        _git(self.repo, "add", "-A")
        _git(self.repo, "commit", "-q", "-m", "ignore")
        _git(self.ext, "merge", "-q", "main")
        self._adopt()
        os.makedirs(os.path.join(self.ext, "node_modules"))
        with open(os.path.join(self.ext, "node_modules", "dep.js"), "w") as fh:
            fh.write("x\n")
        onto = self._advance_main()
        result = agent_exec.isolate_refresh(self.repo, "ext-1", onto=onto)
        self.assertEqual(result["status"], "ok")
        self.assertTrue(os.path.isfile(os.path.join(self.ext, "node_modules", "dep.js")))

    def test_normal_tree_result_has_no_in_place_key(self):
        agent_exec.isolate_create(self.repo, "native", backend="git", carry=False)
        onto = self._advance_main()
        result = agent_exec.isolate_refresh(self.repo, "native", onto=onto)
        self.assertEqual(result["status"], "ok")
        self.assertNotIn("in_place", result)


class RemoveAndSweepTests(_AdoptRepo):
    def test_remove_only_unadopts(self):
        self._edit_ext(3, "EXT\n")
        for force in (False, True):
            self._adopt()
            result = agent_exec.isolate_remove(self.repo, "ext-1", force=force)
            self.assertEqual(result["status"], "unadopted")
            self.assertTrue(os.path.isdir(self.ext))
            self.assertEqual(
                _git(self.repo, "branch", "--list", "feat-x").stdout.strip().lstrip("+ "),
                "feat-x",
            )
            self.assertIsNone(agent_exec._adopted_task(self.ext))

    def test_sweep_reports_external_and_leaves_it(self):
        self._adopt()
        self._edit_ext(3, "EXT\n")
        for dry_run in (True, False):
            result = agent_exec.isolate_sweep(
                self.repo, dry_run=dry_run, force=True, include_current=True, include_live=True,
            )
            records = [w for w in result["worktrees"] if w["task"] == "ext-1"]
            self.assertEqual(len(records), 1)
            self.assertEqual(records[0]["status"], "external")
            self.assertEqual(result["summary"]["external"], 1)
            self.assertEqual(result["summary"]["dirty"], 0)
            self.assertTrue(os.path.isdir(self.ext))
            self.assertIsNotNone(agent_exec._adopted_task(self.ext))
        self.assertIn("external", agent_exec.format_sweep_text(result))

    def test_sweep_older_than_still_leaves_it(self):
        self._adopt()
        result = agent_exec.isolate_sweep(self.repo, older_than=365)
        self.assertEqual(
            [w["status"] for w in result["worktrees"] if w["task"] == "ext-1"], ["external"]
        )
        self.assertTrue(os.path.isdir(self.ext))


class CheckTests(_AdoptRepo):
    def test_check_task_finds_changed_files_of_adopted_tree(self):
        self._adopt()
        self._edit_ext(3, "EXT\n", new_file="ext-new.txt")
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = agent_exec.cmd_check(["--task", "ext-1"])
        self.assertEqual(rc, 0)
        result = json.loads(buf.getvalue())
        self.assertEqual(result["status"], "no-checks")
        self.assertEqual(result["files"], 2)
        self.assertEqual(os.path.realpath(result["tree"]), os.path.realpath(self.ext))


class CliTests(_AdoptRepo):
    def test_cli_adopt_and_unadopt(self):
        rc, out = self._cli("adopt", "--task", "ext-1", "--path", self.ext, "--repo", self.repo)
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out)["status"], "adopted")
        rc, out = self._cli("adopt", "--task", "ext-1", "--path", self.ext, "--repo", self.repo)
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out)["status"], "exists")
        rc, out = self._cli("unadopt", "--task", "ext-1", "--repo", self.repo)
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out)["status"], "unadopted")
        rc, out = self._cli("unadopt", "--task", "ext-1", "--repo", self.repo)
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out)["status"], "absent")

    def test_cli_adopt_error_exits_3(self):
        rc, out = self._cli("adopt", "--task", "t", "--path", self.repo, "--repo", self.repo)
        self.assertEqual(rc, 3)
        self.assertEqual(json.loads(out)["status"], "error")

    def test_cli_usage_errors_exit_2(self):
        self.assertEqual(self._cli("adopt", "--task", "t")[0], 2)
        self.assertEqual(self._cli("adopt", "--path", self.ext)[0], 2)
        self.assertEqual(self._cli("unadopt")[0], 2)
        self.assertEqual(self._cli("adopt", "--task", "t", "--task", "u", "--path", "p")[0], 2)
        self.assertEqual(
            self._cli("adopt", "--task", "t", "--path", "p", "--json", "--text")[0], 2
        )
        self.assertEqual(self._cli("unadopt", "--task", "t", "--bogus")[0], 2)

    def test_cli_text_output(self):
        rc, out = self._cli(
            "adopt", "--task", "ext-1", "--path", self.ext, "--repo", self.repo, "--text"
        )
        self.assertEqual(rc, 0)
        self.assertTrue(out.startswith("adopted"))


if __name__ == "__main__":
    unittest.main()
