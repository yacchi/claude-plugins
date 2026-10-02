# /// script
# requires-python = ">=3.9"
# dependencies = []
# ///
"""Unit tests for agent_exec_checks.py.

Run with: uv run test_agent_exec_checks.py
"""

import os
import shutil
import tempfile
import time
import unittest
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec_checks  # noqa: E402
from agent_exec_checks import failure_excerpt  # noqa: E402


class FailureExcerptTest(unittest.TestCase):
    def test_empty_input_gives_empty_excerpt(self):
        self.assertEqual(failure_excerpt(""), "")
        self.assertEqual(failure_excerpt(None), "")

    def test_ansi_sequences_are_stripped(self):
        out = failure_excerpt("\x1b[31mFAIL\x1b[39m src/a.test.ts\n\x1b]8;;http://x\x07link\x1b]8;;\x07")
        self.assertNotIn("\x1b", out)
        self.assertIn("FAIL src/a.test.ts", out)

    def test_failing_test_name_near_the_top_survives_a_long_output(self):
        noise = "\n".join("progress line %d" % i for i in range(5000))
        text = " FAIL  src/pages/Join.test.tsx > shows the approval note\n" + noise
        out = failure_excerpt(text, limit=2000)
        self.assertIn("shows the approval note", out)
        self.assertIn("progress line 4999", out)
        self.assertLessEqual(len(out), 2000)

    def test_markup_dump_collapses_to_one_line(self):
        dom = "\n".join(['    <div', '      class="flex"', '    >', '      <span>', '      </span>', '    </div>'] * 20)
        text = "TestingLibraryElementError: Unable to find an element\n" + dom + "\n ❯ src/a.test.tsx:12:5"
        out = failure_excerpt(text)
        self.assertIn("markup lines omitted", out)
        self.assertNotIn('class="flex"', out)
        self.assertIn("src/a.test.tsx:12:5", out)

    def test_assertion_diff_lines_are_kept(self):
        text = "\n".join(["x"] * 200 + ["Expected: \"公演A\"", "Received: \"改名した公演\""] + ["y"] * 200)
        out = failure_excerpt(text, limit=1500)
        self.assertIn("Expected: \"公演A\"", out)
        self.assertIn("Received: \"改名した公演\"", out)

    def test_output_without_anchors_falls_back_to_the_tail(self):
        text = "\n".join("line %d" % i for i in range(500))
        out = failure_excerpt(text)
        self.assertIn("line 499", out)
        self.assertNotIn("line 0\n", out)

    def test_limit_is_a_hard_ceiling(self):
        text = "\n".join("error TS2322: bad %d %s" % (i, "z" * 80) for i in range(1000))
        for limit in (100, 500, 4000):
            self.assertLessEqual(len(failure_excerpt(text, limit=limit)), limit)

    def test_clip_keeps_head_and_tail(self):
        out = agent_exec_checks._clip("A" * 500 + "B" * 500, 200)
        self.assertTrue(out.startswith("A"))
        self.assertTrue(out.endswith("B"))
        self.assertLessEqual(len(out), 200)


