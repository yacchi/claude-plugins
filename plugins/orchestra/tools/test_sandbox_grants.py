"""Tests for sandbox-denial detection, auto-grants, the learned grants store,
the grant + resume loop, and tamper protection (agent_exec_sandbox.py and
its plumbing in agent_exec.py)."""

import io
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_exec  # noqa: E402
import agent_exec_sandbox as sb  # noqa: E402


def _tool_end(text):
    return {"type": "tool_execution_end", "toolName": "bash",
            "result": {"content": [{"type": "text", "text": text}], "isError": True},
            "isError": True}


class HomeCase(unittest.TestCase):
    """A throwaway $HOME so grants never touch the real learned store."""

    def setUp(self):
        self.home = os.path.realpath(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.home, True)
        patcher = mock.patch.dict(os.environ, {"HOME": self.home})
        patcher.start()
        self.addCleanup(patcher.stop)
        self.tree = os.path.join(self.home, "work", "tree")
        os.makedirs(self.tree)

    def policy(self, writable=None, deny_read=None):
        return {"writable": writable if writable is not None else [self.tree],
                "deny_read": deny_read or [], "deny_write": [],
                "deny_write_regex": []}

    def h(self, rel):
        return os.path.join(self.home, rel)


class DetectDenialsTests(HomeCase):
    # Shapes captured from real runs under the P2b Seatbelt sandbox (BSD
    # mkdir/touch, Go `go env -w` / GOMODCACHE, Python open(), npm cache),
    # plus the Go EROFS shape bwrap produces.
    def _detect(self, text, **kw):
        return sb.detect_denials([_tool_end(text)], "", self.tree,
                                 kw.get("policy") or self.policy())

    def test_real_shapes(self):
        target = self.h(".pub-cache/probe")
        shapes = {
            "bsd": "mkdir: %s: Operation not permitted" % target,
            "go-open": "go: writing go env config: open %s: operation not permitted" % target,
            "go-mkdir": "go: could not create module cache: mkdir %s: operation not permitted" % target,
            "go-erofs": "mkdir %s: read-only file system" % target,
            "python": "PermissionError: [Errno 1] Operation not permitted: '%s'" % target,
            "node": "npm error FetchError: Invalid response body while trying to fetch "
                    "https://registry.npmjs.org/left-pad: EPERM: operation not permitted, "
                    "mkdir '%s'" % target,
        }
        for name, line in shapes.items():
            with self.subTest(name):
                got = self._detect(line)
                self.assertEqual([d["path"] for d in got], [target])
                self.assertEqual(got[0]["op"], "write")

    def test_real_linux_coreutils_shapes(self):
        # GNU coreutils under bwrap (EROFS) and Landlock (EACCES), captured
        # in tools/dev/linux-sandbox: ASCII quotes in the C locale, U+2018/
        # U+2019 under a UTF-8 locale (the default on a Linux desktop).
        target = self.h(".pub-cache/probe")
        shapes = {
            "touch-c-erofs": "touch: cannot touch '%s': Read-only file system" % target,
            "touch-c-eacces": "touch: cannot touch '%s': Permission denied" % target,
            "mkdir-utf8-erofs": "mkdir: cannot create directory ‘%s’: "
                                "Read-only file system" % target,
            "touch-utf8-eacces": "touch: cannot touch ‘%s’: Permission denied" % target,
            "bash-redirect": "bash: line 1: %s: Read-only file system" % target,
        }
        for name, line in shapes.items():
            with self.subTest(name):
                got = self._detect(line)
                self.assertEqual([d["path"] for d in got], [target])
                self.assertEqual(got[0]["op"], "write")

    def test_stderr_is_a_source_and_relative_paths_resolve_against_cwd(self):
        outside = self.h("work/elsewhere")
        got = sb.detect_denials([], "mkdir: ../elsewhere: Operation not permitted\n",
                                self.tree, self.policy())
        self.assertEqual([(d["path"], d["source"]) for d in got], [(outside, "stderr")])

    def test_denial_inside_writable_set_is_ignored(self):
        self.assertEqual(self._detect(
            "mkdir: %s/x: Operation not permitted" % self.tree), [])

    def test_marker_without_a_path_is_ignored(self):
        self.assertEqual(self._detect("Error: permission denied by policy"), [])

    def test_ordinary_permission_error_is_ignored(self):
        # `find /` hitting a root-only dir, or a read the sandbox never denies.
        self.assertEqual(self._detect("touch: /private/var/db/x: Permission denied"), [])
        self.assertEqual(self._detect("find: %s: Operation not permitted"
                                      % self.h("Library/Mail")), [])

    def test_deny_read_hit_is_intentional(self):
        ssh = self.h(".ssh")
        got = self._detect("cat: %s/id_rsa: Operation not permitted" % ssh,
                           policy=self.policy(deny_read=[ssh]))
        self.assertEqual(got[0]["intentional"], True)

    def test_deduplicated_by_realpath(self):
        real = self.h(".pub-cache/a")
        text = ("mkdir: %s: Operation not permitted\n"
                "mkdir: %s/../a: Operation not permitted\n" % (real, real))
        self.assertEqual(len(self._detect(text)), 1)


