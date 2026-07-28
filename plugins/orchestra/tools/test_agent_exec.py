# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for the anonymized telemetry facility in agent_exec.py.

Run with: uv run test_agent_exec.py
"""

import copy
import io
import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402


class SanitizeTelemetryRecordTests(unittest.TestCase):
    def test_missing_event_returns_none(self):
        self.assertIsNone(agent_exec.sanitize_telemetry_record({}))
        self.assertIsNone(agent_exec.sanitize_telemetry_record({"event": "bogus"}))
        self.assertIsNone(agent_exec.sanitize_telemetry_record({"foo": "bar"}))

    def test_non_dict_input_returns_none(self):
        self.assertIsNone(agent_exec.sanitize_telemetry_record(None))
        self.assertIsNone(agent_exec.sanitize_telemetry_record("run_summary"))
        self.assertIsNone(agent_exec.sanitize_telemetry_record([1, 2, 3]))

    def test_forbidden_keys_are_stripped(self):
        raw = {
            "event": "dispatch",
            "prompt": "do the secret thing",
            "file": "/Users/me/secret.py",
            "path": "/Users/me/project",
            "task_id": "TASK-123",
            "summary": "did a thing",
            "error": "Traceback (most recent call last): ...",
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertIsNotNone(out)
        for forbidden in ("prompt", "file", "path", "task_id", "summary", "error"):
            self.assertNotIn(forbidden, out)

    def test_free_text_disguised_as_enum_value_is_dropped(self):
        raw = {
            "event": "dispatch",
            "reason": "quota: /Users/x/s.py",
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertIsNotNone(out)
        self.assertNotIn("reason", out)

    def test_valid_reason_is_kept(self):
        out = agent_exec.sanitize_telemetry_record({"event": "dispatch", "reason": "quota"})
        self.assertEqual(out["reason"], "quota")

    def test_stamped_fields(self):
        out = agent_exec.sanitize_telemetry_record({"event": "run_summary"})
        self.assertEqual(out["schema_version"], 1)
        self.assertIsInstance(out["ts"], str)
        self.assertTrue(len(out["ts"]) > 0)
        self.assertEqual(out["os"], out["os"].lower())
        import platform
        self.assertEqual(out["os"], platform.system().lower())

    def test_stamped_fields_ignore_caller_supplied_values(self):
        raw = {
            "event": "run_summary",
            "schema_version": 999,
            "ts": "1999-01-01T00:00:00Z",
            "os": "not-a-real-os",
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["schema_version"], 1)
        self.assertNotEqual(out["ts"], "1999-01-01T00:00:00Z")
        self.assertNotEqual(out["os"], "not-a-real-os")

    def test_orchestra_version_pattern(self):
        out = agent_exec.sanitize_telemetry_record(
            {"event": "run_summary", "orchestra_version": "1.2.3"}
        )
        self.assertEqual(out["orchestra_version"], "1.2.3")

        out2 = agent_exec.sanitize_telemetry_record(
            {"event": "run_summary", "orchestra_version": "1.2.3-dirty/path"}
        )
        self.assertNotIn("orchestra_version", out2)

    def test_lane_enum(self):
        out = agent_exec.sanitize_telemetry_record(
            {"event": "run_summary", "lane": "express"}
        )
        self.assertEqual(out["lane"], "express")
        out2 = agent_exec.sanitize_telemetry_record(
            {"event": "run_summary", "lane": "sideways"}
        )
        self.assertNotIn("lane", out2)

    def test_executor_and_cls_enum(self):
        out = agent_exec.sanitize_telemetry_record(
            {"event": "dispatch", "executor": "copilot", "cls": "light"}
        )
        self.assertEqual(out["executor"], "copilot")
        self.assertEqual(out["cls"], "light")

        out2 = agent_exec.sanitize_telemetry_record(
            {"event": "dispatch", "executor": "some-random-cli", "cls": "ultra"}
        )
        self.assertNotIn("executor", out2)
        self.assertNotIn("cls", out2)

    def test_status_enum(self):
        out = agent_exec.sanitize_telemetry_record({"event": "dispatch", "status": "ok"})
        self.assertEqual(out["status"], "ok")
        out2 = agent_exec.sanitize_telemetry_record(
            {"event": "dispatch", "status": "crashed horribly"}
        )
        self.assertNotIn("status", out2)

    def test_resumed_rejects_int_requires_real_bool(self):
        out = agent_exec.sanitize_telemetry_record({"event": "dispatch", "resumed": 1})
        self.assertNotIn("resumed", out)
        out2 = agent_exec.sanitize_telemetry_record({"event": "dispatch", "resumed": 0})
        self.assertNotIn("resumed", out2)
        out3 = agent_exec.sanitize_telemetry_record({"event": "dispatch", "resumed": True})
        self.assertIs(out3["resumed"], True)
        out4 = agent_exec.sanitize_telemetry_record({"event": "dispatch", "resumed": False})
        self.assertIs(out4["resumed"], False)

    def test_nonnegative_int_counters(self):
        raw = {
            "event": "run_summary",
            "task_count": 5,
            "pass": 4,
            "fail": -1,
            "exhausted": 0,
            "fallbacks": "2",
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["task_count"], 5)
        self.assertEqual(out["pass"], 4)
        self.assertNotIn("fail", out)
        self.assertEqual(out["exhausted"], 0)
        self.assertNotIn("fallbacks", out)

    def test_classes_subkey_filtering(self):
        raw = {
            "event": "run_summary",
            "classes": {"light": 3, "standard": 2, "bogus": 1, "deep": -1, "review": "x"},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["classes"], {"light": 3, "standard": 2})

    def test_classes_rejects_non_dict(self):
        out = agent_exec.sanitize_telemetry_record({"event": "run_summary", "classes": "oops"})
        self.assertNotIn("classes", out)

    def test_executors_used_subkey_filtering(self):
        raw = {
            "event": "run_summary",
            "executors_used": {"claude": 2, "copilot": 1, "gemini": 5, "codex": -3},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["executors_used"], {"claude": 2, "copilot": 1})

    def test_rounds_subkey_filtering(self):
        raw = {
            "event": "run_summary",
            "rounds": {"1": 3, "2": 1, "0": 5, "abc": 2, "3": -1, "07": 2},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["rounds"], {"1": 3, "2": 1})

    def test_external_enabled_filtering(self):
        raw = {
            "event": "run_summary",
            "external_enabled": {"copilot": True, "codex": False, "claude": True, "x": 1},
        }
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertEqual(out["external_enabled"], {"copilot": True, "codex": False})

    def test_unknown_keys_dropped(self):
        raw = {"event": "run_summary", "some_new_field": "value"}
        out = agent_exec.sanitize_telemetry_record(raw)
        self.assertNotIn("some_new_field", out)


class BuildDispatchRecordTests(unittest.TestCase):
    def test_shape_without_resume_or_cls(self):
        result = {
            "status": "ok",
            "answer": "the actual answer text",
            "session_id": "sess-1",
            "reason": None,
            "exit_code": 0,
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, None)
        self.assertEqual(record["event"], "dispatch")
        self.assertEqual(record["executor"], "copilot")
        self.assertEqual(record["status"], "ok")
        self.assertIsNone(record["reason"])
        self.assertIs(record["resumed"], False)
        self.assertNotIn("cls", record)
        self.assertNotIn("answer", record)

    def test_shape_with_resume_and_cls(self):
        result = {
            "status": "unavailable",
            "answer": None,
            "session_id": None,
            "reason": "quota",
            "exit_code": 1,
        }
        record = agent_exec.build_dispatch_record("copilot", result, "sess-abc", "standard")
        self.assertIs(record["resumed"], True)
        self.assertEqual(record["cls"], "standard")
        self.assertEqual(record["reason"], "quota")

    def test_sanitizes_cleanly_end_to_end(self):
        result = {
            "status": "ok",
            "answer": "leaked prompt text should never appear",
            "session_id": "sess-1",
            "reason": None,
            "exit_code": 0,
        }
        record = agent_exec.build_dispatch_record("copilot", result, None, "light")
        sanitized = agent_exec.sanitize_telemetry_record(record)
        self.assertIsNotNone(sanitized)
        self.assertNotIn("answer", sanitized)
        self.assertNotIn("session_id", sanitized)
        self.assertEqual(sanitized["executor"], "copilot")
        self.assertEqual(sanitized["cls"], "light")


class TelemetryAppendTests(unittest.TestCase):
    def _cfg(self, tmpdir, enabled):
        return {"telemetry": {"enabled": enabled, "dir": tmpdir}}

    def test_noop_when_disabled(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = self._cfg(tmpdir, False)
            agent_exec.telemetry_append({"event": "dispatch", "status": "ok"}, cfg)
            path = os.path.join(tmpdir, "records.jsonl")
            self.assertFalse(os.path.exists(path))

    def test_writes_when_enabled(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = self._cfg(tmpdir, True)
            agent_exec.telemetry_append({"event": "dispatch", "status": "ok"}, cfg)
            path = os.path.join(tmpdir, "records.jsonl")
            self.assertTrue(os.path.exists(path))
            with open(path, "r", encoding="utf-8") as f:
                lines = [line for line in f if line.strip()]
            self.assertEqual(len(lines), 1)
            record = json.loads(lines[0])
            self.assertEqual(record["event"], "dispatch")
            self.assertEqual(record["status"], "ok")

    def test_invalid_record_is_skipped(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = self._cfg(tmpdir, True)
            agent_exec.telemetry_append({"event": "not-a-real-event"}, cfg)
            path = os.path.join(tmpdir, "records.jsonl")
            self.assertFalse(os.path.exists(path))

    def test_never_raises_on_bad_cfg(self):
        # cfg is not a dict at all -> must not raise.
        try:
            agent_exec.telemetry_append({"event": "dispatch"}, None)
            agent_exec.telemetry_append({"event": "dispatch"}, "not-a-dict")
        except Exception as e:  # pragma: no cover - failure path
            self.fail("telemetry_append raised: %r" % e)

    def test_size_cap_enforced(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            cfg = self._cfg(tmpdir, True)
            path = os.path.join(tmpdir, "records.jsonl")
            os.makedirs(tmpdir, exist_ok=True)
            with open(path, "w", encoding="utf-8") as f:
                for i in range(10000):
                    f.write(json.dumps({"event": "dispatch", "task_count": i}) + "\n")
            agent_exec.telemetry_append({"event": "dispatch", "status": "ok"}, cfg)
            with open(path, "r", encoding="utf-8") as f:
                lines = [line for line in f if line.strip()]
            self.assertEqual(len(lines), 10000)
            last = json.loads(lines[-1])
            self.assertEqual(last["status"], "ok")


class SetTelemetryEnabledInTextTests(unittest.TestCase):
    def test_create_from_scratch(self):
        out = agent_exec.set_telemetry_enabled_in_text(None, True)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)

    def test_existing_content_preserves_comment_and_sibling_block(self):
        text = (
            "# my personal orchestra config\n"
            "tiers:\n"
            "  light: haiku\n"
            "  standard: sonnet\n"
            "\n"
            "telemetry:\n"
            "  enabled: false\n"
        )
        out = agent_exec.set_telemetry_enabled_in_text(text, True)
        self.assertIn("# my personal orchestra config", out)
        self.assertIn("tiers:\n  light: haiku\n  standard: sonnet\n", out)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)

    def test_inserts_enabled_when_block_has_no_child(self):
        text = "telemetry:\n  dir: ~/.claude/orchestra/telemetry\n"
        out = agent_exec.set_telemetry_enabled_in_text(text, True)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)
        self.assertEqual(data["telemetry"]["dir"], "~/.claude/orchestra/telemetry")

    def test_trailing_comment_on_enabled_line_preserved(self):
        text = "telemetry:\n  enabled: false   # keep me\n"
        out = agent_exec.set_telemetry_enabled_in_text(text, True)
        self.assertIn("# keep me", out)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)

    def test_disable_flips_true_to_false(self):
        text = "telemetry:\n  enabled: true\n"
        out = agent_exec.set_telemetry_enabled_in_text(text, False)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], False)

    def test_enable_twice_is_idempotent(self):
        text = "telemetry:\n  enabled: false\n"
        once = agent_exec.set_telemetry_enabled_in_text(text, True)
        twice = agent_exec.set_telemetry_enabled_in_text(once, True)
        self.assertEqual(once, twice)
        import yaml
        data = yaml.safe_load(twice)
        self.assertIs(data["telemetry"]["enabled"], True)
        self.assertEqual(twice.count("enabled:"), 1)

    def test_telemetry_only_in_comment_or_indented_is_not_treated_as_block(self):
        text = (
            "# telemetry: this is just a comment mentioning telemetry:\n"
            "other:\n"
            "  telemetry: not-a-real-block\n"
        )
        out = agent_exec.set_telemetry_enabled_in_text(text, True)
        self.assertIn("# telemetry: this is just a comment mentioning telemetry:", out)
        self.assertIn("  telemetry: not-a-real-block", out)
        import yaml
        data = yaml.safe_load(out)
        self.assertIs(data["telemetry"]["enabled"], True)
        # the fake indented "telemetry" under other: must remain untouched
        self.assertEqual(data["other"]["telemetry"], "not-a-real-block")


class TelemetryEnableDisableCommandTests(unittest.TestCase):
    def test_command_creates_user_scope_file(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            fake_home_claude = os.path.join(tmpdir, "home", ".claude")
            orig_expanduser = os.path.expanduser

            def fake_expanduser(p):
                if p == "~/.claude":
                    return fake_home_claude
                return orig_expanduser(p)

            agent_exec.os.path.expanduser = fake_expanduser
            try:
                rc = agent_exec._cmd_telemetry_toggle(True, [])
            finally:
                agent_exec.os.path.expanduser = orig_expanduser

            self.assertEqual(rc, 0)
            target = os.path.join(fake_home_claude, "orchestra.yaml")
            self.assertTrue(os.path.isfile(target))
            with open(target, "r", encoding="utf-8") as f:
                import yaml
                data = yaml.safe_load(f.read())
            self.assertIs(data["telemetry"]["enabled"], True)

    def test_command_rejects_bad_scope(self):
        rc = agent_exec._cmd_telemetry_toggle(True, ["--scope", "bogus"])
        self.assertEqual(rc, 2)


class ResolveRouteTests(unittest.TestCase):
    """Pure unit tests for resolve_route(): construct a cfg dict directly
    (starting from a deep copy of agent_exec.DEFAULTS, the same shape
    resolve_config() would produce) and a minimal doctor_report with just a
    "ready" section, and check the resolved route."""

    def _cfg(self):
        return copy.deepcopy(agent_exec.DEFAULTS)

    def _ready(self, ok, missing=None):
        return {"ok": ok, "missing": missing or []}

    def test_light_picks_copilot_when_ready(self):
        cfg = self._cfg()
        doctor_report = {"ready": {"copilot": self._ready(True)}}
        route = agent_exec.resolve_route(cfg, doctor_report, "light")
        self.assertEqual(route["executor"], "copilot")
        self.assertEqual(route["dispatch"], "cli")
        self.assertEqual(route["model"], "gpt-5.6-luna")
        self.assertEqual(route["effort"], "medium")
        self.assertEqual(route["source"], "priority")
        self.assertEqual(route["candidates"], ["copilot", "claude"])
        self.assertEqual(route["remaining"], ["claude"])
        self.assertEqual(route["skipped"], [])

    def test_light_falls_back_to_claude_when_copilot_not_ready(self):
        cfg = self._cfg()
        doctor_report = {
            "ready": {"copilot": self._ready(False, ["executor-binary-unavailable"])}
        }
        route = agent_exec.resolve_route(cfg, doctor_report, "light")
        self.assertEqual(route["executor"], "claude")
        self.assertEqual(route["dispatch"], "claude")
        self.assertEqual(route["model"], "haiku")
        self.assertEqual(
            route["skipped"],
            [{"executor": "copilot", "reason": "not-ready:executor-binary-unavailable"}],
        )
        self.assertEqual(route["remaining"], [])

    def test_light_falls_back_to_claude_when_copilot_disabled(self):
        cfg = self._cfg()
        cfg["external_executors"]["copilot"]["enabled"] = False
        route = agent_exec.resolve_route(cfg, {"ready": {}}, "light")
        self.assertEqual(route["executor"], "claude")
        self.assertEqual(route["skipped"], [{"executor": "copilot", "reason": "disabled"}])

    def test_light_falls_back_to_claude_when_class_policy_missing(self):
        cfg = self._cfg()
        del cfg["external_executors"]["copilot"]["class_policy"]["light"]
        doctor_report = {"ready": {"copilot": self._ready(True)}}
        route = agent_exec.resolve_route(cfg, doctor_report, "light")
        self.assertEqual(route["executor"], "claude")
        self.assertEqual(
            route["skipped"], [{"executor": "copilot", "reason": "no-class-policy"}]
        )

    def test_light_not_configured_when_no_external_executors_entry(self):
        cfg = self._cfg()
        del cfg["external_executors"]["copilot"]
        route = agent_exec.resolve_route(cfg, {"ready": {}}, "light")
        self.assertEqual(route["executor"], "claude")
        self.assertEqual(
            route["skipped"], [{"executor": "copilot", "reason": "not-configured"}]
        )

    def test_exhausted_copilot_falls_back_to_claude_with_correct_remaining(self):
        cfg = self._cfg()
        doctor_report = {"ready": {"copilot": self._ready(True)}}
        route = agent_exec.resolve_route(cfg, doctor_report, "light", exhausted=["copilot"])
        self.assertEqual(route["executor"], "claude")
        self.assertEqual(route["skipped"], [{"executor": "copilot", "reason": "exhausted"}])
        self.assertEqual(route["remaining"], [])

    def test_agent_dispatch_not_gated_on_binary_presence(self):
        # codex is dispatch: agent. Point `standard`'s priority straight at
        # it and give it NO ready entry at all (a real doctor report never
        # populates `ready` for dispatch: agent names) -- it must still win.
        cfg = self._cfg()
        cfg["priority"]["standard"]["default"] = ["codex"]
        route = agent_exec.resolve_route(cfg, {"ready": {}}, "standard")
        self.assertEqual(route["executor"], "codex")
        self.assertEqual(route["dispatch"], "agent")
        self.assertEqual(route["model"], "gpt-5.6-luna")
        self.assertEqual(route["effort"], "medium")
        self.assertEqual(route["agent_type"], "codex:codex-rescue")
        self.assertEqual(route["skipped"], [])

    def test_legacy_classes_scan_when_priority_absent(self):
        cfg = self._cfg()
        del cfg["priority"]
        doctor_report = {"ready": {"copilot": self._ready(True)}}
        route = agent_exec.resolve_route(cfg, doctor_report, "light")
        self.assertEqual(route["source"], "classes-legacy")
        self.assertEqual(route["executor"], "copilot")
        self.assertEqual(route["candidates"], ["copilot", "claude"])

    def test_review_resolves_to_claude_sonnet(self):
        cfg = self._cfg()
        route = agent_exec.resolve_route(cfg, {"ready": {}}, "review")
        self.assertEqual(route["executor"], "claude")
        self.assertEqual(route["dispatch"], "claude")
        self.assertEqual(route["model"], "sonnet")
        self.assertIsNone(route["effort"])
        self.assertEqual(route["candidates"], ["claude"])
        self.assertEqual(route["source"], "priority")

    def test_unroutable_when_only_candidate_is_exhausted(self):
        cfg = self._cfg()
        route = agent_exec.resolve_route(cfg, {"ready": {}}, "review", exhausted=["claude"])
        self.assertIsNone(route["executor"])
        self.assertIsNone(route["dispatch"])
        self.assertIsNone(route["model"])
        self.assertEqual(route["skipped"], [{"executor": "claude", "reason": "exhausted"}])
        self.assertEqual(route["remaining"], [])

    def test_light_unroutable_when_all_candidates_including_claude_exhausted(self):
        # Reproduces `agent-exec route --class light --exhausted
        # copilot,claude --json`: claude is normally the terminal fallback,
        # but the exhaustion check runs BEFORE the claude-is-always-fine
        # special case, so explicitly exhausting it too must still yield
        # unroutable rather than recursing/looping forever.
        cfg = self._cfg()
        doctor_report = {"ready": {"copilot": self._ready(True)}}
        route = agent_exec.resolve_route(
            cfg, doctor_report, "light", exhausted=["copilot", "claude"]
        )
        self.assertIsNone(route["executor"])
        self.assertIsNone(route["dispatch"])
        self.assertIsNone(route["model"])
        self.assertEqual(
            route["skipped"],
            [
                {"executor": "copilot", "reason": "exhausted"},
                {"executor": "claude", "reason": "exhausted"},
            ],
        )
        self.assertEqual(route["remaining"], [])


class _IsolatedConfigMixin:
    """Shared helper for tests that need a real resolve_config() call without
    touching the developer's actual dotfiles: redirect ~/.claude to an empty
    temp dir and cwd to a fresh temp project dir containing the given
    .claude/orchestra.yaml content (or no file at all if None)."""

    def _isolated_resolve(self, project_yaml_text):
        fake_home = tempfile.mkdtemp()
        tmp_project = tempfile.mkdtemp()
        orig_expanduser = os.path.expanduser
        orig_cwd = os.getcwd()

        def fake_expanduser(p):
            if p == "~/.claude":
                return os.path.join(fake_home, ".claude")
            return orig_expanduser(p)

        try:
            if project_yaml_text is not None:
                project_claude_dir = os.path.join(tmp_project, ".claude")
                os.makedirs(project_claude_dir, exist_ok=True)
                with open(
                    os.path.join(project_claude_dir, "orchestra.yaml"), "w", encoding="utf-8"
                ) as f:
                    f.write(project_yaml_text)
            agent_exec.os.path.expanduser = fake_expanduser
            os.chdir(tmp_project)
            return agent_exec.resolve_config()
        finally:
            os.chdir(orig_cwd)
            agent_exec.os.path.expanduser = orig_expanduser
            shutil.rmtree(fake_home, ignore_errors=True)
            shutil.rmtree(tmp_project, ignore_errors=True)


class ResolveConfigBackwardCompatTests(_IsolatedConfigMixin, unittest.TestCase):
    """Integration tests through resolve_config() to confirm the new
    always-present DEFAULTS (external_executors/priority) interact correctly
    with the deep-merge: a user override still wins, and omitting a key
    still means "inherit the new default", never "there is no default"."""

    def test_user_yaml_disabling_copilot_still_wins_over_new_default(self):
        resolved, err = self._isolated_resolve(
            "external_executors:\n"
            "  copilot:\n"
            "    enabled: false\n"
        )
        self.assertIsNone(err)
        self.assertIs(resolved["external_executors"]["copilot"]["enabled"], False)
        # the rest of the DEFAULTS copilot block (dispatch/classes/
        # class_policy) survives the merge untouched
        self.assertEqual(resolved["external_executors"]["copilot"]["dispatch"], "cli")
        self.assertEqual(
            resolved["external_executors"]["copilot"]["class_policy"]["light"]["model"],
            "gpt-5.6-luna",
        )

    def test_priority_omitted_inherits_new_default(self):
        resolved, err = self._isolated_resolve("tiers:\n  light: haiku\n")
        self.assertIsNone(err)
        self.assertEqual(resolved["priority"]["light"]["default"], ["copilot", "claude"])
        self.assertEqual(resolved["priority"]["review"]["default"], ["claude"])

    def test_no_config_at_all_still_gets_full_defaults(self):
        resolved, err = self._isolated_resolve(None)
        self.assertIsNone(err)
        self.assertIs(resolved["external_executors"]["copilot"]["enabled"], True)
        self.assertIs(resolved["external_executors"]["codex"]["enabled"], True)
        self.assertIn("priority", resolved)
        self.assertIn("enforcement", resolved)


class CooldownPureFunctionTests(unittest.TestCase):
    def _cfg(self, **seconds):
        cfg = copy.deepcopy(agent_exec.DEFAULTS)
        for key, value in seconds.items():
            cfg["cooldown"]["seconds"][key.replace("_", "-")] = value
        return cfg

    def test_cooldown_seconds_validation(self):
        cfg = self._cfg(rate_limit=12, quota=0, credits=-1, auth=True)
        self.assertEqual(agent_exec.cooldown_seconds_for(None, "rate-limit"), 0)
        disabled = self._cfg(rate_limit=12)
        disabled["cooldown"]["enabled"] = False
        self.assertEqual(agent_exec.cooldown_seconds_for(disabled, "rate-limit"), 0)
        self.assertEqual(agent_exec.cooldown_seconds_for(cfg, "unknown"), 0)
        self.assertEqual(agent_exec.cooldown_seconds_for(cfg, None), 0)
        self.assertEqual(agent_exec.cooldown_seconds_for(cfg, "rate-limit"), 12)
        self.assertEqual(agent_exec.cooldown_seconds_for(cfg, "quota"), 0)
        self.assertEqual(agent_exec.cooldown_seconds_for(cfg, "credits"), 0)
        self.assertEqual(agent_exec.cooldown_seconds_for(cfg, "auth"), 0)

    def test_apply_cooldown_is_copy_and_later_wins(self):
        cfg = self._cfg(rate_limit=10)
        state = {"copilot": {"reason": "quota", "until": 120}}
        original = copy.deepcopy(state)
        kept = agent_exec.apply_cooldown(state, "copilot", "rate-limit", 100, cfg)
        self.assertEqual(state, original)
        self.assertEqual(kept["copilot"]["reason"], "quota")
        overwritten = agent_exec.apply_cooldown(state, "copilot", "rate-limit", 200, cfg)
        self.assertEqual(overwritten["copilot"], {"reason": "rate-limit", "until": 210})
        self.assertEqual(agent_exec.apply_cooldown(state, "x", "auth", 100, cfg), state)
        self.assertEqual(agent_exec.apply_cooldown(state, 1, "rate-limit", 100, cfg), state)

    def test_active_cooldowns_filters_malformed_and_expired(self):
        state = {
            "expired": {"until": 10},
            "boundary": {"until": 100},
            "active": {"reason": "quota", "until": 101},
            "bad": {"until": True},
            "not-entry": "bad",
        }
        self.assertEqual(agent_exec.active_cooldowns(state, 100), {"active": state["active"]})
        self.assertEqual(agent_exec.active_cooldowns(None, 100), {})


class CooldownPersistenceTests(unittest.TestCase):
    def test_load_variants_never_raise(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            missing = os.path.join(tmpdir, "missing.json")
            self.assertEqual(agent_exec.load_cooldown_state(missing), {})
            invalid = os.path.join(tmpdir, "invalid.json")
            with open(invalid, "w", encoding="utf-8") as f:
                f.write("{")
            self.assertEqual(agent_exec.load_cooldown_state(invalid), {})
            with open(invalid, "w", encoding="utf-8") as f:
                f.write("[]")
            self.assertEqual(agent_exec.load_cooldown_state(invalid), {})
            with open(invalid, "w", encoding="utf-8") as f:
                json.dump({"copilot": {"reason": "quota", "until": 200}}, f)
            self.assertEqual(agent_exec.load_cooldown_state(invalid)["copilot"]["reason"], "quota")

    def test_save_round_trip_prunes_and_fails_open(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            path = os.path.join(tmpdir, "nested", "state.json")
            state = {
                "expired": {"reason": "quota", "until": 10},
                "active": {"reason": "quota", "until": 200},
            }
            self.assertTrue(agent_exec.save_cooldown_state(path, state, 100))
            self.assertNotIn("expired", agent_exec.load_cooldown_state(path))
            self.assertIn("active", agent_exec.load_cooldown_state(path))
            parent_file = os.path.join(tmpdir, "not-a-directory")
            with open(parent_file, "w", encoding="utf-8") as f:
                f.write("x")
            self.assertFalse(
                agent_exec.save_cooldown_state(
                    os.path.join(parent_file, "state.json"), state, 100
                )
            )


class ResolveRouteCooldownTests(unittest.TestCase):
    def _cfg(self):
        return copy.deepcopy(agent_exec.DEFAULTS)

    def test_none_preserves_route_except_new_fields(self):
        cfg = self._cfg()
        report = {"ready": {"copilot": {"ok": True, "missing": []}}}
        route = agent_exec.resolve_route(cfg, report, "light")
        self.assertEqual(route["executor"], "copilot")
        self.assertEqual(route["cooldowns_applied"], [])
        self.assertFalse(route["cooldown_bypassed"])

    def test_cooldown_falls_through_to_claude(self):
        cfg = self._cfg()
        report = {"ready": {"copilot": {"ok": True, "missing": []}}}
        route = agent_exec.resolve_route(
            cfg,
            report,
            "light",
            cooldowns={"copilot": {"reason": "rate-limit", "until": 200}},
        )
        self.assertEqual(route["executor"], "claude")
        self.assertEqual(route["skipped"], [{"executor": "copilot", "reason": "cooldown:rate-limit"}])
        self.assertEqual(route["cooldowns_applied"], [{"executor": "copilot", "reason": "rate-limit", "until": 200}])
        self.assertFalse(route["cooldown_bypassed"])

    def test_all_candidates_cooled_down_are_bypassed(self):
        cfg = self._cfg()
        cfg["priority"]["light"]["default"] = ["copilot"]
        report = {"ready": {"copilot": {"ok": True, "missing": []}}}
        route = agent_exec.resolve_route(
            cfg,
            report,
            "light",
            cooldowns={"copilot": {"reason": "quota", "until": 200}},
        )
        self.assertEqual(route["executor"], "copilot")
        self.assertTrue(route["cooldown_bypassed"])
        self.assertEqual(route["cooldowns_applied"], [])


class EnforcementLightClassNormalizationTests(_IsolatedConfigMixin, unittest.TestCase):
    """YAML 1.1 (what yaml.safe_load implements) parses a bareword
    off/on/yes/no as a bool, not a string. resolve_config() must normalize
    enforcement.light_class so every consumer (agent-exec config/doctor, the
    enforce-router hook) always sees one of the two real strings, never a
    stray bool -- and fails SAFE (-> "off") on anything ambiguous rather
    than ever silently meaning "block"."""

    def test_bareword_off_normalizes_to_string_off(self):
        # yaml.safe_load("light_class: off") -> False, not "off" -- this is
        # exactly the trap the shipped example config could fall into if a
        # user's own override copies that unquoted spelling.
        resolved, err = self._isolated_resolve(
            "enforcement:\n  light_class: off\n"
        )
        self.assertIsNone(err)
        self.assertEqual(resolved["enforcement"]["light_class"], "off")
        self.assertIsInstance(resolved["enforcement"]["light_class"], str)

    def test_explicit_block_string_is_preserved(self):
        resolved, err = self._isolated_resolve(
            "enforcement:\n  light_class: block\n"
        )
        self.assertIsNone(err)
        self.assertEqual(resolved["enforcement"]["light_class"], "block")

    def test_bareword_true_fails_safe_to_off_not_block(self):
        # A future on/yes typo (or any other bool) must never be silently
        # upgraded to "block" -- ambiguous always means "off".
        resolved, err = self._isolated_resolve(
            "enforcement:\n  light_class: true\n"
        )
        self.assertIsNone(err)
        self.assertEqual(resolved["enforcement"]["light_class"], "off")

    def test_unrecognized_string_normalizes_to_off(self):
        resolved, err = self._isolated_resolve(
            "enforcement:\n  light_class: banana\n"
        )
        self.assertIsNone(err)
        self.assertEqual(resolved["enforcement"]["light_class"], "off")

    def test_null_normalizes_to_off(self):
        resolved, err = self._isolated_resolve(
            "enforcement:\n  light_class:\n"
        )
        self.assertIsNone(err)
        self.assertEqual(resolved["enforcement"]["light_class"], "off")

    def test_default_with_no_override_is_off(self):
        resolved, err = self._isolated_resolve(None)
        self.assertIsNone(err)
        self.assertEqual(resolved["enforcement"]["light_class"], "off")


class ResolveRoutePathStubTests(unittest.TestCase):
    """Demonstrates, by actually stubbing PATH (not by hand-building a
    doctor_report), that a machine without the Copilot CLI resolves `light`
    to claude/haiku even though copilot ships enabled: true by default."""

    def test_machine_without_copilot_resolves_light_to_claude_haiku(self):
        fake_home = tempfile.mkdtemp()
        tmp_project = tempfile.mkdtemp()
        empty_path_dir = tempfile.mkdtemp()
        orig_expanduser = os.path.expanduser
        orig_cwd = os.getcwd()
        orig_path = os.environ.get("PATH")

        def fake_expanduser(p):
            if p == "~/.claude":
                return os.path.join(fake_home, ".claude")
            return orig_expanduser(p)

        try:
            agent_exec.os.path.expanduser = fake_expanduser
            os.chdir(tmp_project)
            # No copilot/codex/uv/agent-exec shim anywhere on PATH.
            os.environ["PATH"] = empty_path_dir

            resolved, err = agent_exec.resolve_config()
            self.assertIsNone(err)
            # Sanity: the new default really does enable copilot when
            # nothing says otherwise -- the rest of this test is what makes
            # that safe.
            self.assertIs(resolved["external_executors"]["copilot"]["enabled"], True)

            doctor_report = agent_exec._build_doctor_report()
            self.assertFalse(doctor_report["executors"]["copilot"]["available"])

            route = agent_exec.resolve_route(resolved, doctor_report, "light")

            self.assertEqual(route["executor"], "claude")
            self.assertEqual(route["dispatch"], "claude")
            self.assertEqual(route["model"], "haiku")
            skipped_names = [s["executor"] for s in route["skipped"]]
            self.assertIn("copilot", skipped_names)
        finally:
            os.chdir(orig_cwd)
            agent_exec.os.path.expanduser = orig_expanduser
            if orig_path is None:
                os.environ.pop("PATH", None)
            else:
                os.environ["PATH"] = orig_path
            shutil.rmtree(fake_home, ignore_errors=True)
            shutil.rmtree(tmp_project, ignore_errors=True)
            shutil.rmtree(empty_path_dir, ignore_errors=True)


class RouteCommandUsageTests(unittest.TestCase):
    def test_missing_class_is_usage_error(self):
        self.assertEqual(agent_exec.cmd_route([]), 2)

    def test_unknown_option_is_usage_error(self):
        self.assertEqual(agent_exec.cmd_route(["--bogus"]), 2)


class DispatchCommandUsageTests(unittest.TestCase):
    def test_missing_required_options_is_usage_error(self):
        self.assertEqual(agent_exec.cmd_dispatch_route([]), 2)
        self.assertEqual(agent_exec.cmd_dispatch_route(["--class", "light"]), 2)
        self.assertEqual(
            agent_exec.cmd_dispatch_route(["--class", "light", "--prompt-file", "x"]), 2
        )


class DispatchCommandBehaviorTests(unittest.TestCase):
    """Drives cmd_dispatch_route() directly with resolve_config()/
    _build_doctor_report() monkeypatched to controlled fixtures, so these
    tests are independent of the real machine's copilot/codex install."""

    def _run_with_stubbed_route(self, cfg, doctor_report, extra_args):
        orig_resolve_config = agent_exec.resolve_config
        orig_build_doctor_report = agent_exec._build_doctor_report
        agent_exec.resolve_config = lambda: (cfg, None)
        agent_exec._build_doctor_report = lambda: doctor_report
        try:
            with tempfile.TemporaryDirectory() as tmpdir:
                prompt_file = os.path.join(tmpdir, "prompt.txt")
                with open(prompt_file, "w", encoding="utf-8") as f:
                    f.write("do the thing")
                buf = io.StringIO()
                orig_stdout = sys.stdout
                sys.stdout = buf
                try:
                    rc = agent_exec.cmd_dispatch_route(
                        extra_args + ["--prompt-file", prompt_file, "--workdir", tmpdir]
                    )
                finally:
                    sys.stdout = orig_stdout
            return rc, buf.getvalue()
        finally:
            agent_exec.resolve_config = orig_resolve_config
            agent_exec._build_doctor_report = orig_build_doctor_report

    def test_delegate_status_does_not_spawn_subprocess_for_claude_route(self):
        cfg = copy.deepcopy(agent_exec.DEFAULTS)  # review -> [claude] only
        doctor_report = {"ready": {}}

        orig_subprocess_run = agent_exec.subprocess.run

        def boom(*a, **kw):
            raise AssertionError(
                "subprocess.run must not be called when dispatch resolves to claude"
            )

        agent_exec.subprocess.run = boom
        try:
            rc, out = self._run_with_stubbed_route(cfg, doctor_report, ["--class", "review"])
        finally:
            agent_exec.subprocess.run = orig_subprocess_run

        self.assertEqual(rc, 0)
        output = json.loads(out)
        self.assertEqual(output["status"], "delegate")
        self.assertEqual(output["executor"], "claude")
        self.assertEqual(output["model"], "sonnet")
        self.assertIsNone(output["agent_type"])
        self.assertIn("route", output)
        self.assertEqual(output["route"]["executor"], "claude")

    def test_delegate_status_for_agent_dispatch_does_not_spawn_subprocess(self):
        cfg = copy.deepcopy(agent_exec.DEFAULTS)
        cfg["priority"]["standard"]["default"] = ["codex"]
        doctor_report = {"ready": {}}

        orig_subprocess_run = agent_exec.subprocess.run

        def boom(*a, **kw):
            raise AssertionError(
                "subprocess.run must not be called when dispatch resolves to an agent"
            )

        agent_exec.subprocess.run = boom
        try:
            rc, out = self._run_with_stubbed_route(cfg, doctor_report, ["--class", "standard"])
        finally:
            agent_exec.subprocess.run = orig_subprocess_run

        self.assertEqual(rc, 0)
        output = json.loads(out)
        self.assertEqual(output["status"], "delegate")
        self.assertEqual(output["executor"], "codex")
        self.assertEqual(output["agent_type"], "codex:codex-rescue")
        self.assertEqual(output["route"]["dispatch"], "agent")

    def test_cli_route_surfaces_route_key_in_output(self):
        cfg = copy.deepcopy(agent_exec.DEFAULTS)
        doctor_report = {"ready": {"copilot": {"ok": True, "missing": []}}}

        orig_subprocess_run = agent_exec.subprocess.run
        orig_which = agent_exec.shutil.which

        class FakeProc:
            stdout = '{"type":"result","sessionId":"sess-xyz"}\n'
            stderr = ""
            returncode = 0

        def fake_run(argv, env=None, capture_output=None, text=None):
            return FakeProc()

        agent_exec.subprocess.run = fake_run
        agent_exec.shutil.which = (
            lambda name: "/usr/bin/copilot" if name == "copilot" else orig_which(name)
        )
        try:
            rc, out = self._run_with_stubbed_route(cfg, doctor_report, ["--class", "light"])
        finally:
            agent_exec.subprocess.run = orig_subprocess_run
            agent_exec.shutil.which = orig_which

        self.assertEqual(rc, 0)
        output = json.loads(out)
        self.assertEqual(output["status"], "ok")
        self.assertEqual(output["session_id"], "sess-xyz")
        self.assertEqual(output["executor"], "copilot")
        self.assertEqual(output["model"], "gpt-5.6-luna")
        self.assertEqual(output["effort"], "medium")
        self.assertIn("route", output)
        self.assertEqual(output["route"]["executor"], "copilot")
        self.assertEqual(output["route"]["remaining"], ["claude"])

    def test_unroutable_status(self):
        cfg = copy.deepcopy(agent_exec.DEFAULTS)
        doctor_report = {"ready": {}}
        orig_subprocess_run = agent_exec.subprocess.run

        def boom(*a, **kw):
            raise AssertionError("subprocess.run must not be called when unroutable")

        agent_exec.subprocess.run = boom
        try:
            rc, out = self._run_with_stubbed_route(
                cfg, doctor_report, ["--class", "review", "--exhausted", "claude"]
            )
        finally:
            agent_exec.subprocess.run = orig_subprocess_run

        self.assertEqual(rc, 0)
        output = json.loads(out)
        self.assertEqual(output["status"], "unroutable")
        self.assertIsNone(output["route"]["executor"])


