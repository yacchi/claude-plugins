"""Tests for the OS sandbox around CLI executor children
(agent_exec_sandbox.py and its plumbing in agent_exec.py)."""

import io
import json
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_exec  # noqa: E402
import agent_exec_sandbox as sb  # noqa: E402

OK = {"ok": True, "detail": "ok"}
NO = {"ok": False, "detail": "no"}


def _git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


def _repo_with_worktree(base):
    repo = os.path.join(base, "repo")
    os.makedirs(repo)
    _git(repo, "init", "-q")
    with open(os.path.join(repo, "a"), "w") as f:
        f.write("a")
    _git(repo, "add", "a")
    _git(repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "i")
    wt = os.path.join(base, "wt")
    _git(repo, "worktree", "add", "-q", wt)
    return os.path.realpath(repo), os.path.realpath(wt)


def _under_any(path, dirs):
    return any(path == d or path.startswith(d + "/") for d in dirs)


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.base = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.base, True)
        self.home = os.path.realpath(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.home, True)
        self.env = {"TMPDIR": "/nonexistent-tmp"}

    def _policy(self, tree, executor="pi", cfg=None, platform="linux"):
        return sb.build_policy(tree, executor, cfg or {}, home=self.home,
                               env=self.env, platform=platform)

    def test_worktree_run_gets_own_gitdir_and_objects_only(self):
        repo, wt = _repo_with_worktree(self.base)
        policy = self._policy(wt)
        writable = [w for w in policy["writable"]
                    if w not in ("/tmp", "/dev", "/nonexistent-tmp")]
        common = os.path.join(repo, ".git")
        self.assertIn(wt, writable)
        self.assertIn(os.path.join(common, "objects"), writable)
        self.assertIn(os.path.join(common, "worktrees", "wt"), writable)
        # Neither the main checkout nor the common .git (refs/config/hooks).
        self.assertFalse(_under_any(repo, writable))
        self.assertFalse(_under_any(os.path.join(common, "refs"), writable))
        self.assertFalse(_under_any(os.path.join(common, "config"), writable))
        self.assertFalse(_under_any(self.home, writable))

    def test_non_isolated_checkout_keeps_its_git_dir_read_only(self):
        repo, _ = _repo_with_worktree(self.base)
        policy = self._policy(repo)
        self.assertEqual(policy["readonly"], [os.path.join(repo, ".git")])

    def test_paths_are_realpathed(self):
        real = os.path.realpath(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, real, True)
        link = os.path.join(self.base, "link")
        os.symlink(real, link)
        self.env["TMPDIR"] = link
        policy = self._policy(link)
        self.assertEqual(policy["writable"][0], real)
        self.assertIn(real, policy["writable"])
        self.assertNotIn(link, policy["writable"])

    def test_pi_state_dir_is_writable(self):
        policy = self._policy(self.base)
        self.assertIn(os.path.join(self.home, ".pi", "agent"), policy["writable"])


class ConfigUnionTests(unittest.TestCase):
    def test_allow_write_and_deny_read_union_across_layers(self):
        d1 = tempfile.mkdtemp()
        d2 = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d1, True)
        self.addCleanup(shutil.rmtree, d2, True)
        p1 = os.path.join(d1, "orchestra.yaml")
        p2 = os.path.join(d2, "orchestra.yaml")
        with open(p1, "w") as f:
            f.write("sandbox:\n  allow_write: [~/a]\n  deny_read: [~/s1]\n")
        with open(p2, "w") as f:
            f.write("sandbox:\n  mode: off\n  allow_write: [~/b, ~/a]\n")
        with mock.patch.object(agent_exec, "_ordered_layer_paths", return_value=[p1, p2]):
            cfg, err = agent_exec.resolve_config()
        self.assertIsNone(err)
        self.assertEqual(cfg["sandbox"], {
            "mode": "off", "allow_write": ["~/a", "~/b"], "deny_read": ["~/s1"],
        })