class GrantForTests(HomeCase):
    def test_root_plus_one_component(self):
        self.assertEqual(sb.grant_for(self.h(".cache/uv/archive-v0/x")), self.h(".cache/uv"))
        self.assertEqual(sb.grant_for(self.h(".pub-cache")), self.h(".pub-cache"))

    def test_direct_file_inside_root_grants_root(self):
        self.assertEqual(sb.grant_for(self.h(".pnpm-store/x.txt")), self.h(".pnpm-store"))

    def test_prefix_confusion_and_dotdot(self):
        self.assertIsNone(sb.grant_for(self.h(".cache-evil/x")))
        self.assertIsNone(sb.grant_for(self.h(".cache/../.ssh/x")))
        self.assertIsNone(sb.grant_for(self.h(".local/share/mise/installs/x")))

    def test_uv_installed_tools_cannot_be_reopened(self):
        tools = self.h(".local/share/uv/tools")
        self.assertIsNone(sb.grant_for(tools + "/tool/bin"))
        for path in (self.h(".local/share"), tools, tools + "/tool"):
            with self.subTest(path), self.assertRaises(ValueError):
                sb.add_grant(path, "user")
        for platform in ("darwin", "linux"):
            policy = sb.build_policy(self.tree, "pi", {"allow_write": ["~/.local/share"]},
                                     home=self.home, env={}, platform=platform)
            self.assertIn(tools, policy["deny_write"])
        landlock, _ = sb.landlock_policy(dict(policy, writable=policy["writable"] + [tools + "/x"]),
                                         self.tree)
        self.assertNotIn(self.h(".local/share"), landlock["writable"])
        self.assertNotIn(tools + "/x", landlock["writable"])
        rc = sb.sandbox_exec_main(
            ["--write", tools + "/x", "--protect", tools, "--", "true"],
            restrict=lambda w: self.fail("restricted"), execvp=lambda *a: self.fail("ran"))
        self.assertEqual(rc, 126)

    def test_sandbox_allow_refuses_uv_installed_tools(self):
        err = io.StringIO()
        with mock.patch("sys.stderr", err), mock.patch("sys.stdout", io.StringIO()):
            rc = agent_exec.cmd_sandbox(["allow", self.h(".local/share/uv/tools")])
        self.assertEqual(rc, 1)
        self.assertIn("tamper-protected", err.getvalue())
        self.assertFalse(os.path.exists(sb.learned_path()))

    def test_symlink_out_of_a_cache_root_is_rejected(self):
        os.makedirs(self.h(".cache"))
        os.makedirs(self.h(".ssh"))
        os.symlink(self.h(".ssh"), self.h(".cache/x"))
        self.assertIsNone(sb.grant_for(self.h(".cache/x/authorized_keys")))