class MainRoutingTests(unittest.TestCase):
    """Confirms `route`/`dispatch` are wired into main()'s explicit branch
    list and never fall through to the bare-profile passthrough dispatcher
    (cmd_dispatch), which would otherwise treat "route"/"dispatch" as an
    (unknown) profile name."""

    def test_dispatch_and_route_do_not_fall_through_to_profile_passthrough(self):
        calls = []
        orig_cmd_dispatch = agent_exec.cmd_dispatch

        def fake_cmd_dispatch(profile_name, args):
            calls.append(profile_name)
            return 0

        agent_exec.cmd_dispatch = fake_cmd_dispatch
        try:
            # Both are missing their required options, so they should hit
            # their own usage-error paths (exit 2) -- and, crucially, never
            # call cmd_dispatch at all.
            self.assertEqual(agent_exec.main(["dispatch"]), 2)
            self.assertEqual(agent_exec.main(["route"]), 2)
            self.assertEqual(calls, [])
        finally:
            agent_exec.cmd_dispatch = orig_cmd_dispatch


class ParseCopilotJsonlAvailabilityScanTests(unittest.TestCase):
    """The availability scan must read executor health signals only. A
    worker's own answer routinely contains the very words the patterns look
    for -- flagging those marks the executor exhausted for the whole run."""

    @staticmethod
    def _msg(content, phase="final_answer"):
        return json.dumps({
            "type": "assistant.message",
            "data": {"content": content, "phase": phase},
        })

    def _parse(self, stdout, stderr="", exit_code=0):
        return agent_exec.parse_copilot_jsonl(stdout, stderr, exit_code)

    def test_answer_text_never_triggers_unavailable(self):
        for phrase, label in [
            ("Here is the rate limit implementation you asked for.", "rate-limit"),
            ("The login flow now refreshes the token.", "auth"),
            ("Deducts one credit per premium request.", "quota/credits"),
            ("Returns 429 when the quota is exhausted.", "rate-limit/quota"),
        ]:
            with self.subTest(label=label):
                result = self._parse(self._msg(phrase))
                self.assertEqual(result["status"], "ok")
                self.assertIsNone(result["reason"])
                self.assertEqual(result["answer"], phrase)

    def test_non_final_answer_text_is_also_out_of_scope(self):
        result = self._parse(self._msg("rate limit", phase="thinking"))
        self.assertEqual(result["status"], "ok")
        self.assertIsNone(result["reason"])

    def test_stderr_still_triggers_unavailable(self):
        result = self._parse(self._msg("all good"), stderr="error: rate limit exceeded")
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "rate-limit")

    def test_non_assistant_event_still_triggers_unavailable(self):
        stdout = "\n".join([
            json.dumps({"type": "session.error", "data": {"message": "usage limit reached"}}),
            self._msg("partial work"),
        ])
        result = self._parse(stdout)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "quota")

    def test_unparseable_stdout_still_triggers_unavailable(self):
        result = self._parse("not logged in\n" + self._msg("hi"))
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "auth")

    def test_answer_and_session_id_survive_the_narrowed_scan(self):
        stdout = "\n".join([
            self._msg("draft about rate limit", phase="thinking"),
            self._msg("final about rate limit"),
            json.dumps({"type": "result", "sessionId": "sid-1"}),
        ])
        result = self._parse(stdout)
        self.assertEqual(result["answer"], "final about rate limit")
        self.assertEqual(result["session_id"], "sid-1")
        self.assertEqual(result["status"], "ok")

    def test_nonzero_exit_still_wins_when_output_is_clean(self):
        result = self._parse(self._msg("rate limit"), exit_code=1)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "nonzero-exit")