class GlobMatchTest(unittest.TestCase):
    def test_doublestar_matches_zero_or_more_whole_directories(self):
        self.assertTrue(agent_exec_checks.glob_match("**/*.ts", "a.ts"))
        self.assertTrue(agent_exec_checks.glob_match("**/*.ts", "x/y/a.ts"))
        self.assertFalse(agent_exec_checks.glob_match("**/*.ts", "a.tsx"))

    def test_doublestar_in_middle_matches_zero_or_more_whole_directories(self):
        self.assertTrue(agent_exec_checks.glob_match("a/**/b", "a/b"))
        self.assertTrue(agent_exec_checks.glob_match("a/**/b", "a/x/b"))
        self.assertTrue(agent_exec_checks.glob_match("a/**/b", "a/x/y/b"))
        self.assertFalse(agent_exec_checks.glob_match("a/**/b", "a/bc"))
        self.assertFalse(agent_exec_checks.glob_match("a/**/b", "ax/b"))

    def test_doublestar_at_end_matches_zero_or_more_whole_directories(self):
        self.assertTrue(agent_exec_checks.glob_match("src/**", "src/a.ts"))
        self.assertTrue(agent_exec_checks.glob_match("src/**", "src/x/a.ts"))
        self.assertFalse(agent_exec_checks.glob_match("src/**", "srcx/a.ts"))

    def test_single_star_does_not_cross_a_directory_boundary(self):
        self.assertTrue(agent_exec_checks.glob_match("src/*.ts", "src/a.ts"))
        self.assertFalse(agent_exec_checks.glob_match("src/*.ts", "src/x/a.ts"))

    def test_question_mark_matches_exactly_one_character(self):
        self.assertTrue(agent_exec_checks.glob_match("a?.ts", "ab.ts"))
        self.assertFalse(agent_exec_checks.glob_match("a?.ts", "abc.ts"))
        self.assertFalse(agent_exec_checks.glob_match("a?.ts", "a.ts"))

    def test_match_files_with_no_patterns_matches_everything(self):
        files = ["a.ts", "b.py"]
        self.assertEqual(agent_exec_checks.match_files(None, files), files)
        self.assertEqual(agent_exec_checks.match_files([], files), files)

    def test_match_files_filters_by_any_pattern(self):
        files = ["a.ts", "b.tsx", "c.py"]
        self.assertEqual(
            sorted(agent_exec_checks.match_files(["**/*.ts", "**/*.tsx"], files)),
            ["a.ts", "b.tsx"],
        )


class _CheckRunnerTest(unittest.TestCase):
    def setUp(self):
        self.tree = tempfile.mkdtemp(prefix="orch-check-tree-")
        self.slots = tempfile.mkdtemp(prefix="orch-check-slots-")

    def tearDown(self):
        shutil.rmtree(self.tree, ignore_errors=True)
        shutil.rmtree(self.slots, ignore_errors=True)

    def _write(self, rel, content=""):
        path = os.path.join(self.tree, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True) if os.path.dirname(path) else None
        with open(path, "w") as fh:
            fh.write(content)
        return path


class FilesSubstitutionTest(_CheckRunnerTest):
    def test_files_are_quoted_including_spaces_and_quotes(self):
        self._write("a b.ts")
        self._write('c"d.ts')
        item = {
            "name": "lint",
            "run": 'printf "%s\\n" {files} > out.txt',
        }
        result = agent_exec_checks.run_check(
            item, self.tree, ["a b.ts", 'c"d.ts'], self.slots,
        )
        self.assertEqual(result["status"], "pass")
        with open(os.path.join(self.tree, "out.txt")) as fh:
            out = fh.read().splitlines()
        # Each changed file must survive the shell as its OWN argument, spaces
        # and quote characters included -- not split apart by the shell.
        self.assertEqual(out, ["a b.ts", 'c"d.ts'])

    def test_files_are_expressed_relative_to_cwd_and_outside_cwd_dropped(self):
        os.makedirs(os.path.join(self.tree, "web"))
        self._write("web/a.ts")
        self._write("other/b.ts")
        item = {
            "name": "lint", "cwd": "web",
            "run": "printf %s {files} > seen.txt",
        }
        agent_exec_checks.run_check(
            item, self.tree, ["web/a.ts", "other/b.ts"], self.slots,
        )
        with open(os.path.join(self.tree, "web", "seen.txt")) as fh:
            seen = fh.read()
        self.assertEqual(seen, "a.ts")

    def test_deleted_files_are_dropped_from_the_files_list(self):
        self._write("kept.ts")
        item = {"name": "lint", "run": "printf %s {files} > seen.txt"}
        agent_exec_checks.run_check(
            item, self.tree, ["kept.ts", "gone.ts"], self.slots,
        )
        with open(os.path.join(self.tree, "seen.txt")) as fh:
            self.assertEqual(fh.read(), "kept.ts")

    def test_skipped_when_no_files_match_and_run_uses_files(self):
        item = {"name": "lint", "paths": ["**/*.ts"], "run": "true {files}"}
        result = agent_exec_checks.run_check(item, self.tree, ["a.py"], self.slots)
        self.assertEqual(result["status"], "skipped")
        self.assertEqual(result["reason"], "no matching files")

    def test_skipped_when_run_has_no_files_placeholder_but_paths_do_not_match(self):
        item = {"name": "lint", "paths": ["**/*.ts"], "run": "true"}
        result = agent_exec_checks.run_check(item, self.tree, ["a.py"], self.slots)
        self.assertEqual(result["status"], "skipped")

    def test_no_paths_means_the_check_always_runs(self):
        item = {"name": "lint", "run": "true"}
        result = agent_exec_checks.run_check(item, self.tree, [], self.slots)
        self.assertEqual(result["status"], "pass")