class BackendSelectionTests(unittest.TestCase):
    def setUp(self):
        self.tree = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tree, True)

    def _prepare(self, probes, executor="pi", mode="auto"):
        return sb.prepare(executor, self.tree, {"mode": mode}, probe_results=probes)

    def test_failed_bwrap_probe_falls_back_to_landlock(self):
        spec = self._prepare({"seatbelt": NO, "bwrap": NO,
                              "landlock": {"ok": True, "abi": 4, "detail": ""}})
        self.assertEqual(spec["backend"], "landlock")
        # pi's config lives inside its writable state dir: Landlock cannot
        # deny inside an allowed subtree, so it is reported, not claimed.
        self.assertEqual(spec["limits"], ["deny_read", "signal", "pi-config"])
        argv = sb.wrap_argv(["pi", "-p"], self.tree, spec)
        self.assertEqual(argv[2], "_sandbox-exec")
        self.assertEqual(argv[-3:], ["--", "pi", "-p"])

    def test_bwrap_probe_runs_once_per_process(self):
        calls = []

        def fake_run(argv, **kw):
            calls.append(argv)
            return subprocess.CompletedProcess(argv, 1, "", "denied")
        with mock.patch.object(sb, "_PROBES", None), \
                mock.patch.object(sb.sys, "platform", "linux"), \
                mock.patch.object(sb.shutil, "which", return_value="/usr/bin/bwrap"), \
                mock.patch.object(sb.subprocess, "run", fake_run), \
                mock.patch.object(sb, "landlock_abi", return_value=3), \
                mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop(sb.FORCE_ENV, None)
            first = sb.probes()
            sb.probes()
        self.assertEqual(len(calls), 1)
        self.assertFalse(first["bwrap"]["ok"])
        self.assertTrue(first["landlock"]["ok"])

    def test_required_without_backend_refuses(self):
        spec = self._prepare({"seatbelt": NO, "bwrap": NO,
                              "landlock": dict(NO, abi=0)}, mode="required")
        self.assertTrue(spec["refuse"])
        auto = self._prepare({"seatbelt": NO, "bwrap": NO,
                              "landlock": dict(NO, abi=0)})
        self.assertFalse(auto["refuse"])
        self.assertEqual(auto["backend"], "none")

    def test_codex_is_never_wrapped(self):
        spec = self._prepare({"seatbelt": OK, "bwrap": OK, "landlock": OK},
                             executor="codex")
        self.assertEqual(spec["backend"], "codex-native")
        self.assertEqual(sb.wrap_argv(["codex", "exec"], self.tree, spec),
                         ["codex", "exec"])


class BwrapArgvTests(unittest.TestCase):
    def test_carve_out_and_deny_read_order(self):
        policy = {"writable": ["/r", "/r/.git/objects", "/dev", "/missing"],
                  "readonly": ["/r/.git"],
                  "deny_read": ["/h/.ssh", "/h/.netrc", "/h/none"]}
        exists = lambda p: p not in ("/missing", "/h/none")  # noqa: E731
        isdir = lambda p: p != "/h/.netrc"  # noqa: E731
        argv = sb._bwrap_argv(["pi"], "/r", policy, exists=exists, isdir=isdir)
        self.assertEqual(argv, [
            "bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
            "--bind", "/r", "/r",
            "--ro-bind", "/r/.git", "/r/.git",
            "--bind", "/r/.git/objects", "/r/.git/objects",
            "--tmpfs", "/h/.ssh",
            "--ro-bind", "/dev/null", "/h/.netrc",
            "--unshare-pid", "--die-with-parent", "--chdir", "/r", "--", "pi",
        ])


class FakeLibc:
    def __init__(self, abi):
        self.abi = abi
        self.calls = []

    def syscall(self, nr, *args):
        if nr == sb._SYS_CREATE_RULESET and args[0] is None:
            return self.abi
        if nr == sb._SYS_CREATE_RULESET:
            self.calls.append(("create", bytes(args[0].raw)[:args[1].value]))
            return 99
        if nr == sb._SYS_ADD_RULE:
            self.calls.append(("rule", bytes(args[2].raw)[:12]))
            return 0
        if nr == sb._SYS_RESTRICT_SELF:
            self.calls.append(("restrict",))
            return 0
        raise AssertionError(nr)

    def prctl(self, *args):
        self.calls.append(("prctl", args[0]))
        return 0


class LandlockTests(unittest.TestCase):
    def _restrict(self, abi):
        libc = FakeLibc(abi)
        sb.landlock_restrict(["/w"], libc=libc, abi=abi,
                             open_fn=lambda p, f: {"/": 3, "/w": 4}[p],
                             close_fn=lambda fd: None, isdir=lambda p: True)
        return libc.calls

    def test_abi6_scopes_signals_and_restricts_after_no_new_privs(self):
        calls = self._restrict(6)
        handled = sb.landlock_fs_rights(6)
        self.assertEqual(calls[0], ("create", struct.pack("=QQQ", handled, 0, 2)))
        self.assertEqual(calls[1], ("rule", struct.pack("=Qi", 0b1101, 3)))
        self.assertEqual(calls[2], ("rule", struct.pack("=Qi", handled, 4)))
        self.assertEqual([c[0] for c in calls[3:]], ["prctl", "restrict"])

    def test_old_abi_passes_only_known_fields(self):
        calls = self._restrict(1)
        self.assertEqual(calls[0], ("create", struct.pack("=Q", (1 << 13) - 1)))

    def test_parent_never_restricts_itself(self):
        spec = sb.prepare("pi", tempfile.gettempdir(), {"mode": "auto"},
                          probe_results={"seatbelt": NO, "bwrap": NO,
                                         "landlock": {"ok": True, "abi": 6, "detail": ""}})
        with mock.patch.object(sb, "landlock_restrict", side_effect=AssertionError):
            sb.wrap_argv(["pi"], tempfile.gettempdir(), spec)
        seen = {}
        rc = sb.sandbox_exec_main(
            ["--write", "/w", "--", "pi", "-p"],
            restrict=lambda w: seen.setdefault("writes", w),
            execvp=lambda f, a: seen.setdefault("exec", a),
        )
        self.assertEqual(seen, {"writes": ["/w"], "exec": ["pi", "-p"]})
        self.assertEqual(rc, 127)

    def test_child_fails_closed(self):
        def boom(_):
            raise OSError("nope")
        ran = []
        rc = sb.sandbox_exec_main(["--", "pi"], restrict=boom,
                                  execvp=lambda f, a: ran.append(a))
        self.assertEqual((rc, ran), (126, []))