class ParseCopilotJsonlDefaultDenyRegressionTests(unittest.TestCase):
    """The reproduced production regression: a tool-call/tool-result event
    body containing an availability-pattern word, alongside a clean final
    answer and exit_code 0, must not be flagged. These are worker text
    (the prompt it was given, the file contents it is writing), not
    executor health -- and the default-deny scan must know the difference
    without losing genuine signal from stderr, unparseable stdout, or a
    real error-typed event."""

    @staticmethod
    def _final(content):
        return json.dumps({
            "type": "assistant.message",
            "data": {"content": content, "phase": "final_answer"},
        })

    @staticmethod
    def _tool_call(body):
        return json.dumps({
            "type": "tool.call",
            "data": {"name": "Write", "input": {"content": body}},
        })

    @staticmethod
    def _tool_result(body):
        return json.dumps({
            "type": "tool.result",
            "data": {"output": body},
        })

    def _parse(self, stdout, stderr="", exit_code=0):
        return agent_exec.parse_copilot_jsonl(stdout, stderr, exit_code)

    def test_tool_call_and_tool_result_bodies_never_trigger_unavailable(self):
        for phrase, label in [
            ("please avoid the word quota in the docs", "quota"),
            ("document our rate limit handling", "rate limit"),
            ("this API deducts one credit per call", "credits"),
            ("update the authenticate() helper", "authenticate"),
        ]:
            with self.subTest(label=label):
                stdout = "\n".join([
                    self._tool_call(phrase),
                    self._tool_result(phrase),
                    self._final("Done."),
                ])
                result = self._parse(stdout, exit_code=0)
                self.assertEqual(result["status"], "ok")
                self.assertIsNone(result["reason"])
                self.assertEqual(result["answer"], "Done.")

    def test_genuine_stderr_quota_error_still_detected(self):
        stdout = "\n".join([
            self._tool_call("mentions quota"),
            self._final("Done."),
        ])
        result = self._parse(stdout, stderr="fatal: quota exceeded", exit_code=0)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "quota")

    def test_genuine_error_typed_json_event_still_detected(self):
        stdout = "\n".join([
            json.dumps({"type": "session.error", "data": {"message": "not logged in"}}),
            self._final("Done."),
        ])
        result = self._parse(stdout, exit_code=0)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "auth")

    def test_result_event_with_explicit_failure_flag_is_detected(self):
        stdout = "\n".join([
            json.dumps({"type": "result", "success": False, "error": "insufficient credits"}),
            self._final("Done."),
        ])
        result = self._parse(stdout, exit_code=0)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "credits")

    def test_unparseable_plain_text_stdout_line_still_detected(self):
        stdout = "\n".join([
            "error: rate limit exceeded, try again later",
            self._final("Done."),
        ])
        result = self._parse(stdout, exit_code=0)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "rate-limit")

    def test_nonzero_exit_with_no_pattern_anywhere_is_unchanged(self):
        stdout = "\n".join([
            self._tool_call("nothing suspicious"),
            self._final("Done."),
        ])
        result = self._parse(stdout, exit_code=1)
        self.assertEqual(result["status"], "unavailable")
        self.assertEqual(result["reason"], "nonzero-exit")

    def test_bare_json_scalar_line_is_not_scanned(self):
        # A malformed/unexpected top-level JSON value (not an object) is
        # parseable JSON but has no `type` to judge -- default-deny means
        # it is neither treated as an error source nor crashes the parser.
        stdout = "\n".join([
            json.dumps(["quota", "exceeded"]),
            self._final("Done."),
        ])
        result = self._parse(stdout, exit_code=0)
        self.assertEqual(result["status"], "ok")
        self.assertIsNone(result["reason"])