class LearnedStoreTests(HomeCase):
    def test_store_defaults_to_local_state_and_honors_absolute_xdg_state_home(self):
        self.assertEqual(sb.learned_path(), self.h(".local/state/orchestra/sandbox-learned.json"))
        xdg = self.h("state")
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": xdg}):
            self.assertEqual(sb.learned_path(), os.path.join(xdg, "orchestra", "sandbox-learned.json"))
            self.assertIn(os.path.join(xdg, "orchestra"), sb.tamper_paths())
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": "relative-state"}):
            self.assertEqual(sb.learned_path(), self.h(".local/state/orchestra/sandbox-learned.json"))

    def test_local_tree_does_not_block_pnpm_cache_grants(self):
        pnpm = self.h(".local/share/pnpm/store")
        self.assertEqual(sb.grant_for(pnpm + "/v3/package"),
                         self.h(".local/share/pnpm/store/v3"))
        local = self.h("." + ".local")
        policy = sb.build_policy(self.tree, "pi", {"allow_write": [local]},
                                 home=self.home, env={})
        self.assertNotIn(local, policy["deny_write"])
        err = io.StringIO()
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": ""}), \
                mock.patch.object(agent_exec, "resolve_config", return_value=(agent_exec.DEFAULTS, None)), \
                mock.patch("sys.stderr", err), mock.patch("sys.stdout", io.StringIO()):
            rc = agent_exec.cmd_sandbox(["allow", "~/" + ".local"])
        self.assertEqual(rc, 1)
        self.assertIn("tamper-protected", err.getvalue())

    def test_atomic_0600_and_roundtrip(self):
        sb.add_grant(self.h(".cache/uv"), "auto", self.h(".cache/uv/x"))
        path = sb.learned_path()
        self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
        grants, warning = sb.load_learned()
        self.assertIsNone(warning)
        self.assertEqual([g["path"] for g in grants], [self.h(".cache/uv")])
        self.assertEqual(os.listdir(os.path.dirname(path)), ["sandbox-learned.json"])

    def test_corrupt_file_is_tolerated_with_a_warning(self):
        os.makedirs(os.path.dirname(sb.learned_path()))
        with open(sb.learned_path(), "w") as f:
            f.write("{not json")
        grants, warning = sb.load_learned()
        self.assertEqual(grants, [])
        self.assertIn("unreadable", warning)
        # A run still builds its policy.
        sb.build_policy(self.tree, "pi", {}, home=self.home, env={})

    def test_invalid_entries_are_not_trusted(self):
        os.makedirs(os.path.dirname(sb.learned_path()))
        with open(sb.learned_path(), "w") as f:
            json.dump({"version": 1, "grants": [
                {"path": self.h(".config"), "source": "auto"},
                {"path": self.home, "source": "user"},
                {"path": self.h(".claude"), "source": "user"},
            ]}, f)
        grants, warning = sb.load_learned()
        self.assertEqual(grants, [])
        self.assertIn("ignored 3", warning)

    def test_auto_grant_must_be_eligible_and_user_grant_refusals(self):
        with self.assertRaises(ValueError):
            sb.add_grant(self.h(".config/x"), "auto")
        for bad in ("/", self.home, self.h(".claude/x"), self.h(".ssh/x")):
            with self.subTest(bad), self.assertRaises(ValueError):
                sb.add_grant(bad, "user")
        sb.add_grant(self.h(".config/x"), "user")

    def test_grants_join_the_writable_set(self):
        sb.add_grant(self.h(".pub-cache/p"), "auto")
        policy = sb.build_policy(self.tree, "pi", {}, home=self.home, env={})
        self.assertIn(self.h(".pub-cache/p"), policy["writable"])

    def test_root_grant_is_built_by_every_backend_policy(self):
        root = self.h(".cache")
        sb.add_grant(root, "auto")
        base = sb.build_policy(self.tree, "pi", {}, home=self.home, env={})
        self.assertIn(root, base["writable"])
        seatbelt, _ = sb.seatbelt_profile(base)
        self.assertIn(root, [v for _k, v in sb.seatbelt_profile(base)[1]])
        landlock, _limits = sb.landlock_policy(base, self.tree)
        self.assertIn(root, landlock["writable"])
        self.assertIn(root, sb._bwrap_argv(["pi"], self.tree, base,
                                           exists=lambda p: True)[::3] +
                      sb._bwrap_argv(["pi"], self.tree, base,
                                     exists=lambda p: True))


