# /// script
# requires-python = ">=3.9"
# ///
"""Unit tests for agent_exec_wave_plan.py.

Run with: uv run --quiet test_agent_exec_wave_plan.py
"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec_wave_plan as wp  # noqa: E402


def _write(dirpath, name, content):
    path = os.path.join(dirpath, name)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(content)
    return path


class PlanFixture:
    """Builds a plan.json (plus the spec/preamble files it points at) in a
    temp dir and returns the path, so tests only spell out what varies."""

    def __init__(self, tmpdir):
        self.dir = tmpdir

    def spec(self, pkg_id):
        return _write(self.dir, "specs/%s.md" % pkg_id, "# %s\n" % pkg_id)

    def preamble(self):
        return _write(self.dir, "preamble.md", "# preamble\n")

    def write_plan(self, packages, preamble=None, external_done=None):
        plan = {"packages": packages}
        if preamble is not None:
            plan["preamble"] = preamble
        if external_done is not None:
            plan["external_done"] = external_done
        path = os.path.join(self.dir, "plan.json")
        with open(path, "w") as f:
            json.dump(plan, f)
        return path

    def pkg(self, pkg_id, cls="standard", depends_on=None, files_owned=None, prio=None, spec=True):
        d = {"id": pkg_id, "cls": cls}
        d["spec"] = self.spec(pkg_id) if spec else "specs/%s.md" % pkg_id
        if depends_on is not None:
            d["depends_on"] = depends_on
        if files_owned is not None:
            d["files_owned"] = files_owned
        if prio is not None:
            d["prio"] = prio
        return d


class LoadPlanValidationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.fx = PlanFixture(self.tmp.name)

    def _plan_path(self, text):
        path = os.path.join(self.tmp.name, "plan.json")
        with open(path, "w") as f:
            f.write(text)
        return path

    def test_invalid_json(self):
        path = self._plan_path("{not json")
        with self.assertRaises(wp.PlanError):
            wp.load_plan(path)

    def test_missing_packages(self):
        path = self._plan_path(json.dumps({}))
        with self.assertRaises(wp.PlanError):
            wp.load_plan(path)

    def test_missing_id(self):
        pkg = self.fx.pkg("CORE-1")
        del pkg["id"]
        path = self.fx.write_plan([pkg])
        with self.assertRaisesRegex(wp.PlanError, "id"):
            wp.load_plan(path)

    def test_missing_spec(self):
        pkg = self.fx.pkg("CORE-1")
        del pkg["spec"]
        path = self.fx.write_plan([pkg])
        with self.assertRaisesRegex(wp.PlanError, "CORE-1"):
            wp.load_plan(path)

    def test_missing_cls(self):
        pkg = self.fx.pkg("CORE-1")
        del pkg["cls"]
        path = self.fx.write_plan([pkg])
        with self.assertRaisesRegex(wp.PlanError, "CORE-1"):
            wp.load_plan(path)

    def test_duplicate_id(self):
        path = self.fx.write_plan([self.fx.pkg("CORE-1"), self.fx.pkg("CORE-1")])
        with self.assertRaisesRegex(wp.PlanError, "CORE-1"):
            wp.load_plan(path)

    def test_bad_cls(self):
        pkg = self.fx.pkg("CORE-1", cls="medium")
        path = self.fx.write_plan([pkg])
        with self.assertRaisesRegex(wp.PlanError, "CORE-1"):
            wp.load_plan(path)

    def test_depends_on_unknown_id(self):
        pkg = self.fx.pkg("CORE-1", depends_on=["GHOST"])
        path = self.fx.write_plan([pkg])
        with self.assertRaisesRegex(wp.PlanError, "CORE-1"):
            wp.load_plan(path)

    def test_depends_on_external_done_is_ok(self):
        pkg = self.fx.pkg("CORE-1", depends_on=["EXT-1"])
        path = self.fx.write_plan([pkg], external_done=["EXT-1"])
        plan = wp.load_plan(path)
        self.assertEqual(plan["packages"][0]["depends_on"], ["EXT-1"])

    def test_dependency_cycle(self):
        a = self.fx.pkg("A", depends_on=["B"])
        b = self.fx.pkg("B", depends_on=["A"])
        path = self.fx.write_plan([a, b])
        with self.assertRaisesRegex(wp.PlanError, "A"):
            wp.load_plan(path)

    def test_spec_path_not_existing(self):
        pkg = self.fx.pkg("CORE-1", spec=False)
        path = self.fx.write_plan([pkg])
        with self.assertRaisesRegex(wp.PlanError, "CORE-1"):
            wp.load_plan(path)

    def test_preamble_path_not_existing(self):
        pkg = self.fx.pkg("CORE-1")
        path = self.fx.write_plan([pkg], preamble=["missing-preamble.md"])
        with self.assertRaises(wp.PlanError):
            wp.load_plan(path)

    def test_defaults_filled(self):
        pkg = {"id": "CORE-1", "spec": self.fx.spec("CORE-1"), "cls": "light"}
        path = self.fx.write_plan([pkg])
        plan = wp.load_plan(path)
        p = plan["packages"][0]
        self.assertEqual(p["prio"], 0)
        self.assertEqual(p["depends_on"], [])
        self.assertEqual(p["files_owned"], [])
        self.assertEqual(plan["preamble"], [])
        self.assertEqual(plan["external_done"], [])

    def test_relative_spec_path_resolved_against_plan_dir(self):
        pkg = self.fx.pkg("CORE-1", spec=False)  # spec="specs/CORE-1.md", not created yet
        self.fx.spec("CORE-1")
        path = self.fx.write_plan([pkg])
        plan = wp.load_plan(path)
        self.assertTrue(os.path.isabs(plan["packages"][0]["spec"]))
        self.assertTrue(os.path.isfile(plan["packages"][0]["spec"]))

    def test_relative_preamble_path_resolved_against_plan_dir(self):
        self.fx.preamble()
        pkg = self.fx.pkg("CORE-1")
        path = self.fx.write_plan([pkg], preamble=["preamble.md"])
        plan = wp.load_plan(path)
        self.assertTrue(os.path.isabs(plan["preamble"][0]))
        self.assertTrue(os.path.isfile(plan["preamble"][0]))

    def test_rules_are_loaded(self):
        pkg = self.fx.pkg("CORE-1", files_owned=["src/**"])
        path = self.fx.write_plan([pkg])
        with open(path) as f:
            raw = json.load(f)
        raw["rules"] = [{"if": "src/**", "require": ["tests/**"]}]
        with open(path, "w") as f:
            json.dump(raw, f)
        self.assertEqual(wp.load_plan(path)["rules"], raw["rules"])

    def test_invalid_rules_shape(self):
        pkg = self.fx.pkg("CORE-1")
        path = self.fx.write_plan([pkg])
        with open(path) as f:
            raw = json.load(f)
        raw["rules"] = [{"if": "src/**", "require": "tests/**"}]
        with open(path, "w") as f:
            json.dump(raw, f)
        with self.assertRaises(wp.PlanError):
            wp.load_plan(path)


class StemOverlapTests(unittest.TestCase):
    def test_stem_glob_star(self):
        self.assertEqual(wp.stem("src/**"), "src")

    def test_stem_plain_path(self):
        self.assertEqual(wp.stem("src/a.ts"), "src/a.ts")

    def test_stem_dot_slash_prefix(self):
        self.assertEqual(wp.stem("./src/a/b.ts"), "src/a/b.ts")

    def test_stem_trailing_slash(self):
        self.assertEqual(wp.stem("src/a/"), "src/a")

    def test_stem_empty_glob(self):
        self.assertEqual(wp.stem("*.md"), "")

    def test_overlap_glob_vs_file(self):
        self.assertTrue(wp.overlaps(["src/**"], ["src/a.ts"]))

    def test_overlap_prefix_no_boundary(self):
        self.assertFalse(wp.overlaps(["src/a"], ["src/ab"]))

    def test_overlap_dir_vs_dotslash_file(self):
        self.assertTrue(wp.overlaps(["src/a/"], ["./src/a/b.ts"]))

    def test_overlap_empty_stem_matches_anything(self):
        self.assertTrue(wp.overlaps(["*.md"], ["src/whatever/x.ts"]))

    def test_overlap_disjoint(self):
        self.assertFalse(wp.overlaps(["src/a/**"], ["lib/b/**"]))

    def test_overlap_equal_stem(self):
        self.assertTrue(wp.overlaps(["src/a.ts"], ["src/a.ts"]))


class PlanLintTests(unittest.TestCase):
    def test_deletion_literal_and_glob_ownership(self):
        plan = {
            "packages": [
                {"id": "DEL", "spec": self._spec("remove this\n"), "files_owned": ["src/a.py"]},
                {"id": "GLOB", "spec": self._spec("delete this\n"), "files_owned": ["src/**"]},
            ],
            "rules": [],
        }
        warnings = wp.lint_plan(plan)
        self.assertEqual([w["pkg"] for w in warnings], ["DEL"])
        self.assertIn("owns only literal paths", warnings[0]["message"])

    def test_deletion_english_and_japanese_positive_negative(self):
        plan = {
            "packages": [
                {"id": "JP", "spec": self._spec("機能を削除する\n"), "files_owned": ["a"]},
                {"id": "EN", "spec": self._spec("deprecate this\n"), "files_owned": ["b", "c"]},
                {"id": "OK", "spec": self._spec("remove this\n"), "files_owned": ["d/*"]},
            ],
            "rules": [],
        }
        self.assertEqual([w["pkg"] for w in wp.lint_plan(plan)], ["JP", "EN"])

    def test_unreadable_spec_is_ignored(self):
        plan = {"packages": [{"id": "X", "spec": "/no/such/spec", "files_owned": ["a"]}], "rules": []}
        self.assertEqual(wp.lint_plan(plan), [])

    def test_non_utf8_spec_is_ignored(self):
        fd, path = tempfile.mkstemp()
        with os.fdopen(fd, "wb") as f:
            f.write(b"\xff\xfe delete")
        self.addCleanup(os.remove, path)
        plan = {"packages": [{"id": "X", "spec": path, "files_owned": ["a"]}], "rules": []}
        self.assertEqual(wp.lint_plan(plan), [])

    def test_project_rule_missing_and_satisfied(self):
        plan = {
            "packages": [
                {"id": "BAD", "spec": self._spec("ok\n"), "files_owned": ["src/a.py"]},
                {"id": "OK", "spec": self._spec("ok\n"), "files_owned": ["src/b.py", "tests/**"]},
            ],
            "rules": [{"if": "src/**", "require": ["tests/**"]}],
        }
        warnings = wp.lint_plan(plan)
        self.assertEqual([w["pkg"] for w in warnings], ["BAD"])
        self.assertIn("tests/**", warnings[0]["message"])

    def _spec(self, text):
        import tempfile
        fd, path = tempfile.mkstemp()
        with os.fdopen(fd, "w") as f:
            f.write(text)
        return path


def _pkgs(*specs):
    """specs: list of (id, cls, depends_on, files_owned, prio)"""
    out = []
    for s in specs:
        pkg_id, cls, depends_on, files_owned, prio = (list(s) + [None] * 5)[:5]
        out.append(
            {
                "id": pkg_id,
                "cls": cls or "standard",
                "spec": "x",
                "depends_on": depends_on or [],
                "files_owned": files_owned or [],
                "prio": prio or 0,
            }
        )
    return out


class BlockedTests(unittest.TestCase):
    def test_blocked_via_failed_dependency(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        states = {"A": {"status": "failed"}, "B": {"status": "pending"}}
        self.assertEqual(wp.blocked(plan, states), {"B"})

    def test_blocked_transitive_two_levels(self):
        plan = {
            "packages": _pkgs(
                ("A", None, None, None, None),
                ("B", None, ["A"], None, None),
                ("C", None, ["B"], None, None),
            )
        }
        states = {"A": {"status": "failed"}, "B": {"status": "pending"}, "C": {"status": "pending"}}
        self.assertEqual(wp.blocked(plan, states), {"B", "C"})

    def test_blocked_via_needs(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        states = {"A": {"status": "needs"}, "B": {"status": "pending"}}
        self.assertEqual(wp.blocked(plan, states), {"B"})

    def test_not_blocked_when_dep_ok(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        states = {"A": {"status": "integrated"}, "B": {"status": "pending"}}
        self.assertEqual(wp.blocked(plan, states), set())

    def test_missing_from_states_counts_as_pending(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        self.assertEqual(wp.blocked(plan, {}), set())


class SelectTests(unittest.TestCase):
    def test_respects_dependencies(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        states = {"A": {"status": "pending"}, "B": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=10), ["A"])

    def test_dependency_integrated_unblocks(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        states = {"A": {"status": "integrated"}, "B": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=10), ["B"])

    def test_dependency_external_done_unblocks(self):
        plan = {"packages": _pkgs(("B", None, ["EXT"], None, None)), "external_done": ["EXT"]}
        states = {"B": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=10), ["B"])

    def test_prio_order(self):
        plan = {
            "packages": _pkgs(
                ("A", None, None, None, 0),
                ("B", None, None, None, 5),
                ("C", None, None, None, 1),
            )
        }
        states = {"A": {"status": "pending"}, "B": {"status": "pending"}, "C": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=10), ["B", "C", "A"])

    def test_plan_order_tiebreak(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, None, None, None))}
        states = {"A": {"status": "pending"}, "B": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=10), ["A", "B"])

    def test_overlap_with_in_flight_excluded(self):
        plan = {"packages": _pkgs(("A", None, None, ["src/a/**"], None), ("B", None, None, ["src/a/x.ts"], None))}
        states = {"A": {"status": "implementing"}, "B": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=10), [])

    def test_overlap_among_candidates_only_first_selected(self):
        plan = {
            "packages": _pkgs(
                ("A", None, None, ["src/a/**"], None),
                ("B", None, None, ["src/a/x.ts"], None),
            )
        }
        states = {"A": {"status": "pending"}, "B": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=10), ["A"])

    def test_max_in_flight_minus_in_flight(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, None, None, None), ("C", None, None, None, None))}
        states = {"A": {"status": "implementing"}, "B": {"status": "pending"}, "C": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=2), ["B"])

    def test_max_in_flight_never_negative(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, None, None, None))}
        states = {"A": {"status": "implementing"}, "B": {"status": "implementing"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=1), [])

    def test_max_in_flight_zero(self):
        plan = {"packages": _pkgs(("A", None, None, None, None))}
        states = {"A": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=0), [])

    def test_serialize_overlap_false_allows_both(self):
        plan = {
            "packages": _pkgs(
                ("A", None, None, ["src/a/**"], None),
                ("B", None, None, ["src/a/x.ts"], None),
            )
        }
        states = {"A": {"status": "pending"}, "B": {"status": "pending"}}
        self.assertEqual(
            wp.select(plan, states, max_in_flight=10, serialize_overlap=False), ["A", "B"]
        )

    def test_blocked_excluded_from_select(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        states = {"A": {"status": "failed"}, "B": {"status": "pending"}}
        self.assertEqual(wp.select(plan, states, max_in_flight=10), [])


class SummaryTests(unittest.TestCase):
    def test_counts_all_statuses_present(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, None, None, None))}
        states = {"A": {"status": "pending"}, "B": {"status": "integrated"}}
        s = wp.summary(plan, states)
        for status in (
            "pending",
            "implementing",
            "verifying",
            "fixing",
            "ready",
            "integrated",
            "needs",
            "failed",
            "blocked",
        ):
            self.assertIn(status, s)
        self.assertEqual(s["pending"], 1)
        self.assertEqual(s["integrated"], 1)
        self.assertEqual(s["total"], 2)

    def test_runnable_counts_select_with_huge_max(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        states = {"A": {"status": "pending"}, "B": {"status": "pending"}}
        s = wp.summary(plan, states)
        self.assertEqual(s["runnable"], 1)


class TopoOrderTests(unittest.TestCase):
    def test_deterministic_ties_by_plan_order(self):
        plan = {"packages": _pkgs(("B", None, None, None, None), ("A", None, None, None, None))}
        self.assertEqual(wp.topo_order(plan), ["B", "A"])

    def test_respects_dependencies(self):
        plan = {"packages": _pkgs(("A", None, None, None, None), ("B", None, ["A"], None, None))}
        self.assertEqual(wp.topo_order(plan), ["A", "B"])

    def test_chain(self):
        plan = {
            "packages": _pkgs(
                ("C", None, ["B"], None, None),
                ("B", None, ["A"], None, None),
                ("A", None, None, None, None),
            )
        }
        self.assertEqual(wp.topo_order(plan), ["A", "B", "C"])


if __name__ == "__main__":
    unittest.main()