@unittest.skipUnless(sys.platform == "darwin" and sb._probe_seatbelt()["ok"],
                     "needs a working sandbox-exec")
class SeatbeltEnforcementTests(unittest.TestCase):
    """Real enforcement under the generated SBPL: guards rule precedence and
    signal confinement, which a string comparison of the profile cannot."""

    def test_profile_enforces_write_read_and_signal_rules(self):
        base = os.path.realpath(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, base, True)
        tree, outside, secret = (os.path.join(base, n) for n in ("tree", "out", "secret"))
        for d in (tree, outside, secret):
            os.makedirs(d)
        with open(os.path.join(secret, "k"), "w") as f:
            f.write("k")
        policy = {"writable": [tree, "/dev"], "readonly": [],
                  "deny_read": [secret]}
        spec = {"backend": "seatbelt", "policy": policy}
        sleeper = subprocess.Popen(["sleep", "60"])
        self.addCleanup(sleeper.kill)
        script = (
            "echo x > %s/f && echo w-in;"
            "echo x > %s/f 2>/dev/null || echo w-out-denied;"
            "cat %s/k 2>/dev/null || echo r-denied;"
            "kill %d 2>/dev/null || echo kill-denied;"
            "sleep 30 & kill $! && echo kill-own"
        ) % (tree, outside, secret, sleeper.pid)
        out = subprocess.run(sb.wrap_argv(["/bin/sh", "-c", script], tree, spec),
                             capture_output=True, text=True).stdout.split()
        self.assertEqual(out, ["w-in", "w-out-denied", "r-denied", "kill-denied", "kill-own"])
        self.assertIsNone(sleeper.poll())


class DispatchPlumbingTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.prompt = os.path.join(self.tmp, "p.md")
        with open(self.prompt, "w") as f:
            f.write("hi")
        cfg = agent_exec.copy.deepcopy(agent_exec.DEFAULTS)
        cfg["sandbox"]["mode"] = "required"
        cfg["cooldown"]["path"] = os.path.join(self.tmp, "state.json")
        cfg["ledger"]["enabled"] = False
        patches = [
            mock.patch.object(agent_exec, "resolve_config", return_value=(cfg, None)),
            mock.patch.object(agent_exec, "_build_doctor_report", return_value={}),
            mock.patch.object(agent_exec, "resolve_route", return_value={
                "executor": "pi", "dispatch": "cli", "model": "m", "effort": "low",
                "agent_type": None}),
            mock.patch.dict(os.environ, {sb.FORCE_ENV: "none"}),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def test_required_mode_without_backend_never_spawns(self):
        buf = io.StringIO()
        with mock.patch.object(agent_exec, "_run_pi_capture",
                               side_effect=AssertionError("spawned")), \
                mock.patch.object(sys, "stdout", buf):
            rc = agent_exec.cmd_dispatch_route([
                "--class", "standard", "--prompt-file", self.prompt,
                "--workdir", self.tmp, "--isolate", "never",
            ])
        self.assertEqual(rc, 0)
        out = json.loads(buf.getvalue())
        self.assertEqual((out["status"], out["reason"]), ("unavailable", "sandbox"))
        self.assertEqual(out["sandbox"]["backend"], "none")
        self.assertFalse(os.path.exists(os.path.join(self.tmp, "state.json")))

    def test_run_exec_path_execs_the_wrapped_argv(self):
        os.environ.pop(sb.FORCE_ENV)
        spec = {"backend": "seatbelt", "enforced": True, "reason": "", "limits": [],
                "writable": [], "refuse": False,
                "policy": {"writable": [self.tmp], "readonly": [], "deny_read": []}}
        seen = {}
        with mock.patch.object(agent_exec, "_sandbox_spec", return_value=spec), \
                mock.patch.object(agent_exec.shutil, "which", return_value="/bin/pi"), \
                mock.patch.object(agent_exec.os, "execvpe",
                                  side_effect=lambda f, a, e: seen.update(file=f, argv=a)):
            agent_exec.cmd_run(["pi", "--model", "m", "--effort", "low",
                                "--workdir", self.tmp, "--prompt-file", self.prompt])
        self.assertEqual(seen["file"], sb.SANDBOX_EXEC)
        self.assertEqual(seen["argv"][0], sb.SANDBOX_EXEC)
        self.assertIn("pi", seen["argv"])


if __name__ == "__main__":
    unittest.main()