class OrderingTest(_CheckRunnerTest):
    def test_stops_at_first_failure_unless_all(self):
        items = [
            {"name": "a", "run": "false"},
            {"name": "b", "run": "true"},
        ]
        results = agent_exec_checks.run_checks(items, self.tree, [], self.slots)
        by_name = {r["name"]: r for r in results}
        self.assertEqual(by_name["a"]["status"], "fail")
        self.assertEqual(by_name["b"]["status"], "skipped")
        self.assertEqual(by_name["b"]["reason"], "not run after earlier failure")

    def test_all_flag_runs_every_check(self):
        items = [
            {"name": "a", "run": "false"},
            {"name": "b", "run": "true"},
        ]
        results = agent_exec_checks.run_checks(
            items, self.tree, [], self.slots, run_all=True
        )
        by_name = {r["name"]: r for r in results}
        self.assertEqual(by_name["a"]["status"], "fail")
        self.assertEqual(by_name["b"]["status"], "pass")


class FixTest(_CheckRunnerTest):
    def test_fix_runs_before_run_and_its_result_is_ignored(self):
        item = {
            "name": "lint",
            "fix": "echo fixed > marker.txt; exit 7",
            "run": "test -f marker.txt",
        }
        result = agent_exec_checks.run_check(item, self.tree, [], self.slots)
        self.assertEqual(result["status"], "pass")
        self.assertTrue(os.path.isfile(os.path.join(self.tree, "marker.txt")))


class TimeoutTest(_CheckRunnerTest):
    def test_timeout_kills_the_whole_process_group(self):
        child_marker = os.path.join(self.tree, "child.pid")
        item = {
            "name": "slow",
            "run": "sleep 30 & echo $! > %s; wait" % child_marker,
            "timeout": 1,
        }
        result = agent_exec_checks.run_check(item, self.tree, [], self.slots)
        self.assertEqual(result["status"], "fail")
        self.assertTrue(result["timed_out"])
        time.sleep(0.3)
        with open(child_marker) as fh:
            child_pid = int(fh.read().strip())
        with self.assertRaises(OSError):
            os.kill(child_pid, 0)


class JunitExcerptTest(_CheckRunnerTest):
    def test_failing_testcase_names_lead_the_excerpt(self):
        junit_xml = (
            "<testsuite>"
            '<testcase classname="pkg.Foo" name="test_one">'
            "<failure message=\"boom: expected 1 got 2\">trace...</failure>"
            "</testcase>"
            '<testcase classname="pkg.Foo" name="test_two"></testcase>'
            "</testsuite>"
        )
        item = {
            "name": "test",
            "run": "printf noise; mkdir -p reports; echo %r > reports/junit.xml; false"
            % junit_xml,
            "junit": "reports/junit.xml",
        }
        result = agent_exec_checks.run_check(item, self.tree, [], self.slots)
        self.assertEqual(result["status"], "fail")
        self.assertIn("pkg.Foo > test_one: boom: expected 1 got 2", result["excerpt"])
        self.assertNotIn("test_two", result["excerpt"])

    def test_junit_file_is_deleted_before_running_so_a_stale_one_is_never_read(self):
        os.makedirs(os.path.join(self.tree, "reports"))
        with open(os.path.join(self.tree, "reports", "junit.xml"), "w") as fh:
            fh.write(
                '<testsuite><testcase classname="Old" name="stale">'
                '<failure message="stale failure"></failure></testcase></testsuite>'
            )
        item = {"name": "test", "run": "false", "junit": "reports/junit.xml"}
        result = agent_exec_checks.run_check(item, self.tree, [], self.slots)
        self.assertNotIn("stale failure", result["excerpt"])