class TamperProtectionTests(HomeCase):
    def test_denies_win_over_every_allow_in_sbpl(self):
        policy = sb.build_policy(self.tree, "pi",
                                 {"allow_write": ["~/.claude", "~/.pi"]},
                                 home=self.home, env={}, platform="darwin")
        sbpl, params = sb.seatbelt_profile(policy)
        values = dict(params)
        last = sbpl.rsplit("(deny file-write*", 1)[1]
        for path in (sb.learned_path(self.home), self.h(".claude/orchestra.yaml"),
                     self.h(".pi/agent/extensions"), self.h(".pi/agent/settings.json")):
            key = [k for k, v in values.items() if v == path and k.startswith("D")]
            self.assertTrue(key, path)
            self.assertIn('(param "%s")' % key[0], last)

    def test_landlock_drops_roots_holding_tamper_files_and_reports_limits(self):
        policy = sb.build_policy(self.tree, "pi", {"allow_write": ["~/.claude"]},
                                 home=self.home, env={}, platform="linux")
        out, limits = sb.landlock_policy(policy, self.tree)
        self.assertNotIn(self.h(".claude"), out["writable"])
        self.assertIn(self.h(".pi/agent"), out["writable"])
        self.assertIn("pi-config", limits)

    def test_landlock_child_asserts_no_root_contains_a_protected_file(self):
        rc = sb.sandbox_exec_main(
            ["--write", self.h(".local"), "--protect", sb.learned_path(), "--", "true"],
            restrict=lambda w: self.fail("restricted"), execvp=lambda *a: self.fail("ran"))
        self.assertEqual(rc, 126)

    @unittest.skipUnless(sys.platform == "darwin" and os.path.exists(sb.SANDBOX_EXEC),
                         "needs macOS sandbox-exec")
    def test_seatbelt_blocks_tamper_and_pi_config_writes(self):
        os.makedirs(self.h(".pi/agent"))
        os.makedirs(self.h(".claude/orchestra"))
        with open(self.h(".pi/agent/settings.json"), "w") as f:
            f.write("{}")
        policy = sb.build_policy(self.tree, "pi", {"allow_write": ["~/.claude"]},
                                 home=self.home, env={}, platform="darwin")
        spec = {"backend": "seatbelt", "policy": policy}

        def can_write(path):
            argv = sb.wrap_argv(["/bin/sh", "-c", 'echo x >> "$1"', "sh", path],
                                self.tree, spec)
            return subprocess.run(argv, capture_output=True).returncode == 0

        self.assertTrue(can_write(self.h(".pi/agent/settings.json.lock")))
        self.assertFalse(can_write(self.h(".pi/agent/settings.json")))
        self.assertFalse(os.path.exists(self.h(".pi/agent/extensions")))
        argv = sb.wrap_argv(["/bin/mkdir", "-p", self.h(".pi/agent/extensions")],
                            self.tree, spec)
        self.assertNotEqual(subprocess.run(argv, capture_output=True).returncode, 0)
        self.assertFalse(can_write(sb.learned_path()))
        self.assertTrue(can_write(self.h(".claude/orchestra/other.json")))
        self.assertFalse(can_write(self.h(".local/share/uv/tools/x")))


class ChildEnvTests(unittest.TestCase):
    def test_uv_tool_dir_goes_to_temp_only_when_sandboxed_and_unset(self):
        spec = {"backend": "seatbelt", "policy": {}}
        env = sb.child_env(spec, {"TMPDIR": "/tmp"})
        self.assertEqual(env["UV_TOOL_DIR"],
                         os.path.join(os.path.realpath("/tmp"), "orchestra-uv-tools"))
        self.assertEqual(sb.child_env(spec, {"UV_TOOL_DIR": "/x"})["UV_TOOL_DIR"], "/x")
        self.assertNotIn("UV_TOOL_DIR", sb.child_env({"backend": "none"}, {}))


def _pi(status="ok", answer="done", sid="s1", texts=(), tokens=10):
    return 0, {
        "status": status, "answer": answer, "session_id": sid, "resumed": False,
        "reason": None, "exit_code": 0,
        "usage": {"tokens": {"input_tokens": tokens}, "cost_micro_usd": tokens},
        "_denial_input": {"events": [_tool_end(t) for t in texts], "stderr": ""},
    }


