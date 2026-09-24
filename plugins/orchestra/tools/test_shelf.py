# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for `agent-exec shelf`, the per-worktree stash.

`git stash` keeps one stack shared by every worktree, and parallel workers
popping it swapped each other's changes. The worker guard now denies it, so
`shelf` has to cover what workers actually used stash for -- confirming a new
test fails without the fix, checking one commit's staged state alone -- and it
must never let one worktree see another's entries.

Run with: uv run test_shelf.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
CLI = os.path.join(HERE, "agent_exec.py")
ENV = dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null",
           ORCHESTRA_ALIVE_DIR=tempfile.mkdtemp(prefix="orch-alive-"))


def git(cwd, *args):
    return subprocess.run(["git"] + list(args), cwd=cwd, capture_output=True, text=True,
                          check=True, env=ENV).stdout


def shelf(cwd, *args):
    p = subprocess.run([sys.executable, CLI, "shelf"] + list(args), cwd=cwd,
                       capture_output=True, text=True, env=ENV)
    return p.returncode, (json.loads(p.stdout) if p.stdout.strip() else None)


def read(path):
    with open(path) as f:
        return f.read()


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


class ShelfTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="orch-shelf-")
        self.repo = os.path.join(self.tmp, "repo")
        os.makedirs(self.repo)
        git(self.repo, "init", "-q", "-b", "main", ".")
        git(self.repo, "config", "user.email", "t@e.com")
        git(self.repo, "config", "user.name", "T")
        write(os.path.join(self.repo, "src/app.ts"), "old\n")
        write(os.path.join(self.repo, "src/keep.ts"), "keep\n")
        with open(os.path.join(self.repo, "logo.bin"), "wb") as f:
            f.write(bytes(range(256)))
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", "init")
        self.w1 = os.path.join(self.tmp, "w1")
        self.w2 = os.path.join(self.tmp, "w2")
        git(self.repo, "worktree", "add", "-q", "-b", "orchestra/s/t1", self.w1)
        git(self.repo, "worktree", "add", "-q", "-b", "orchestra/s/t2", self.w2)

    def status(self, wt):
        return git(wt, "status", "--porcelain")

    def test_round_trip_restores_tracked_untracked_and_binary(self):
        write(os.path.join(self.w1, "src/app.ts"), "fix\n")
        write(os.path.join(self.w1, "src/new.test.ts"), "test\n")
        with open(os.path.join(self.w1, "logo.bin"), "wb") as f:
            f.write(bytes(reversed(range(256))))
        rc, out = shelf(self.w1, "push", "-m", "check the test fails without the fix")
        self.assertEqual((rc, out["status"]), (0, "ok"))
        self.assertEqual(self.status(self.w1), "")          # clean tree = HEAD
        self.assertFalse(os.path.exists(os.path.join(self.w1, "src/new.test.ts")))
        rc, out = shelf(self.w1, "pop")
        self.assertEqual((rc, out["status"]), (0, "ok"))
        self.assertEqual(read(os.path.join(self.w1, "src/app.ts")), "fix\n")
        self.assertEqual(read(os.path.join(self.w1, "src/new.test.ts")), "test\n")
        with open(os.path.join(self.w1, "logo.bin"), "rb") as f:
            self.assertEqual(f.read(), bytes(reversed(range(256))))
        self.assertEqual(shelf(self.w1, "list")[1]["entries"], [])

    def test_the_accident_cannot_happen(self):
        # Two parallel worktrees push and pop in interleaved order: each gets its own work back.
        write(os.path.join(self.w1, "src/app.ts"), "from w1\n")
        write(os.path.join(self.w2, "src/app.ts"), "from w2\n")
        shelf(self.w1, "push")
        shelf(self.w2, "push")
        self.assertEqual(len(shelf(self.w1, "list")[1]["entries"]), 1)
        shelf(self.w1, "pop")
        shelf(self.w2, "pop")
        self.assertEqual(read(os.path.join(self.w1, "src/app.ts")), "from w1\n")
        self.assertEqual(read(os.path.join(self.w2, "src/app.ts")), "from w2\n")
        self.assertEqual(git(self.repo, "stash", "list"), "")   # refs/stash untouched

    def test_pathspec_shelves_only_those_paths(self):
        write(os.path.join(self.w1, "src/app.ts"), "fix\n")
        write(os.path.join(self.w1, "src/keep.ts"), "also changed\n")
        shelf(self.w1, "push", "--", "src/app.ts")
        self.assertEqual(read(os.path.join(self.w1, "src/app.ts")), "old\n")
        self.assertEqual(read(os.path.join(self.w1, "src/keep.ts")), "also changed\n")
        shelf(self.w1, "pop")
        self.assertEqual(read(os.path.join(self.w1, "src/app.ts")), "fix\n")

    def test_keep_index_leaves_the_staged_state(self):
        write(os.path.join(self.w1, "src/app.ts"), "commit one\n")
        git(self.w1, "add", "src/app.ts")
        write(os.path.join(self.w1, "src/keep.ts"), "commit two\n")
        write(os.path.join(self.w1, "src/extra.ts"), "commit two, new\n")
        rc, out = shelf(self.w1, "push", "--keep-index")
        self.assertEqual(out["status"], "ok")
        self.assertEqual(read(os.path.join(self.w1, "src/app.ts")), "commit one\n")   # staged stays
        self.assertEqual(read(os.path.join(self.w1, "src/keep.ts")), "keep\n")
        self.assertFalse(os.path.exists(os.path.join(self.w1, "src/extra.ts")))
        git(self.w1, "commit", "-q", "-m", "one")
        shelf(self.w1, "pop")
        self.assertEqual(read(os.path.join(self.w1, "src/keep.ts")), "commit two\n")
        self.assertEqual(read(os.path.join(self.w1, "src/extra.ts")), "commit two, new\n")

    def test_staged_new_file_is_shelved_and_restored(self):
        write(os.path.join(self.w1, "src/staged.ts"), "staged new\n")
        git(self.w1, "add", "src/staged.ts")
        shelf(self.w1, "push")
        self.assertFalse(os.path.exists(os.path.join(self.w1, "src/staged.ts")))
        self.assertEqual(self.status(self.w1), "")
        shelf(self.w1, "pop")
        self.assertEqual(read(os.path.join(self.w1, "src/staged.ts")), "staged new\n")

    def test_empty_push_leaves_nothing(self):
        rc, out = shelf(self.w1, "push")
        self.assertEqual((rc, out["status"]), (0, "empty"))
        self.assertEqual(shelf(self.w1, "list")[1]["entries"], [])

    def test_conflicting_pop_keeps_the_entry(self):
        write(os.path.join(self.w1, "src/app.ts"), "mine\n")
        shelf(self.w1, "push")
        write(os.path.join(self.w1, "src/app.ts"), "someone else\n")
        git(self.w1, "commit", "-q", "-am", "moved on")
        rc, out = shelf(self.w1, "pop")
        self.assertEqual((rc, out["status"]), (1, "conflicted"))
        self.assertEqual(len(shelf(self.w1, "list")[1]["entries"]), 1)

    def test_apply_keeps_and_drop_removes(self):
        write(os.path.join(self.w1, "src/app.ts"), "fix\n")
        shelf(self.w1, "push")
        shelf(self.w1, "apply")
        self.assertEqual(read(os.path.join(self.w1, "src/app.ts")), "fix\n")
        self.assertEqual(len(shelf(self.w1, "list")[1]["entries"]), 1)
        shelf(self.w1, "drop")
        self.assertEqual(shelf(self.w1, "list")[1]["entries"], [])

    def test_entries_stack_and_pop_by_id(self):
        write(os.path.join(self.w1, "src/app.ts"), "first\n")
        shelf(self.w1, "push")
        write(os.path.join(self.w1, "src/keep.ts"), "second\n")
        shelf(self.w1, "push")
        rc, out = shelf(self.w1, "pop", "0")
        self.assertEqual(out["id"], 0)
        self.assertEqual(read(os.path.join(self.w1, "src/app.ts")), "first\n")
        self.assertEqual([e["id"] for e in shelf(self.w1, "list")[1]["entries"]], [1])

    def test_main_tree_is_refused_by_default(self):
        write(os.path.join(self.repo, "src/app.ts"), "user work\n")
        rc, out = shelf(self.repo, "push")
        self.assertEqual((rc, out["status"]), (3, "error"))
        self.assertEqual(read(os.path.join(self.repo, "src/app.ts")), "user work\n")
        rc, out = shelf(self.repo, "push", "--allow-main-tree")
        self.assertEqual(out["status"], "ok")

    def test_nothing_lands_in_the_tree(self):
        write(os.path.join(self.w1, "src/app.ts"), "fix\n")
        shelf(self.w1, "push")
        self.assertEqual(self.status(self.w1), "")   # the store lives in the git dir


if __name__ == "__main__":
    unittest.main()