class SlotsTest(_CheckRunnerTest):
    def test_max_parallel_one_serializes_two_concurrent_runners(self):
        import threading

        log = os.path.join(self.tree, "timeline.txt")
        item = {
            "name": "serial",
            "run": (
                "echo start-$$ >> %s; sleep 0.4; echo end-$$ >> %s" % (log, log)
            ),
        }

        def worker():
            agent_exec_checks.run_check(
                item, self.tree, [], self.slots, max_parallel=1
            )

        threads = [threading.Thread(target=worker) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=10)

        with open(log) as fh:
            lines = [ln.strip() for ln in fh if ln.strip()]
        self.assertEqual(len(lines), 4)
        # Serialized: the second start only ever appears after the first end.
        starts = [i for i, ln in enumerate(lines) if ln.startswith("start-")]
        ends = [i for i, ln in enumerate(lines) if ln.startswith("end-")]
        self.assertEqual(ends[0], starts[1] - 1)


class OnEventTest(_CheckRunnerTest):
    def test_start_end_pairs_in_order(self):
        events = []
        items = [{"name": "a", "run": "true"}, {"name": "b", "run": "false"}]
        agent_exec_checks.run_checks(items, self.tree, [], self.slots,
                                     on_event=events.append)
        self.assertEqual([(e["event"], e["detail"]["name"]) for e in events], [
            ("check-start", "a"), ("check-end", "a"),
            ("check-start", "b"), ("check-end", "b")])
        self.assertEqual(events[0]["detail"]["tree"], self.tree)
        self.assertEqual(events[1]["detail"]["status"], "pass")
        self.assertEqual(events[3]["detail"]["status"], "fail")
        self.assertIsInstance(events[1]["detail"]["seconds"], float)

    def test_no_events_for_skipped(self):
        events = []
        items = [
            {"name": "nomatch", "run": "true", "paths": ["*.py"]},
            {"name": "a", "run": "false"},
            {"name": "after", "run": "true"},
        ]
        results = agent_exec_checks.run_checks(items, self.tree, ["x.txt"], self.slots,
                                               on_event=events.append)
        self.assertEqual([r["status"] for r in results], ["skipped", "fail", "skipped"])
        self.assertEqual({e["detail"]["name"] for e in events}, {"a"})
        self.assertEqual(len(events), 2)

    def test_raising_callback_is_ignored(self):
        def boom(event):
            raise RuntimeError("nope")

        results = agent_exec_checks.run_checks(
            [{"name": "a", "run": "true"}], self.tree, [], self.slots, on_event=boom)
        self.assertEqual(results[0]["status"], "pass")

    def test_run_check_accepts_on_event(self):
        events = []
        result = agent_exec_checks.run_check(
            {"name": "a", "run": "true"}, self.tree, [], self.slots, on_event=events.append)
        self.assertEqual(result["status"], "pass")
        self.assertEqual([e["event"] for e in events], ["check-start", "check-end"])