class GrantLoopTests(HomeCase):
    def setUp(self):
        super().setUp()
        self.spec = {"backend": "seatbelt", "enforced": True, "reason": "", "limits": [],
                     "writable": [self.tree], "refuse": False,
                     "policy": self.policy()}
        self.calls = []

    def run_loop(self, results, backend=None):
        results = list(results)
        spec = dict(self.spec, backend=backend or self.spec["backend"])

        def fake(profile, model, effort, workdir, prompt, resume, fmt, sandbox=None,
                 watchdog=None, cls="standard"):
            self.calls.append((prompt, resume))
            return results.pop(0)

        with mock.patch.object(agent_exec, "_run_executor_capture", fake), \
                mock.patch.object(agent_exec, "_sandbox_spec", return_value=spec):
            return agent_exec._capture_with_grants(
                "pi", "m", "low", self.tree, "task", None, spec, self.tree, {})[1]

    def deny(self, rel):
        return "mkdir: %s: Operation not permitted" % self.h(rel)

    def test_safe_denial_is_granted_persisted_and_resumed_in_the_same_session(self):
        r = self.run_loop([_pi(texts=[self.deny(".pub-cache/p/x")]), _pi(tokens=5)])
        self.assertEqual(r["status"], "ok")
        self.assertEqual(r["sandbox"]["cycles"], 1)
        self.assertEqual(r["sandbox"]["granted"], [self.h(".pub-cache/p")])
        self.assertEqual(self.calls[1][1], "s1")
        self.assertIn(self.h(".pub-cache/p"), self.calls[1][0])
        self.assertEqual(r["usage"]["tokens"]["input_tokens"], 15)
        self.assertEqual(r["usage"]["cost_micro_usd"], 15)
        self.assertNotIn("_denial_input", r)
        self.assertEqual([g["path"] for g in sb.load_learned()[0]], [self.h(".pub-cache/p")])

    def test_mixed_set_persists_the_safe_one_and_does_not_resume(self):
        r = self.run_loop([_pi(answer="mkdir failed: Operation not permitted",
                               texts=[self.deny(".pub-cache/p/x"), self.deny(".config/q")])])
        self.assertEqual(r["status"], "ok")
        self.assertEqual(len(self.calls), 1)
        by_path = {d["path"]: d for d in r["sandbox"]["denials"]}
        self.assertTrue(by_path[self.h(".pub-cache/p/x")]["granted"])
        self.assertFalse(by_path[self.h(".config/q")]["auto"])
        self.assertEqual([g["path"] for g in sb.load_learned()[0]], [self.h(".pub-cache/p")])

    def test_a_run_that_worked_around_the_denial_stays_ok(self):
        r = self.run_loop([_pi(answer="wrote it to the repo instead",
                               texts=[self.deny(".config/q"), "ok"])])
        self.assertEqual(r["status"], "ok")
        self.assertEqual(len(r["sandbox"]["denials"]), 1)

    def test_cycle_cap(self):
        r = self.run_loop([_pi(texts=[self.deny(".cache/a/x")]),
                           _pi(texts=[self.deny(".cache/b/x")]),
                           _pi(texts=[self.deny(".cache/c/x")])])
        self.assertEqual(len(self.calls), 3)
        self.assertEqual(r["sandbox"]["cycles"], 2)
        self.assertEqual(r["status"], "ok")
        self.assertEqual(r["sandbox"]["grant_note"], "granted; not resumed: cycle-cap")

    def test_grant_without_a_session_is_noted_not_resumed(self):
        r = self.run_loop([_pi(sid=None, texts=[self.deny(".cache/a/x")])])
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(r["status"], "ok")
        self.assertEqual(r["sandbox"]["grant_note"], "granted; not resumed: no-session")

    def test_resumed_run_without_session_id_keeps_the_session(self):
        r = self.run_loop([_pi(texts=[self.deny(".cache/a/x")]),
                           _pi(status="error", sid=None)])
        self.assertEqual((r["status"], r["session_id"]), ("error", "s1"))
        self.assertNotIn("grant_note", r["sandbox"])

    def test_direct_file_grant_does_not_create_file_as_directory(self):
        path = self.h(".pnpm-store/probe.txt")
        os.makedirs(os.path.dirname(path))
        with open(path, "w") as fh:
            fh.write("x")
        r = self.run_loop([_pi(texts=[self.deny(".pnpm-store/probe.txt")]),
                           _pi(tokens=5)], backend="bwrap")
        self.assertEqual(r["sandbox"]["granted"], [self.h(".pnpm-store")])
        self.assertTrue(os.path.isfile(path))

    def test_grant_that_does_not_resolve_stops(self):
        r = self.run_loop([_pi(texts=[self.deny(".cache/a/x")]),
                           _pi(answer="still Operation not permitted",
                               texts=[self.deny(".cache/a/x")])])
        self.assertEqual(len(self.calls), 2)
        self.assertEqual(r["status"], "ok")
        self.assertEqual(r["sandbox"]["denials"][0]["why"], "grant did not resolve the denial")

    def test_ok_answer_with_unsafe_denial_stays_ok_with_advisory(self):
        r = self.run_loop([_pi(answer="DONE", texts=[self.deny(".config/outside")])])
        self.assertEqual(r["status"], "ok")
        denial = r["sandbox"]["denials"][0]
        self.assertFalse(denial["auto"])
        self.assertEqual(denial["grant_candidate"], self.h(".config/outside"))
        self.assertIn("why", denial)

    def test_failed_run_with_unsafe_denial_needs_permission(self):
        r = self.run_loop([_pi(status="error", answer="DONE",
                               texts=[self.deny(".config/outside")])])
        self.assertEqual((r["status"], r["reason"]), ("needs-permission", "sandbox"))
        self.assertFalse(r["sandbox"]["denials"][0]["auto"])