class RecordUnavailableCooldownOutcomeGuardTests(unittest.TestCase):
    """`record_unavailable_cooldown` must refuse to persist a cooldown for a
    run that evidently succeeded (exit 0 + a real answer), no matter what
    `status`/`reason` upstream computed -- a belt-and-braces backstop for
    any future gap in the availability scan."""

    def _cfg(self, path):
        cfg = copy.deepcopy(agent_exec.DEFAULTS)
        cfg["cooldown"]["path"] = path
        cfg["cooldown"]["seconds"]["quota"] = 3600
        cfg["cooldown"]["seconds"]["rate-limit"] = 3600
        return cfg

    def test_exit_zero_with_real_answer_writes_nothing(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            path = os.path.join(tmpdir, "state.json")
            cfg = self._cfg(path)
            wrote = agent_exec.record_unavailable_cooldown(
                cfg, "copilot", "quota", 100, exit_code=0, answer="Here is the answer."
            )
            self.assertFalse(wrote)
            self.assertFalse(os.path.exists(path))

    def test_exit_nonzero_writes_cooldown(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            path = os.path.join(tmpdir, "state.json")
            cfg = self._cfg(path)
            wrote = agent_exec.record_unavailable_cooldown(
                cfg, "copilot", "quota", 100, exit_code=1, answer=None
            )
            self.assertTrue(wrote)
            self.assertIn("copilot", agent_exec.load_cooldown_state(path))

    def test_exit_zero_with_empty_answer_writes_cooldown(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            path = os.path.join(tmpdir, "state.json")
            cfg = self._cfg(path)
            for empty_answer in (None, "", "   "):
                with self.subTest(answer=repr(empty_answer)):
                    if os.path.exists(path):
                        os.remove(path)
                    wrote = agent_exec.record_unavailable_cooldown(
                        cfg, "copilot", "rate-limit", 100, exit_code=0, answer=empty_answer
                    )
                    self.assertTrue(wrote)
                    self.assertIn("copilot", agent_exec.load_cooldown_state(path))

    def test_omitted_exit_code_and_answer_behave_as_before(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            path = os.path.join(tmpdir, "state.json")
            cfg = self._cfg(path)
            wrote = agent_exec.record_unavailable_cooldown(cfg, "copilot", "quota", 100)
            self.assertTrue(wrote)
            self.assertIn("copilot", agent_exec.load_cooldown_state(path))


if __name__ == "__main__":
    unittest.main()