class ParseFailedFilesTest(unittest.TestCase):
    def test_vitest_lines(self):
        out = " FAIL  src/a.test.ts > suite > case\n FAIL  src/b.test.ts\n"
        self.assertEqual(
            agent_exec_checks.parse_failed_files(out),
            ["src/a.test.ts", "src/b.test.ts"],
        )

    def test_jest_lines_deduplicated(self):
        out = "FAIL src/a.test.js\nFAIL src/a.test.js\nPASS src/c.test.js\n"
        self.assertEqual(
            agent_exec_checks.parse_failed_files(out), ["src/a.test.js"])

    def test_pytest_lines(self):
        out = (
            "FAILED tests/test_a.py::test_x - assert 1 == 2\n"
            "FAILED tests/test_a.py::test_y\n"
            "FAILED tests/test_b.py::T::test_z\n"
        )
        self.assertEqual(
            agent_exec_checks.parse_failed_files(out),
            ["tests/test_a.py", "tests/test_b.py"],
        )

    def test_mixed_output_keeps_order(self):
        out = "FAILED tests/test_a.py::t\n FAIL  web/x.test.ts > a\n"
        self.assertEqual(
            agent_exec_checks.parse_failed_files(out),
            ["tests/test_a.py", "web/x.test.ts"],
        )

    def test_ansi_coloured_output(self):
        out = "\x1b[31m FAIL \x1b[39m  src/a.test.ts > x\n\x1b[1mFAILED\x1b[0m t/test_b.py::t\n"
        self.assertEqual(
            agent_exec_checks.parse_failed_files(out),
            ["src/a.test.ts", "t/test_b.py"],
        )

    def test_no_parsable_failures(self):
        self.assertEqual(agent_exec_checks.parse_failed_files("boom\n"), [])
        self.assertEqual(agent_exec_checks.parse_failed_files(None), [])


class PhaseTest(_CheckRunnerTest):
    def test_phase_order_then_config_order_is_stable(self):
        items = [
            {"name": "t1", "run": "true"},
            {"name": "h", "phase": "heavy", "run": "true"},
            {"name": "l1", "phase": "lint", "run": "true"},
            {"name": "p", "phase": "prepare", "run": "true"},
            {"name": "ty", "phase": "type", "run": "true"},
            {"name": "l2", "phase": "lint", "run": "true"},
            {"name": "t2", "phase": "test", "run": "true"},
        ]
        results = agent_exec_checks.run_checks(items, self.tree, [], self.slots)
        self.assertEqual(
            [r["name"] for r in results],
            ["p", "l1", "l2", "ty", "t1", "t2", "h"],
        )

    def test_invalid_phase_is_a_config_failure(self):
        results = agent_exec_checks.run_checks(
            [{"name": "a", "phase": "bogus", "run": "true"}],
            self.tree, [], self.slots)
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0]["name"], "config")
        self.assertEqual(results[0]["status"], "fail")
        self.assertIn("bogus", results[0]["reason"])


class EnsureTest(_CheckRunnerTest):
    def test_runs_only_when_path_is_missing(self):
        self._write("built/marker")
        marker = os.path.join(self.tree, "ran.txt")
        ensure = [
            {"exists": "built/marker", "run": "echo present >> %s" % marker},
            {"exists": "missing/out", "run": "echo missing >> %s" % marker},
        ]
        results = agent_exec_checks.run_checks(
            [{"name": "a", "run": "true"}], self.tree, [], self.slots,
            ensure=ensure)
        with open(marker) as fh:
            self.assertEqual(fh.read().split(), ["missing"])
        self.assertEqual([r["status"] for r in results], ["pass"])

    def test_failure_stops_the_run(self):
        results = agent_exec_checks.run_checks(
            [{"name": "a", "run": "true"}], self.tree, [], self.slots,
            ensure=[{"exists": "nope", "run": "echo bad; exit 1"}])
        self.assertEqual(results[0]["name"], "ensure:nope")
        self.assertEqual(results[0]["status"], "fail")
        self.assertEqual(results[1]["name"], "a")
        self.assertEqual(results[1]["status"], "skipped")


_FLAKY_SCRIPT = (
    "if [ -e first-ran ]; then exit 0; fi; touch first-ran; "
    "echo ' FAIL  a.test.ts > case'; exit 1"
)