class NeedsPermissionDispatchTests(HomeCase):
    def test_not_unavailable_keeps_session_and_records_counts_only(self):
        prompt = os.path.join(self.home, "p.md")
        with open(prompt, "w") as f:
            f.write("x")
        cfg = json.loads(json.dumps(agent_exec.DEFAULTS))
        cfg["ledger"]["dir"] = os.path.join(self.home, "runs")
        cfg["telemetry"] = {"enabled": True, "dir": os.path.join(self.home, "tel")}
        result = {"status": "needs-permission", "reason": "sandbox", "answer": "a",
                  "session_id": "s9", "exit_code": 0, "usage": None,
                  "resumed": True,
                  "sandbox": {"backend": "seatbelt", "denials": [
                      {"path": self.h(".config/secret-name"), "auto": False}],
                      "granted": [], "cycles": 0}}
        buf = io.StringIO()
        route = {"dispatch": "cli", "executor": "pi", "model": "m", "effort": "low",
                 "agent_type": None}
        with mock.patch.object(agent_exec, "resolve_config", return_value=(cfg, None)), \
                mock.patch.object(agent_exec, "resolve_route", return_value=route), \
                mock.patch.object(agent_exec, "_build_doctor_report", return_value={}), \
                mock.patch.object(agent_exec, "_capture_with_grants",
                                  return_value=(0, result, {"backend": "seatbelt"})), \
                mock.patch.object(agent_exec, "record_unavailable_cooldown",
                                  side_effect=AssertionError("cooldown")), \
                mock.patch.object(sys, "stdout", buf):
            agent_exec.cmd_dispatch_route([
                "--class", "standard", "--prompt-file", prompt, "--workdir", self.tree,
                "--isolate", "never", "--task", "t1"])
        out = json.loads(buf.getvalue())
        self.assertEqual(out["status"], "needs-permission")
        self.assertTrue(out["resumed"])
        self.assertEqual(agent_exec.read_task_session(cfg, "pi", "t1")["session_id"], "s9")
        session_out = io.StringIO()
        with mock.patch.object(agent_exec, "resolve_config", return_value=(cfg, None)), \
                mock.patch.object(sys, "stdout", session_out):
            agent_exec.cmd_dispatch_route(["session", "--task", "t1", "--json"])
        session = json.loads(session_out.getvalue())
        self.assertEqual((session["executor"], session["session_id"]), ("pi", "s9"))
        dumped = ""
        for root in (cfg["ledger"]["dir"], cfg["telemetry"]["dir"]):
            for dirpath, _d, files in os.walk(root):
                for name in files:
                    with open(os.path.join(dirpath, name)) as f:
                        dumped += f.read()
        self.assertIn('"needs-permission"', dumped)
        self.assertIn('"sandbox_denials": 1', dumped)
        self.assertNotIn("secret-name", dumped)


if __name__ == "__main__":
    unittest.main()
