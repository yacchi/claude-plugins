"""Tests for orchestra-managed tools and pi's dedicated agent dir
(agent_exec_tools.py and its plumbing in agent_exec.py / agent_exec_sandbox.py).
Every subprocess and PATH lookup is injected; nothing touches the network."""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_exec  # noqa: E402
import agent_exec_sandbox as sb  # noqa: E402
import agent_exec_tools as tools  # noqa: E402

SPEC = "npm:@earendil-works/pi-coding-agent"


def cfg_for(source="auto", version="1.2.0", installer="auto"):
    return {"tools": {"installer": installer,
                      "pi": {"source": source, "spec": SPEC, "version": version,
                             "bin": "pi"}}}


def defaults_copy():
    return json.loads(json.dumps(agent_exec.DEFAULTS))


class StateCase(unittest.TestCase):
    """Throwaway HOME/XDG so the state dir never touches the real one."""

    def setUp(self):
        self.home = os.path.realpath(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.home, True)
        patcher = mock.patch.dict(os.environ, {"HOME": self.home})
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop("XDG_STATE_HOME", None)
        os.environ.pop("PI_CODING_AGENT_DIR", None)

    def make_bin(self, rel):
        path = os.path.join(self.home, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as fh:
            fh.write("#!/bin/sh\n")
        os.chmod(path, 0o755)
        return path

    def record(self, path, version="1.2.0", spec=SPEC):
        tools.save_manifest({"version": 1, "tools": {"pi": {
            "spec": spec, "version": version, "installer": "mise",
            "path": path, "installed_at": "t"}}})


class ResolveTests(StateCase):
    def test_managed_never_falls_back_to_path(self):
        which = mock.Mock(return_value="/usr/bin/pi")
        r = tools.resolve("pi", cfg_for("managed"), which=which)
        self.assertIsNone(r["path"])
        self.assertEqual(r["reason"], "managed-not-installed")
        which.assert_not_called()

    def test_managed_stale_version_is_not_valid(self):
        self.record(self.make_bin("bin/pi"), version="1.1.0")
        r = tools.resolve("pi", cfg_for("managed"), which=lambda n: "/usr/bin/pi")
        self.assertIsNone(r["path"])
        self.assertEqual(r["reason"], "managed-stale")

    def test_auto_prefers_stale_managed_over_path(self):
        path = self.make_bin("bin/pi")
        self.record(path, version="1.1.0")
        r = tools.resolve("pi", cfg_for("auto"), which=lambda n: "/usr/bin/pi")
        self.assertEqual((r["path"], r["source"], r["reason"]),
                         (path, "managed", "managed-stale"))
        self.assertTrue(r["hint"])

    def test_auto_falls_back_to_path_with_hint(self):
        r = tools.resolve("pi", cfg_for("auto"), which=lambda n: "/usr/bin/pi")
        self.assertEqual((r["path"], r["source"]), ("/usr/bin/pi", "system"))
        self.assertTrue(r["hint"])

    def test_managed_record_with_missing_file_is_stale(self):
        self.record(os.path.join(self.home, "gone", "pi"))
        r = tools.resolve("pi", cfg_for("managed"), which=lambda n: None)
        self.assertEqual(r["reason"], "managed-stale")

    def test_corrupt_manifest_is_empty(self):
        os.makedirs(tools.tools_root())
        with open(tools.manifest_path(), "w") as fh:
            fh.write("{not json")
        self.assertEqual(tools.load_manifest()["tools"], {})


class InstallTests(StateCase):
    def runner(self, pi_path, fail_on=None):
        calls = []

        def run(argv, **kw):
            calls.append((argv, kw))
            if fail_on and argv[1] == fail_on:
                return subprocess.CompletedProcess(argv, 3, "", "boom " * 200)
            out = pi_path + "\n" if argv[1] == "exec" else ""
            return subprocess.CompletedProcess(argv, 0, out, "")
        run.calls = calls
        return run

    def test_mise_runs_in_isolation_and_records_manifest(self):
        pi = self.make_bin("mise-data/pi")
        run = self.runner(pi)
        res = tools.install(["pi"], cfg_for(), run=run,
                            which=lambda n: "/usr/bin/mise" if n == "mise" else None)
        self.assertEqual(res[0]["status"], "installed")
        self.assertEqual(res[0]["path"], pi)
        root = os.path.join(tools.tools_root(), "mise")
        for argv, kw in run.calls:
            self.assertEqual(kw["cwd"], os.path.join(root, "config"))
            env = kw["env"]
            self.assertEqual(env["MISE_DATA_DIR"], os.path.join(root, "data"))
            self.assertEqual(env["MISE_GLOBAL_CONFIG_FILE"],
                             os.path.join(root, "config", "config.toml"))
            self.assertEqual(env["MISE_CONFIG_DIR"], os.path.join(root, "config"))
            self.assertEqual(env["MISE_CEILING_PATHS"], os.path.join(root, "config"))
            self.assertEqual(env["MISE_YES"], "1")
            self.assertTrue(env["MISE_CACHE_DIR"].startswith(root))
            self.assertTrue(env["MISE_STATE_DIR"].startswith(root))
        self.assertEqual(run.calls[0][0], ["mise", "install", SPEC + "@1.2.0"])
        self.assertEqual(tools.load_manifest()["tools"]["pi"]["path"], pi)

    def test_failed_install_keeps_previous_manifest(self):
        old = self.make_bin("old/pi")
        self.record(old, version="1.1.0")
        before = open(tools.manifest_path()).read()
        res = tools.install(["pi"], cfg_for(), installer="mise",
                            run=self.runner(old, fail_on="install"))
        self.assertEqual(res[0]["status"], "failed")
        self.assertLessEqual(len(res[0]["error"]), 500)
        self.assertEqual(open(tools.manifest_path()).read(), before)

    def test_valid_record_is_already_without_subprocess(self):
        self.record(self.make_bin("bin/pi"))
        run = mock.Mock()
        res = tools.install(["pi"], cfg_for(), run=run)
        self.assertEqual(res[0]["status"], "already")
        run.assert_not_called()

    def test_npm_rejects_non_npm_spec(self):
        cfg = cfg_for()
        cfg["tools"]["pi"]["spec"] = "github:x/y"
        res = tools.install(["pi"], cfg, installer="npm", run=mock.Mock())
        self.assertEqual(res[0]["status"], "failed")
        self.assertEqual(res[0]["error"], "npm installer only supports npm: specs")

    def test_manifest_write_is_atomic_replace(self):
        with mock.patch.object(tools.os, "replace", wraps=os.replace) as rep:
            tools.save_manifest({"version": 1, "tools": {}})
        src, dst = rep.call_args[0]
        self.assertEqual(os.path.dirname(src), os.path.dirname(dst))
        self.assertEqual(os.listdir(os.path.dirname(dst)), ["manifest.json"])


class AgentDirTests(StateCase):
    def managed(self):
        return os.path.join(sb.state_dir(), "pi-agent")

    def test_regular_auth_json_is_never_replaced(self):
        d = self.managed()
        os.makedirs(d)
        auth = os.path.join(d, "auth.json")
        with open(auth, "w") as fh:
            fh.write("mine")
        src = os.path.join(self.home, ".pi", "agent")
        os.makedirs(src)
        with open(os.path.join(src, "auth.json"), "w") as fh:
            fh.write("theirs")
        tools.ensure_pi_agent_dir(d)
        self.assertFalse(os.path.islink(auth))
        self.assertEqual(open(auth).read(), "mine")

    def test_existing_symlink_is_kept(self):
        d = self.managed()
        os.makedirs(d)
        auth = os.path.join(d, "auth.json")
        os.symlink("/nonexistent/elsewhere.json", auth)
        src = os.path.join(self.home, ".pi", "agent")
        os.makedirs(src)
        with open(os.path.join(src, "auth.json"), "w") as fh:
            fh.write("x")
        tools.ensure_pi_agent_dir(d)
        self.assertEqual(os.readlink(auth), "/nonexistent/elsewhere.json")

    def test_links_users_auth_and_dir_is_private(self):
        src = os.path.join(self.home, ".pi", "agent")
        os.makedirs(src)
        with open(os.path.join(src, "auth.json"), "w") as fh:
            fh.write("x")
        d = self.managed()
        tools.ensure_pi_agent_dir(d)
        self.assertEqual(os.readlink(os.path.join(d, "auth.json")),
                         os.path.join(src, "auth.json"))
        self.assertEqual(os.stat(d).st_mode & 0o777, 0o700)

    def test_env_pointing_at_managed_dir_does_not_self_link(self):
        d = self.managed()
        tools.ensure_pi_agent_dir(d, env={"PI_CODING_AGENT_DIR": d})
        self.assertFalse(os.path.lexists(os.path.join(d, "auth.json")))

    def test_env_override_dir_is_the_link_source(self):
        other = os.path.join(self.home, "other")
        os.makedirs(other)
        with open(os.path.join(other, "auth.json"), "w") as fh:
            fh.write("x")
        d = self.managed()
        tools.ensure_pi_agent_dir(d, env={"PI_CODING_AGENT_DIR": other})
        self.assertEqual(os.readlink(os.path.join(d, "auth.json")),
                         os.path.join(other, "auth.json"))

    def test_user_mode_leaves_env_alone(self):
        cfg = {"external_executors": {"pi": {"agent_dir": "user"}}}
        self.assertEqual(tools.pi_env_overrides(cfg), {})
        self.assertFalse(os.path.exists(self.managed()))

    def test_sandbox_state_dirs_include_symlink_target_dir(self):
        src = os.path.join(self.home, "real")
        os.makedirs(src)
        with open(os.path.join(src, "auth.json"), "w") as fh:
            fh.write("x")
        d = self.managed()
        os.makedirs(d)
        os.symlink(os.path.join(src, "auth.json"), os.path.join(d, "auth.json"))
        env = {"PI_CODING_AGENT_DIR": d}
        self.assertEqual(sb.executor_state_dirs("pi", self.home, env), [d, src])
        policy = sb.build_policy(self.home, "pi", {}, home=self.home, env=env,
                                 learned=[])
        self.assertIn(src, policy["writable"])
        self.assertIn(os.path.join(src, "settings.json"), policy["deny_write"])


class SpawnTests(StateCase):
    def capture(self, cfg, pi_path):
        seen = {}

        def spawn(argv, **kw):
            seen.update(argv=argv, env=kw["env"])
            return 0, "", ""

        with mock.patch.object(agent_exec, "resolve_config", return_value=(cfg, None)), \
                mock.patch.object(agent_exec.agent_exec_tools, "resolve",
                                  return_value={"path": pi_path}), \
                mock.patch.object(agent_exec, "_spawn_capture", side_effect=spawn):
            agent_exec._run_pi_capture("pi", "m", "low", self.home, "hi", None)
        return seen

    def test_pi_capture_uses_resolved_path_and_managed_env(self):
        pi = self.make_bin("tools/pi")
        seen = self.capture(defaults_copy(), pi)
        self.assertEqual(seen["argv"][0], pi)
        self.assertEqual(seen["env"]["PI_CODING_AGENT_DIR"],
                         os.path.join(sb.state_dir(), "pi-agent"))

    def test_pi_capture_user_mode_does_not_set_agent_dir(self):
        cfg = defaults_copy()
        cfg["external_executors"]["pi"]["agent_dir"] = "user"
        seen = self.capture(cfg, self.make_bin("tools/pi"))
        self.assertNotIn("PI_CODING_AGENT_DIR", seen["env"])

    def test_context_files_false_adds_flag(self):
        argv = agent_exec._build_pi_argv("pi", None, None, "/w", "p", None, "json",
                                         context_files=False)
        self.assertIn("--no-context-files", argv)

    def test_doctor_ready_when_pi_managed_but_not_on_path(self):
        pi = self.make_bin("tools/pi")
        self.record(pi)
        with mock.patch.object(agent_exec.shutil, "which", return_value=None), \
                mock.patch.object(agent_exec, "resolve_config",
                                  return_value=(defaults_copy(), None)), \
                mock.patch.object(agent_exec, "_find_shim",
                                  return_value={"installed": True, "on_path": True}), \
                mock.patch.object(agent_exec, "_scan_permission_rule",
                                  return_value={"found": True}):
            report = agent_exec._build_doctor_report()
        self.assertTrue(report["ready"]["pi"]["ok"])
        self.assertEqual(report["executors"]["pi"]["source"], "managed")
        self.assertEqual(report["executors"]["pi"]["path"], pi)


if __name__ == "__main__":
    unittest.main()