class RetryTest(_CheckRunnerTest):
    def test_retry_rescues_flaky_test(self):
        item = {"name": "t", "run": _FLAKY_SCRIPT, "retry_alone": _FLAKY_SCRIPT + " # {failed}"}
        results = agent_exec_checks.run_checks([item], self.tree, [], self.slots)
        self.assertEqual(results[0]["status"], "pass")
        self.assertEqual(results[0]["flaky"], ["a.test.ts"])
        self.assertEqual(results[0]["excerpt"], "")

    def test_retry_uses_run_when_it_has_files_placeholder(self):
        self._write("a.test.ts")
        run = "if [ -e first-ran ]; then exit 0; fi; touch first-ran; echo ' FAIL  a.test.ts'; exit 1 # {files}"
        results = agent_exec_checks.run_checks(
            [{"name": "t", "run": run}], self.tree, ["a.test.ts"], self.slots)
        self.assertEqual(results[0]["status"], "pass")
        self.assertEqual(results[0]["flaky"], ["a.test.ts"])

    def test_retry_failing_again_keeps_original_excerpt(self):
        run = "echo ' FAIL  a.test.ts > original'; exit 1"
        item = {"name": "t", "run": run,
                "retry_alone": "echo 'retry noise'; exit 1 # {failed}"}
        results = agent_exec_checks.run_checks([item], self.tree, [], self.slots)
        self.assertEqual(results[0]["status"], "fail")
        self.assertIn("original", results[0]["excerpt"])
        self.assertNotIn("retry noise", results[0]["excerpt"])
        self.assertNotIn("flaky", results[0])

    def test_no_retry_without_parsable_failures(self):
        marker = os.path.join(self.tree, "retried.txt")
        item = {"name": "t", "run": "echo boom; exit 1",
                "retry_alone": "touch %s # {failed}" % marker}
        results = agent_exec_checks.run_checks([item], self.tree, [], self.slots)
        self.assertEqual(results[0]["status"], "fail")
        self.assertFalse(os.path.exists(marker))

    def test_retry_disabled(self):
        item = {"name": "t", "run": _FLAKY_SCRIPT, "retry_alone": "true # {failed}"}
        results = agent_exec_checks.run_checks(
            [item], self.tree, [], self.slots, retry_failed_alone=False)
        self.assertEqual(results[0]["status"], "fail")

    def test_retry_parses_raw_output_not_the_clipped_excerpt(self):
        lines = "".join(" FAIL  f%03d.test.ts\n" % i for i in range(400))
        self._write("out.txt", lines)
        item = {
            "name": "t",
            "run": "cat out.txt; exit 1",
            "retry_alone": "printf '%s\\n' {failed} > args.txt # x",
        }
        results = agent_exec_checks.run_checks([item], self.tree, [], self.slots)
        self.assertEqual(results[0]["status"], "pass")
        self.assertEqual(len(results[0]["flaky"]), 400)
        self.assertIn("f399.test.ts", results[0]["flaky"])

    def test_rerun_holds_all_slots(self):
        import fcntl
        import threading

        os.makedirs(self.slots, exist_ok=True)
        handle = open(os.path.join(self.slots, "slot-0.lock"), "a+")
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
        done = threading.Event()
        out = {}

        def worker():
            out["r"] = agent_exec_checks.rerun_failed_alone(
                " FAIL  a.test.ts\n", "true # {failed}", self.tree,
                self.slots, 2, 10)
            done.set()

        thread = threading.Thread(target=worker)
        thread.start()
        try:
            self.assertFalse(done.wait(0.8))
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
            handle.close()
        thread.join(timeout=10)
        self.assertEqual(out["r"]["status"], "pass")

    def test_rerun_skipped_without_failures(self):
        r = agent_exec_checks.rerun_failed_alone(
            "nothing", "true # {failed}", self.tree, self.slots, 2, 10)
        self.assertEqual(r["status"], "skipped")


if __name__ == "__main__":
    unittest.main()
